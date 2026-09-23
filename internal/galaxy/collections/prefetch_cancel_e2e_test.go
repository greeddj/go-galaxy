package collections_test

// Tests that every prefetch download is canceled and every worker joined
// before Start returns, so none can touch the Store or commit an artifact
// after the backend lock is released.

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// countingRoundTripper counts RoundTrip calls entered and not yet returned on
// any exit path: a deterministic signal that no request outlives Start, where
// a goroutine count would be flaky.
type countingRoundTripper struct {
	rt    http.RoundTripper
	gauge atomic.Int32
}

// RoundTrip delegates to rt, bracketing the call with gauge's increment and
// (deferred) decrement.
func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.gauge.Add(1)
	defer c.gauge.Add(-1)
	return c.rt.RoundTrip(req)
}

// newPrefetchCancelFixture serves acme.app depending on acme.lib, with a
// cold cache and a counted client that has no Timeout, so only Close's cancel
// can unblock a prefetch worker parked in a hung download.
func newPrefetchCancelFixture(t *testing.T) (*config.Config, *infra.Infra, *fakegalaxy.Server, *countingRoundTripper) {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme.app")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "app", "1.0.0", map[string]string{"acme.lib": ">=1.0.0"})
	s.AddVersion("acme", "lib", "1.0.0", nil)

	countingRT := &countingRoundTripper{rt: s.Client().Transport}
	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          4,
	}
	runtime := infra.New(noopPrinter{}, &http.Client{Transport: countingRT})
	return cfg, runtime, s, countingRT
}

// TestPrefetchWorkersJoinedOnLevelFailure pins that when level 0 fails, the
// prefetch worker hung in acme.app's artifact GET is canceled and joined
// before Start returns, leaving no request in flight.
func TestPrefetchWorkersJoinedOnLevelFailure(t *testing.T) {
	t.Parallel()
	cfg, runtime, s, countingRT := newPrefetchCancelFixture(t)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "lib", fakegalaxy.Fault{Status: http.StatusInternalServerError, Count: -1})
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "app", fakegalaxy.Fault{Hang: true, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from acme.lib's persistently failing artifact download, got nil")
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}
	// Deterministic: Close cancels then joins, so the hung RoundTrip returns
	// and decrements the gauge before Start can return.
	if got := countingRT.gauge.Load(); got != 0 {
		t.Errorf("in-flight HTTP requests immediately after Start returned = %d, want 0", got)
	}
	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "app"))
	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "lib"))
}

// TestPrefetchWorkersJoinedOnSuccess checks the zero-in-flight invariant on a
// fault-free install; a happy-path sanity check only, since the level-failure
// test is the one an unjoined worker would fail.
func TestPrefetchWorkersJoinedOnSuccess(t *testing.T) {
	t.Parallel()
	cfg, runtime, _, countingRT := newPrefetchCancelFixture(t)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "app")
	assertManifestInstalled(t, cfg.DownloadPath, "lib")
	if got := countingRT.gauge.Load(); got != 0 {
		t.Errorf("in-flight HTTP requests immediately after Start returned = %d, want 0", got)
	}
}
