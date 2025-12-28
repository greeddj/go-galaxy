package collections

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

type installState struct {
	backend cacheManager.Backend
	store   *store.Store
	release func() error
}

type installPlan struct {
	collections map[string]collection
	graph       map[string][]string
	levels      [][]string
	prefetch    *prefetcher
}

// Start installs collections according to the provided configuration.
func Start(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	err := runInstall(ctx, cfg, runtime)
	if err != nil {
		runtime.Output.PersistentPrintf("❌ Error: %s", err.Error())
	}
	return err
}

func runInstall(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	runtime.Output.Printf("🚀 Starting installation process")
	start := time.Now()
	state, err := initInstall(ctx, cfg, runtime)
	if err != nil {
		return err
	}
	defer func() {
		if state.release != nil {
			_ = state.release()
		}
	}()
	defer func() {
		_ = state.backend.Close(ctx)
	}()

	plan, err := prepareInstallPlan(ctx, cfg, runtime, state)
	if err != nil {
		return err
	}
	failures, err := installLevels(
		ctx,
		cfg,
		runtime,
		state.store,
		state.backend.Artifacts(),
		plan.collections,
		plan.graph,
		plan.levels,
		plan.prefetch,
	)
	if err != nil {
		return err
	}

	return finalizeInstall(ctx, runtime, state.backend, state.store, failures, start)
}

func prepareInstallPlan(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState) (*installPlan, error) {
	prep, err := loadRoots(cfg, runtime)
	if err != nil {
		return nil, err
	}

	resolveStart := time.Now()
	runtime.Output.Printf("🧩 resolve dependencies")
	resolved, graph, err := resolveCollectionsInternal(
		ctx,
		newCollectionDeps(cfg, runtime, state.store),
		prep.AllRoots,
		true,
		true,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve dependencies: %w", err)
	}
	runtime.Output.DebugSincef(resolveStart, "%s", "resolve dependencies")

	collections, err := buildCollectionsMap(resolved)
	if err != nil {
		return nil, err
	}

	roots, err := buildRootKeys(prep, resolved)
	if err != nil {
		return nil, err
	}
	state.store.SetRoots("last_run", roots)

	prefetchStart := time.Now()
	prefetch := startPrefetcher(
		ctx,
		newPrefetchDeps(cfg, runtime, state.store, state.backend.Artifacts()),
		collections,
	)
	runtime.Output.DebugSincef(prefetchStart, "%s", "prefetch schedule")

	levelStart := time.Now()
	levels, err := buildInstallLevels(graph)
	if err != nil {
		return nil, err
	}
	runtime.Output.DebugSincef(levelStart, "%s", "build install levels")

	return &installPlan{
		collections: collections,
		graph:       graph,
		levels:      levels,
		prefetch:    prefetch,
	}, nil
}

func initInstall(ctx context.Context, cfg *config.Config, runtime *infra.Infra) (*installState, error) {
	runtime.Output.Printf("🚀 init cache backend")
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		return nil, err
	}
	if err := backend.Open(ctx); err != nil {
		return nil, err
	}
	releaseLock, err := backend.Lock(ctx)
	if err != nil {
		_ = backend.Close(ctx)
		return nil, err
	}

	snapshotStart := time.Now()
	runtime.Output.Printf("🚀 load storage")
	st, err := backend.LoadStore(ctx)
	if err != nil {
		_ = releaseLock()
		_ = backend.Close(ctx)
		return nil, err
	}
	runtime.Output.DebugSincef(snapshotStart, "%s", "load snapshot")
	if cfg.ClearCache {
		st.ClearCaches()
		if err := backend.ClearFiles(ctx); err != nil {
			_ = releaseLock()
			_ = backend.Close(ctx)
			return nil, err
		}
	}
	if err := backend.RecordProject(ctx, cfg.RequirementsFile, cfg.DownloadPath); err != nil {
		runtime.Output.PersistentPrintf("⚠️ Failed to record project: %v", err)
	}

	return &installState{
		backend: backend,
		store:   st,
		release: releaseLock,
	}, nil
}

