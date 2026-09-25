// Package collections implements install, warm, lock and outdated for
// collections and roles. Cache access funnels through withBackend, whose work
// runs under the lock's holder context; outdated opens no backend at all.
package collections

import (
	"context"
	"fmt"
	"os"
	"time"

	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

type installState struct {
	backend      cacheManager.Backend
	store        *store.Store
	release      func() error
	extractStore *extracted.Store
	// gitMemo is the run-wide table of discovered git collections, on the
	// state because the install phase reads what the resolve phase wrote.
	gitMemo *gitDiscoveryMemo
	// roleMemo is gitMemo's counterpart for roles: what the resolve phase
	// learned about each role, read by the install phase.
	roleMemo *roleDiscoveryMemo
	// urlMemo is gitMemo's counterpart for url sources, read by the solver
	// and the install phase.
	urlMemo *urlDiscoveryMemo
}

// resolveDeps builds the dependency set a resolve runs with: the store, and
// the git store and memo every phase shares.
func (s *installState) resolveDeps(cfg *config.Config, runtime *infra.Infra) collectionDeps {
	return newCollectionDeps(cfg, runtime, s.store).withSources(s.backend.Artifacts(), s.gitMemo, s.roleMemo, s.urlMemo)
}

type installPlan struct {
	collections map[string]collection
	graph       map[string][]string
	// roles is every role the requirements file and their dependencies
	// resolved to, keyed by install name; see resolveRoles.
	roles    roleResolution
	prefetch *prefetcher
	// verify is this run's signature verification state, nil when the run
	// verifies nothing; it comes from the requirements roots, so it lives on
	// the plan rather than on the state.
	verify *verifyContext
	levels [][]string
}

// stateWork is the work half of a collection command's lifecycle/work split:
// it runs against an already-initialized installState and never touches the
// backend lifecycle itself - not state.release, not state.backend.Close.
type stateWork func(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, start time.Time) error

// withBackend opens the backend, takes its exclusive lock, runs work under
// the holder context and judges both the init error and work's result with
// cacheManager.LockLostError; Close keeps the caller's ctx to run after loss.
func withBackend(ctx context.Context, cfg *config.Config, runtime *infra.Infra, banner string, work stateWork) error {
	runtime.Output.Printf("%s", banner)
	start := time.Now()
	lockCtx, state, err := initInstall(ctx, cfg, runtime)
	if err != nil {
		return cacheManager.LockLostError(ctx, lockCtx, err)
	}
	defer func() {
		if state.release != nil {
			if err := state.release(); err != nil {
				runtime.Output.Errorf("Lock release: %v", err)
			}
		}
	}()
	defer func() {
		_ = state.backend.Close(ctx)
	}()
	// Removes every --no-cache build or download discovery handed to the
	// install phase that no install worker took.
	defer state.gitMemo.cleanup()
	defer state.roleMemo.cleanup()
	defer state.urlMemo.cleanup()

	return cacheManager.LockLostError(ctx, lockCtx, work(lockCtx, cfg, runtime, state, start))
}

// initInstall is the one funnel install, warm and lock pass through, so no
// command can skip the dry-run banner. It returns the holder context on
// post-lock failures too, so a stolen lock is reported as such.
func initInstall(ctx context.Context, cfg *config.Config, runtime *infra.Infra) (context.Context, *installState, error) {
	if cfg.DryRun {
		dryRunBanner(runtime)
	}
	// A warning, not a usage error: both flags often arrive from an ambient CI
	// environment. cfg.Check is deliberately not read, since lock --check
	// --refresh still honors --refresh.
	if cfg.Refresh && cfg.Offline {
		runtime.Output.Warnf("--offline: skipping --refresh; cached state is the only source of truth offline")
	}
	runtime.Output.Printf("Init cache backend")
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		return nil, nil, err
	}
	// Every cache-state operation runs under its own budget, so a stall cannot
	// hold the exclusive lock indefinitely; WithCleanSaveSkip wraps outermost
	// so an unneeded save is skipped before any timer is built.
	backend = cacheManager.WithCleanSaveSkip(cacheManager.WithStateDeadline(backend, runtime.StateDeadline()))
	if err := backend.Open(ctx); err != nil {
		return nil, nil, err
	}
	// One unwind for every failure path after a successful Open, so any
	// return before the commit gives back what was taken; a failed Open
	// closes nothing.
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

	extractStore := newExtractStore(cfg)
	// The exclusive lock is held, so every leftover download or extract temp
	// is a dead run's orphan and safe to reclaim.
	sweepDeadRunTemps(lockCtx, runtime, backend, extractStore)

	snapshotStart := time.Now()
	runtime.Output.Printf("Load storage")
	st, err := backend.LoadStore(lockCtx)
	if err != nil {
		return lockCtx, nil, err
	}
	runtime.Output.DebugSincef(snapshotStart, "%s", "Load snapshot")
	if err := clearCacheIfRequested(lockCtx, cfg, runtime, backend, st); err != nil {
		return lockCtx, nil, err
	}
	recordProjectUnlessDryRun(lockCtx, cfg, runtime, backend)

	committed = true
	return lockCtx, &installState{
		backend:      backend,
		store:        st,
		release:      releaseLock,
		extractStore: extractStore,
		gitMemo:      newGitDiscoveryMemo(),
		roleMemo:     newRoleDiscoveryMemo(),
		urlMemo:      newURLDiscoveryMemo(),
	}, nil
}

