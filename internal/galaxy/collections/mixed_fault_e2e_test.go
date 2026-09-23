package collections_test

// One run with a byte-dripped and a stalled collection must exit ExitInstall,
// not ExitInterrupt, because the stall renders its watchdog's context.Canceled
// with %v. The 2ms drip stays far inside the 100ms idle window, even under -race.

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

// mixedFaultTimeout is the watchdog idle window and http.Client timeout; it
// must stay small so the stall trips well inside mixedFaultDeadline.
const mixedFaultTimeout = 100 * time.Millisecond

// mixedFaultDeadline is the artifact download deadline, above the stall's
// 1.8s worst case: four 100ms idle windows plus at most 1400ms of backoff.
const mixedFaultDeadline = 3 * time.Second

// newMixedFaultFixture registers acme.drip and acme.stall on a fresh fake
// server with a cold cache, NoCache (each acquired once) and two workers, so
// both installs fail concurrently.
func newMixedFaultFixture(t *testing.T) (*config.Config, *infra.Infra, *fakegalaxy.Server) {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirementsMulti(t, reqPath, "acme.drip", "acme.stall")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "drip", "1.0.0", nil)
	s.AddVersion("acme", "stall", "1.0.0", nil)

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          2,
		Timeout:          mixedFaultTimeout,
		NoCache:          true,
	}
	runtime := infra.New(noopPrinter{}, fetch.New(cfg.Timeout, nil))
	runtime.ArtifactDownloadDeadline = mixedFaultDeadline
	return cfg, runtime, s
}

// TestMixedDripAndStallDoesNotClassifyAsInterrupt pins that a run joining
// ErrArtifactDownloadDeadline and ErrReadStalled exits ExitInstall; the stall
// uses StallAfterBytes, since Hang would trip ResponseHeaderTimeout first.
func TestMixedDripAndStallDoesNotClassifyAsInterrupt(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newMixedFaultFixture(t)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "drip", fakegalaxy.Fault{DripInterval: 2 * time.Millisecond, Count: -1})
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "stall", fakegalaxy.Fault{StallAfterBytes: 8, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a mixed drip/stall run, got nil")
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}
	// Both causes must be present, or a single-cause run could pass the
	// classification checks below by accident.
	if !errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
		t.Fatalf("expected errors.Is ErrArtifactDownloadDeadline, got %v", err)
	}
	if !errors.Is(err, helpers.ErrReadStalled) {
		t.Fatalf("expected errors.Is ErrReadStalled, got %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("!errors.Is(err, context.Canceled) failed: err = %v", err)
	}
	// The ExitInterrupt check is implied by the ExitInstall one; it is kept to
	// name the property this test covers.
	if got := exitcode.FromError(err); got == exitcode.ExitInterrupt {
		t.Errorf("exitcode.FromError(err) = %d, want != ExitInterrupt (%d)", got, exitcode.ExitInterrupt)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitInstall {
		t.Errorf("exitcode.FromError(err) = %d, want ExitInstall (%d)", got, exitcode.ExitInstall)
	}
	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "drip"))
	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "stall"))
}

// TestMixedFaultFixtureInstallsWithoutFaults is the positive control: the same
// fixture with no fault armed installs both collections, so its timeouts are
// not what fails the faulted run.
func TestMixedFaultFixtureInstallsWithoutFaults(t *testing.T) {
	t.Parallel()
	cfg, runtime, _ := newMixedFaultFixture(t)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "drip")
	assertManifestInstalled(t, cfg.DownloadPath, "stall")
}
