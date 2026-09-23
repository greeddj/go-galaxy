// Package archive unpacks and shape-probes tar.gz artifacts, refusing by
// sentinel escaping paths and symlinks, duplicate entries and anything past the
// helpers caps. Nothing here uses os.Root: the caller contains the destination.
package archive

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/gzipstream"
)

// The probe's decompressor holds one 64 KiB block, not pgzip's 4 x 1 MiB: it is
// the only one the download pool opens, so it stays out of the install memory
// budget. A block size of 512 or less makes pgzip fall back to its 1 MiB.
const (
	probeGzipBlockSize = 64 << 10
	probeGzipBlocks    = 1
)

// ExtractTarGz extracts a tar.gz archive into dstDir with safety checks. ctx
// is checked on every read on both sides of the decompressor, and a fired check
// keeps context.Canceled reachable so the run exits as interrupted.
func ExtractTarGz(ctx context.Context, tarGzFile, dstDir string) error {
	info, err := os.Stat(tarGzFile)
	if err != nil {
		return fmt.Errorf("failed to stat file %s: %w", tarGzFile, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%w: %s", helpers.ErrFileIsEmpty, tarGzFile)
	}

	//nolint:gosec // tarGzFile is a user-provided archive path expected by CLI.
	file, err := os.Open(tarGzFile)
	if err != nil {
		return fmt.Errorf("failed to open tar.gz file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	return ExtractTarGzStream(ctx, file, dstDir)
}

// ExtractTarGzStream extracts a tar.gz stream into dstDir with safety checks.
// It does not close r. ctx bounds the unpack, as described on ExtractTarGz.
func ExtractTarGzStream(ctx context.Context, r io.Reader, dstDir string) error {
	return extractTarGzStream(ctx, r, dstDir, helpers.ArchiveMaxDecompressedSize)
}

// extractTarGzStream is ExtractTarGzStream with the decompressed-stream cap
// injected for tests. That cap is the real decompression-bomb bound, applied
// here to count the bytes archive/tar consumes without returning a header.
func extractTarGzStream(ctx context.Context, r io.Reader, dstDir string, maxDecompressed int64) error {
	uncompressedStream, err := gzipstream.NewReader(ctx, r)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() {
		_ = uncompressedStream.Close()
	}()

	// A reader rather than a per-entry check, so one huge entry is interruptible
	// mid-body; internal/gzipstream watches the compressed side.
	limited := &decompressedLimitReader{r: uncompressedStream, over: helpers.ErrArchiveDecompressedTooLarge, max: maxDecompressed}
	tarReader := tar.NewReader(&contextReader{ctx: ctx, r: limited})
	return extractTarEntries(tarReader, dstDir, helpers.ArchiveMaxEntryCount)
}

// contextReader fails a read once ctx is done, returning ctx.Err() unwrapped so
// exitcode can classify it. Like decompressedLimitReader, it must not Seek.
//
//nolint:containedctx // io.Reader cannot take a context per call
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// decompressedLimitReader fails with over once more than max bytes leave the
// decompressor; helpers.NewSizeLimitedReader would report a network fault. It
// must not implement io.Seeker, or archive/tar would skip bodies unseen.
type decompressedLimitReader struct {
	r    io.Reader
	err  error
	over error
	max  int64
	n    int64
}

// Read fails with over once the count passes max. The crossing call returns
// zero bytes, since io.ReadAtLeast and io.CopyN drop an error paired with a
// satisfied request; the error is sticky and p is clamped to overrun one byte.
func (r *decompressedLimitReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	// Keeps p[:remaining+1] in range: a max below zero drives remaining
	// negative, and at -2 or lower that reslice panics.
	remaining := max(r.max-r.n, 0)
	// Taking this branch means remaining < len(p), hence remaining+1 <= len(p),
	// so the reslice is always within p.
	if remaining < int64(len(p)) {
		p = p[:remaining+1]
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	if r.n > r.max {
		r.err = fmt.Errorf("%w: read %d bytes, limit is %d bytes", r.over, r.n, r.max)
		return 0, r.err
	}
	return n, err
}

func extractTarEntries(tarReader *tar.Reader, dstDir string, maxEntries int64) error {
	var declared, entries int64
	// verifiedDirs memoizes components proven real directories in this
	// extraction; it is never shared, so a plain map needs no locking.
	verifiedDirs := make(map[string]struct{})
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("error reading tar archive: %w", err)
		}
		// entries counts every header, whatever its typeflag, so a tarbomb of
		// zero-byte entries is refused. It bounds headers, not inodes; the byte
		// bounds are chargeEntrySize and decompressedLimitReader.
		entries++
		if entries > maxEntries {
			return fmt.Errorf("%w: %d", helpers.ErrArchiveTooManyEntries, maxEntries)
		}
		if err := chargeEntrySize(header, &declared); err != nil {
			return err
		}
		if err := handleTarEntry(tarReader, header, dstDir, verifiedDirs); err != nil {
			return err
		}
	}
}

// chargeEntrySize charges header.Size against the per-entry and total budgets
// for every typeflag. Sparse and 'x'/'L'/'K' headers make header.Size no bound
// on bytes read, so this refuses early; decompressedLimitReader is the bound.
func chargeEntrySize(header *tar.Header, declared *int64) error {
	if header.Size < 0 {
		return fmt.Errorf("%w: %q", helpers.ErrArchiveEntryHasNegativeSize, header.Name)
	}
	if header.Size > helpers.ArchiveMaxEntrySize {
		return fmt.Errorf("%w: %q: %d bytes", helpers.ErrArchiveEntryIsTooLarge, header.Name, header.Size)
	}
	if *declared+header.Size > helpers.ArchiveMaxTotalSize {
		return fmt.Errorf("%w: %d bytes", helpers.ErrArchiveExceedsMaxSize, helpers.ArchiveMaxTotalSize)
	}
	*declared += header.Size
	return nil
}

func handleTarEntry(tarReader *tar.Reader, header *tar.Header, dstDir string, verifiedDirs map[string]struct{}) error {
	relPath, err := sanitizeArchivePath(header.Name)
	if err != nil {
		return err
	}
	if relPath == "" {
		return nil
	}
	targetPath := filepath.Join(dstDir, relPath)
	if err := ensureNoSymlinkParents(dstDir, relPath, verifiedDirs); err != nil {
		return err
	}

	switch header.Typeflag {
	case tar.TypeDir:
		return extractDir(targetPath, verifiedDirs)
	case tar.TypeReg:
		return extractRegularFile(tarReader, header, targetPath, verifiedDirs)
	case tar.TypeSymlink:
		return extractSymlink(relPath, targetPath, header, verifiedDirs)
	case tar.TypeLink:
		return extractHardlink(dstDir, targetPath, header, verifiedDirs)
	default:
		// Skipped, not refused: the entry was already counted and charged, and
		// its bytes pass decompressedLimitReader (see chargeEntrySize).
		return nil
	}
}

func extractDir(targetPath string, verifiedDirs map[string]struct{}) error {
	if err := ensureDir(targetPath, verifiedDirs); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", targetPath, err)
	}
	return nil
}

// ensureDir creates dir unless this extraction already proved it a real
// directory, then memoizes it. Callers must run ensureNoSymlinkParents on the
// entry first, or a symlink os.MkdirAll found would be memoized as a directory.
func ensureDir(dir string, verifiedDirs map[string]struct{}) error {
	if _, ok := verifiedDirs[dir]; ok {
		return nil
	}
	if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
		return err
	}
	verifiedDirs[dir] = struct{}{}
	return nil
}

