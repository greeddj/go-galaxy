package extracted

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// helloFileContent is the shared "foo.txt" fixture body, one constant so its
// uses as tarball content and expected read-back do not look coincidental.
const helloFileContent = "hello"

func TestStoreEnsureExtractsOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{
		"foo.txt":     helloFileContent,
		"sub/bar.txt": "world",
	})

	store := NewStore(filepath.Join(dir, "cache"))
	sha := mustHash(t, tarPath)

	got, err := store.Ensure(context.Background(), sha, tarPath, SHAFromRecord)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !filepath.IsAbs(got) && filepath.Base(filepath.Dir(got)) != RootDirName {
		t.Fatalf("unexpected root: %s", got)
	}

	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(got, "foo.txt"))
	if err != nil {
		t.Fatalf("read foo.txt: %v", err)
	}
	if string(body) != helloFileContent {
		t.Fatalf("foo.txt = %q", body)
	}
	if _, err := os.Stat(filepath.Join(got, ReadyMarker)); err != nil {
		t.Fatalf("ready marker missing: %v", err)
	}

	// Calling again must be idempotent and return the same path without
	// re-extracting (we delete the tarball to prove it).
	if err := os.Remove(tarPath); err != nil {
		t.Fatalf("remove tar: %v", err)
	}
	got2, err := store.Ensure(context.Background(), sha, tarPath, SHAFromRecord)
	if err != nil {
		t.Fatalf("Ensure idempotent: %v", err)
	}
	if got2 != got {
		t.Fatalf("path mismatch: %q vs %q", got, got2)
	}
}

func TestStoreEnsureConcurrent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"file": "data"})
	store := NewStore(filepath.Join(dir, "cache"))

	sha := mustHash(t, tarPath)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := store.Ensure(context.Background(), sha, tarPath, SHAFromRecord)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Ensure: %v", err)
		}
	}
}

// TestMaterializeUsesHardlinks pins that Materialize hardlinks files and skips
// the ReadyMarker. The nested entry has no directory header, so sub/ exists
// under dst only because materializeEntry's directory arm creates it.
func TestMaterializeUsesHardlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{
		"foo.txt":     helloFileContent,
		"sub/bar.txt": "world",
	})
	store := NewStore(filepath.Join(dir, "cache"))
	src, err := store.Ensure(context.Background(), mustHash(t, tarPath), tarPath, SHAFromRecord)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	dst := filepath.Join(dir, "install")
	if err := Materialize(src, dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(dst, "sub", "bar.txt"))
	if err != nil {
		t.Fatalf("read materialized: %v", err)
	}
	if string(body) != "world" {
		t.Fatalf("sub/bar.txt = %q", body)
	}
	if _, err := os.Stat(filepath.Join(dst, ReadyMarker)); !os.IsNotExist(err) {
		t.Fatalf("ReadyMarker should not be materialized: err=%v", err)
	}

	srcStat, err := os.Stat(filepath.Join(src, "foo.txt"))
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	dstStat, err := os.Stat(filepath.Join(dst, "foo.txt"))
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if !os.SameFile(srcStat, dstStat) {
		t.Fatalf("expected hard-linked file (same inode)")
	}
}

// TestEnsureRejectsTarballNotMatchingSHA proves Ensure refuses to ingest a
// tarball into the CAS tree under a sha its bytes do not actually hash to,
// leaving neither a finalized entry nor a leftover tmp directory behind.
func TestEnsureRejectsTarballNotMatchingSHA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
	store := NewStore(filepath.Join(dir, "cache"))

	// A well-formed but wrong sha256: valid-length (64) hex that is not
	// tarPath's actual hash.
	const wrongSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	if _, err := store.Ensure(context.Background(), wrongSHA, tarPath, SHAFromRecord); !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is ErrSHA256Mismatch, got %v", err)
	}

	if _, err := os.Stat(filepath.Join(store.Root(), wrongSHA)); !os.IsNotExist(err) {
		t.Fatalf("expected no CAS tree for the rejected sha, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), wrongSHA+tmpSuffix)); !os.IsNotExist(err) {
		t.Fatalf("expected no leftover tmp dir for the rejected sha, stat err=%v", err)
	}
}

// TestEnsureAcceptsMatchingSHA proves Ensure succeeds and finalizes the CAS
// tree when the supplied sha does match the tarball's actual bytes.
func TestEnsureAcceptsMatchingSHA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
	store := NewStore(filepath.Join(dir, "cache"))

	sha := mustHash(t, tarPath)
	got, err := store.Ensure(context.Background(), sha, tarPath, SHAFromRecord)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(got, ReadyMarker)); err != nil {
		t.Fatalf("ready marker missing: %v", err)
	}
}

