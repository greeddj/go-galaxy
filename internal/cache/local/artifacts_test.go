package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// testSHA is a canonical 64-char lowercase hex digest used across these
// tests: "0123456789abcdef" repeated four times, so its length is
// self-evidently correct at a glance.
const testSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// testArtifactKey is the cache key every test in this file commits and
// fetches under, factored out since none of them ever vary it.
const testArtifactKey = "ns.name-1.0.0.tar.gz"

// commitTempArtifact seeds a temp file with content and commits it under
// testArtifactKey, returning the resulting ArtifactFile. It fails the test
// on any error.
func commitTempArtifact(t *testing.T, a *Artifacts, content []byte, meta map[string]string) cacheManager.ArtifactFile {
	t.Helper()
	file, cleanup, err := a.TempFile(context.Background(), ".download-")
	if err != nil {
		t.Fatalf("TempFile error: %v", err)
	}
	defer cleanup()
	if _, err := file.Write(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}
	result, err := a.Commit(context.Background(), testArtifactKey, file.Name(), meta)
	if err != nil {
		t.Fatalf("Commit error: %v", err)
	}
	return result
}

func TestArtifactsCommitWritesSHASidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)

	result := commitTempArtifact(t, a, []byte("tarball bytes"), map[string]string{"sha256": testSHA})

	if got := result.Meta["sha256"]; got != testSHA {
		t.Fatalf("expected Commit to return sha256 %q in Meta, got %q", testSHA, got)
	}
	sidecarPath := filepath.Join(dir, testArtifactKey) + helpers.ArtifactSHASidecarSuffix
	data, err := os.ReadFile(sidecarPath) //nolint:gosec // sidecarPath is a test-controlled temp path.
	if err != nil {
		t.Fatalf("expected sidecar file to exist, read error: %v", err)
	}
	if strings.TrimSpace(string(data)) != testSHA {
		t.Fatalf("expected sidecar content %q, got %q", testSHA, string(data))
	}
}

// TestArtifactsCommitSwallowsSidecarWriteFailure pins that Commit succeeds
// with the tarball in place when a directory blocks the sidecar's path.
func TestArtifactsCommitSwallowsSidecarWriteFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)

	sidecarPath := filepath.Join(dir, testArtifactKey) + helpers.ArtifactSHASidecarSuffix
	if err := os.MkdirAll(sidecarPath, helpers.DirMod); err != nil {
		t.Fatalf("seed blocking directory at sidecar path: %v", err)
	}

	file, cleanup, err := a.TempFile(context.Background(), ".download-")
	if err != nil {
		t.Fatalf("TempFile error: %v", err)
	}
	defer cleanup()
	if err := file.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}

	result, err := a.Commit(context.Background(), testArtifactKey, file.Name(), map[string]string{"sha256": testSHA})
	if err != nil {
		t.Fatalf("expected Commit to succeed despite the sidecar write failure, got %v", err)
	}
	wantPath := filepath.Join(dir, testArtifactKey)
	if result.Path != wantPath {
		t.Fatalf("expected committed path %q, got %q", wantPath, result.Path)
	}
	if _, statErr := os.Stat(result.Path); statErr != nil {
		t.Fatalf("expected the tarball to be committed, stat error: %v", statErr)
	}
}

func TestArtifactsFetchReturnsSidecarSHA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	commitTempArtifact(t, a, []byte("tarball bytes"), map[string]string{"sha256": testSHA})

	fetched, err := a.Fetch(context.Background(), testArtifactKey)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if got := fetched.Meta["sha256"]; got != testSHA {
		t.Fatalf("expected Fetch to return sha256 %q via Meta, got %q", testSHA, got)
	}
}

func TestArtifactsFetchMissingSidecarReturnsNilMeta(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	// Commit with no sha256 in meta, so no sidecar is ever written.
	commitTempArtifact(t, a, []byte("tarball bytes"), nil)

	fetched, err := a.Fetch(context.Background(), testArtifactKey)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if fetched.Meta != nil {
		t.Fatalf("expected nil Meta when no sidecar exists, got %#v", fetched.Meta)
	}
}

func TestArtifactsFetchTornSidecarReturnsNilMeta(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	commitTempArtifact(t, a, []byte("tarball bytes"), map[string]string{"sha256": testSHA})

	// Simulate a torn/short write: truncate the sidecar to fewer than 64 hex chars.
	sidecarPath := filepath.Join(dir, testArtifactKey) + helpers.ArtifactSHASidecarSuffix
	if err := os.WriteFile(sidecarPath, []byte(testSHA[:10]), helpers.FileMod); err != nil {
		t.Fatalf("truncate sidecar: %v", err)
	}

	fetched, err := a.Fetch(context.Background(), testArtifactKey)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if fetched.Meta != nil {
		t.Fatalf("expected nil Meta for a torn sidecar, got %#v", fetched.Meta)
	}
}

