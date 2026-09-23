// Package cleanup implements the cleanup command: it computes reachability
// over every project in the cache registry before deleting anything, then
// removes unreachable collections and roles through an os.Root per project.
package cleanup

import (
	"context"
	"fmt"

	"github.com/Masterminds/semver/v3"
	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// installedCollection is an installed collection found on disk. Namespace
// and Name are the walked ansible_collections/<ns>/<name> directory names,
// never manifest content, so a MANIFEST.json cannot redirect a removal path.
type installedCollection struct {
	Parsed         *semver.Version
	Key            string
	FQDN           string
	Namespace      string
	Name           string
	Version        string
	InstallPath    string
	CollectionsDir string
}

type cleanupState struct {
	backend  cacheManager.Backend
	store    *store.Store
	registry *store.ProjectRegistry
	release  func() error
}

// Start runs the cleanup process for unused collections.
func Start(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	return runCleanup(ctx, cfg, runtime)
}

// runCleanup owns the backend lifecycle and is the one place the lock-loss
// verdict (cacheManager.LockLostError) wraps the outcome; the work runs under
// the holder context and stops at the next collection, role, key or entry.
func runCleanup(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	lockCtx, state, err := initCleanup(ctx, cfg, runtime)
	if err != nil {
		return cacheManager.LockLostError(ctx, lockCtx, err)
	}
	if state == nil {
		// initCleanup returns a non-nil state on success; this guard is
		// defensive and has nothing acquired to release.
		return nil
	}
	// Register release and close before any work, so even cleanupWithState's
	// empty-registry no-op gives back the lock and the backend.
	defer func() {
		if state.release != nil {
			if err := state.release(); err != nil {
				runtime.Output.Errorf("Lock release: %v", err)
			}
		}
	}()
	defer func() {
		if state.backend != nil {
			_ = state.backend.Close(ctx)
		}
	}()

	return cacheManager.LockLostError(ctx, lockCtx, cleanupWithState(lockCtx, cfg, runtime, state))
}

// cleanupWithState does cleanup's work on an initialized state: reachability,
// removal, the legacy and extracted sweeps, then the save. runCleanup owns the
// lock and the backend, so this never releases or closes them.
func cleanupWithState(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *cleanupState) error {
	if state.registry == nil || len(state.registry.Projects) == 0 {
		runtime.Output.Printf("No projects recorded for GC.")
		return nil
	}
	warnIfSnapshotNotPersisted(runtime, state.store)

	reachable, installedByKey, roles, err := buildReachable(runtime, state.registry, state.store)
	if err != nil {
		return err
	}
	removed, err := removeUnused(ctx, cfg, runtime, state.backend, state.store, reachable, installedByKey)
	if err != nil {
		return err
	}
	removedRoles, err := removeUnusedRoles(ctx, cfg, runtime, state.backend, state.store, roles.reachable, roles.byName)
	if err != nil {
		return err
	}
	removed += removedRoles
	// Re-read the holder context: the last key's check passed before its
	// removal, so a lock lost during it must not go on to sweep a cache
	// another holder is writing.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cleanup stopped before sweeping cached artifacts: %w", err)
	}
	sweepLegacyArtifacts(ctx, cfg, runtime, state.backend, installedByKey)
	sweepExtractedStore(ctx, cfg, runtime, state.store, reachable, installedByKey, roles)
	return finalizeCleanup(ctx, cfg, runtime, state.backend, state.store, removed)
}

// warnIfSnapshotNotPersisted tells the operator why sweepExtractedStore skips
// when st records no content, and why finalizeCleanup's save also skips when
// no snapshot was persisted; the guarded functions themselves stay silent.
func warnIfSnapshotNotPersisted(runtime *infra.Infra, st *store.Store) {
	if st.HasRecordedContent() {
		return
	}
	if st.WasPersisted() {
		runtime.Output.Warnf(
			"the persisted snapshot records nothing about what is installed or warmed; skipping the extracted-cache sweep this run",
		)
		return
	}
	runtime.Output.Warnf(
		"no persisted snapshot was found; skipping the extracted-cache sweep and leaving the snapshot untouched this run",
	)
}

// initCleanup opens the backend, takes its lock and loads the snapshot and
// registry, returning the holder context even on post-lock failures so a load
// broken by a stolen lock is judged as lock loss; Open and Close keep ctx.
func initCleanup(ctx context.Context, cfg *config.Config, runtime *infra.Infra) (context.Context, *cleanupState, error) {
	runtime.Output.Printf("Init cache backend")
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		return nil, nil, err
	}
	// Bound every state load and save under the held lock so a stall cannot
	// block other runners; WithCleanSaveSkip is outermost so an unneeded save
	// skips before a deadline timer exists.
	backend = cacheManager.WithCleanSaveSkip(cacheManager.WithStateDeadline(backend, runtime.StateDeadline()))
	if err := backend.Open(ctx); err != nil {
		return nil, nil, err
	}
	// One deferred unwind, registered only after a successful Open, gives back
	// whatever was taken on every failure path until committed is set.
	var releaseLock func() error
	committed := false
	defer func() {
		if committed {
			return
		}
		unwindBackend(ctx, backend, releaseLock)
	}()

	lockCtx, releaseLock, err := backend.Lock(ctx)
	if err != nil {
		return nil, nil, err
	}
	runtime.Output.Printf("Load storage")
	st, err := backend.LoadStore(lockCtx)
	if err != nil {
		return lockCtx, nil, err
	}
	runtime.Output.Printf("Load projects registry")
	registry, err := backend.LoadProjectRegistry(lockCtx)
	if err != nil {
		return lockCtx, nil, err
	}
	committed = true
	return lockCtx, &cleanupState{
		backend:  backend,
		store:    st,
		registry: registry,
		release:  releaseLock,
	}, nil
}

// unwindBackend releases the lock, when one was granted, then closes the
// backend, swallowing both errors: it runs on a failing path whose own error
// is the one the operator needs.
func unwindBackend(ctx context.Context, backend cacheManager.Backend, releaseLock func() error) {
	if releaseLock != nil {
		_ = releaseLock()
	}
	_ = backend.Close(ctx)
}

func finalizeCleanup(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
	removed int,
) error {
	// Never save a store that was never persisted: that would fabricate a
	// persisted-and-empty snapshot the next run reads as evidence that nothing
	// is installed or warmed anywhere.
	if !cfg.DryRun && st.WasPersisted() {
		if err := backend.SaveStore(ctx, st); err != nil {
			return err
		}
	}
	if cfg.DryRun {
		runtime.Output.Okf("Dry-run cleanup complete. Candidates: %d", removed)
		return nil
	}
	runtime.Output.Okf("Cleanup complete. Removed: %d", removed)
	return nil
}