func extractRegularFile(tarReader *tar.Reader, header *tar.Header, targetPath string, verifiedDirs map[string]struct{}) error {
	if err := ensureDir(filepath.Dir(targetPath), verifiedDirs); err != nil {
		return fmt.Errorf("failed to create directories for %s: %w", targetPath, err)
	}
	// Strip write bits at creation: extracted files are hard-linked into every
	// install, so masking the mode anywhere downstream would alias a write.
	mode := helpers.ReadOnlyPerm(header.FileInfo().Mode().Perm())
	//nolint:gosec // targetPath is sanitized archive entry under dstDir.
	file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return classifyOpenRegularFileError(targetPath, header.Name, err)
	}
	if _, err := io.CopyN(file, tarReader, header.Size); err != nil {
		_ = file.Close()
		return fmt.Errorf("failed to write file %s: %w", targetPath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close file %s: %w", targetPath, err)
	}
	return nil
}

// classifyOpenRegularFileError reports a failed open over a path that already
// exists as helpers.ErrArchiveDuplicateEntry: read-only extraction turns a
// second entry for one path into a bare EACCES (or EISDIR over a directory).
func classifyOpenRegularFileError(targetPath, entryName string, openErr error) error {
	if _, statErr := os.Stat(targetPath); statErr == nil {
		return fmt.Errorf("%w: %s", helpers.ErrArchiveDuplicateEntry, entryName)
	}
	return fmt.Errorf("failed to create file %s: %w", targetPath, openErr)
}

func extractSymlink(relPath, targetPath string, header *tar.Header, verifiedDirs map[string]struct{}) error {
	linkTarget, err := safeSymlinkTarget(relPath, header.Linkname)
	if err != nil {
		return err
	}
	if err := ensureDir(filepath.Dir(targetPath), verifiedDirs); err != nil {
		return fmt.Errorf("failed to create directories for %s: %w", targetPath, err)
	}
	if err := os.Symlink(linkTarget, targetPath); err != nil {
		return fmt.Errorf("failed to create symlink %s -> %s: %w", targetPath, linkTarget, err)
	}
	return nil
}

