package collections_test

// Artifact download deadline end to end: a byte-drip fault keeps making
// progress, so the read-inactivity watchdog (set far above the deadline) never
// trips and only helpers.ArtifactDownloadDeadline can end the run.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// dripWatchdogTimeout is the watchdog idle window (and http.Client timeout)
// every fixture here uses, far above any armed deadline so only the artifact
// download deadline can end a drip-faulted run.
const dripWatchdogTimeout = 10 * time.Second

// newDripFixture serves one acme.solo collection from a fresh fake server.
// noCache false keeps the prefetcher on, so two acquisitions run.
// watchdogTimeout is a parameter because fetch.New bakes it into the client.
func newDripFixture(
	t *testing.T,
	noCache bool,
	deadline time.Duration,
	watchdogTimeout time.Duration,
) (*config.Config, *infra.Infra, *fakegalaxy.Server) {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme.solo")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "solo", "1.0.0", nil)

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
		Timeout:          watchdogTimeout,
		NoCache:          noCache,
	}
	runtime := infra.New(noopPrinter{}, fetch.New(cfg.Timeout, nil))
	runtime.ArtifactDownloadDeadline = deadline
	return cfg, runtime, s
}

// TestArtifactByteDripFailsAtTheDownloadDeadline pins that a byte-drip, which
// never trips the watchdog, fails closed at the whole-acquisition deadline
// with the artifact endpoint hit exactly once.
func TestArtifactByteDripFailsAtTheDownloadDeadline(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newDripFixture(t, true, 500*time.Millisecond, dripWatchdogTimeout)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "solo", fakegalaxy.Fault{DripInterval: 5 * time.Millisecond, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a byte-dripped artifact download, got nil")
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
		t.Fatalf("expected errors.Is ErrArtifactDownloadDeadline, got %v", err)
	}
	// Fixture tripwires only: one failing collection carries the deadline
	// sentinel or a watchdog stall, never both, so once the check above passes
	// these two can fail only if the fixture gains a second failing collection.
	if errors.Is(err, helpers.ErrReadStalled) {
		t.Fatalf("expected the drip to defeat the watchdog, not trip it: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("must not match context.Canceled: %v", err)
	}

	if got := runtime.Metrics.Totals().BytesDownloaded; got < 2 {
		t.Errorf("BytesDownloaded = %d, want >= 2 (progress made throughout, the byte-drip signature)", got)
	}
	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "solo"))
	// One artifact request pins terminality: downloadRetryable never retries the
	// deadline sentinel. It does not pin that the budget spans the acquisition
	// rather than one attempt; a per-attempt budget would also give 1.
	if got := s.Count(fakegalaxy.EndpointArtifact); got != 1 {
		t.Errorf("EndpointArtifact count = %d, want 1 (the deadline is terminal: no retry follows it)", got)
	}
}

// TestArtifactDripFixtureInstallsWithoutTheFault is the positive control: the
// same fixture with no fault installs, so the 500ms deadline alone does not
// fail an ordinary install.
func TestArtifactDripFixtureInstallsWithoutTheFault(t *testing.T) {
	t.Parallel()
	cfg, runtime, _ := newDripFixture(t, true, 500*time.Millisecond, dripWatchdogTimeout)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "solo")
}

// TestByteDripCostsExactlyTwoAcquisitionsPerCollection pins that a failed
// prefetch is non-fatal, so the install worker spends a second full budget:
// the reason helpers.ArtifactDownloadDeadline is 15 minutes rather than 30.
func TestByteDripCostsExactlyTwoAcquisitionsPerCollection(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newDripFixture(t, false, 400*time.Millisecond, dripWatchdogTimeout)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "solo", fakegalaxy.Fault{DripInterval: 5 * time.Millisecond, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a byte-dripped artifact download, got nil")
	}
	if !errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
		t.Fatalf("expected errors.Is ErrArtifactDownloadDeadline, got %v", err)
	}

	if got := s.Count(fakegalaxy.EndpointArtifact); got != 2 {
		t.Errorf("EndpointArtifact count = %d, want exactly 2 (one prefetch acquisition, one install-path acquisition, "+
			"each terminal once its deadline fires), not 2*helpers.FetchRetryMaxAttempts=%d", got, 2*helpers.FetchRetryMaxAttempts)
	}
}
