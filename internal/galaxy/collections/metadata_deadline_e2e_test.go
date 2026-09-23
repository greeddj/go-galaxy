package collections_test

// End-to-end tests for helpers.MetadataFetchDeadline: a byte-drip on root
// metadata never trips the read watchdog (dripWatchdogTimeout sits far above
// metadataDripBudget), so only the whole-request deadline can end the run.

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

// metadataDripBudget is the deps.runtime.MetadataFetchDeadline override
// every fixture in this file uses.
const metadataDripBudget = 500 * time.Millisecond

// newMetadataDripFixture registers acme.solo on a fresh fake server and returns
// a config and runtime whose watchdog window, dripWatchdogTimeout, sits far
// above deadline, so only the metadata deadline can end a drip-faulted run.
func newMetadataDripFixture(t *testing.T, deadline time.Duration) (*config.Config, *infra.Infra, *fakegalaxy.Server) {
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
		Timeout:          dripWatchdogTimeout,
	}
	runtime := infra.New(noopPrinter{}, fetch.New(cfg.Timeout, nil))
	runtime.MetadataFetchDeadline = deadline
	return cfg, runtime, s
}

// TestMetadataByteDripFailsTheRunAtTheMetadataDeadline pins that a byte-drip on
// root metadata fails the run with helpers.ErrMetadataFetchDeadline, not behind
// helpers.ErrInstallationFailed, and installs nothing.
func TestMetadataByteDripFailsTheRunAtTheMetadataDeadline(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newMetadataDripFixture(t, metadataDripBudget)
	s.Fail(fakegalaxy.EndpointRootMetadata, "acme", "solo", fakegalaxy.Fault{DripInterval: 5 * time.Millisecond, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a byte-dripped metadata response, got nil")
	}
	if !errors.Is(err, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("expected errors.Is ErrMetadataFetchDeadline, got %v", err)
	}
	if errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("a resolve-time metadata deadline must not classify as a per-collection install failure: %v", err)
	}
	// A tripwire on the fixture's parameters, not on production: a watchdog
	// win would already fail the ErrMetadataFetchDeadline check above.
	if errors.Is(err, context.Canceled) {
		t.Fatalf("must not match context.Canceled: %v", err)
	}
	if errors.Is(err, helpers.ErrReadStalled) {
		t.Fatalf("expected the drip to defeat the watchdog, not trip it: %v", err)
	}

	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "solo"))
}

// TestMetadataDripFixtureInstallsWithoutTheFault is the positive control for
// TestMetadataByteDripFailsTheRunAtTheMetadataDeadline: the identical
// fixture with no fault armed installs successfully under the same budget.
func TestMetadataDripFixtureInstallsWithoutTheFault(t *testing.T) {
	t.Parallel()
	cfg, runtime, _ := newMetadataDripFixture(t, metadataDripBudget)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "solo")
}

// TestRootMetadataDeadlineAbortsTheServerWalk pins that a metadata deadline on
// the first server aborts the server walk rather than advancing to the second,
// which has the collection: a deadline is not a 404.
func TestRootMetadataDeadlineAbortsTheServerWalk(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvB.AddVersion("ns", "x", "1.0.0", nil)
	srvA.Fail(fakegalaxy.EndpointRootMetadata, "", "", fakegalaxy.Fault{DripInterval: 5 * time.Millisecond, Count: -1})

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))
	runtime := multiServerRuntime(cfg)
	runtime.MetadataFetchDeadline = metadataDripBudget

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("expected errors.Is ErrMetadataFetchDeadline, got %v", err)
	}
	// A tripwire on the fixture, not on production: it fires if server a ever
	// gains ns.x or a second fault, breaking the two-server exclusion.
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0 (the deadline must abort the walk, not advance it)", got)
	}
}

// TestRootMetadataServerWalkPositiveControl proves the two-server fixture
// reaches server b when server a 404s, so the srvB.Total() == 0 check in
// TestRootMetadataDeadlineAbortsTheServerWalk is not vacuous.
func TestRootMetadataServerWalkPositiveControl(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := srvB.Total(); got == 0 {
		t.Fatalf("srvB.Total() = %d, want > 0 (without the fault, the walk must advance to server b)", got)
	}
}
