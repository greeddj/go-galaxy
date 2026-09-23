package cleanup

// These tests prove Start's initCleanup wraps its real local backend with
// cacheManager.WithCleanSaveSkip; cleanup never writes the first snapshot, so
// the fixture seeds a persisted one the way a prior install would.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// reloadCleanupLastSnapshot returns the persisted Meta.LastSnapshot read
// through a fresh backend, never one Start touched, so each call observes
// what is actually on disk.
func reloadCleanupLastSnapshot(t *testing.T, cfg *config.Config, runtime *infra.Infra) time.Time {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("cacheBackend.New: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("backend.Close: %v", err)
		}
	}()

	st, err := backend.LoadStore(t.Context())
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	return st.MetaSnapshot().LastSnapshot
}

// TestCleanupThatRemovesNothingDoesNotRewriteTheSnapshot pins that a cleanup
// removing nothing leaves LastSnapshot untouched, while one that removes an
// unreachable collection advances it (the positive control).
func TestCleanupThatRemovesNothingDoesNotRewriteTheSnapshot(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	seedInstallTree(t, downloadPath)

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	writeReachableRequirements(t, reqPath)
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	// Cleanup never writes the first persisted snapshot, so seed one the way
	// a prior install would have left it.
	seedSnapshotInstalled(t, cfg, runtime, map[string]store.InstalledEntry{
		"ns.name@1.0.0": {Source: "https://galaxy.example.com/api", ArtifactSHA256: "deadbeef"},
	})
	seedStamp := reloadCleanupLastSnapshot(t, cfg, runtime)
	if seedStamp.IsZero() {
		t.Fatal("expected a non-zero LastSnapshot after seeding the persisted snapshot")
	}

	firstStamp := runCleanupAndReload(t, cfg, runtime, "first Start (removes nothing, ns.name is reachable)")
	assertLastSnapshotEqual(t, firstStamp, seedStamp, "a no-op cleanup")

	idleStamp := runCleanupAndReload(t, cfg, runtime, "second Start (still removes nothing)")
	assertLastSnapshotEqual(t, idleStamp, seedStamp, "a second no-op cleanup")

	// Positive control: rewriting the project's requirements.yml to name
	// nothing makes ns.name@1.0.0 unreachable, so the third run actually
	// removes it - a real Delete* call, which must advance the stamp.
	writeUnreachableRequirements(t, reqPath)
	changedStamp := runCleanupAndReload(t, cfg, runtime, "third Start (a real removal)")
	if !changedStamp.After(idleStamp) {
		t.Fatalf("LastSnapshot after a real removal = %v, want strictly after %v", changedStamp, idleStamp)
	}

	manifestPath := filepath.Join(downloadPath, "ansible_collections", "ns", "name", "MANIFEST.json")
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("expected the now-unreferenced collection to actually be removed, stat error: %v", err)
	}
}

// writeReachableRequirements writes a requirements.yml at path naming
// ns.name, the collection seedInstallTree always seeds - so a project
// pointed at it treats that collection as reachable.
func writeReachableRequirements(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("collections:\n  - name: ns.name\n"), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
}

// writeUnreachableRequirements overwrites path with an empty requirements
// file, making whatever it previously named unreachable.
func writeUnreachableRequirements(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("rewrite requirements.yml: %v", err)
	}
}

// runCleanupAndReload runs Start, failing the test with label on error, then
// returns the persisted snapshot's LastSnapshot.
func runCleanupAndReload(t *testing.T, cfg *config.Config, runtime *infra.Infra, label string) time.Time {
	t.Helper()
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	return reloadCleanupLastSnapshot(t, cfg, runtime)
}

// assertLastSnapshotEqual fails the test unless got equals want, naming label
// in the failure message.
func assertLastSnapshotEqual(t *testing.T, got, want time.Time, label string) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("LastSnapshot after %s = %v, want unchanged from %v", label, got, want)
	}
}
