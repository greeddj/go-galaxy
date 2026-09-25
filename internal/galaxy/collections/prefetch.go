package collections

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/psvmcc/hub/pkg/types"
)

// prefetcher coordinates background metadata and artifact downloads.
type prefetcher struct {
	meta   map[string]*types.GalaxyCollectionVersionInfo
	errs   map[string]error
	done   map[string]chan struct{}
	cancel context.CancelFunc
	// prefetched holds each downloaded, committed artifact until Wait hands
	// its temp off (ownership transfers there) or Close reclaims it.
	prefetched map[string]downloadResult
	// presence is the artifact keys the scan found cached and did not
	// schedule; written once before any consumer runs, read without a lock.
	presence map[string]bool
	mu       sync.Mutex
	wg       sync.WaitGroup
}

// startPrefetcher schedules prefetch tasks for collections, queued by install
// level so downloads track the install consumer; warm passes nil levels, so
// its queue is ordered by key alone.
func startPrefetcher(ctx context.Context, deps prefetchDeps, collections map[string]collection, levels [][]string) *prefetcher {
	cfg := deps.cfg
	artifacts := deps.artifacts
	p := &prefetcher{
		meta:       make(map[string]*types.GalaxyCollectionVersionInfo),
		errs:       make(map[string]error),
		done:       make(map[string]chan struct{}),
		prefetched: make(map[string]downloadResult),
	}
	// The prefetcher is disabled here rather than in prefetchOne: a dry run must
	// not download ahead. presence stays nil, so every worker probes the store.
	if cfg == nil || cfg.NoCache || cfg.DryRun || artifacts == nil {
		return p
	}

	tasks := buildPrefetchTasks(ctx, deps, collections, p)
	if len(tasks) == 0 {
		return p
	}
	sortTasksByLevel(tasks, buildLevelIndex(levels))

	// Derive a cancellable child context so Close can abort every in-flight
	// worker download without waiting for the caller's own ctx to end - the
	// worker pool must never outlive runInstall's backend lock.
	pfCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	taskCh := makeTaskChannel(tasks)
	startPrefetchWorkers(pfCtx, deps, p, taskCh)
	return p
}

// buildLevelIndex maps each install key to its zero-based install level, so the
// prefetcher can order its download queue to match the level-ordered install
// consumer. Pre-sized to the total key count.
func buildLevelIndex(levels [][]string) map[string]int {
	total := 0
	for _, level := range levels {
		total += len(level)
	}
	index := make(map[string]int, total)
	for i, level := range levels {
		for _, key := range level {
			index[key] = i
		}
	}
	return index
}

// sortTasksByLevel orders tasks by install level, then key. Order is latency
// only, since every consumer blocks on Wait for its own key; a key missing
// from levelIndex sorts at level 0.
func sortTasksByLevel(tasks []collection, levelIndex map[string]int) {
	slices.SortFunc(tasks, func(a, b collection) int {
		ka, kb := a.key(), b.key()
		if c := cmp.Compare(levelIndex[ka], levelIndex[kb]); c != 0 {
			return c
		}
		return strings.Compare(ka, kb)
	})
}

// buildPrefetchTasks probes the cache in parallel, bounded by DownloadWorkers,
// writing only disjoint slice elements, then registers tasks and builds presence
// sequentially. A scheduled key never enters presence: the prefetcher writes it.
func buildPrefetchTasks(
	ctx context.Context,
	deps prefetchDeps,
	collections map[string]collection,
	p *prefetcher,
) []collection {
	cols := make([]collection, 0, len(collections))
	for _, col := range collections {
		cols = append(cols, col)
	}

	keep := make([]bool, len(cols))
	cached := make([]bool, len(cols))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(deps.cfg.DownloadWorkers, 1))
	for i := range cols {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			keep[i], cached[i] = shouldSchedulePrefetch(ctx, deps, cols[i])
		})
	}
	wg.Wait()

	presence := make(map[string]bool, len(cols))
	tasks := make([]collection, 0, len(cols))
	for i, col := range cols {
		if cached[i] {
			presence[artifactKey(col)] = true
		}
		if keep[i] {
			p.register(col.key())
			tasks = append(tasks, col)
		}
	}
	p.presence = presence
	return tasks
}

// shouldSchedulePrefetch returns (schedule, cached), never both true. It is
// fail-open and uses the cheap installRecordMatches, since a wrong answer here
// costs only a download; installCollection's canSkipInstall is the real gate.
func shouldSchedulePrefetch(ctx context.Context, deps prefetchDeps, col collection) (bool, bool) {
	if !isSupportedType(col.Type) {
		return false, false
	}
	if target, ok := newInstallTarget(deps.root, deps.cfg, col); ok && installRecordMatches(target, col, deps.st) {
		return false, false
	}
	ok, err := deps.artifacts.Has(ctx, artifactKey(col))
	if err != nil {
		return true, false
	}
	return !ok, ok
}

