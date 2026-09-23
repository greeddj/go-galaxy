// Package extracted is the content-addressed store of unpacked tarballs: each
// sha256 is extracted once, then hard-linked (or copied) into installs. Its
// writes resolve through an os.Root at the cache directory, never one lower.
package extracted

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// RootDirName is the directory name under cacheDir holding extracted entries.
	RootDirName = "extracted"
	// ReadyMarker is the file written last to mark a complete extraction.
	ReadyMarker = ".ready"
	tmpSuffix   = ".tmp"
	// ingestPrefix names the temp directories IngestReader creates under the
	// store root before a stream is finalized via Promote.
	ingestPrefix = "ingest-"
	// ingestNameAttempts bounds how many random names mkdirTemp tries before
	// giving up, matching os.MkdirTemp's own retry discipline for a name
	// collision.
	ingestNameAttempts = 10000

	// ReadyMarkerPayload is the only ReadyMarker content isReady accepts. It is
	// a version tag: bumping it makes every existing tree fail isReady and
	// rebuild once, so a tree extracted before a hardening change is not reused.
	ReadyMarkerPayload = "ro1"
	// readyMarkerMaxReadSize bounds the read of a ready marker; a longer file
	// is corrupt or hostile and is rejected without learning its real length.
	readyMarkerMaxReadSize = 64
)

var (
	// ErrStoreNotConfigured indicates the store has no cache directory.
	ErrStoreNotConfigured = errors.New("extracted store is not configured")
	// ErrSHAEmpty indicates a missing SHA256 identifier.
	ErrSHAEmpty = errors.New("artifact sha is empty")
	// ErrSHAUnsafe indicates a SHA256 identifier that is not safe to use as a
	// single path element - see entryRel's own doc comment for where such a
	// value can originate.
	ErrSHAUnsafe = errors.New("artifact sha is not a single path element")
	// ErrTempOutsideStore indicates a temp path handed to Promote or Discard
	// outside the store's cache directory; both RemoveAll it, so the refusal
	// keeps them from being aimed at an arbitrary path.
	ErrTempOutsideStore = errors.New("temp path is outside the extracted store")
	// ErrStoreDirUnusable indicates the store directory is a symlink leading
	// out of the cache directory or a non-directory, both of which the
	// containment root reports only as a bare "file exists".
	ErrStoreDirUnusable = errors.New("extracted store directory is not usable")
	// errIngestTempExhausted indicates mkdirTemp could not find an unused
	// name. It is unexported because no caller can act on it differently from
	// any other ingest failure.
	errIngestTempExhausted = errors.New("could not create an ingest temp directory")
)

// SHAProvenance states how the sha handed to Ensure relates to tarPath's
// bytes, deciding whether Ensure hashes the file first. The zero value is
// SHAFromRecord, so an undeclared provenance verifies rather than trusts.
type SHAProvenance int

const (
	// SHAFromRecord marks a sha read back from an earlier record (a sidecar,
	// the snapshot, server metadata) that this process never checked against
	// tarPath; Ensure hashes the file before extracting it into the store.
	SHAFromRecord SHAProvenance = iota
	// SHASelfComputed marks a sha this process computed over tarPath's bytes
	// itself, as a hashing download does, so Ensure ingests without a re-read.
	SHASelfComputed
)

// Store materializes tarballs once per SHA and reuses them via hardlinks.
type Store struct {
	locks    map[string]*sync.Mutex
	cacheDir string
	mu       sync.Mutex
}

// NewStore returns a Store rooted at cacheDir/extracted, or nil if cacheDir
// is empty (in which case callers should fall back to direct extraction).
func NewStore(cacheDir string) *Store {
	if cacheDir == "" {
		return nil
	}
	return &Store{
		cacheDir: cacheDir,
		locks:    make(map[string]*sync.Mutex),
	}
}

// Root returns the on-disk root of the store. It is a display and test
// affordance only: nothing in this package resolves a path by joining onto
// it, since every real operation goes through the containment root instead.
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return filepath.Join(s.cacheDir, RootDirName)
}

