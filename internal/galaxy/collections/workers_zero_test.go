package collections

// These tests pin that cfg.Workers == 0, reachable only from a Config built
// in code, cannot deadlock the semaphore of warmCollections or
// runInstallLevel: both size it with max(cfg.Workers, 1).

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// zeroWorkersDeadlockTimeout is how long a dispatch loop may take before it
// counts as deadlocked: loose for slow CI, tight enough to fail in seconds.
const zeroWorkersDeadlockTimeout = 5 * time.Second

// TestWarmCollectionsZeroWorkersDoesNotDeadlock proves warmCollections
// completes (rather than hanging forever on its semaphore's first send) when
// cfg.Workers is 0.
func TestWarmCollectionsZeroWorkersDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	cfg := &config.Config{CacheDir: cacheDir, Workers: 0, Offline: true}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	state := &installState{
		backend: local.New(cacheDir),
		store:   store.New(),
	}
	collections := map[string]collection{
		"acme.widgets@1.0.0": {Namespace: "acme", Name: "widgets", Version: "1.0.0"},
	}

	done := make(chan failureSummary, 1)
	go func() {
		done <- warmCollections(context.Background(), cfg, runtime, state, collections, nil)
	}()

	select {
	case <-done:
		// Returned - Workers == 0 was treated as at least one worker.
	case <-time.After(zeroWorkersDeadlockTimeout):
		t.Fatal("warmCollections(Workers: 0) did not return: the semaphore channel was never buffered, " +
			"so its first send blocked forever with no worker started yet to drain it")
	}
}

// TestRunInstallLevelZeroWorkersDoesNotDeadlock proves runInstallLevel
// completes when cfg.Workers is 0, the install side of the same hazard.
func TestRunInstallLevelZeroWorkersDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Workers: 0, Offline: true}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	// A nil root makes installCollection fail fast, which is still a prompt
	// return: only the absence of a deadlock is under test.
	deps := newInstallDeps(cfg, runtime, store.New(), nil, nil, nil, nil, nil)
	collections := map[string]collection{
		"acme.widgets@1.0.0": {Namespace: "acme", Name: "widgets", Version: "1.0.0"},
	}
	graph := map[string][]string{}
	level := []string{"acme.widgets@1.0.0"}
	var failures failureRecorder

	done := make(chan error, 1)
	go func() {
		done <- runInstallLevel(context.Background(), deps, collections, graph, level, &prefetcher{}, &failures)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runInstallLevel: %v", err)
		}
	case <-time.After(zeroWorkersDeadlockTimeout):
		t.Fatal("runInstallLevel(Workers: 0) did not return: the semaphore channel was never buffered, " +
			"so its first send blocked forever with no worker started yet to drain it")
	}
}