// unwindBackend releases the lock (nil when Lock itself failed) and then
// closes the backend after initInstall fails, swallowing both errors so the
// run's own failure is the one reported.
func unwindBackend(ctx context.Context, backend cacheManager.Backend, releaseLock func() error) {
	if releaseLock != nil {
		_ = releaseLock()
	}
	_ = backend.Close(ctx)
}

// clearCacheIfRequested honors --clear-cache by wiping the in-memory caches
// and the cached artifact files, except under --dry-run, which must never
// perform that destructive mutation.
func clearCacheIfRequested(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
) error {
	if !cfg.ClearCache {
		return nil
	}
	if cfg.DryRun {
		runtime.Output.Warnf("--dry-run: skipping --clear-cache")
		return nil
	}
	st.ClearCaches()
	return backend.ClearFiles(ctx)
}

// recordProjectUnlessDryRun records this project in the registry except
// under --dry-run: a previewed broken requirements file enrolled there would
// abort every cleanup sharing this cache.
func recordProjectUnlessDryRun(ctx context.Context, cfg *config.Config, runtime *infra.Infra, backend cacheManager.Backend) {
	if cfg.DryRun {
		return
	}
	if err := backend.RecordProject(ctx, cfg.RequirementsFile, cfg.DownloadPath, cfg.RolesPath); err != nil {
		runtime.Output.Warnf("Failed to record project: %v", err)
	}
}

// newExtractStore returns a content-addressable extraction store, or nil
// when caching is disabled or no cache directory is configured.
func newExtractStore(cfg *config.Config) *extracted.Store {
	if cfg == nil || cfg.NoCache {
		return nil
	}
	return extracted.NewStore(cfg.CacheDir)
}

// sweepDeadRunTemps best-effort reclaims temps a killed run left, safe only
// under the exclusive lock; it runs even under --dry-run, since an orphan
// temp asserts nothing a later read relies on.
func sweepDeadRunTemps(ctx context.Context, runtime *infra.Infra, backend cacheManager.Backend, extractStore *extracted.Store) {
	if err := backend.SweepTemp(ctx); err != nil {
		runtime.Output.Warnf("Failed to sweep leftover download temps: %v", err)
	}
	if err := extractStore.SweepTemp(); err != nil {
		runtime.Output.Warnf("Failed to sweep leftover extract temps: %v", err)
	}
}