// Ensure extracts tarPath under sha unless that tree is already ready, and
// returns the tree's path; concurrent callers for one sha share the work.
// prov decides whether tarPath is hashed first (see SHAProvenance).
func (s *Store) Ensure(ctx context.Context, sha, tarPath string, prov SHAProvenance) (string, error) {
	if s == nil {
		return "", ErrStoreNotConfigured
	}
	if sha == "" {
		return "", ErrSHAEmpty
	}
	rel, ok := entryRel(sha)
	if !ok {
		return "", ErrSHAUnsafe
	}
	final := s.abs(rel)
	if s.readyRel(rel) {
		return final, nil
	}

	lock := s.lockFor(sha)
	lock.Lock()
	defer lock.Unlock()

	if s.readyRel(rel) {
		return final, nil
	}
	// A recorded sha must match the bytes before they are keyed under it, or
	// every project referencing that sha would get content not hashing to it.
	// Only this CAS-absent path pays the read, at most once per sha.
	if prov != SHASelfComputed {
		if err := verifyTarballSHA(tarPath, sha); err != nil {
			return "", err
		}
	}
	return s.extractInto(ctx, rel, tarPath)
}

// Ready reports whether sha's tree is present and finalized, changing nothing.
// It checks the marker's exact payload through isReady, as Ensure does, so a
// legacy tree Ensure would rebuild is not ready; an unsafe sha reports false.
func (s *Store) Ready(sha string) bool {
	if s == nil {
		return false
	}
	rel, ok := entryRel(sha)
	return ok && s.readyRel(rel)
}

// verifyTarballSHA reports nil when tarPath hashes to sha, and otherwise an
// error wrapping helpers.ErrSHA256Mismatch, the install path's integrity class.
func verifyTarballSHA(tarPath, sha string) error {
	actual, err := archive.FileHashSHA256(tarPath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, sha) {
		return fmt.Errorf("%w: %s: %s != %s", helpers.ErrSHA256Mismatch, tarPath, actual, sha)
	}
	return nil
}

// IngestReader extracts a tar.gz stream into a fresh temp directory under the
// store, to be finalized by Promote or dropped by Discard. It always drains r
// to EOF, so an upstream io.Pipe writer cannot deadlock on an early gzip end.
func (s *Store) IngestReader(ctx context.Context, r io.Reader) (string, error) {
	if s == nil {
		_, _ = io.Copy(io.Discard, r)
		return "", ErrStoreNotConfigured
	}
	root, err := s.openRootForWrite()
	if err != nil {
		_, _ = io.Copy(io.Discard, r)
		return "", err
	}
	defer func() { _ = root.Close() }()

	tmpRel, err := mkdirTemp(root)
	if err != nil {
		_, _ = io.Copy(io.Discard, r)
		return "", err
	}
	extractErr := archive.ExtractTarGzStream(ctx, r, s.abs(tmpRel))
	_, _ = io.Copy(io.Discard, r)
	if extractErr != nil {
		_ = root.RemoveAll(tmpRel)
		return "", extractErr
	}
	return s.abs(tmpRel), nil
}

// Promote renames an IngestReader temp tree into sha's entry, or drops it when
// sha is already finalized. sha is trusted unhashed, so it must be hashed from
// the streamed bytes; a bad sha is refused before tmpRoot is examined.
func (s *Store) Promote(tmpRoot, sha string) (string, error) {
	if s == nil {
		return "", ErrStoreNotConfigured
	}
	if sha == "" {
		_ = s.Discard(tmpRoot)
		return "", ErrSHAEmpty
	}
	rel, ok := entryRel(sha)
	if !ok {
		_ = s.Discard(tmpRoot)
		return "", ErrSHAUnsafe
	}

	root, err := s.openRootForWrite()
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()

	tmpRel, ok := s.rel(tmpRoot)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrTempOutsideStore, tmpRoot)
	}

	lock := s.lockFor(sha)
	lock.Lock()
	defer lock.Unlock()

	if isReady(root, rel) {
		_ = root.RemoveAll(tmpRel)
		return s.abs(rel), nil
	}
	if err := writeReadyMarker(root, tmpRel); err != nil {
		_ = root.RemoveAll(tmpRel)
		return "", err
	}
	return s.finalize(root, tmpRel, rel)
}