// TestEnsureSelfComputedSkipsVerify pins that a SHASelfComputed sha is trusted
// without hashing the file: a sha that is not the tarball's hash still
// extracts and finalizes under the declared name.
func TestEnsureSelfComputedSkipsVerify(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
	store := NewStore(filepath.Join(dir, "cache"))

	// Well-formed hex that is not tarPath's actual hash: a verifying Ensure
	// would refuse it, a trusting one must not even notice.
	const declaredSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	got, err := store.Ensure(context.Background(), declaredSHA, tarPath, SHASelfComputed)
	if err != nil {
		t.Fatalf("Ensure with SHASelfComputed: %v", err)
	}
	if filepath.Base(got) != declaredSHA {
		t.Fatalf("extracted under %q, want the declared sha %q", filepath.Base(got), declaredSHA)
	}
	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(got, "foo.txt"))
	if err != nil {
		t.Fatalf("read foo.txt: %v", err)
	}
	if string(body) != helloFileContent {
		t.Fatalf("foo.txt = %q", body)
	}
	if _, err := os.Stat(filepath.Join(got, ReadyMarker)); err != nil {
		t.Fatalf("ready marker missing: %v", err)
	}
}

// TestEnsureZeroProvenanceVerifies pins SHAProvenance's zero value to the
// verifying behavior, so an undeclared provenance costs a hash, never a skip.
func TestEnsureZeroProvenanceVerifies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
	store := NewStore(filepath.Join(dir, "cache"))

	const wrongSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	var zero SHAProvenance
	if _, err := store.Ensure(context.Background(), wrongSHA, tarPath, zero); !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is ErrSHA256Mismatch for the zero provenance, got %v", err)
	}
}

func TestStoreSweep(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "cache"))

	seedFinalizedEntry(t, store, "keep")
	seedFinalizedEntry(t, store, "drop")

	if err := store.Sweep(t.Context(), map[string]bool{"keep": true}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "keep")); err != nil {
		t.Fatalf("keep was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "drop")); !os.IsNotExist(err) {
		t.Fatalf("drop survived sweep: err=%v", err)
	}
}

// TestStoreSweepStopsWhenTheContextEnds pins that Sweep checks ctx before each
// entry: a canceled sweep removes nothing and returns context.Canceled, where
// TestStoreSweep removes "drop" from the identical fixture.
func TestStoreSweepStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "cache"))

	seedFinalizedEntry(t, store, "keep")
	seedFinalizedEntry(t, store, "drop")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := store.Sweep(ctx, map[string]bool{"keep": true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sweep = %v, want errors.Is context.Canceled", err)
	}
	for _, name := range []string{"keep", "drop"} {
		if _, err := os.Stat(filepath.Join(store.Root(), name)); err != nil {
			t.Fatalf("%s was removed by a canceled sweep: %v", name, err)
		}
	}
}

// TestStoreSweepPlan proves SweepPlan reports the same set Sweep would
// remove, sorted for deterministic output, without touching the filesystem.
func TestStoreSweepPlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "cache"))

	seedFinalizedEntry(t, store, "keep")
	seedFinalizedEntry(t, store, "drop-b")
	seedFinalizedEntry(t, store, "drop-a")

	planned, err := store.SweepPlan(map[string]bool{"keep": true})
	if err != nil {
		t.Fatalf("SweepPlan: %v", err)
	}
	want := []string{"drop-a", "drop-b"}
	if len(planned) != len(want) || planned[0] != want[0] || planned[1] != want[1] {
		t.Fatalf("SweepPlan = %v, want %v", planned, want)
	}

	// SweepPlan must not mutate anything: every entry, including the ones
	// it planned to drop, is still present on disk afterward.
	for _, name := range []string{"keep", "drop-a", "drop-b"} {
		if _, statErr := os.Stat(filepath.Join(store.Root(), name)); statErr != nil {
			t.Fatalf("SweepPlan removed %s from disk: %v", name, statErr)
		}
	}
}

// TestStoreSweepPlanMissingRoot proves SweepPlan on a store whose root
// directory does not exist yet returns (nil, nil) rather than an error,
// matching Sweep's own behavior on a missing root.
func TestStoreSweepPlanMissingRoot(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	planned, err := store.SweepPlan(map[string]bool{"keep": true})
	if err != nil {
		t.Fatalf("SweepPlan: %v", err)
	}
	if planned != nil {
		t.Fatalf("expected nil plan for a missing root, got %v", planned)
	}
}

// TestStoreSweepPlanPropagatesRealReadDirError pins that a permission error
// listing the store is returned, not treated as a missing store. Skipped
// under root, which bypasses permission bits.
func TestStoreSweepPlanPropagatesRealReadDirError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "cache"))
	if err := os.MkdirAll(store.Root(), helpers.DirMod); err != nil {
		t.Fatalf("failed to create store root: %v", err)
	}
	if err := os.Chmod(store.Root(), 0o000); err != nil {
		t.Fatalf("failed to chmod store root unreadable: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(store.Root(), helpers.DirMod); err != nil {
			t.Errorf("failed to restore store root perms: %v", err)
		}
	})

	if _, err := store.SweepPlan(map[string]bool{}); err == nil {
		t.Fatalf("expected a non-nil error reading an unreadable store root")
	}
}

