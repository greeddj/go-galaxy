package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	bolt "go.etcd.io/bbolt"
)

func TestBackendOpenDoesNotOpenBolt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	metaPath := filepath.Join(dir, helpers.StoreSnapshotMeta)

	// Hold the meta DB's file lock externally. If Open still touched Bolt,
	// it would either fail (timeout) or hang - here it must do neither.
	holder, err := bolt.Open(metaPath, helpers.FileMod, nil)
	if err != nil {
		t.Fatalf("failed to open holder DB: %v", err)
	}
	defer func() {
		_ = holder.Close()
	}()

	b := New(dir)
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("expected Open to succeed without touching Bolt, got %v", err)
	}
}

// TestBackendOpenRejectsEmptyCacheDir pins that an empty cache dir fails with
// the shared helpers.ErrCacheDirEmpty, which the exit-code mapping reads as a
// usage error.
func TestBackendOpenRejectsEmptyCacheDir(t *testing.T) {
	t.Parallel()

	b := New("")
	if err := b.Open(context.Background()); !errors.Is(err, helpers.ErrCacheDirEmpty) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheDirEmpty), got %v", err)
	}
}

// TestBackendClearFilesRejectsEmptyCacheDir pins ClearFiles' own guard:
// without it store.ClearCacheFiles("") swallows os.ReadDir's ErrNotExist and
// a Backend with no cache dir reports success on a destructive operation.
func TestBackendClearFilesRejectsEmptyCacheDir(t *testing.T) {
	t.Parallel()

	t.Run("empty cache dir", func(t *testing.T) {
		t.Parallel()
		b := New("")
		if err := b.ClearFiles(context.Background()); !errors.Is(err, helpers.ErrCacheDirEmpty) {
			t.Fatalf("expected errors.Is(err, helpers.ErrCacheDirEmpty), got %v", err)
		}
	})

	// The positive control: a real cacheDir reaches and passes ClearFiles,
	// proving "empty cache dir" above is refused by the guard and not by
	// some unrelated failure that would refuse any cacheDir.
	t.Run("real cache dir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCacheFile(t, dir, "acme-widgets-1.0.0.tar.gz", []byte("tarball"))

		b := New(dir)
		if err := b.ClearFiles(context.Background()); err != nil {
			t.Fatalf("ClearFiles: %v", err)
		}
		assertFileAbsent(t, dir, "acme-widgets-1.0.0.tar.gz")
	})
}

func TestBackendLockFailsFastWhenHeld(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx := context.Background()

	b1 := openedBackend(t, dir)
	release1 := lockOrFatal(t, b1)

	b2 := openedBackend(t, dir)
	assertLockFailsFast(t, b2)

	if err := release1(); err != nil {
		t.Fatalf("release1 failed: %v", err)
	}

	release2 := lockOrFatal(t, b2)
	if err := release2(); err != nil {
		t.Fatalf("release2 failed: %v", err)
	}

	if err := b1.Close(ctx); err != nil {
		t.Fatalf("b1.Close failed: %v", err)
	}
	if err := b2.Close(ctx); err != nil {
		t.Fatalf("b2.Close failed: %v", err)
	}
}

// TestBackendSweepTempRemovesDownloadOrphansKeepsRealFiles proves SweepTemp
// removes only leftover download-temp files: a committed tarball, its sha256
// sidecar, and the consolidated Bolt database must all survive.
func TestBackendSweepTempRemovesDownloadOrphansKeepsRealFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeCacheFile(t, dir, helpers.ArtifactDownloadTempPrefix+"abc", []byte("orphan"))
	writeCacheFile(t, dir, helpers.ArtifactDownloadTempPrefix+"xyz", []byte("orphan"))
	writeCacheFile(t, dir, "acme-widgets-1.0.0.tar.gz", []byte("tarball"))
	writeCacheFile(t, dir, "acme-widgets-1.0.0.tar.gz"+helpers.ArtifactSHASidecarSuffix, []byte("sha256"))
	writeCacheFile(t, dir, helpers.StoreDBLocal, []byte("bolt"))

	b := New(dir)
	if err := b.SweepTemp(context.Background()); err != nil {
		t.Fatalf("SweepTemp error: %v", err)
	}

	assertFileAbsent(t, dir, helpers.ArtifactDownloadTempPrefix+"abc")
	assertFileAbsent(t, dir, helpers.ArtifactDownloadTempPrefix+"xyz")
	assertFileExists(t, dir, "acme-widgets-1.0.0.tar.gz")
	assertFileExists(t, dir, "acme-widgets-1.0.0.tar.gz"+helpers.ArtifactSHASidecarSuffix)
	assertFileExists(t, dir, helpers.StoreDBLocal)
}

