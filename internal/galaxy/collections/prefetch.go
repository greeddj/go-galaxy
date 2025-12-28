package collections

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/psvmcc/hub/pkg/types"
)

// prefetcher coordinates background metadata and artifact downloads.
type prefetcher struct {
	mu   sync.Mutex
	meta map[string]*types.GalaxyCollectionVersionInfo
	errs map[string]error
	done map[string]chan struct{}
}

// startPrefetcher schedules prefetch tasks for collections.
func startPrefetcher(ctx context.Context, deps prefetchDeps, collections map[string]collection) *prefetcher {
	cfg := deps.cfg
	artifacts := deps.artifacts
	p := &prefetcher{
		meta: make(map[string]*types.GalaxyCollectionVersionInfo),
		errs: make(map[string]error),
		done: make(map[string]chan struct{}),
	}
	if cfg == nil || cfg.NoCache || artifacts == nil {
		return p
	}

	tasks := buildPrefetchTasks(ctx, deps, collections, p)
	if len(tasks) == 0 {
		return p
	}

	taskCh := makeTaskChannel(tasks)
	startPrefetchWorkers(ctx, deps, p, taskCh)
	return p
}

func buildPrefetchTasks(
	ctx context.Context,
	deps prefetchDeps,
	collections map[string]collection,
	p *prefetcher,
) []collection {
	cfg := deps.cfg
	st := deps.st
	artifacts := deps.artifacts
	tasks := make([]collection, 0, len(collections))
	for _, col := range collections {
		if !isGalaxyType(col.Type) {
			continue
		}
		installPath := filepath.Join(cfg.DownloadPath, "ansible_collections", col.Namespace, col.Name)
		if canSkipInstall(cfg, col, installPath, st) {
			continue
		}
		if ok, err := artifacts.Has(ctx, artifactKey(col)); err == nil && ok {
			continue
		}
		p.register(col.key())
		tasks = append(tasks, col)
	}
	return tasks
}

func makeTaskChannel(tasks []collection) chan collection {
	taskCh := make(chan collection, len(tasks))
	for _, col := range tasks {
		taskCh <- col
	}
	close(taskCh)
	return taskCh
}

func startPrefetchWorkers(
	ctx context.Context,
	deps prefetchDeps,
	p *prefetcher,
	taskCh chan collection,
) {
	cfg := deps.cfg
	for range max(cfg.Workers, 1) {
		go func() {
			for col := range taskCh {
				meta, err := prefetchOne(ctx, deps, col)
				p.finish(col.key(), meta, err)
			}
		}()
	}
}

func prefetchOne(
	ctx context.Context,
	deps prefetchDeps,
	col collection,
) (*types.GalaxyCollectionVersionInfo, error) {
	meta, err := loadCollectionMetadata(ctx, deps.collectionDeps, col)
	if err != nil {
		return nil, err
	}
	key := artifactKey(col)
	ok, statErr := deps.artifacts.Has(ctx, key)
	if statErr != nil {
		return meta, statErr
	}
	if ok {
		return meta, nil
	}
	_, err = downloadCollectionToCache(ctx, newInstallDeps(deps.cfg, deps.runtime, deps.st, deps.artifacts, nil), key, meta, true)
	return meta, err
}

// Wait blocks until prefetch for key completes and returns its metadata/error.
func (p *prefetcher) Wait(key string) (*types.GalaxyCollectionVersionInfo, bool, error) {
	p.mu.Lock()
	done := p.done[key]
	p.mu.Unlock()
	if done == nil {
		return nil, false, nil
	}
	<-done
	p.mu.Lock()
	meta := p.meta[key]
	err := p.errs[key]
	p.mu.Unlock()
	return meta, true, err
}

// register allocates a completion channel for a key.
func (p *prefetcher) register(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.done[key]; ok {
		return
	}
	p.done[key] = make(chan struct{})
}

// finish records completion data for a prefetch task.
func (p *prefetcher) finish(key string, meta *types.GalaxyCollectionVersionInfo, err error) {
	p.mu.Lock()
	p.meta[key] = meta
	p.errs[key] = err
	done := p.done[key]
	p.mu.Unlock()
	if done != nil {
		close(done)
	}
}