func loadRoots(cfg *config.Config, runtime *infra.Infra) (*rootPreparation, error) {
	runtime.Output.Printf("🗂️ load collections from requirements file")
	collectionsDirect, rolesFound, err := loadRequirements(cfg.RequirementsFile, cfg.Server)
	if err != nil {
		return nil, fmt.Errorf("failed to load requirements file: %w", err)
	}
	if rolesFound {
		runtime.Output.PersistentPrintf("⚠️ requirements.yml contains roles, but roles are not supported.")
	}
	runtime.Output.Printf("🧩 prepare roots")
	prep, err := prepareRoots(cfg, collectionsDirect)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare requirements: %w", err)
	}
	return prep, nil
}

func buildCollectionsMap(resolved map[string]collection) (map[string]collection, error) {
	collections := make(map[string]collection, len(resolved))
	for _, col := range resolved {
		key := col.key()
		if _, ok := collections[key]; ok {
			return nil, fmt.Errorf("%w: %s", helpers.ErrDuplicateCollectionKey, key)
		}
		collections[key] = col
	}
	return collections, nil
}

func buildRootKeys(prep *rootPreparation, resolved map[string]collection) ([]string, error) {
	roots := make([]string, 0, len(prep.AllRoots))
	for _, col := range prep.AllRoots {
		fqdn := fmt.Sprintf("%s.%s", col.Namespace, col.Name)
		resolvedCol, ok := resolved[fqdn]
		if !ok {
			return nil, fmt.Errorf("%w: %s", helpers.ErrMissingResolvedRoot, fqdn)
		}
		roots = append(roots, resolvedCol.key())
	}
	return roots, nil
}

func installLevels(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	artifacts cacheManager.ArtifactStore,
	collections map[string]collection,
	graph map[string][]string,
	levels [][]string,
	prefetch *prefetcher,
) (int32, error) {
	depsCtx := newInstallDeps(cfg, runtime, st, artifacts, nil)
	var failures int32
	for _, level := range levels {
		var wg sync.WaitGroup
		sem := make(chan struct{}, cfg.Workers)

		for _, key := range level {
			col, ok := collections[key]
			if !ok {
				return failures, fmt.Errorf("%w for: %s", helpers.ErrMissingCollection, key)
			}
			depKeys := graph[key]
			if depKeys == nil {
				depKeys = []string{}
			}
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				meta, ok, prefetchErr := prefetch.Wait(col.key())
				if ok && prefetchErr != nil {
					runtime.Output.PersistentPrintf("⚠️ Prefetch failed for %s: %v", col.key(), prefetchErr)
				}
				if err := installCollection(ctx, col, depsCtx, depKeys, meta); err != nil {
					runtime.Output.PersistentPrintf("❌ Failed: %s.%s error: %s", col.Namespace, col.Name, err)
					atomic.AddInt32(&failures, 1)
				} else {
					runtime.Output.PersistentPrintf("✅ Installed: %s.%s", col.Namespace, col.Name)
				}
			})
		}

		wg.Wait()
		if atomic.LoadInt32(&failures) > 0 {
			break
		}
	}
	return atomic.LoadInt32(&failures), nil
}

func finalizeInstall(
	ctx context.Context,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
	failures int32,
	start time.Time,
) error {
	saveStart := time.Now()
	if err := backend.SaveStore(ctx, st); err != nil {
		return err
	}
	runtime.Output.DebugSincef(saveStart, "%s", "save snapshot")
	if failures > 0 {
		runtime.Output.PersistentPrintf("⚠️ Completed with errors: %d failed. Took %s", failures, time.Since(start).Round(time.Second))
		return fmt.Errorf("%w for %d collections", helpers.ErrInstallationFailed, failures)
	}
	runtime.Output.PersistentPrintf("🤩 All done. Took %s", time.Since(start).Round(time.Second))
	return nil
}