// TestStoreSweepTempRemovesOrphanTempsKeepsFinalized pins that SweepTemp
// removes both an "ingest-" and a "<sha>.tmp" directory and leaves a
// finalized entry and its ready marker alone.
func TestStoreSweepTempRemovesOrphanTempsKeepsFinalized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "cache"))

	finalized := seedFinalizedEntry(t, store, "deadbeef")

	ingestDir := filepath.Join(store.Root(), "ingest-orphan")
	seedTempEntry(t, ingestDir)

	extractTmpDir := filepath.Join(store.Root(), "cafebabe"+tmpSuffix)
	seedTempEntry(t, extractTmpDir)

	if err := store.SweepTemp(); err != nil {
		t.Fatalf("SweepTemp: %v", err)
	}

	if _, err := os.Stat(ingestDir); !os.IsNotExist(err) {
		t.Fatalf("ingest-* dir survived SweepTemp: err=%v", err)
	}
	if _, err := os.Stat(extractTmpDir); !os.IsNotExist(err) {
		t.Fatalf("<sha>.tmp dir survived SweepTemp: err=%v", err)
	}
	if _, err := os.Stat(finalized); err != nil {
		t.Fatalf("finalized entry removed by SweepTemp: %v", err)
	}
	if _, err := os.Stat(filepath.Join(finalized, ReadyMarker)); err != nil {
		t.Fatalf("finalized ready marker removed by SweepTemp: %v", err)
	}
}

// mustHash returns the sha256 hex digest of the file at path, failing the
// test on error. Used to derive the real hash of a fixture tarball rather
// than hardcoding a value that Ensure's poisoning guard would now reject.
func mustHash(t *testing.T, path string) string {
	t.Helper()
	sha, err := archive.FileHashSHA256(path)
	if err != nil {
		t.Fatalf("FileHashSHA256(%s): %v", path, err)
	}
	return sha
}

// seedFinalizedEntry creates a finalized entry called name, holding only the
// ReadyMarker, without Ensure, so a test can use a readable, non-sha name.
func seedFinalizedEntry(t *testing.T, store *Store, name string) string {
	t.Helper()
	dir := filepath.Join(store.Root(), name)
	if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ReadyMarker), []byte(ReadyMarkerPayload), helpers.FileMod); err != nil {
		t.Fatalf("write ready marker in %s: %v", dir, err)
	}
	return dir
}

// seedTempEntry creates a directory at path containing a single file,
// simulating a leftover temp entry left under a store root by a killed run.
func seedTempEntry(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, "partial"), []byte("x"), helpers.FileMod); err != nil {
		t.Fatalf("write file in %s: %v", path, err)
	}
}

// TestStoreSweepTempMissingRootAndNilReceiver proves SweepTemp treats a
// not-yet-created store root as nothing to sweep, and is safe to call on a
// nil *Store (the "caching disabled" case), both returning a nil error.
func TestStoreSweepTempMissingRootAndNilReceiver(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	if err := store.SweepTemp(); err != nil {
		t.Fatalf("expected nil error for a missing store root, got %v", err)
	}

	var nilStore *Store
	if err := nilStore.SweepTemp(); err != nil {
		t.Fatalf("expected nil error for a nil store, got %v", err)
	}
}

func TestNewStoreEmptyCacheDir(t *testing.T) {
	t.Parallel()
	if got := NewStore(""); got != nil {
		t.Fatalf("expected nil store for empty cacheDir, got %v", got)
	}
}

func TestIngestAndPromote(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{
		"a.txt":   "alpha",
		"b/c.txt": "charlie",
	})
	store := NewStore(filepath.Join(dir, "cache"))

	final := ingestAndPromote(t, store, tarPath, "deadbeef")
	verifyPromoted(t, final)

	// Promote again from a second ingest of the same SHA must be a no-op
	// (existing finalized entry wins, tmp removed).
	final2 := ingestAndPromote(t, store, tarPath, "deadbeef")
	if final2 != final {
		t.Fatalf("expected same final path, got %q vs %q", final, final2)
	}
}

func ingestAndPromote(t *testing.T, store *Store, tarPath, sha string) string {
	t.Helper()
	//nolint:gosec // path under t.TempDir().
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	tmp, err := store.IngestReader(context.Background(), f)
	if err != nil {
		t.Fatalf("IngestReader: %v", err)
	}
	final, err := store.Promote(tmp, sha)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	return final
}

func verifyPromoted(t *testing.T, final string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(final, ReadyMarker)); err != nil {
		t.Fatalf("ReadyMarker missing: %v", err)
	}
	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(final, "b", "c.txt"))
	if err != nil {
		t.Fatalf("read b/c.txt: %v", err)
	}
	if string(body) != "charlie" {
		t.Fatalf("b/c.txt = %q", body)
	}
}

func writeTarball(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write tar: %v", err)
	}
}

// seedCASTree extracts a small tarball into a fresh store through Ensure and
// returns the store and the extracted tree.
func seedCASTree(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{
		"foo.txt":     helloFileContent,
		"sub/bar.txt": "world",
	})
	store := NewStore(filepath.Join(dir, "cache"))
	got, err := store.Ensure(context.Background(), mustHash(t, tarPath), tarPath, SHAFromRecord)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return store, got
}

// casEntryInfo resolves path's fs.FileInfo and its root-relative name,
// factored out of checkCASEntryMode purely to keep each function's
// cyclomatic complexity under the linter's budget.
func casEntryInfo(root, path string, d fs.DirEntry) (string, fs.FileInfo, error) {
	info, err := d.Info()
	if err != nil {
		return "", nil, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", nil, err
	}
	return rel, info, nil
}