// Discard removes a temp tree IngestReader created, refusing any path outside
// the store's cache directory rather than turning RemoveAll loose on it.
func (s *Store) Discard(tmpRoot string) error {
	if s == nil || tmpRoot == "" {
		return nil
	}
	tmpRel, ok := s.rel(tmpRoot)
	if !ok {
		return fmt.Errorf("%w: %s", ErrTempOutsideStore, tmpRoot)
	}
	root, err := s.openRoot()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()
	return root.RemoveAll(tmpRel)
}

// Remove deletes sha's extracted entry, best-effort. An empty or unsafe sha
// removes nothing and returns nil, so nil does not mean the entry is gone.
func (s *Store) Remove(sha string) error {
	if s == nil || sha == "" {
		return nil
	}
	rel, ok := entryRel(sha)
	if !ok {
		return nil
	}
	root, err := s.openRoot()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()
	return root.RemoveAll(rel)
}

// SweepPlan lists, sorted, the entries Sweep would remove because keep lacks
// them, mutating nothing, which is what a dry-run reports. A missing store
// directory yields (nil, nil), as Sweep treats it.
func (s *Store) SweepPlan(keep map[string]bool) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	root, err := s.openRoot()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = root.Close() }()

	entries, exists, err := readStoreDir(root)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	planned := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if keep[name] {
			continue
		}
		planned = append(planned, name)
	}
	slices.Sort(planned)
	return planned, nil
}

// Sweep removes entries not in keep, going on past a failed removal and
// returning the first. ctx is checked before each entry and its error wins,
// so a cleanup that lost the cache lock stops before the next tree.
func (s *Store) Sweep(ctx context.Context, keep map[string]bool) error {
	if s == nil {
		return nil
	}
	planned, err := s.SweepPlan(keep)
	if err != nil {
		return err
	}
	if len(planned) == 0 {
		return nil
	}

	root, err := s.openRoot()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()

	var firstErr error
	for _, name := range planned {
		if err := ctx.Err(); err != nil {
			return err
		}
		if removeErr := root.RemoveAll(path.Join(RootDirName, name)); removeErr != nil && firstErr == nil {
			firstErr = removeErr
		}
	}
	return firstErr
}

// SweepTemp removes the "ingest-" and "<sha>.tmp" temp directories a killed
// run left under the store, never a finalized entry. The caller must hold the
// cache lock, or a live run's in-flight temps would match too.
func (s *Store) SweepTemp() error {
	if s == nil {
		return nil
	}
	root, err := s.openRoot()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()

	entries, _, err := readStoreDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !isTempEntryName(name) {
			continue
		}
		if err := root.RemoveAll(path.Join(RootDirName, name)); err != nil {
			return err
		}
	}
	return nil
}

// isTempEntryName reports whether name is one of the two temp forms the store
// creates under its root, as opposed to a finalized CAS tree named by a bare
// sha.
func isTempEntryName(name string) bool {
	return strings.HasPrefix(name, ingestPrefix) || strings.HasSuffix(name, tmpSuffix)
}

// entryRel returns sha's tree as a root-relative slash path, or ok=false when
// sha, which may come from a lockfile pin or snapshot record, is not one path
// element. It runs before the root opens, so a bad sha never touches the disk.
func entryRel(sha string) (string, bool) {
	if !helpers.IsPathElement(sha) {
		return "", false
	}
	return path.Join(RootDirName, sha), true
}

// abs turns a root-relative slash path into the absolute path callers outside
// this package receive.
func (s *Store) abs(rel string) string {
	return filepath.Join(s.cacheDir, filepath.FromSlash(rel))
}