func makeTaskChannel(tasks []collection) chan collection {
	taskCh := make(chan collection, len(tasks))
	for _, col := range tasks {
		taskCh <- col
	}
	close(taskCh)
	return taskCh
}

// startPrefetchWorkers starts the pool draining taskCh, sized by
// DownloadWorkers rather than Workers: a prefetch worker only downloads and
// hashes into a temp file, it never extracts.
func startPrefetchWorkers(
	pfCtx context.Context,
	deps prefetchDeps,
	p *prefetcher,
	taskCh chan collection,
) {
	cfg := deps.cfg
	for range max(cfg.DownloadWorkers, 1) {
		p.wg.Go(func() {
			for col := range taskCh {
				meta, result, err := prefetchOne(pfCtx, deps, col)
				p.finish(col.key(), meta, result, err)
			}
		})
	}
}

// prefetchOne fetches col's artifact ahead of time, after its metadata for a
// Galaxy source, returning the downloadResult the install worker reuses; a
// metadata error returns a zero downloadResult.
func prefetchOne(
	ctx context.Context,
	deps prefetchDeps,
	col collection,
) (*types.GalaxyCollectionVersionInfo, downloadResult, error) {
	if col.isGit() || col.isURL() {
		sourceDeps := newInstallDeps(deps.cfg, deps.runtime, deps.st, deps.artifacts, nil, nil, nil, nil)
		sourceDeps.collectionDeps = sourceDeps.withSources(deps.gitStore, deps.gitMemo, deps.roleMemo, deps.urlMemo)
		if col.isURL() {
			result, err := urlFetchToCache(ctx, sourceDeps, col, true)
			return nil, result, err
		}
		result, err := gitFetchToCache(ctx, sourceDeps, col, true)
		return nil, result, err
	}
	meta, err := versionMetadata(ctx, deps.collectionDeps, col)
	if err != nil {
		return nil, downloadResult{}, err
	}
	// No Has re-probe: this task is the key's only committer this run, since
	// its consumer blocks in Wait. Nil root, extract store and verify context
	// keep this worker a policy-free cache filler that installs nothing.
	downloadDeps := newInstallDeps(deps.cfg, deps.runtime, deps.st, deps.artifacts, nil, nil, nil, nil)
	downloadDeps.collectionDeps = downloadDeps.withSources(deps.gitStore, deps.gitMemo, deps.roleMemo, deps.urlMemo)
	result, err := downloadCollectionToCache(ctx, downloadDeps, artifactKey(col), col.Source, meta, true)
	if err != nil {
		return meta, downloadResult{}, err
	}
	return meta, result, nil
}

// Wait blocks until key's prefetch completes and returns its metadata,
// artifact and error. The temp's ownership moves to the caller: the entry is
// deleted so Close never reclaims it too.
func (p *prefetcher) Wait(key string) (*types.GalaxyCollectionVersionInfo, downloadResult, bool, error) {
	p.mu.Lock()
	done := p.done[key]
	p.mu.Unlock()
	if done == nil {
		return nil, downloadResult{}, false, nil
	}
	<-done
	p.mu.Lock()
	meta := p.meta[key]
	err := p.errs[key]
	result := p.prefetched[key]
	delete(p.prefetched, key)
	p.mu.Unlock()
	return meta, result, true, err
}

// Close cancels in-flight downloads, joins every worker and reclaims each temp
// no consumer claimed; it is idempotent. finish runs for every task, canceled
// ones too, so no Wait can deadlock.
func (p *prefetcher) Close() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()

	p.mu.Lock()
	for key, result := range p.prefetched {
		cleanupIfNeeded(result.Cleanup)
		delete(p.prefetched, key)
	}
	p.mu.Unlock()
}

// cachedArtifacts returns the scan's presence hints by artifactKey. Built
// before any worker is dispatched and never mutated, it needs no lock; a
// disabled prefetcher returns nil, which reads as empty.
func (p *prefetcher) cachedArtifacts() map[string]bool {
	return p.presence
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

// finish records completion data for a prefetch task, stashing any
// downloaded artifact in p.prefetched so a later Wait(key) can hand it off to
// an install worker instead of it being fetched again.
func (p *prefetcher) finish(key string, meta *types.GalaxyCollectionVersionInfo, result downloadResult, err error) {
	p.mu.Lock()
	p.meta[key] = meta
	p.errs[key] = err
	if result.Path != "" {
		p.prefetched[key] = result
	}
	done := p.done[key]
	p.mu.Unlock()
	if done != nil {
		close(done)
	}
}