func prepareInstallPlan(
	ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, root *os.Root,
) (*installPlan, error) {
	roots, roleRoots, err := loadRoots(cfg, runtime)
	if err != nil {
		return nil, err
	}

	// Built from the requirements roots, under --frozen too, and ahead of the
	// prefetcher so a keyring or signatures misconfiguration fails the run
	// before any background download is scheduled.
	verify, err := newVerifyContext(cfg, runtime, roots)
	if err != nil {
		return nil, err
	}

	resolved, graph, err := resolveOrLoadLockfile(ctx, cfg, runtime, state, roots, verify)
	if err != nil {
		return nil, err
	}

	// Roles resolve ahead of the prefetcher, so a missing role or a refusing
	// repository fails the run before any background download is scheduled.
	roles, err := resolveOrLoadRoles(ctx, cfg, runtime, state, roleRoots)
	if err != nil {
		return nil, err
	}

	// Levels are built before the prefetcher, so a dependency cycle fails
	// before any prefetch worker exists and the queue follows level order.
	collections, levels, err := planCollections(runtime, roots, resolved, graph)
	if err != nil {
		return nil, err
	}

	prefetchStart := time.Now()
	prefetchDeps := newPrefetchDeps(cfg, runtime, state.store, state.backend.Artifacts(), root)
	prefetchDeps.collectionDeps = prefetchDeps.withSources(state.backend.Artifacts(), state.gitMemo, state.roleMemo, state.urlMemo)
	prefetch := startPrefetcher(ctx, prefetchDeps, collections, levels)
	runtime.Output.DebugSincef(prefetchStart, "%s", "Prefetch schedule")

	return &installPlan{
		collections: collections,
		graph:       graph,
		roles:       roles,
		levels:      levels,
		prefetch:    prefetch,
		verify:      verify,
	}, nil
}

// resolveOrLoadRoles is resolveOrLoadLockfile for the roles list: under
// --frozen every role comes from the lockfile, else from discovery.
func resolveOrLoadRoles(
	ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, roots []requirements.RoleRequirement,
) (roleResolution, error) {
	if cfg.Frozen {
		lf, err := lockfile.LoadRequired(lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile))
		if err != nil {
			return roleResolution{}, err
		}
		return resolveRolesFromLockfile(lf, roots)
	}
	return resolveRoles(ctx, state.resolveDeps(cfg, runtime), roots)
}

// resolveOrLoadLockfile resolves through the solver, or under --frozen
// builds resolved and graph from the lockfile with no network calls; the
// locked download URLs stand in for metadata only when verify is off.
func resolveOrLoadLockfile(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	roots []collection,
	verify *verifyContext,
) (map[string]collection, map[string][]string, error) {
	if cfg.Frozen {
		path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
		runtime.Output.Printf("Frozen: using lockfile %s", path)
		lf, err := lockfile.LoadRequired(path)
		if err != nil {
			return nil, nil, err
		}
		// A server's signatures ride on the version metadata, so a verifying
		// run must still fetch it and cannot take the locked URL alone.
		return resolveFromLockfile(cfg, lf, roots, !verify.enabled())
	}

	resolveStart := time.Now()
	runtime.Output.Printf("Resolve dependencies")
	resolved, graph, err := resolveCollectionsInternal(
		ctx,
		state.resolveDeps(cfg, runtime),
		roots,
		resolveTopLevel,
	)
	if err != nil {
		return nil, nil, annotateOfflineConflict(cfg, fmt.Errorf("failed to resolve dependencies: %w", err))
	}
	runtime.Output.DebugSincef(resolveStart, "%s", "Resolve dependencies")
	return resolved, graph, nil
}

// loadRoots parses the requirements file, galaxy.toml or requirements.yml,
// into collection and role roots. A collection without source: keeps an empty
// Source, so it walks the whole server list rather than being nailed to one.
func loadRoots(cfg *config.Config, runtime *infra.Infra) ([]collection, []requirements.RoleRequirement, error) {
	runtime.Output.Printf("Load collections from requirements file")
	collectionsDirect, file, err := loadRequirements(cfg.RequirementsFile, "")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load requirements file: %w", err)
	}
	for _, w := range file.Warnings {
		runtime.Output.Warnf("%s", w)
	}
	// The first point that knows whether the run has roles, and so the only
	// place the queued roles_path warnings can be judged worth printing.
	if len(file.Roles) > 0 {
		runtime.WarnRoleConfig(cfg)
	}
	runtime.Output.Printf("Prepare roots")
	roots, err := prepareRoots(collectionsDirect)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to prepare requirements: %w", err)
	}
	return roots, file.Roles, nil
}

