package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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

// TestSweepDownloadTempsRemovesOnlyDownloadTemps pins that the sweep removes
// top-level download temps alone: artifacts, sidecars, the database, the lock,
// the registry and a directory carrying the prefix all stay.
func TestSweepDownloadTempsRemovesOnlyDownloadTemps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	temps := []string{helpers.ArtifactDownloadTempPrefix + "123", helpers.ArtifactDownloadTempPrefix + "abc.sha256"}
	kept := []string{
		"ns.name-1.0.0.tar.gz",
		"ns.name-1.0.0.tar.gz" + helpers.ArtifactSHASidecarSuffix,
		helpers.StoreDBLocal,
		helpers.StoreDBLock,
		helpers.StoreDBProjects,
		"leftover.tmp",
	}
	for _, name := range append(slices.Clone(temps), kept...) {
		writeCacheFile(t, dir, name, []byte("x"))
	}
	keptDir := helpers.ArtifactDownloadTempPrefix + "dir"
	if err := os.Mkdir(filepath.Join(dir, keptDir), helpers.DirMod); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	if err := SweepDownloadTemps(dir); err != nil {
		t.Fatalf("SweepDownloadTemps: %v", err)
	}
	for _, name := range temps {
		assertFileAbsent(t, dir, name)
	}
	for _, name := range append(kept, keptDir) {
		assertFileExists(t, dir, name)
	}
}

// TestCacheSweepsTreatAMissingDirAsEmpty pins that both sweeps succeed on a
// cache dir that was never created.
func TestCacheSweepsTreatAMissingDirAsEmpty(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	if err := ClearCacheFiles(missing); err != nil {
		t.Errorf("ClearCacheFiles(missing) = %v, want nil", err)
	}
	if err := SweepDownloadTemps(missing); err != nil {
		t.Errorf("SweepDownloadTemps(missing) = %v, want nil", err)
	}
}

// TestCacheSweepsReportACacheDirThatIsAFile pins that a regular file where the
// cache dir belongs is an error for both sweeps, not an empty cache.
func TestCacheSweepsReportACacheDirThatIsAFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCacheFile(t, root, "cache", []byte("not a directory"))
	notDir := filepath.Join(root, "cache")
	if err := ClearCacheFiles(notDir); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("ClearCacheFiles(file) = %v, want ENOTDIR", err)
	}
	if err := SweepDownloadTemps(notDir); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("SweepDownloadTemps(file) = %v, want ENOTDIR", err)
	}
	assertFileExists(t, root, "cache")
}

// TestClearCacheFilesSkipsDirectories pins that a directory whose name matches
// a sweepable pattern is left alone along with its contents.
func TestClearCacheFilesSkipsDirectories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nested := filepath.Join(dir, "ns.name-1.0.0.tar.gz")
	if err := os.Mkdir(nested, helpers.DirMod); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	writeCacheFile(t, nested, "inner.tar.gz", []byte("x"))
	if err := ClearCacheFiles(dir); err != nil {
		t.Fatalf("ClearCacheFiles: %v", err)
	}
	assertFileExists(t, nested, "inner.tar.gz")
}

// TestCacheSweepsReportAFileTheyCannotRemove pins that a removal refused by a
// read-only cache dir is returned as fs.ErrPermission and the file stays.
func TestCacheSweepsReportAFileTheyCannotRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	temp := helpers.ArtifactDownloadTempPrefix + "1"
	writeCacheFile(t, dir, temp, []byte("x"))
	//nolint:gosec // G302: 0o555 is a directory mode (read and traverse, no write), the condition under test.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, helpers.DirMod) })
	if err := os.WriteFile(filepath.Join(dir, "probe"), nil, helpers.FileMod); err == nil {
		t.Skip("this process can write to a 0o555 directory (running as root?)")
	}

	if err := ClearCacheFiles(dir); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("ClearCacheFiles = %v, want fs.ErrPermission", err)
	}
	if err := SweepDownloadTemps(dir); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("SweepDownloadTemps = %v, want fs.ErrPermission", err)
	}
	assertFileExists(t, dir, temp)
}