// assertCASEntryMode checks one CAS entry's mode: the .ready marker is
// helpers.FileMod, a directory is helpers.DirMod, and any other entry has no
// write bit.
func assertCASEntryMode(t *testing.T, rel string, info fs.FileInfo, isDir bool) {
	t.Helper()
	switch {
	case rel == ReadyMarker:
		if info.Mode().Perm() != helpers.FileMod {
			t.Errorf(".ready mode = %o, want %o", info.Mode().Perm(), helpers.FileMod)
		}
	case isDir:
		if info.Mode().Perm() != helpers.DirMod {
			t.Errorf("directory %s mode = %o, want %o", rel, info.Mode().Perm(), helpers.DirMod)
		}
	default:
		if info.Mode().Perm()&0o222 != 0 {
			t.Errorf("file %s carries a write bit: mode %o", rel, info.Mode().Perm())
		}
	}
}

// checkCASEntryMode is TestCASTreeIsReadOnly's WalkDir callback, split out to
// keep the test within the cyclomatic-complexity budget.
func checkCASEntryMode(t *testing.T, root, path string, d fs.DirEntry, walkErr error) error {
	t.Helper()
	if walkErr != nil {
		return walkErr
	}
	if path == root {
		return nil
	}
	rel, info, err := casEntryInfo(root, path, d)
	if err != nil {
		return err
	}
	assertCASEntryMode(t, rel, info, d.IsDir())
	return nil
}