// rel is abs's inverse, refusing a path outside the cache directory. The check
// is lexical, a precise early refusal; containment itself is the root's job.
func (s *Store) rel(abs string) (string, bool) {
	relPath, err := filepath.Rel(s.cacheDir, abs)
	if err != nil {
		return "", false
	}
	if relPath == "." || relPath == ".." || strings.HasPrefix(relPath, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return filepath.ToSlash(relPath), true
}

// openRoot opens the containment root without creating the cache directory,
// so a read-only caller on a missing cache gets fs.ErrNotExist to degrade on.
func (s *Store) openRoot() (*os.Root, error) {
	return os.OpenRoot(s.cacheDir)
}

// openRootForWrite creates the cache and store directories and opens the
// root. The cache directory's own MkdirAll is unrooted: it is the boundary.
func (s *Store) openRootForWrite() (*os.Root, error) {
	if err := os.MkdirAll(s.cacheDir, helpers.DirMod); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s.cacheDir)
	if err != nil {
		return nil, err
	}
	if err := root.MkdirAll(RootDirName, helpers.DirMod); err != nil {
		classified := classifyStoreDirError(root, err)
		_ = root.Close()
		return nil, classified
	}
	return root, nil
}

// classifyStoreDirError turns the root's bare "file exists" for an escaping
// symlink or a non-directory at the store name into ErrStoreDirUnusable. Any
// other error, such as a permission problem, is returned unchanged.
func classifyStoreDirError(root *os.Root, err error) error {
	info, statErr := root.Lstat(RootDirName)
	if statErr != nil {
		return err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%w: %s is a symlink that does not resolve to a directory inside the cache directory",
			ErrStoreDirUnusable, RootDirName)
	case !info.IsDir():
		return fmt.Errorf("%w: %s is not a directory", ErrStoreDirUnusable, RootDirName)
	default:
		return err
	}
}

// readStoreDir lists the store directory, reporting exists=false when it is
// absent, which is not a failure and differs from empty; other errors are
// classified.
func readStoreDir(root *os.Root) ([]fs.DirEntry, bool, error) {
	entries, err := fs.ReadDir(root.FS(), RootDirName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, classifyStoreDirError(root, err)
	}
	return entries, true, nil
}

// readyRel reports whether the tree at rel carries a current ready marker,
// opening its own root; a missing cache directory reports false.
func (s *Store) readyRel(rel string) bool {
	root, err := s.openRoot()
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	return isReady(root, rel)
}

// extractInto unpacks tarPath into a temp tree beside rel and renames it in.
// The untar gets a plain path: the root just created the temp, so nothing is
// pre-planted in it, and archive checks each entry's symlink parents.
func (s *Store) extractInto(ctx context.Context, rel, tarPath string) (string, error) {
	root, err := s.openRootForWrite()
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()

	tmpRel := rel + tmpSuffix
	// Checked, not discarded: this and the RemoveAll in finalize are the two
	// destructive primitives here, and a refusal by the root is exactly the
	// signal that something under the cache directory is redirecting them.
	if err := root.RemoveAll(tmpRel); err != nil {
		return "", err
	}
	if err := root.MkdirAll(tmpRel, helpers.DirMod); err != nil {
		return "", err
	}
	if err := archive.ExtractTarGz(ctx, tarPath, s.abs(tmpRel)); err != nil {
		_ = root.RemoveAll(tmpRel)
		return "", err
	}
	if err := writeReadyMarker(root, tmpRel); err != nil {
		_ = root.RemoveAll(tmpRel)
		return "", err
	}
	return s.finalize(root, tmpRel, rel)
}

// finalize replaces the tree at rel with the finished temp tree at tmpRel,
// cleaning the temp up on either failure so a refused promotion never leaves
// a half-named directory behind for SweepTemp to find later.
func (s *Store) finalize(root *os.Root, tmpRel, rel string) (string, error) {
	if err := root.RemoveAll(rel); err != nil {
		_ = root.RemoveAll(tmpRel)
		return "", err
	}
	if err := root.Rename(tmpRel, rel); err != nil {
		_ = root.RemoveAll(tmpRel)
		return "", err
	}
	return s.abs(rel), nil
}

