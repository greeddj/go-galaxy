package collections

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// newLoadStoreFailureFixture builds a config whose cache directory holds
// garbage in place of the Bolt snapshot, so initInstall fails at LoadStore,
// and returns that path for the positive control to delete.
func newLoadStoreFailureFixture(t *testing.T) (*config.Config, string) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	dbPath := filepath.Join(cacheDir, helpers.StoreDBLocal)
	mustWriteFile(t, dbPath, []byte("not a bolt database"))

	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	return &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
	}, dbPath
}

// TestInitInstallLoadStoreFailureReturnsHolderContext asserts initInstall's
// LoadStore failure returns the holder context, not nil: LockLostError needs it
// to report a stolen lock rather than a corrupt snapshot.
func TestInitInstallLoadStoreFailureReturnsHolderContext(t *testing.T) {
	t.Parallel()
	cfg, dbPath := newLoadStoreFailureFixture(t)
	runtime := infra.New(noopPrinter{}, http.DefaultClient)

	ctx := t.Context()
	lockCtx, state, err := initInstall(ctx, cfg, runtime)
	if err == nil {
		t.Fatalf("expected initInstall to fail against a corrupt snapshot store")
	}
	if !errors.Is(err, helpers.ErrCorruptSnapshotStore) {
		t.Fatalf("initInstall error = %v, want errors.Is helpers.ErrCorruptSnapshotStore", err)
	}
	if state != nil {
		t.Fatalf("initInstall returned a state alongside its failure: %+v", state)
	}
	if lockCtx != ctx {
		t.Fatalf("initInstall returned holder context %v, want the ctx it was handed", lockCtx)
	}

	assertInitInstallSucceedsOnce(t, cfg, runtime, dbPath)
}

// assertInitInstallSucceedsOnce is the positive control: with the garbage
// removed, initInstall succeeds on the same cache directory, which also shows
// the failed run released its lock.
func assertInitInstallSucceedsOnce(t *testing.T, cfg *config.Config, runtime *infra.Infra, dbPath string) {
	t.Helper()
	ctx := t.Context()
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("remove the corrupt snapshot store: %v", err)
	}
	lockCtx, state, err := initInstall(ctx, cfg, runtime)
	if err != nil {
		t.Fatalf("initInstall against a clean cache dir: %v", err)
	}
	t.Cleanup(func() {
		if state.release != nil {
			_ = state.release()
		}
		_ = state.backend.Close(context.Background())
	})
	if lockCtx != ctx {
		t.Fatalf("initInstall returned holder context %v on success, want the ctx it was handed", lockCtx)
	}
}