func TestArtifactsFetchGarbageSidecarReturnsNilMeta(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	commitTempArtifact(t, a, []byte("tarball bytes"), map[string]string{"sha256": testSHA})

	// Overwrite the sidecar with non-hex garbage of the right length.
	garbage := strings.Repeat("z", 64)
	sidecarPath := filepath.Join(dir, testArtifactKey) + helpers.ArtifactSHASidecarSuffix
	if err := os.WriteFile(sidecarPath, []byte(garbage), helpers.FileMod); err != nil {
		t.Fatalf("corrupt sidecar: %v", err)
	}

	fetched, err := a.Fetch(context.Background(), testArtifactKey)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if fetched.Meta != nil {
		t.Fatalf("expected nil Meta for a garbage sidecar, got %#v", fetched.Meta)
	}
}

func TestArtifactsDeleteRemovesTarballAndSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	commitTempArtifact(t, a, []byte("tarball bytes"), map[string]string{"sha256": testSHA})

	tarballPath := filepath.Join(dir, testArtifactKey)
	sidecarPath := tarballPath + helpers.ArtifactSHASidecarSuffix
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("expected sidecar to exist before Delete, stat error: %v", err)
	}

	if err := a.Delete(context.Background(), testArtifactKey); err != nil {
		t.Fatalf("Delete error: %v", err)
	}
	if _, err := os.Stat(tarballPath); !os.IsNotExist(err) {
		t.Fatalf("expected tarball to be removed, stat error: %v", err)
	}
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Fatalf("expected sidecar to be removed, stat error: %v", err)
	}
}

// TestArtifactsDeleteToleratesMissingSidecar confirms Delete succeeds when
// only the tarball exists (e.g. it was committed with no sha256 in meta),
// so the absence of a sidecar is never treated as an error.
func TestArtifactsDeleteToleratesMissingSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	commitTempArtifact(t, a, []byte("tarball bytes"), nil)

	if err := a.Delete(context.Background(), testArtifactKey); err != nil {
		t.Fatalf("Delete error: %v", err)
	}
}

// The digest-shape table lives in helpers.TestIsSHA256Hex; this package only
// calls helpers.IsSHA256Hex, so it keeps no copy of it.

// assertLocalMetaFoundMatchesHas fails the test unless Has reports the same
// presence for testArtifactKey that Meta just did, the equality
// cacheManager.ArtifactStore requires and dryRunArtifactMeta relies on.
func assertLocalMetaFoundMatchesHas(t *testing.T, a *Artifacts, metaFound bool) {
	t.Helper()
	hasFound, err := a.Has(context.Background(), testArtifactKey)
	if err != nil {
		t.Fatalf("Has error: %v", err)
	}
	if hasFound != metaFound {
		t.Fatalf("Has found=%v, Meta found=%v, want equal", hasFound, metaFound)
	}
}

// TestArtifactsMetaAbsentReportsNotFound proves Meta reports found=false with
// a nil map and a nil error for a key that was never committed - the
// identical outcome Has itself reports for the same key.
func TestArtifactsMetaAbsentReportsNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)

	meta, found, err := a.Meta(context.Background(), testArtifactKey)
	if err != nil {
		t.Fatalf("Meta error: %v", err)
	}
	if found {
		t.Fatalf("expected found=false for an absent key, got true (meta=%#v)", meta)
	}
	if meta != nil {
		t.Fatalf("expected a nil meta map for an absent key, got %#v", meta)
	}
	assertLocalMetaFoundMatchesHas(t, a, found)
}

// TestArtifactsMetaPresentWithValidDigestReturnsIt proves Meta surfaces a
// committed artifact's sidecar digest exactly as Fetch itself would.
func TestArtifactsMetaPresentWithValidDigestReturnsIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	commitTempArtifact(t, a, []byte("tarball bytes"), map[string]string{"sha256": testSHA})

	meta, found, err := a.Meta(context.Background(), testArtifactKey)
	if err != nil {
		t.Fatalf("Meta error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for a committed key")
	}
	if got := meta["sha256"]; got != testSHA {
		t.Fatalf("expected Meta to report sha256 %q, got %q", testSHA, got)
	}
	assertLocalMetaFoundMatchesHas(t, a, found)
}