// TestCASTreeIsReadOnly pins the CAS tree's modes after Ensure: regular files
// carry no write bit, directories keep helpers.DirMod, and the .ready marker,
// go-galaxy's own sidecar, stays helpers.FileMod.
func TestCASTreeIsReadOnly(t *testing.T) {
	t.Parallel()
	_, got := seedCASTree(t)

	err := filepath.WalkDir(got, func(path string, d fs.DirEntry, walkErr error) error {
		return checkCASEntryMode(t, got, path, d, walkErr)
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestInstalledFilesAreReadOnly pins that Materialize links each file to its
// CAS inode with the identical read-only mode, while install directories stay
// writable at helpers.DirMod.
func TestInstalledFilesAreReadOnly(t *testing.T) {
	t.Parallel()
	_, src := seedCASTree(t)
	dst := filepath.Join(t.TempDir(), "install")
	if err := Materialize(src, dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	for _, rel := range []string{"foo.txt", filepath.Join("sub", "bar.txt")} {
		srcInfo, err := os.Stat(filepath.Join(src, rel))
		if err != nil {
			t.Fatalf("stat src %s: %v", rel, err)
		}
		dstInfo, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("stat dst %s: %v", rel, err)
		}
		if dstInfo.Mode().Perm()&0o222 != 0 {
			t.Fatalf("installed %s carries a write bit: mode %o", rel, dstInfo.Mode().Perm())
		}
		if dstInfo.Mode().Perm() != srcInfo.Mode().Perm() {
			t.Fatalf("%s: installed mode %o != CAS mode %o", rel, dstInfo.Mode().Perm(), srcInfo.Mode().Perm())
		}
		if !os.SameFile(srcInfo, dstInfo) {
			t.Fatalf("expected %s to be hard-linked to its CAS source (same inode)", rel)
		}
	}

	subInfo, err := os.Stat(filepath.Join(dst, "sub"))
	if err != nil {
		t.Fatalf("stat installed sub dir: %v", err)
	}
	if subInfo.Mode().Perm() != helpers.DirMod {
		t.Fatalf("installed directory mode = %o, want %o", subInfo.Mode().Perm(), helpers.DirMod)
	}
}

// TestInPlaceEditIsBlocked pins that opening an installed hard-linked file for
// writing fails with a permission error and leaves the CAS bytes unchanged.
// Skipped under root, which bypasses permission bits.
func TestInPlaceEditIsBlocked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits do not block writes")
	}
	t.Parallel()

	_, src := seedCASTree(t)
	dst := filepath.Join(t.TempDir(), "install")
	if err := Materialize(src, dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	installed := filepath.Join(dst, "foo.txt")

	//nolint:gosec // installed is derived from a t.TempDir() fixture, not external input; this is the exact write attempt under test.
	f, err := os.OpenFile(installed, os.O_WRONLY, 0)
	if err == nil {
		_ = f.Close()
		t.Fatalf("expected opening the installed file for writing to fail")
	}
	if !os.IsPermission(err) {
		t.Fatalf("expected a permission error, got %v", err)
	}

	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(src, "foo.txt"))
	if err != nil {
		t.Fatalf("read CAS file: %v", err)
	}
	if string(body) != helloFileContent {
		t.Fatalf("CAS file bytes changed by the blocked write attempt: %q", body)
	}
}

// TestReplaceByRenameDoesNotCorruptCAS pins the protection's scope: renaming a
// new file over an installed one succeeds, since directories stay writable,
// and detaches it from the CAS inode without changing the CAS bytes.
func TestReplaceByRenameDoesNotCorruptCAS(t *testing.T) {
	t.Parallel()

	_, src := seedCASTree(t)
	dst := filepath.Join(t.TempDir(), "install")
	if err := Materialize(src, dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	installed := filepath.Join(dst, "foo.txt")

	replacement := filepath.Join(t.TempDir(), "replacement.txt")
	if err := os.WriteFile(replacement, []byte("evil"), helpers.FileMod); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := os.Rename(replacement, installed); err != nil {
		t.Fatalf("rename over installed file: %v", err)
	}

	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(src, "foo.txt"))
	if err != nil {
		t.Fatalf("read CAS file: %v", err)
	}
	if string(body) != helloFileContent {
		t.Fatalf("CAS file corrupted by a rename over the installed copy: got %q", body)
	}
}

// assertCopyFileCreatesIndependentCopy asserts copyFile yields dst with want's
// content, a mode of exactly perm, and an inode distinct from src.
func assertCopyFileCreatesIndependentCopy(t *testing.T, dst, src, want string, perm os.FileMode) {
	t.Helper()
	if err := copyFile(src, dst, perm); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(body) != want {
		t.Fatalf("dst content = %q, want %q", body, want)
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if dstInfo.Mode().Perm() != perm {
		t.Fatalf("dst mode = %o, want %o", dstInfo.Mode().Perm(), perm)
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	if os.SameFile(srcInfo, dstInfo) {
		t.Fatalf("expected copyFile to create a distinct inode, not a hard link")
	}
}

// assertMaterializeFileSurfacesRemoveError occupies dst with a non-empty
// directory, which os.Remove cannot delete, and asserts materializeFile
// returns that remove's own ENOTEMPTY rather than a later link or open error.
func assertMaterializeFileSurfacesRemoveError(t *testing.T, dir, src string, perm os.FileMode) {
	t.Helper()
	occupiedDst := filepath.Join(dir, "occupied-dst")
	if err := os.MkdirAll(filepath.Join(occupiedDst, "occupant"), helpers.DirMod); err != nil {
		t.Fatalf("mkdir non-empty occupied dst: %v", err)
	}
	err := materializeFile(src, occupiedDst, perm)
	if err == nil {
		t.Fatalf("expected materializeFile to fail against a non-empty directory at dst")
	}
	if !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("expected the checked remove's own ENOTEMPTY error, got: %v", err)
	}
}

// TestMaterializeCopyFallback drives copyFile's cross-device fallback without
// a second filesystem, then materializeFile's checked-remove error path.
func TestMaterializeCopyFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	const casPerm = os.FileMode(0o444)
	const payload = "payload"
	src := filepath.Join(dir, "src.txt")
	//nolint:gosec // test fixture; the permissive create mode is immediately narrowed by the Chmod below.
	if err := os.WriteFile(src, []byte(payload), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.Chmod(src, casPerm); err != nil {
		t.Fatalf("chmod src to CAS mode: %v", err)
	}

	assertCopyFileCreatesIndependentCopy(t, filepath.Join(dir, "dst.txt"), src, payload, casPerm)
	assertMaterializeFileSurfacesRemoveError(t, dir, src, casPerm)
}

// TestLegacyReadyMarkerForcesRebuild pins that a tree with the legacy "ok"
// marker and a writable file is not trusted: Ensure rebuilds it read-only
// with the current ReadyMarkerPayload.
func TestLegacyReadyMarkerForcesRebuild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
	sha := mustHash(t, tarPath)

	store := NewStore(filepath.Join(dir, "cache"))
	final := filepath.Join(store.Root(), sha)
	if err := os.MkdirAll(final, helpers.DirMod); err != nil {
		t.Fatalf("mkdir legacy final: %v", err)
	}
	//nolint:gosec // test fixture: the legacy tree's writable file is the exact condition under test, not a leak risk.
	if err := os.WriteFile(filepath.Join(final, "foo.txt"), []byte(helloFileContent), 0o644); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(final, ReadyMarker), []byte("ok"), helpers.FileMod); err != nil {
		t.Fatalf("write legacy marker: %v", err)
	}

	got, err := store.Ensure(context.Background(), sha, tarPath, SHAFromRecord)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	info, err := os.Stat(filepath.Join(got, "foo.txt"))
	if err != nil {
		t.Fatalf("stat rebuilt file: %v", err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("rebuilt file mode = %o, want 0444 (the legacy tree must be rebuilt hardened)", info.Mode().Perm())
	}
	//nolint:gosec // path under t.TempDir().
	marker, err := os.ReadFile(filepath.Join(got, ReadyMarker))
	if err != nil {
		t.Fatalf("read ready marker: %v", err)
	}
	if string(marker) != ReadyMarkerPayload {
		t.Fatalf("ready marker = %q, want %q", marker, ReadyMarkerPayload)
	}
}

// TestStoreReady pins that Ready matches Ensure's isReady check: false before
// extraction, for an empty sha, for the legacy "ok" payload a bare stat would
// accept, and for a traversal sha; true for a finalized tree.
func TestStoreReady(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
	sha := mustHash(t, tarPath)

	store := NewStore(filepath.Join(dir, "cache"))

	if store.Ready(sha) {
		t.Error("expected Ready to report false before the tree is ever extracted")
	}
	if store.Ready("") {
		t.Error("expected Ready to report false for an empty sha")
	}

	if _, err := store.Ensure(context.Background(), sha, tarPath, SHAFromRecord); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !store.Ready(sha) {
		t.Error("expected Ready to report true for a genuinely promoted tree")
	}

	// Overwrite the marker with the legacy pre-hardening payload - the exact
	// shape TestLegacyReadyMarkerForcesRebuild builds by hand - and prove Ready
	// rejects it just as Ensure's own isReady check would.
	markerPath := filepath.Join(store.Root(), sha, ReadyMarker)
	if err := os.WriteFile(markerPath, []byte("ok"), helpers.FileMod); err != nil {
		t.Fatalf("write legacy marker: %v", err)
	}
	if store.Ready(sha) {
		t.Error("expected Ready to report false for a tree carrying the legacy \"ok\" marker payload")
	}

	// A traversal sha naming a sibling that holds a valid marker must still
	// report false: entryRel rejects it before any path is joined.
	siblingDir := filepath.Join(store.Root(), "..", "sibling")
	if err := os.MkdirAll(siblingDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	if err := os.WriteFile(filepath.Join(siblingDir, ReadyMarker), []byte(ReadyMarkerPayload), helpers.FileMod); err != nil {
		t.Fatalf("write sibling ready marker: %v", err)
	}
	if store.Ready("../sibling") {
		t.Error("expected Ready to report false for a traversal sha, even one naming a directory with a valid ready marker")
	}
}

// TestEnsureAndPromoteRejectTraversalSHA pins that Ensure and Promote refuse a
// sha that is not one path element, creating nothing, and that Promote still
// removes a temp inside the store while leaving one outside it untouched.
func TestEnsureAndPromoteRejectTraversalSHA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})

	cacheDir := filepath.Join(dir, "cache")
	store := NewStore(cacheDir)
	const traversalSHA = "../../../../../../etc/ssh"

	if _, err := store.Ensure(context.Background(), traversalSHA, tarPath, SHAFromRecord); !errors.Is(err, ErrSHAUnsafe) {
		t.Errorf("Ensure(%q, ...) error = %v, want ErrSHAUnsafe", traversalSHA, err)
	}
	// The rejection precedes opening any root, so not even the cache directory
	// may exist afterward.
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Errorf("expected Ensure to create nothing at all for a rejected sha, cache dir stat error = %v", err)
	}

	outsideTmp := t.TempDir()
	if _, err := store.Promote(outsideTmp, traversalSHA); !errors.Is(err, ErrSHAUnsafe) {
		t.Errorf("Promote(_, %q) error = %v, want ErrSHAUnsafe", traversalSHA, err)
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Errorf("expected Promote to create nothing at all for a rejected sha, cache dir stat error = %v", err)
	}
	if _, err := os.Stat(outsideTmp); err != nil {
		t.Errorf("expected Promote to leave a temp outside the store alone, stat error = %v", err)
	}

	// Positive control for the assertion above: the identical rejection, with
	// a temp that genuinely sits under the store, does remove it.
	insideTmp := filepath.Join(store.Root(), "ingest-inside")
	if err := os.MkdirAll(insideTmp, helpers.DirMod); err != nil {
		t.Fatalf("mkdir in-store temp: %v", err)
	}
	if _, err := store.Promote(insideTmp, traversalSHA); !errors.Is(err, ErrSHAUnsafe) {
		t.Errorf("Promote(inStoreTmp, %q) error = %v, want ErrSHAUnsafe", traversalSHA, err)
	}
	if _, err := os.Stat(insideTmp); !os.IsNotExist(err) {
		t.Errorf("expected Promote to remove a temp inside the store on rejection, stat error = %v", err)
	}
}