// buildCollectionsMap folds the resolved set into a key-addressed map,
// refusing an unsafe identifier (helpers.SplitFQDN checks no path safety), an
// inexact version and a duplicate key before any install work starts.
func buildCollectionsMap(resolved map[string]collection) (map[string]collection, error) {
	collections := make(map[string]collection, len(resolved))
	for _, col := range resolved {
		if !helpers.IsPathElement(col.Namespace) || !helpers.IsPathElement(col.Name) {
			// The version is identity context only; this branch fires on
			// the namespace or name alone.
			return nil, fmt.Errorf("%w: ns=%q name=%q version=%q",
				helpers.ErrUnsafeCollectionIdentifier, col.Namespace, col.Name, col.Version)
		}
		if !helpers.IsExactVersion(col.Version) {
			return nil, fmt.Errorf("%w: %s.%s: %q", helpers.ErrInvalidCollectionVersion, col.Namespace, col.Name, col.Version)
		}
		key := col.key()
		if _, ok := collections[key]; ok {
			return nil, fmt.Errorf("%w: %s", helpers.ErrDuplicateCollectionKey, key)
		}
		collections[key] = col
	}
	return collections, nil
}

// planCollections folds the resolved set into a map, checks every named root
// came back and orders the graph into install levels, so install and warm
// refuse a dropped root and a dependency cycle alike.
func planCollections(
	runtime *infra.Infra, roots []collection, resolved map[string]collection, graph map[string][]string,
) (map[string]collection, [][]string, error) {
	collections, err := buildCollectionsMap(resolved)
	if err != nil {
		return nil, nil, err
	}
	if err := verifyRootsResolved(roots, resolved); err != nil {
		return nil, nil, err
	}
	levelStart := time.Now()
	levels, err := buildInstallLevels(graph)
	if err != nil {
		return nil, nil, err
	}
	runtime.Output.DebugSincef(levelStart, "%s", "Build install levels")
	return collections, levels, nil
}

// verifyRootsResolved fails with helpers.ErrMissingResolvedRoot for the first
// root left unresolved; on a fresh solve it is the only check that catches a
// solver silently dropping a root.
func verifyRootsResolved(roots []collection, resolved map[string]collection) error {
	for _, col := range roots {
		// A nameless git or url root has no fqdn to look up; discovery and,
		// under --frozen, verifyRootsAgainstLockfile verify it instead.
		if (col.isGit() || col.isURL()) && col.Namespace == "" && col.Name == "" {
			continue
		}
		fqdn := fmt.Sprintf("%s.%s", col.Namespace, col.Name)
		if _, ok := resolved[fqdn]; !ok {
			return fmt.Errorf("%w: %s", helpers.ErrMissingResolvedRoot, fqdn)
		}
	}
	return nil
}

// saveDryRunSnapshotIfPersisted saves state.store only when it was persisted
// before this run, so a dry run never creates a snapshot that cleanup would
// read as "nothing installed".
func saveDryRunSnapshotIfPersisted(ctx context.Context, runtime *infra.Infra, state *installState) error {
	if !state.store.WasPersisted() {
		runtime.Output.Warnf(
			"no persisted snapshot was found; a dry run will not create one, so the metadata caches this run built are discarded",
		)
		return nil
	}
	return state.backend.SaveStore(ctx, state.store)
}

// annotateSaveFailure folds a snapshot-save failure into the run's primary
// error, keeping both matchable through errors.Is so the primary failure's
// exit class is not lost; it returns primary when the save succeeded.
func annotateSaveFailure(primary, saveErr error) error {
	if saveErr == nil {
		return primary
	}
	return fmt.Errorf("%w; snapshot save failed: %w", primary, saveErr)
}