// mkdirTemp is os.MkdirTemp through root, returning the root-relative path.
// Root.Mkdir refuses an existing name and never follows a planted symlink, so
// retrying on fs.ErrExist is safe; random names keep collisions rare.
func mkdirTemp(root *os.Root) (string, error) {
	for range ingestNameAttempts {
		rel := path.Join(RootDirName, ingestPrefix+rand.Text())
		err := root.Mkdir(rel, helpers.DirMod)
		if err == nil {
			return rel, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", errIngestTempExhausted
}

func (s *Store) lockFor(sha string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, ok := s.locks[sha]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[sha] = lock
	}
	return lock
}

// writeReadyMarker writes ReadyMarkerPayload into dirRel's ready marker after
// removing any file there: a tarball may ship its own ".ready", extracted
// read-only, which a plain WriteFile would fail to truncate with EACCES.
func writeReadyMarker(root *os.Root, dirRel string) error {
	p := path.Join(dirRel, ReadyMarker)
	if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return root.WriteFile(p, []byte(ReadyMarkerPayload), helpers.FileMod)
}

// readReadyMarker reads rel capped at readyMarkerMaxReadSize, reporting
// ok=false for a missing or unreadable file and for one longer than the cap.
func readReadyMarker(root *os.Root, rel string) (string, bool) {
	f, err := root.Open(rel)
	if err != nil {
		return "", false
	}
	defer func() {
		_ = f.Close()
	}()

	buf := make([]byte, readyMarkerMaxReadSize+1)
	n, err := io.ReadFull(f, buf)
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return string(buf[:n]), true
	default:
		return "", false
	}
}

// isReady reports whether dirRel holds a finalized tree whose marker is exactly
// ReadyMarkerPayload. A legacy marker means writable files, so that tree is
// rebuilt rather than hard-linked into installs.
func isReady(root *os.Root, dirRel string) bool {
	content, ok := readReadyMarker(root, path.Join(dirRel, ReadyMarker))
	return ok && content == ReadyMarkerPayload
}

// Materialize mirrors srcRoot into dstRoot by hardlink, copying what cannot be
// linked, and skips the root ReadyMarker. dstRoot's MkdirAll and the directory
// arm of materializeEntry create every parent a file or symlink is written to.
func Materialize(srcRoot, dstRoot string) error {
	if err := os.MkdirAll(dstRoot, helpers.DirMod); err != nil {
		return err
	}
	return filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == ReadyMarker {
			return nil
		}
		dst := filepath.Join(dstRoot, rel)
		return materializeEntry(path, dst, d)
	})
}

func materializeEntry(src, dst string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	mode := info.Mode()
	switch {
	case d.IsDir():
		return os.MkdirAll(dst, mode.Perm())
	case mode&fs.ModeSymlink != 0:
		return materializeSymlink(src, dst)
	case mode.IsRegular():
		return materializeFile(src, dst, mode.Perm())
	default:
		return nil
	}
}

func materializeSymlink(src, dst string) error {
	target, err := os.Readlink(src)
	if err != nil {
		return err
	}
	_ = os.Remove(dst)
	return os.Symlink(target, dst)
}

// materializeFile links src into dst, copying when the link fails. The remove
// first is checked: any failure but fs.ErrNotExist would otherwise surface
// later as a confusing EEXIST or EISDIR instead of its real cause.
func materializeFile(src, dst string, perm os.FileMode) error {
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst, perm)
}

// copyFile is materializeFile's byte-copy fallback. The Chmod after the copy
// re-asserts perm past the umask, keeping installed mode equal to CAS mode;
// on the open descriptor it works even when perm has no write bit.
func copyFile(src, dst string, perm os.FileMode) error {
	//nolint:gosec // src comes from the trusted extracted store.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	//nolint:gosec // dst is derived from a sanitized archive path under dstRoot.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Chmod(perm); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