// TestRemoveRefusesTraversalSHA pins that Remove leaves a traversal target
// intact and returns nil, its "nothing touched" result for an unsafe sha.
func TestRemoveRefusesTraversalSHA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "cache"))

	target := filepath.Join(dir, "sibling-victim")
	if err := os.MkdirAll(target, helpers.DirMod); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	marker := filepath.Join(target, "keepme")
	if err := os.WriteFile(marker, []byte("keep"), helpers.FileMod); err != nil {
		t.Fatalf("write marker file: %v", err)
	}

	if err := store.Remove("../sibling-victim"); err != nil {
		t.Errorf("Remove: %v, want nil", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("expected the traversal target to survive Remove, stat error: %v", err)
	}
}

// TestTarballShippingReadyMarkerPromotes pins that a tarball carrying its own
// read-only root ".ready" still promotes: writeReadyMarker removes it first
// and writes the current payload instead of failing with EACCES.
func TestTarballShippingReadyMarkerPromotes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{
		"foo.txt":   helloFileContent,
		ReadyMarker: "not-ready-yet",
	})
	store := NewStore(filepath.Join(dir, "cache"))

	//nolint:gosec // path under t.TempDir().
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("open tarball: %v", err)
	}
	defer func() { _ = f.Close() }()
	tmp, err := store.IngestReader(context.Background(), f)
	if err != nil {
		t.Fatalf("IngestReader: %v", err)
	}
	sha := mustHash(t, tarPath)
	final, err := store.Promote(tmp, sha)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	//nolint:gosec // path under t.TempDir().
	content, err := os.ReadFile(filepath.Join(final, ReadyMarker))
	if err != nil {
		t.Fatalf("read ready marker: %v", err)
	}
	if string(content) != ReadyMarkerPayload {
		t.Fatalf("ready marker = %q, want %q (the tarball's own .ready entry must be overwritten)", content, ReadyMarkerPayload)
	}
	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(final, "foo.txt"))
	if err != nil {
		t.Fatalf("read foo.txt: %v", err)
	}
	if string(body) != helloFileContent {
		t.Fatalf("foo.txt = %q", body)
	}
}

