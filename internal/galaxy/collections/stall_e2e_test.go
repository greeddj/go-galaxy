package collections_test

// End-to-end coverage of the artifact read watchdog: a mid-body stall is
// caught rather than hanging, whether it recovers on retry or persists across
// every attempt.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// stallTimeout is the watchdog idle window and client timeout, far above any
// loopback round trip, so only a deliberately stalled body read trips it.
const stallTimeout = 100 * time.Millisecond

// waitBoundStall bounds each wait in TestParentCancelDuringStallExitsInterrupt,
// so a genuine deadlock fails the test instead of hanging the suite.
const waitBoundStall = 5 * time.Second

// newStallFixture returns a cold-cache run of one dependency-free collection
// over the real watchdog client fetch.New, with NoCache disabling the
// prefetcher so each download attempt is exactly one artifact request.
func newStallFixture(t *testing.T) (*config.Config, *infra.Infra, *fakegalaxy.Server) {
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
		Timeout:          stallTimeout,
		NoCache:          true,
	}
	runtime := infra.New(noopPrinter{}, fetch.New(cfg.Timeout, nil))
	return cfg, runtime, s
}

// TestArtifactStallRecoversOnRetry pins that a one-shot mid-body stall is
// caught by the watchdog and the retried download completes the install.
func TestArtifactStallRecoversOnRetry(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newStallFixture(t)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "solo", fakegalaxy.Fault{StallAfterBytes: 8, Count: 1})

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "solo")
	// One stalled attempt the watchdog ended, then one full download.
	if got := s.Count(fakegalaxy.EndpointArtifact); got != 2 {
		t.Errorf("EndpointArtifact count = %d, want 2 (1 stalled + 1 success)", got)
	}
}

// TestArtifactPersistentStallFailsBounded pins that a stall on every attempt
// is retried FetchRetryMaxAttempts times, then fails as ErrInstallationFailed
// with ExitInstall, never as context.Canceled or an interrupt.
func TestArtifactPersistentStallFailsBounded(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newStallFixture(t)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "solo", fakegalaxy.Fault{StallAfterBytes: 8, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a persistently stalled artifact download, got nil")
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}
	// The watchdog, not the artifact download deadline, must end this run;
	// every attempt stalls well within runtime.ArtifactDeadline().
	if errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
		t.Fatalf("expected the watchdog, not the artifact download deadline, to end this run: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("must not match context.Canceled: %v", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitInstall {
		t.Errorf("exitcode.FromError(err) = %d, want ExitInstall (%d)", got, exitcode.ExitInstall)
	}
	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "solo"))
	// Reaching here proves the watchdog fired on every attempt.
	if got := s.Count(fakegalaxy.EndpointArtifact); got != helpers.FetchRetryMaxAttempts {
		t.Errorf("EndpointArtifact count = %d, want helpers.FetchRetryMaxAttempts=%d", got, helpers.FetchRetryMaxAttempts)
	}
}

// TestArtifactStallBytesCountedPerAttemptWithNoMissOnFailure pins that every
// attempt's partial read counts toward BytesDownloaded, and that an acquisition
// that never succeeds records no cache miss.
func TestArtifactStallBytesCountedPerAttemptWithNoMissOnFailure(t *testing.T) {
	t.Parallel()
	const stallAfterBytes = 8 // same K as TestArtifactStallRecoversOnRetry above
	cfg, runtime, s := newStallFixture(t)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "solo", fakegalaxy.Fault{StallAfterBytes: stallAfterBytes, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a persistently stalled artifact download, got nil")
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}

	totals := runtime.Metrics.Totals()
	wantBytes := int64(stallAfterBytes) * int64(helpers.FetchRetryMaxAttempts)
	if totals.BytesDownloaded != wantBytes {
		t.Errorf("BytesDownloaded = %d, want %d (%d attempts, each stalling after exactly %d real bytes)",
			totals.BytesDownloaded, wantBytes, helpers.FetchRetryMaxAttempts, stallAfterBytes)
	}
	if totals.CacheMisses != 0 {
		t.Errorf("CacheMisses = %d, want 0 (the acquisition never completed successfully, so it never reaches AddCacheMiss)",
			totals.CacheMisses)
	}
}

// TestParentCancelDuringStallExitsInterrupt pins that canceling the caller's
// context during a stall surfaces context.Canceled and ExitInterrupt; every
// interleaving of cancel with the read, backoff or next request agrees.
func TestParentCancelDuringStallExitsInterrupt(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newStallFixture(t)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "solo", fakegalaxy.Fault{StallAfterBytes: 8, Count: -1})

	ctx, cancel := context.WithCancel(context.Background())
	// Also deferred: a t.Fatal before the explicit cancel() would leave the
	// stalled request parked and block the fake server's Close forever.
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- collections.Start(ctx, cfg, runtime)
	}()

	// The endpoint counter increments before the stall blocks, so Count
	// reliably signals that a request reached the fake server.
	deadline := time.Now().Add(waitBoundStall)
	for s.Count(fakegalaxy.EndpointArtifact) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Count(fakegalaxy.EndpointArtifact) < 1 {
		t.Fatal("artifact endpoint never received a request before the poll deadline")
	}
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(waitBoundStall):
		t.Fatal("Start did not return after the parent context was canceled")
	}
	if err == nil {
		t.Fatal("expected an error from a canceled stalled artifact download, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected errors.Is context.Canceled, got %v", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitInterrupt {
		t.Errorf("exitcode.FromError(err) = %d, want ExitInterrupt (%d)", got, exitcode.ExitInterrupt)
	}
}