// TestBackendSweepTempMissingDirIsNoError proves SweepTemp treats a
// not-yet-created cache directory as nothing to sweep, matching
// store.SweepDownloadTemps' own ErrNotExist handling.
func TestBackendSweepTempMissingDirIsNoError(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	b := New(dir)
	if err := b.SweepTemp(context.Background()); err != nil {
		t.Fatalf("expected nil error for a missing cache dir, got %v", err)
	}
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

// openedBackend creates and opens a Backend rooted at dir, failing the test
// on any error.
func openedBackend(t *testing.T, dir string) *Backend {
	t.Helper()
	b := New(dir)
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	return b
}

// lockOrFatal acquires b's lock, failing the test on any error.
func lockOrFatal(t *testing.T, b *Backend) func() error {
	t.Helper()
	_, release, err := b.Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock failed: %v", err)
	}
	return release
}

// assertLockFailsFast asserts that b.Lock returns ErrAnotherInstanceIsRunning
// promptly instead of blocking, proving the lock is taken before any Bolt
// file is opened.
func assertLockFailsFast(t *testing.T, b *Backend) {
	t.Helper()

	type lockResult struct {
		release func() error
		err     error
	}
	resultCh := make(chan lockResult, 1)
	go func() {
		_, release, err := b.Lock(context.Background())
		resultCh <- lockResult{release: release, err: err}
	}()

	select {
	case res := <-resultCh:
		if !errors.Is(res.err, helpers.ErrAnotherInstanceIsRunning) {
			t.Fatalf("expected ErrAnotherInstanceIsRunning, got %v", res.err)
		}
		if res.release != nil {
			t.Fatalf("expected nil release on failed lock, got non-nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Lock hung instead of failing fast")
	}
}

// TestBackendClassifiesItsOwnFailures pins the local backend's cache classes:
// a permission failure is unusable, any other failure unavailable, and an
// empty cache dir and contention keep their own verdicts.
func TestBackendClassifiesItsOwnFailures(t *testing.T) {
	t.Parallel()

	t.Run("permission failure is unusable", func(t *testing.T) {
		t.Parallel()
		dir := readOnlyDir(t)
		// Lock, not Open: MkdirAll succeeds on an existing read-only directory,
		// so the failure lands where the lock file is created, as in a real run.
		_, _, err := New(dir).Lock(context.Background())
		if !errors.Is(err, helpers.ErrCacheBackendUnusable) {
			t.Fatalf("Lock on a read-only cache dir = %v, want errors.Is helpers.ErrCacheBackendUnusable", err)
		}
	})

	t.Run("other filesystem failure is unavailable", func(t *testing.T) {
		t.Parallel()
		// A regular file where a parent directory belongs: the resulting
		// ENOTDIR is neither a permission problem nor an absence, which is
		// the whole "everything else" half of the split.
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), helpers.FileMod); err != nil {
			t.Fatalf("write blocker: %v", err)
		}
		err := New(filepath.Join(blocker, "cache")).Open(context.Background())
		if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
			t.Fatalf("Open under a regular file = %v, want errors.Is helpers.ErrCacheBackendUnavailable", err)
		}
	})

	t.Run("an empty cache dir stays a usage error", func(t *testing.T) {
		t.Parallel()
		err := New("").Open(context.Background())
		if !errors.Is(err, helpers.ErrCacheDirEmpty) {
			t.Fatalf("Open with no cache dir = %v, want errors.Is helpers.ErrCacheDirEmpty", err)
		}
		if errors.Is(err, helpers.ErrCacheBackendUnusable) || errors.Is(err, helpers.ErrCacheBackendUnavailable) {
			t.Errorf("an operator's own configuration mistake must not be reclassified: %v", err)
		}
	})

	t.Run("contention keeps its own class", testContentionKeepsItsOwnClass)
}