// TestRemovalPathsWithReadOnlyFiles pins that Remove, Sweep, SweepTemp and a
// plain os.RemoveAll of a materialized install all delete read-only files,
// since removal needs only the directory's write bit.
func TestRemovalPathsWithReadOnlyFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// remove performs the removal against target (a finalized CAS tree
		// for the Store-level cases, or a Materialize'd install tree for the
		// last one) and returns the path that must be gone afterward.
		remove func(t *testing.T, store *Store, target string) (removed string, err error)
		name   string
	}{
		{
			name: "Store.Remove",
			remove: func(t *testing.T, store *Store, target string) (string, error) {
				t.Helper()
				return target, store.Remove(filepath.Base(target))
			},
		},
		{
			name: "Sweep",
			remove: func(t *testing.T, store *Store, target string) (string, error) {
				t.Helper()
				return target, store.Sweep(t.Context(), map[string]bool{})
			},
		},
		{
			name: "SweepTemp",
			remove: func(t *testing.T, store *Store, target string) (string, error) {
				t.Helper()
				// SweepTemp deliberately never touches a finalized entry, so
				// reshape it into the "<sha>.tmp" form its matcher targets.
				tempTarget := target + tmpSuffix
				if err := os.Rename(target, tempTarget); err != nil {
					t.Fatalf("rename to temp shape: %v", err)
				}
				return tempTarget, store.SweepTemp()
			},
		},
		{
			name: "os.RemoveAll over a Materialize'd install tree",
			remove: func(t *testing.T, _ *Store, target string) (string, error) {
				t.Helper()
				installDir := filepath.Join(filepath.Dir(target), "install")
				if err := Materialize(target, installDir); err != nil {
					t.Fatalf("Materialize: %v", err)
				}
				return installDir, os.RemoveAll(installDir)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, target := seedCASTree(t)

			removed, err := tt.remove(t, store, target)
			if err != nil {
				t.Fatalf("removal failed against a hardened tree: %v", err)
			}
			if _, statErr := os.Stat(removed); !os.IsNotExist(statErr) {
				t.Fatalf("expected %s to be removed, stat error: %v", removed, statErr)
			}
		})
	}
}

// TestNewFilesCanStillBeCreatedInInstallTree pins that the read-only hardening
// stops at files: a new directory and file, as __pycache__ needs, can still be
// created inside an install tree.
func TestNewFilesCanStillBeCreatedInInstallTree(t *testing.T) {
	t.Parallel()
	_, src := seedCASTree(t)
	dst := filepath.Join(t.TempDir(), "install")
	if err := Materialize(src, dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	newDir := filepath.Join(dst, "__pycache__")
	if err := os.Mkdir(newDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir __pycache__ in hardened install tree: %v", err)
	}
	newFile := filepath.Join(newDir, "module.cpython-312.pyc")
	//nolint:gosec // test fixture; an ordinary writable mode is exactly what a real __pycache__ write would use.
	if err := os.WriteFile(newFile, []byte("compiled"), 0o644); err != nil {
		t.Fatalf("write new file in hardened install tree: %v", err)
	}
	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(newFile)
	if err != nil {
		t.Fatalf("read new file: %v", err)
	}
	if string(body) != "compiled" {
		t.Fatalf("new file content = %q", body)
	}
}

// escapingStoreFixture builds a cache whose store directory is an absolute
// symlink to an outside directory holding a victim every sweep would delete if
// followed; it returns the store and the victim's path.
func escapingStoreFixture(t *testing.T) (*Store, string) {
	t.Helper()

	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	outside := filepath.Join(dir, "outside")
	victim := filepath.Join(outside, "ingest-victim")
	if err := os.MkdirAll(victim, helpers.DirMod); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keepme"), []byte("keep"), helpers.FileMod); err != nil {
		t.Fatalf("write victim file: %v", err)
	}
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(cacheDir, RootDirName)); err != nil {
		t.Fatalf("symlink store dir: %v", err)
	}
	return NewStore(cacheDir), filepath.Join(victim, "keepme")
}

// realStoreFixture is escapingStoreFixture's positive control: the identical
// layout with a real store directory instead of the symlink, so every
// operation the escaping fixture must refuse is shown to succeed here.
func realStoreFixture(t *testing.T) (*Store, string) {
	t.Helper()

	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	victim := filepath.Join(cacheDir, RootDirName, "ingest-victim")
	if err := os.MkdirAll(victim, helpers.DirMod); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keepme"), []byte("keep"), helpers.FileMod); err != nil {
		t.Fatalf("write victim file: %v", err)
	}
	return NewStore(cacheDir), filepath.Join(victim, "keepme")
}

