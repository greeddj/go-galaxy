package collections

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// install, warm and lock --dry-run save their metadata caches through
// saveDryRunSnapshotIfPersisted and must surface a failing save with the exit
// class the real path gives it; a classified sentinel makes that assertable.

// seedPersistedSnapshot saves an empty snapshot to cacheDir so a later state
// loads with WasPersisted() true; otherwise saveDryRunSnapshotIfPersisted
// would skip the save and nothing here would test a save failure.
func seedPersistedSnapshot(t *testing.T, cacheDir string) {
	t.Helper()

	backend := local.New(cacheDir)
	ctx := context.Background()
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("seeding backend.Open: %v", err)
	}
	if err := backend.SaveStore(ctx, store.New()); err != nil {
		t.Fatalf("seeding SaveStore: %v", err)
	}
	if err := backend.Close(ctx); err != nil {
		t.Fatalf("seeding backend.Close: %v", err)
	}
}

// newDryRunSaveFailureFixture returns a dry-run config over a cache holding a
// persisted snapshot and an empty requirements file, and a state whose
// SaveStore always fails with helpers.ErrCacheBackendUnavailable.
func newDryRunSaveFailureFixture(t *testing.T) (*config.Config, *installState) {
	t.Helper()

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	seedPersistedSnapshot(t, cacheDir)

	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	cfg := &config.Config{
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		LockFile:         filepath.Join(root, "galaxy.lock"),
		Workers:          1,
		Offline:          true,
		DryRun:           true,
	}
	state := newCacheBackendUnavailableSaveFailState(t, cfg)
	if !state.store.WasPersisted() {
		t.Fatal("fixture did not produce a persisted snapshot, so the save under test would be skipped")
	}
	return cfg, state
}

// TestDryRunSurfacesItsTailSaveFailure pins that install, warm and lock
// --dry-run each return a failing tail save over a persisted snapshot and
// classify it ExitNetwork, as the real path does; none writes a lockfile.
func TestDryRunSurfacesItsTailSaveFailure(t *testing.T) {
	t.Parallel()

	for _, tc := range dryRunSaveFailureCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, state := newDryRunSaveFailureFixture(t)
			runtime := infra.New(noopPrinter{}, nil)

			err := tc.run(context.Background(), cfg, runtime, state)
			if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
				t.Fatalf("%s --dry-run swallowed its tail save failure: err = %v", tc.name, err)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitNetwork {
				t.Errorf("exitcode.FromError(err) = %d, want ExitNetwork (%d)", got, exitcode.ExitNetwork)
			}
			// The lockfile assertion belongs to every row, not just lock's:
			// a dry run that wrote one would be a different defect, and the
			// save failure must not be the reason it did not.
			if _, statErr := os.Stat(cfg.LockFile); !os.IsNotExist(statErr) {
				t.Errorf("expected no lockfile from a dry run, stat error = %v", statErr)
			}
		})
	}
}

// dryRunSaveFailureCase is one row of TestDryRunSurfacesItsTailSaveFailure:
// the command entry point to run under --dry-run.
type dryRunSaveFailureCase struct {
	run  func(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState) error
	name string
}

// dryRunSaveFailureCases returns one row per command that saves through
// saveDryRunSnapshotIfPersisted.
func dryRunSaveFailureCases() []dryRunSaveFailureCase {
	return []dryRunSaveFailureCase{
		{
			name: "install",
			run: func(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState) error {
				return installWithState(ctx, cfg, runtime, state, time.Now())
			},
		},
		{
			name: "warm",
			run: func(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState) error {
				return warmWithState(ctx, cfg, runtime, state, time.Now())
			},
		},
		{
			name: "lock",
			run: func(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState) error {
				return lockWithState(ctx, cfg, runtime, state, time.Now())
			},
		},
	}
}
