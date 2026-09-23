package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestClearCacheFilesPreservesLiveState pins that --clear-cache sweeps a stray
// tarball but keeps the lock file and the Bolt snapshot, whose saved data still
// loads afterward.
func TestClearCacheFilesPreservesLiveState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	dbs, err := OpenDBs(dir, helpers.BoltOpenTimeout)
	if err != nil {
		t.Fatalf("OpenDBs error: %v", err)
	}
	st := New()
	st.SetInstalled("a.b@1.0.0", InstalledEntry{ArtifactSHA256: "abc"})
	if err := Save(dbs, st); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	if err := dbs.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	writeCacheFile(t, dir, "x.tar.gz", []byte("tarball"))
	writeCacheFile(t, dir, helpers.StoreDBLock, []byte("lock"))

	if err := ClearCacheFiles(dir); err != nil {
		t.Fatalf("ClearCacheFiles error: %v", err)
	}

	assertFileAbsent(t, dir, "x.tar.gz")
	assertFileExists(t, dir, helpers.StoreDBLock)

	reopened, err := OpenDBs(dir, helpers.BoltOpenTimeout)
	if err != nil {
		t.Fatalf("reopen OpenDBs error: %v", err)
	}
	t.Cleanup(func() {
		_ = reopened.Close()
	})
	loaded, err := Load(reopened)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != "abc" {
		t.Fatalf("expected installed entry to survive clear, got %#v (ok=%v)", installed, ok)
	}
}

// TestIsDeleteCacheNameNeverDeletesLiveArtifacts pins that the delete set holds
// neither the lock file (unlinking a flocked file lets two runs hold the lock)
// nor the Bolt snapshot, the only copy of cached state.
func TestIsDeleteCacheNameNeverDeletesLiveArtifacts(t *testing.T) {
	t.Parallel()
	if isDeleteCacheName(helpers.StoreDBLock) {
		t.Fatalf("expected the lock file to never be in the delete set")
	}
	if isDeleteCacheName(helpers.StoreDBLocal) {
		t.Fatalf("expected the consolidated database to never be in the delete set")
	}
}

// TestClearCacheFilesPreservesLockFile confirms a plain lock file (not a
// real flock, so the test stays platform-neutral) survives ClearCacheFiles.
func TestClearCacheFilesPreservesLockFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCacheFile(t, dir, helpers.StoreDBLock, []byte("lock"))

	if err := ClearCacheFiles(dir); err != nil {
		t.Fatalf("ClearCacheFiles error: %v", err)
	}
	assertFileExists(t, dir, helpers.StoreDBLock)
}

// TestClearCacheFilesReclaimsLegacySnapshotFiles confirms the nine
// pre-consolidation per-bucket snapshot files are reclaimed as orphans,
// since the consolidated single-file layout never opens them.
func TestClearCacheFilesReclaimsLegacySnapshotFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacyNames := []string{
		helpers.StoreSnapshotMeta,
		helpers.StoreSnapshotAPICache,
		helpers.StoreSnapshotDepsCache,
		helpers.StoreSnapshotInstalled,
		helpers.StoreSnapshotGraph,
		helpers.StoreSnapshotRequirements,
		helpers.StoreSnapshotRoots,
		helpers.StoreSnapshotResolved,
		helpers.StoreSnapshotVersions,
	}
	for _, name := range legacyNames {
		writeCacheFile(t, dir, name, []byte("legacy"))
	}

	if err := ClearCacheFiles(dir); err != nil {
		t.Fatalf("ClearCacheFiles error: %v", err)
	}
	for _, name := range legacyNames {
		assertFileAbsent(t, dir, name)
	}
}

// TestClearCacheFilesRemovesArtifactSHASidecar confirms --clear-cache sweeps
// a sha256 sidecar left next to a cached tarball, so a stale digest can
// never survive a clear and be picked up against different bytes later.
func TestClearCacheFilesRemovesArtifactSHASidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCacheFile(t, dir, "x.tar.gz"+helpers.ArtifactSHASidecarSuffix, []byte(strings.Repeat("a", 64)))

	if err := ClearCacheFiles(dir); err != nil {
		t.Fatalf("ClearCacheFiles error: %v", err)
	}
	assertFileAbsent(t, dir, "x.tar.gz"+helpers.ArtifactSHASidecarSuffix)
}

// writeCacheFile writes a small file under dir, failing the test on error.
func writeCacheFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, helpers.FileMod); err != nil {
		t.Fatalf("failed to write %s: %v", name, err)
	}
}

// assertFileExists fails the test unless the named file is present.
func assertFileExists(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatalf("expected %s to exist, stat error: %v", name, err)
	}
}

// assertFileAbsent fails the test unless the named file is gone.
func assertFileAbsent(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed, stat error: %v", name, err)
	}
}

// TestClearCacheFilesUnlinksASymlinkedEntryWithoutFollowingIt pins that a link
// planted at a sweepable name is unlinked by os.Remove and its target survives,
// which is why the flat cache directory needs no containment root.
func TestClearCacheFilesUnlinksASymlinkedEntryWithoutFollowingIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "victim.txt")
	if err := os.WriteFile(victim, []byte("outside the cache directory"), helpers.FileMod); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	link := filepath.Join(dir, "ns.name-1.0.0.tar.gz")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatalf("symlink cache entry: %v", err)
	}

	if err := ClearCacheFiles(dir); err != nil {
		t.Fatalf("ClearCacheFiles: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("expected the symlinked entry to be unlinked, lstat error = %v", err)
	}
	//nolint:gosec // victim is under this test's own t.TempDir fixture.
	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("expected the file outside the cache directory to survive: %v", err)
	}
	if string(body) != "outside the cache directory" {
		t.Errorf("file outside the cache directory was rewritten: %q", body)
	}
}