// TestStoreRefusesAnEscapingStoreDirectory pins that every store entry point
// refuses a store directory symlinked out of the cache and leaves its target
// intact, each row checked against realStoreFixture as a positive control.
func TestStoreRefusesAnEscapingStoreDirectory(t *testing.T) {
	t.Parallel()

	for _, tc := range escapingStoreCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, victim := escapingStoreFixture(t)
			if err := tc.run(t, store); err == nil {
				t.Errorf("%s through an escaping store directory returned no error", tc.name)
			}
			if _, err := os.Stat(victim); err != nil {
				t.Errorf("expected the tree outside the cache directory to survive, stat error: %v", err)
			}

			// Positive control: the same call against a real store directory
			// succeeds, so the refusal above cannot be this operation failing
			// for some reason of its own.
			control, controlVictim := realStoreFixture(t)
			if err := tc.run(t, control); err != nil {
				t.Fatalf("%s against a real store directory: %v", tc.name, err)
			}
			if tc.sweeps {
				if _, err := os.Stat(controlVictim); !os.IsNotExist(err) {
					t.Errorf("expected %s to remove the entry inside a real store, stat error: %v", tc.name, err)
				}
			}
		})
	}
}

// escapingStoreCase is one row of TestStoreRefusesAnEscapingStoreDirectory;
// sweeps marks the rows whose control run must delete the fixture entry.
type escapingStoreCase struct {
	run    func(t *testing.T, s *Store) error
	name   string
	sweeps bool
}

// escapingStoreCases returns one row per store entry point that resolves a
// path under the store directory.
func escapingStoreCases() []escapingStoreCase {
	return []escapingStoreCase{
		{
			name:   "Sweep",
			sweeps: true,
			run: func(t *testing.T, s *Store) error {
				t.Helper()
				return s.Sweep(t.Context(), nil)
			},
		},
		{
			name:   "SweepTemp",
			sweeps: true,
			run: func(_ *testing.T, s *Store) error {
				return s.SweepTemp()
			},
		},
		{
			name: "SweepPlan",
			run: func(_ *testing.T, s *Store) error {
				_, err := s.SweepPlan(nil)
				return err
			},
		},
		{
			name: "Ensure",
			run: func(t *testing.T, s *Store) error {
				t.Helper()
				tarPath := filepath.Join(t.TempDir(), "src.tar.gz")
				writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
				_, err := s.Ensure(context.Background(), mustHash(t, tarPath), tarPath, SHAFromRecord)
				return err
			},
		},
		{
			name: "IngestReader",
			run: func(t *testing.T, s *Store) error {
				t.Helper()
				tarPath := filepath.Join(t.TempDir(), "src.tar.gz")
				writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
				//nolint:gosec // tarPath is this test's own t.TempDir fixture.
				f, err := os.Open(tarPath)
				if err != nil {
					t.Fatalf("open tarball: %v", err)
				}
				defer func() { _ = f.Close() }()
				_, err = s.IngestReader(context.Background(), f)
				return err
			},
		},
	}
}

// TestStoreDirUnusableNamesWhatIsWrong pins that Ensure reports
// ErrStoreDirUnusable, not the bare "file exists" of mkdirat, for both an
// escaping symlink and a regular file at the store directory name.
func TestStoreDirUnusableNamesWhatIsWrong(t *testing.T) {
	t.Parallel()

	t.Run("escaping symlink", func(t *testing.T) {
		t.Parallel()
		store, _ := escapingStoreFixture(t)
		tarPath := filepath.Join(t.TempDir(), "src.tar.gz")
		writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
		if _, err := store.Ensure(context.Background(), mustHash(t, tarPath), tarPath, SHAFromRecord); !errors.Is(err, ErrStoreDirUnusable) {
			t.Errorf("Ensure error = %v, want ErrStoreDirUnusable", err)
		}
	})

	t.Run("regular file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cacheDir := filepath.Join(dir, "cache")
		if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
			t.Fatalf("mkdir cache: %v", err)
		}
		if err := os.WriteFile(filepath.Join(cacheDir, RootDirName), []byte("not a directory"), helpers.FileMod); err != nil {
			t.Fatalf("write blocking file: %v", err)
		}
		tarPath := filepath.Join(dir, "src.tar.gz")
		writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
		store := NewStore(cacheDir)
		if _, err := store.Ensure(context.Background(), mustHash(t, tarPath), tarPath, SHAFromRecord); !errors.Is(err, ErrStoreDirUnusable) {
			t.Errorf("Ensure error = %v, want ErrStoreDirUnusable", err)
		}
	})
}

// TestDiscardRefusesATempOutsideTheStore pins that Discard removes a temp
// under the store and refuses one outside it with ErrTempOutsideStore.
func TestDiscardRefusesATempOutsideTheStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "cache"))

	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, helpers.DirMod); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := store.Discard(outside); !errors.Is(err, ErrTempOutsideStore) {
		t.Errorf("Discard(outside) error = %v, want ErrTempOutsideStore", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("expected the outside temp to survive Discard, stat error: %v", err)
	}

	inside := filepath.Join(store.Root(), "ingest-inside")
	if err := os.MkdirAll(inside, helpers.DirMod); err != nil {
		t.Fatalf("mkdir inside: %v", err)
	}
	if err := store.Discard(inside); err != nil {
		t.Errorf("Discard(inside): %v", err)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Errorf("expected the in-store temp to be removed, stat error: %v", err)
	}
}
