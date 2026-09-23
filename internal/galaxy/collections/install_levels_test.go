package collections

// Tests for runInstallLevel's ErrMissingCollection return, driven through
// installLevels: it must join every worker already dispatched in the level
// rather than leak it past runInstall's lock release.

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// TestInstallLevelsMissingCollectionSurfaces pins that a level key absent from
// the collections map fails installLevels with helpers.ErrMissingCollection,
// here with no worker dispatched at all.
func TestInstallLevelsMissingCollectionSurfaces(t *testing.T) {
	t.Parallel()

	const missingKey = "acme.missing@1.0.0"
	cfg := &config.Config{Offline: true, DownloadPath: t.TempDir(), Workers: 1}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	st := store.New()
	root := newTestCollectionsRoot(t, cfg.DownloadPath)
	// Wait(missingKey) is never reached (the guard trips before any dispatch),
	// so a bare prefetcher with only its done map allocated is enough.
	prefetch := &prefetcher{done: make(map[string]chan struct{})}

	levels := [][]string{{missingKey}}
	deps := newInstallDeps(cfg, runtime, st, nil, nil, root, nil, nil)
	plan := &installPlan{
		collections: map[string]collection{},
		graph:       map[string][]string{},
		levels:      levels,
		prefetch:    prefetch,
	}
	_, err := installLevels(context.Background(), deps, plan)
	if !errors.Is(err, helpers.ErrMissingCollection) {
		t.Fatalf("err = %v, want errors.Is helpers.ErrMissingCollection", err)
	}
}

// newBlockedPrefetcher builds a prefetcher whose Wait(key) blocks until
// finish(key, ...) closes its done channel, holding a worker mid-flight as a
// real prefetcher would not do on demand.
func newBlockedPrefetcher(key string) *prefetcher {
	return &prefetcher{
		meta:       make(map[string]*types.GalaxyCollectionVersionInfo),
		errs:       make(map[string]error),
		prefetched: make(map[string]downloadResult),
		done:       map[string]chan struct{}{key: make(chan struct{})},
	}
}

// TestInstallLevelsJoinsInFlightWorkerOnMissingCollection pins that the
// ErrMissingCollection guard does not return until the level's already
// dispatched worker, parked in prefetch.Wait, has finished.
func TestInstallLevelsJoinsInFlightWorkerOnMissingCollection(t *testing.T) {
	t.Parallel()

	col1 := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	key1 := col1.key()
	const missingKey = "acme.missing@1.0.0"

	collections := map[string]collection{key1: col1}
	graph := map[string][]string{key1: {}}
	levels := [][]string{{key1, missingKey}}

	p := newBlockedPrefetcher(key1)

	cfg := &config.Config{Offline: true, DownloadPath: t.TempDir(), Workers: 2}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	st := store.New()
	root := newTestCollectionsRoot(t, cfg.DownloadPath)

	deps := newInstallDeps(cfg, runtime, st, nil, nil, root, nil, nil)
	plan := &installPlan{
		collections: collections,
		graph:       graph,
		levels:      levels,
		prefetch:    p,
	}

	done := make(chan error, 1)
	go func() {
		_, err := installLevels(context.Background(), deps, plan)
		done <- err
	}()

	// release unblocks key1's worker and also runs on cleanup, so the
	// goroutine ends even if an assertion fails; the worker then fails fast
	// on helpers.ErrOfflineMode without touching the network.
	release := sync.OnceFunc(func() {
		p.finish(key1, &types.GalaxyCollectionVersionInfo{}, downloadResult{}, nil)
	})
	t.Cleanup(release)

	select {
	case err := <-done:
		t.Fatalf("installLevels returned without joining its in-flight worker: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Expected: installLevels stays blocked in runInstallLevel's
		// deferred wg.Wait() until key1's worker is released below.
	}

	release()

	select {
	case err := <-done:
		if !errors.Is(err, helpers.ErrMissingCollection) {
			t.Fatalf("err = %v, want errors.Is helpers.ErrMissingCollection", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("installLevels did not return after its in-flight worker was released")
	}
}