// TestArtifactsMetaPresentWithNoSidecarReportsFoundNilMeta pins the "cached,
// no recorded metadata" state: an artifact committed with no sha256 reports
// found=true with a nil meta map.
func TestArtifactsMetaPresentWithNoSidecarReportsFoundNilMeta(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	commitTempArtifact(t, a, []byte("tarball bytes"), nil)

	meta, found, err := a.Meta(context.Background(), testArtifactKey)
	if err != nil {
		t.Fatalf("Meta error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for a committed key with no sidecar")
	}
	if meta != nil {
		t.Fatalf("expected a nil meta map with no sidecar, got %#v", meta)
	}
	assertLocalMetaFoundMatchesHas(t, a, found)
}

// TestArtifactsMetaPresentWithNonHexSidecarReportsFoundNilMeta pins that Meta
// applies Fetch's helpers.IsSHA256Hex gate: a non-hex sidecar yields
// found=true, since the tarball is cached, with a nil meta map.
func TestArtifactsMetaPresentWithNonHexSidecarReportsFoundNilMeta(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	commitTempArtifact(t, a, []byte("tarball bytes"), map[string]string{"sha256": testSHA})

	garbage := strings.Repeat("z", 64)
	sidecarPath := filepath.Join(dir, testArtifactKey) + helpers.ArtifactSHASidecarSuffix
	if err := os.WriteFile(sidecarPath, []byte(garbage), helpers.FileMod); err != nil {
		t.Fatalf("corrupt sidecar: %v", err)
	}

	meta, found, err := a.Meta(context.Background(), testArtifactKey)
	if err != nil {
		t.Fatalf("Meta error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true: the tarball itself is still cached even though its sidecar is garbage")
	}
	if meta != nil {
		t.Fatalf("expected a nil meta map for a non-hex sidecar, got %#v", meta)
	}
	assertLocalMetaFoundMatchesHas(t, a, found)
}

// TestArtifactsNeverActThroughASymlinkedCacheEntry pins why the flat layout
// needs no os.Root: a key is one path element, and Commit's os.Rename and
// Delete's os.Remove replace or unlink a planted symlink, never its target.
func TestArtifactsNeverActThroughASymlinkedCacheEntry(t *testing.T) {
	t.Parallel()

	t.Run("Commit replaces the symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		victim := seedSymlinkedCacheEntry(t, dir)

		a := NewArtifacts(dir)
		commitTempArtifact(t, a, []byte("committed bytes"), map[string]string{"sha256": testSHA})

		assertVictimIntact(t, victim)
		entry := filepath.Join(dir, testArtifactKey)
		info, err := os.Lstat(entry)
		if err != nil {
			t.Fatalf("lstat committed entry: %v", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Error("expected Commit to replace the symlink with a real file, it is still a symlink")
		}
		//nolint:gosec // entry is under this test's own t.TempDir fixture.
		body, err := os.ReadFile(entry)
		if err != nil {
			t.Fatalf("read committed entry: %v", err)
		}
		if string(body) != "committed bytes" {
			t.Errorf("committed entry = %q, want the freshly committed bytes", body)
		}
	})

	t.Run("Delete unlinks the symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		victim := seedSymlinkedCacheEntry(t, dir)

		a := NewArtifacts(dir)
		if err := a.Delete(context.Background(), testArtifactKey); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		assertVictimIntact(t, victim)
		if _, err := os.Lstat(filepath.Join(dir, testArtifactKey)); !os.IsNotExist(err) {
			t.Errorf("expected Delete to unlink the cache entry, lstat error = %v", err)
		}
	})
}

// seedSymlinkedCacheEntry plants a symlink at the cache entry testArtifactKey
// resolves to, pointing at a file outside the cache directory, and returns
// that file's path.
func seedSymlinkedCacheEntry(t *testing.T, cacheDir string) string {
	t.Helper()

	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte(victimContent), helpers.FileMod); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(cacheDir, testArtifactKey)); err != nil {
		t.Fatalf("symlink cache entry: %v", err)
	}
	return victim
}

// victimContent is the body seedSymlinkedCacheEntry writes and
// assertVictimIntact expects back, unchanged.
const victimContent = "the file a symlinked cache entry points at"

// assertVictimIntact fails the test unless the file outside the cache
// directory is still present with its original bytes.
func assertVictimIntact(t *testing.T, victim string) {
	t.Helper()

	//nolint:gosec // victim is under this test's own t.TempDir fixture.
	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("expected the file outside the cache directory to survive: %v", err)
	}
	if string(body) != victimContent {
		t.Errorf("file outside the cache directory was rewritten: %q", body)
	}
}