func extractHardlink(dstDir, targetPath string, header *tar.Header, verifiedDirs map[string]struct{}) error {
	linkRel, err := sanitizeArchivePath(header.Linkname)
	if err != nil {
		return err
	}
	if linkRel == "" {
		return fmt.Errorf("%w for %s", helpers.ErrHardlinkTargetIsEmpty, header.Name)
	}
	if err := ensureNoSymlinkParents(dstDir, linkRel, verifiedDirs); err != nil {
		return err
	}
	target := filepath.Join(dstDir, linkRel)
	if err := ensureDir(filepath.Dir(targetPath), verifiedDirs); err != nil {
		return fmt.Errorf("failed to create directories for %s: %w", targetPath, err)
	}
	if err := os.Link(target, targetPath); err != nil {
		return fmt.Errorf("failed to create hardlink %s -> %s: %w", targetPath, target, err)
	}
	return nil
}

// ProbeTarGz reports whether path is gzip holding a tar stream, stopping at the
// first tar header within helpers.ArchiveProbeMaxBytes. It keeps error pages
// out of a shared cache slot; truncation or corruption past that goes unseen.
func ProbeTarGz(ctx context.Context, path string) error {
	//nolint:gosec // path is an artifact temp file this process just wrote.
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open artifact for a shape probe: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	gz, err := gzipstream.NewReaderN(ctx, file, probeGzipBlockSize, probeGzipBlocks)
	if err != nil {
		return notTarGzError(path, err)
	}
	defer func() {
		_ = gz.Close()
	}()

	limited := &decompressedLimitReader{r: gz, over: helpers.ErrArtifactTarHeaderNotFound, max: helpers.ArchiveProbeMaxBytes}
	if _, err := tar.NewReader(limited).Next(); err != nil && !errors.Is(err, io.EOF) {
		// A crossed scan bound gets its own headline: the stream is gzip and tar,
		// so calling it neither would mislead the operator.
		if errors.Is(err, helpers.ErrArtifactTarHeaderNotFound) {
			return fmt.Errorf("%w: %s", helpers.ErrArtifactTarHeaderNotFound, path)
		}
		return notTarGzError(path, err)
	}
	return nil
}

// notTarGzError wraps a shape failure in helpers.ErrArtifactNotTarGz with the
// path, except the caller's own cancellation or deadline, returned unchanged
// so an interrupted probe is not reported as a bad artifact.
func notTarGzError(path string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %s: %w", helpers.ErrArtifactNotTarGz, path, err)
}

// FileHashSHA256 calculates the SHA256 hash of a file on disk.
func FileHashSHA256(path string) (string, error) {
	//nolint:gosec // path is caller-provided and expected for hashing.
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
	}()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sanitizeArchivePath validates and normalizes a tar entry path.
func sanitizeArchivePath(name string) (string, error) {
	if name == "" {
		return "", helpers.ErrArchiveEntryHasEmptyName
	}
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if cleaned == "." {
		return "", nil
	}
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("%w: %s", helpers.ErrArchiveEntryIsAbsolutePath, name)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %s", helpers.ErrArchiveEntryEscapesDestination, name)
	}
	return cleaned, nil
}

// ensureNoSymlinkParents refuses relPath when any existing component is a
// symlink, skipping memoized directories. The memo is sound only while
// extraction never replaces an existing path with another kind of object.
func ensureNoSymlinkParents(baseDir, relPath string, verifiedDirs map[string]struct{}) error {
	// Unreachable through sanitizeArchivePath; kept for an unsanitized caller.
	if relPath == "" || relPath == "." {
		return nil
	}
	current := baseDir
	for part := range strings.SplitSeq(relPath, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := checkPathComponentNotSymlink(current, verifiedDirs); err != nil {
			return err
		}
	}
	return nil
}

// checkPathComponentNotSymlink Lstats one component for ensureNoSymlinkParents
// and memoizes it only when it is a real directory, never a symlink.
func checkPathComponentNotSymlink(current string, verifiedDirs map[string]struct{}) error {
	if _, ok := verifiedDirs[current]; ok {
		return nil
	}
	info, err := os.Lstat(current)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to stat path %s: %w", current, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", helpers.ErrArchivePathContainsSymlinkComponent, current)
	}
	if info.IsDir() {
		verifiedDirs[current] = struct{}{}
	}
	return nil
}

// safeSymlinkTarget validates a symlink target within the archive.
func safeSymlinkTarget(relPath, linkName string) (string, error) {
	if linkName == "" {
		return "", fmt.Errorf("%w for %s", helpers.ErrSymlinkTargetIsEmpty, relPath)
	}
	if filepath.IsAbs(linkName) || filepath.VolumeName(linkName) != "" {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetIsAbsolute, linkName)
	}
	cleaned := filepath.Clean(filepath.FromSlash(linkName))
	if cleaned == "." {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTarget, linkName)
	}
	baseDir := filepath.Dir(relPath)
	resolved := filepath.Clean(filepath.Join(baseDir, cleaned))
	if resolved == "." {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetResolvesToRoot, linkName)
	}
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetEscapesDestination, linkName)
	}
	relTarget, err := filepath.Rel(baseDir, resolved)
	if err != nil {
		return "", err
	}
	if relTarget == "." {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetResolvesToSelf, linkName)
	}
	return relTarget, nil
}
