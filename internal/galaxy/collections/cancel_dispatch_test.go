package collections

// These tests prove runInstallLevel and warmCollections dispatch nothing new
// after cancellation and return no error for it, so the run's tail still saves.
// Each "canceled" subtest has a "live" control showing the fixture can fail.

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

// newCancelDispatchInstallFixture builds an installDeps and a three-key level
// with a nil root, so every installCollection fails offline and
// deterministically through newInstallTarget's nil-root guard.
func newCancelDispatchInstallFixture(
	t *testing.T,
) (installDeps, map[string]collection, map[string][]string, []string, *capturingPrinter) {
	t.Helper()
	cfg := &config.Config{Workers: 1, Offline: true}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	deps := newInstallDeps(cfg, runtime, store.New(), nil, nil, nil, nil, nil)
	collections := map[string]collection{
		"acme.alpha@1.0.0": {Namespace: "acme", Name: "alpha", Version: "1.0.0"},
		"acme.beta@1.0.0":  {Namespace: "acme", Name: "beta", Version: "1.0.0"},
		"acme.gamma@1.0.0": {Namespace: "acme", Name: "gamma", Version: "1.0.0"},
	}
	graph := map[string][]string{}
	level := []string{"acme.alpha@1.0.0", "acme.beta@1.0.0", "acme.gamma@1.0.0"}
	return deps, collections, graph, level, printer
}

// waitRunInstallLevel waits for runInstallLevel's result on done, failing on
// a non-nil result or when zeroWorkersDeadlockTimeout elapses first.
func waitRunInstallLevel(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runInstallLevel returned %v, want nil", err)
		}
	case <-time.After(zeroWorkersDeadlockTimeout):
		t.Fatal("runInstallLevel did not return before the timeout")
	}
}

// TestRunInstallLevelStopsDispatchingAfterCancel proves runInstallLevel's
// dispatch loop, over a three-key level, stops handing out further keys once
// the run's context is canceled.
func TestRunInstallLevelStopsDispatchingAfterCancel(t *testing.T) {
	t.Parallel()

	t.Run("canceled", func(t *testing.T) {
		t.Parallel()
		deps, collections, graph, level, printer := newCancelDispatchInstallFixture(t)
		var failures failureRecorder
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		done := make(chan error, 1)
		go func() {
			done <- runInstallLevel(ctx, deps, collections, graph, level, &prefetcher{}, &failures)
		}()
		waitRunInstallLevel(t, done)

		// Every dispatched collection fails, so zero failures means none was
		// dispatched.
		if got := failures.count(); got != 0 {
			t.Fatalf("failures.count() = %d, want 0: a canceled run must dispatch no collection", got)
		}
		if printer.hasErrContaining("Failed: ") {
			t.Fatalf("printer recorded a \"Failed: \" line after cancellation: %v", printer.errs)
		}
	})

	// live proves the same fixture fails three times when not canceled.
	t.Run("live", func(t *testing.T) {
		t.Parallel()
		deps, collections, graph, level, printer := newCancelDispatchInstallFixture(t)
		var failures failureRecorder

		done := make(chan error, 1)
		go func() {
			done <- runInstallLevel(context.Background(), deps, collections, graph, level, &prefetcher{}, &failures)
		}()
		waitRunInstallLevel(t, done)

		if got := failures.count(); got != 3 {
			t.Fatalf(
				"failures.count() = %d, want 3: every collection in the level must fail through the fixture's nil-root guard",
				got,
			)
		}
		if !printer.hasErrContaining("Failed: ") {
			t.Fatalf("expected a \"Failed: \" line from the live control, got %v", printer.errs)
		}
	})
}

// newCancelDispatchWarmFixture builds a warm run over three collections with
// no server and Offline set, so every warmOne fails offline through an empty
// server-candidate list.
func newCancelDispatchWarmFixture(
	t *testing.T,
) (*config.Config, *infra.Infra, *installState, map[string]collection, *capturingPrinter) {
	t.Helper()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	cfg := &config.Config{CacheDir: cacheDir, Workers: 1, Offline: true}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	state := &installState{backend: local.New(cacheDir), store: store.New()}
	collections := map[string]collection{
		"acme.alpha@1.0.0": {Namespace: "acme", Name: "alpha", Version: "1.0.0"},
		"acme.beta@1.0.0":  {Namespace: "acme", Name: "beta", Version: "1.0.0"},
		"acme.gamma@1.0.0": {Namespace: "acme", Name: "gamma", Version: "1.0.0"},
	}
	return cfg, runtime, state, collections, printer
}

// waitWarmCollections returns warmCollections's failureSummary from done,
// failing the test when zeroWorkersDeadlockTimeout elapses first.
func waitWarmCollections(t *testing.T, done <-chan failureSummary) failureSummary {
	t.Helper()
	select {
	case summary := <-done:
		return summary
	case <-time.After(zeroWorkersDeadlockTimeout):
		t.Fatal("warmCollections did not return before the timeout")
		return failureSummary{}
	}
}

// TestWarmCollectionsStopsDispatchingAfterCancel proves warmCollections
// dispatches nothing once ctx is canceled and still returns its
// failureSummary rather than an error derived from ctx.Err().
func TestWarmCollectionsStopsDispatchingAfterCancel(t *testing.T) {
	t.Parallel()

	t.Run("canceled", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, state, collections, printer := newCancelDispatchWarmFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		done := make(chan failureSummary, 1)
		go func() {
			done <- warmCollections(ctx, cfg, runtime, state, collections, nil)
		}()
		summary := waitWarmCollections(t, done)

		// Every dispatched collection fails, so zero failures means none was
		// dispatched.
		if summary.count != 0 {
			t.Fatalf("summary.count = %d, want 0: a canceled run must dispatch no collection", summary.count)
		}
		if printer.hasErrContaining("Failed: ") {
			t.Fatalf("printer recorded a \"Failed: \" line after cancellation: %v", printer.errs)
		}
	})

	// live proves the same fixture fails three times when not canceled.
	t.Run("live", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, state, collections, printer := newCancelDispatchWarmFixture(t)

		done := make(chan failureSummary, 1)
		go func() {
			done <- warmCollections(context.Background(), cfg, runtime, state, collections, nil)
		}()
		summary := waitWarmCollections(t, done)

		if summary.count != 3 {
			t.Fatalf(
				"summary.count = %d, want 3: every collection must fail through the fixture's empty server-candidate list",
				summary.count,
			)
		}
		if !printer.hasErrContaining("Failed: ") {
			t.Fatalf("expected a \"Failed: \" line from the live control, got %v", printer.errs)
		}
	})
}