// testContentionKeepsItsOwnClass pins that contention stays
// ErrAnotherInstanceIsRunning: reclassified, "another process holds the
// cache" would become "the cache is unusable", the wrong advice.
func testContentionKeepsItsOwnClass(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, release, err := New(dir).Lock(context.Background())
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	defer func() { _ = release() }()

	_, _, err = New(dir).Lock(context.Background())
	if !errors.Is(err, helpers.ErrAnotherInstanceIsRunning) {
		t.Fatalf("second Lock = %v, want errors.Is helpers.ErrAnotherInstanceIsRunning", err)
	}
	if errors.Is(err, helpers.ErrCacheBackendUnusable) || errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Errorf("contention must not be reclassified as a backend problem: %v", err)
	}
}

// readOnlyDir returns a directory this process cannot write to, restoring its
// mode afterwards so t.TempDir's own cleanup can remove it.
func readOnlyDir(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	//nolint:gosec // 0o555 is a directory mode (read+traverse, no write), which is the condition under test.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, helpers.DirMod); err != nil {
			t.Errorf("restoring directory mode: %v", err)
		}
	})
	// A process running as root ignores the mode entirely, which would make
	// the caller assert against a directory it can still write to. Prove the
	// condition holds before returning it.
	probe := filepath.Join(dir, "writable-probe")
	if err := os.WriteFile(probe, []byte("x"), helpers.FileMod); err == nil {
		_ = os.Remove(probe)
		t.Skip("this process can write to a 0o555 directory (running as root?), so the permission case is unreachable here")
	}
	return dir
}

// TestBackendRoundTripsRoleBucketsAndRolesPath pins that SaveStore/LoadStore
// keep an installed role and a role pin, and that RecordProject keeps a roles
// path resolved against the project directory.
func TestBackendRoundTripsRoleBucketsAndRolesPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := openedBackend(t, dir)
	release := lockOrFatal(t, b)
	defer func() {
		if err := release(); err != nil {
			t.Errorf("release: %v", err)
		}
	}()

	assertRoleBucketsRoundTrip(t, b, dir)
	assertRolesPathRoundTrip(t, b)
}

// assertRoleBucketsRoundTrip saves a store holding one installed role and one
// role pin through b and reads both back.
func assertRoleBucketsRoundTrip(t *testing.T, b *Backend, dir string) {
	t.Helper()
	ctx := context.Background()
	const (
		roleName = "nginx"
		commit   = "0123456789abcdef0123456789abcdef01234567"
		pinKey   = "acme.nginx,v1.2.3"
	)
	st := store.New()
	st.SetInstalledRole(roleName, store.InstalledRoleEntry{
		InstallPath:    filepath.Join(dir, "roles", roleName),
		Source:         "git+https://github.com/acme/ansible-role-nginx.git#@" + commit,
		ArtifactSHA256: "role-sha",
		Version:        "v1.2.3",
		Deps:           []string{"common"},
	})
	st.SetRolePin(pinKey, store.RolePinEntry{Repository: "https://github.com/acme/ansible-role-nginx.git", Commit: commit})
	if err := b.SaveStore(ctx, st); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}
	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	role, ok := loaded.GetInstalledRole(roleName)
	if !ok || role.ArtifactSHA256 != "role-sha" || len(role.Deps) != 1 || role.Deps[0] != "common" {
		t.Fatalf("installed role after round trip: %#v (ok=%t)", role, ok)
	}
	if pin, ok := loaded.GetRolePin(pinKey); !ok || pin.Commit != commit {
		t.Fatalf("role pin after round trip: %#v (ok=%t)", pin, ok)
	}
	if !loaded.HasRecordedContent() {
		t.Fatalf("a snapshot holding only an installed role did not record content")
	}
}

// assertRolesPathRoundTrip records a project with a relative roles path
// through b and reads the resolved path back from the registry.
func assertRolesPathRoundTrip(t *testing.T, b *Backend) {
	t.Helper()
	ctx := context.Background()
	projectDir := t.TempDir()
	reqPath := filepath.Join(projectDir, "requirements.yml")
	if err := b.RecordProject(ctx, reqPath, "collections", "roles"); err != nil {
		t.Fatalf("RecordProject: %v", err)
	}
	registry, err := b.LoadProjectRegistry(ctx)
	if err != nil {
		t.Fatalf("LoadProjectRegistry: %v", err)
	}
	record, ok := registry.Projects[projectDir]
	if !ok {
		t.Fatalf("no record under %q, got %#v", projectDir, registry.Projects)
	}
	if want := filepath.Join(projectDir, "roles"); record.RolesPath != want {
		t.Fatalf("RolesPath = %q, want %q", record.RolesPath, want)
	}
}
