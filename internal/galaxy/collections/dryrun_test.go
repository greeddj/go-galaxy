package collections

// Tests for dryrun.go's shared machinery; the end-to-end dry-run substitutions
// (nothing downloaded, installed or recorded) are covered against a live fake
// Galaxy server in dry_run_e2e_test.go.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestClassifyDryRunSortedOrder pins classifyDryRun's report to sorted key
// order, not completion order. Workers: 8 and 24 keys are load-bearing: a
// serial probe would keep dispatch order and hide a completion-order bug.
func TestClassifyDryRunSortedOrder(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Workers: 8}

	const keyCount = 24
	cols := make(map[string]collection, keyCount)
	want := make([]string, keyCount)
	for i := range keyCount {
		// Zero-padded so lexicographic (slices.Sort) order equals the
		// generated numeric order, letting want be built in one straight
		// pass rather than pre-sorted by hand.
		name := fmt.Sprintf("c%02d", i)
		key := fmt.Sprintf("ns.%s@1.0.0", name)
		cols[key] = collection{Namespace: "ns", Name: name, Version: "1.0.0"}
		want[i] = fmt.Sprintf("Would install: %s (would download)", key)
	}

	for i := range 15 {
		printer := &capturingPrinter{}
		runtime := infra.New(printer, http.DefaultClient)

		// artifacts and root are nil: a nil store reads as not cached and a nil
		// root settles nothing, the only classification this test needs.
		classifyDryRun(context.Background(), runtime, cfg, cols, installDryRunVerbs, installDryRunProbe(cfg, nil, nil, nil))

		got := printer.okLines()
		if len(got) != len(want) {
			t.Fatalf("iteration %d: okLines has %d entries, want %d", i, len(got), len(want))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("iteration %d: okLines[%d] = %q, want %q (report order must be sorted, not completion order)", i, j, got[j], want[j])
			}
		}
		// Pins install's summary wording, assembled from installDryRunVerbs.
		wantSummary := fmt.Sprintf("Dry run: %d would install, 0 already up to date, 0 would fail", keyCount)
		if !printer.hasPersistentPrintContaining(wantSummary) {
			t.Fatalf("iteration %d: expected persistent print containing %q, got %v", i, wantSummary, printer.persists)
		}
	}
}

// TestClassifyDryRunReportsCacheHitVsMiss pins that classifyDryRun tells a
// cached artifact (acme.app, installed first) from one never fetched
// (acme.other).
func TestClassifyDryRunReportsCacheHitVsMiss(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)
	srv.AddVersion("acme", "other", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())
	if err := installWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("installWithState (populate acme.app's cache entry): %v", err)
	}

	cols := map[string]collection{
		"acme.app@1.0.0":   {Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL()},
		"acme.other@1.0.0": {Namespace: "acme", Name: "other", Version: "1.0.0", Source: srv.URL()},
	}
	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, srv.Client())
	// A fresh store, not state.store, so the install-record arm never matches
	// and only the cache-hit classification is exercised.
	installRoot := newTestCollectionsRoot(t, cfg.DownloadPath)
	probe := installDryRunProbe(cfg, store.New(), state.backend.Artifacts(), installRoot)
	classifyDryRun(context.Background(), reportRuntime, cfg, cols, installDryRunVerbs, probe)

	if !printer.hasOkContaining("acme.app@1.0.0 (artifact cached)") {
		t.Errorf("expected acme.app reported as cached, got okLines %v", printer.okLines())
	}
	if !printer.hasOkContaining("acme.other@1.0.0 (would download)") {
		t.Errorf("expected acme.other reported as would-download, got okLines %v", printer.okLines())
	}
	if !printer.hasPersistentPrintContaining("2 would install, 0 already up to date") {
		t.Errorf("expected a summary line counting both collections as would-install, got %v", printer.persists)
	}
}

// errSwitchOrderProbeStub is a probe failure distinct from
// helpers.ErrOfflineMode, so the switch-order tests can tell which one won.
var errSwitchOrderProbeStub = errors.New("stub probe failure, must not surface when the offline guard applies")

// TestReportDryRunResultsOfflineGuardOutranksProbeFailure pins
// reportDryRunResults' case order: an uncached collection under --offline is
// recorded under the offline cause even when its probe also returned a fail.
func TestReportDryRunResultsOfflineGuardOutranksProbeFailure(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Workers: 1, Offline: true}
	col := collection{Namespace: "ns", Name: "app", Version: "1.0.0"}
	cols := map[string]collection{col.key(): col}
	probe := func(context.Context, collection) dryRunClassification {
		return dryRunClassification{cached: false, fail: errSwitchOrderProbeStub}
	}

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	summary := classifyDryRun(context.Background(), runtime, cfg, cols, installDryRunVerbs, probe)

	if summary.count != 1 {
		t.Fatalf("summary.count = %d, want 1", summary.count)
	}
	if !errors.Is(summary.cause, helpers.ErrOfflineMode) {
		t.Errorf("expected the recorded cause to be helpers.ErrOfflineMode, got %v", summary.cause)
	}
	if errors.Is(summary.cause, errSwitchOrderProbeStub) {
		t.Errorf("expected the probe's own fail to be shadowed by the offline guard, got %v", summary.cause)
	}
	if !printer.hasErrContaining("not cached and --offline forbids downloading") {
		t.Errorf("expected the offline \"Would fail\" line, got errs=%v", printer.errs)
	}
	if printer.hasErrContaining(errSwitchOrderProbeStub.Error()) {
		t.Errorf("expected the probe's own error text never to reach stderr, got errs=%v", printer.errs)
	}
}

// TestReportDryRunResultsReportsProbeFailureWhenCached is the positive
// control for the offline-guard test: on the same fixture with the artifact
// cached, the probe's own fail wins.
func TestReportDryRunResultsReportsProbeFailureWhenCached(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Workers: 1, Offline: true}
	col := collection{Namespace: "ns", Name: "app", Version: "1.0.0"}
	cols := map[string]collection{col.key(): col}
	probe := func(context.Context, collection) dryRunClassification {
		return dryRunClassification{cached: true, fail: errSwitchOrderProbeStub}
	}

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	summary := classifyDryRun(context.Background(), runtime, cfg, cols, installDryRunVerbs, probe)

	if summary.count != 1 {
		t.Fatalf("summary.count = %d, want 1", summary.count)
	}
	if !errors.Is(summary.cause, errSwitchOrderProbeStub) {
		t.Errorf("expected the recorded cause to be the probe's own fail, got %v", summary.cause)
	}
	if errors.Is(summary.cause, helpers.ErrOfflineMode) {
		t.Errorf("expected no offline cause once the collection is reported cached, got %v", summary.cause)
	}
	if !printer.hasErrContaining(errSwitchOrderProbeStub.Error()) {
		t.Errorf("expected the probe's own \"Would fail\" line, got errs=%v", printer.errs)
	}
	if printer.hasErrContaining("not cached and --offline forbids downloading") {
		t.Errorf("expected no offline line once the collection is reported cached, got errs=%v", printer.errs)
	}
}

// TestInstallDryRunProbeMarksUpToDate pins that installDryRunProbe reports a
// collection whose real install satisfies installRecordMatches as up to date,
// not as "would install".
func TestInstallDryRunProbeMarksUpToDate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())
	if err := installWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("installWithState (perform the real install): %v", err)
	}

	cols := map[string]collection{
		"acme.app@1.0.0": {Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL()},
	}
	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, srv.Client())
	installRoot := newTestCollectionsRoot(t, cfg.DownloadPath)
	probe := installDryRunProbe(cfg, state.store, state.backend.Artifacts(), installRoot)
	classifyDryRun(context.Background(), reportRuntime, cfg, cols, installDryRunVerbs, probe)

	if !printer.hasPersistentPrintContaining("Up to date: acme.app@1.0.0") {
		t.Errorf("expected acme.app reported as up to date, got %v", printer.persists)
	}
	if len(printer.okLines()) != 0 {
		t.Errorf("expected no \"would install\" line for an already-satisfied install, got %v", printer.okLines())
	}
	if !printer.hasPersistentPrintContaining("0 would install, 1 already up to date") {
		t.Errorf("expected a summary line counting the collection as already up to date, got %v", printer.persists)
	}
}

// TestClassifyDryRunMirrorsIsCacheHitUnderNoCache pins that
// dryRunArtifactMeta mirrors isCacheHit's --no-cache guard: a warm cache
// under --no-cache is reported "would download", as a real install behaves.
func TestClassifyDryRunMirrorsIsCacheHitUnderNoCache(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())
	if err := installWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("installWithState (populate acme.app's cache entry): %v", err)
	}

	// Report against the same warm cache, but with --no-cache now set: a real
	// install against this cfg would not read the cache at all.
	cfg.NoCache = true
	cols := map[string]collection{
		"acme.app@1.0.0": {Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL()},
	}
	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, srv.Client())
	// A fresh store, not state.store, so the install-record arm never matches
	// and only the --no-cache classification is exercised.
	installRoot := newTestCollectionsRoot(t, cfg.DownloadPath)
	probe := installDryRunProbe(cfg, store.New(), state.backend.Artifacts(), installRoot)
	classifyDryRun(context.Background(), reportRuntime, cfg, cols, installDryRunVerbs, probe)

	if !printer.hasOkContaining("acme.app@1.0.0 (would download)") {
		t.Errorf("expected acme.app reported as would-download under --no-cache despite a warm cache, got %v", printer.okLines())
	}
	if printer.hasOkContaining("artifact cached") {
		t.Errorf("expected no \"artifact cached\" report under --no-cache, got %v", printer.okLines())
	}
}

// TestClassifyDryRunNeverDeletesDriftedExtractMarker pins that a drifted
// install is reported "would install" through checkExtractMarker while its
// marker survives, unlike verifyExtractMarker, which deletes it.
func TestClassifyDryRunNeverDeletesDriftedExtractMarker(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())
	if err := installWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("installWithState (perform the real install): %v", err)
	}

	entry, ok := state.store.GetInstalled("acme.app@1.0.0")
	if !ok {
		t.Fatalf("expected acme.app to be recorded installed")
	}
	markerPath := collectionMarkerPath(entry.InstallPath, collection{Namespace: "acme", Name: "app", Version: "1.0.0"}, entry.ArtifactSHA256)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("expected the extract marker to exist right after install, stat error: %v", err)
	}

	// Drift the tree: a real install's verifyExtractMarker would delete the
	// marker here; classifyDryRun must detect the drift and leave it alone.
	mustWriteFile(t, filepath.Join(entry.InstallPath, "drifted-file.txt"), []byte("unexpected"))

	cols := map[string]collection{
		"acme.app@1.0.0": {Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL()},
	}
	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, srv.Client())
	installRoot := newTestCollectionsRoot(t, cfg.DownloadPath)
	probe := installDryRunProbe(cfg, state.store, state.backend.Artifacts(), installRoot)
	classifyDryRun(context.Background(), reportRuntime, cfg, cols, installDryRunVerbs, probe)

	if printer.hasPersistentPrintContaining("Up to date") {
		t.Errorf("expected the drifted install NOT reported up to date, got %v", printer.persists)
	}
	if !printer.hasOkContaining("acme.app@1.0.0") {
		t.Errorf("expected the drifted install reported as would-install, got okLines %v", printer.okLines())
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("expected the extract marker to survive classifyDryRun despite the drift, stat error: %v", err)
	}
}

// newDriftedOfflineEvictedFixture installs acme.app, evicts its cached
// artifact and drifts its installed tree; cfg.Offline is left false for the
// caller to set.
func newDriftedOfflineEvictedFixture(t *testing.T) (*config.Config, *installState, map[string]collection) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())
	if err := installWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("installWithState (perform the real install): %v", err)
	}

	entry, ok := state.store.GetInstalled("acme.app@1.0.0")
	if !ok {
		t.Fatalf("expected acme.app to be recorded installed")
	}

	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL()}
	if err := state.backend.Artifacts().Delete(context.Background(), artifactKey(col)); err != nil {
		t.Fatalf("evict cached artifact: %v", err)
	}
	mustWriteFile(t, filepath.Join(entry.InstallPath, "drifted-file.txt"), []byte("unexpected"))

	return cfg, state, map[string]collection{"acme.app@1.0.0": col}
}

// assertReportsSingleWouldFail asserts classifyDryRun reported exactly one
// would-fail collection, named key, on the Errorf tier, and reported it on
// neither the "up to date" nor the "would install" tier.
func assertReportsSingleWouldFail(t *testing.T, printer *capturingPrinter, summary failureSummary, key string) {
	t.Helper()
	if summary.count != 1 {
		t.Fatalf("expected classifyDryRun to report 1 would-fail collection, got %d", summary.count)
	}
	if !printer.hasErrContaining("Would fail: " + key) {
		t.Errorf("expected a \"Would fail: %s\" line, got %v", key, printer.errs)
	}
	if printer.hasPersistentPrintContaining("Up to date") || printer.hasOkContaining(key) {
		t.Errorf("expected no \"up to date\" or \"would install\" report for %s, got persists=%v oks=%v",
			key, printer.persists, printer.okLines())
	}
}

// assertFailsOfflineClosed asserts err matches both
// helpers.ErrInstallationFailed and helpers.ErrOfflineMode, the classification
// a real failed --offline install returns.
func assertFailsOfflineClosed(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error for a would-fail collection")
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Errorf("expected errors.Is helpers.ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, helpers.ErrOfflineMode) {
		t.Errorf("expected errors.Is helpers.ErrOfflineMode, got %v", err)
	}
}

// TestInstallDryRunDriftedOfflineEvictedReportsWouldFailAndFails pins
// preview/run agreement for a drifted install with an evicted artifact under
// --offline: "would fail" and the same error class, never "up to date".
func TestInstallDryRunDriftedOfflineEvictedReportsWouldFailAndFails(t *testing.T) {
	t.Parallel()
	cfg, state, cols := newDriftedOfflineEvictedFixture(t)
	cfg.Offline = true

	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, http.DefaultClient)
	installRoot := newTestCollectionsRoot(t, cfg.DownloadPath)
	probe := installDryRunProbe(cfg, state.store, state.backend.Artifacts(), installRoot)
	summary := classifyDryRun(context.Background(), reportRuntime, cfg, cols, installDryRunVerbs, probe)
	assertReportsSingleWouldFail(t, printer, summary, "acme.app@1.0.0")

	// installDryRun's own error-wrap must classify identically to a real
	// failed install.
	plan := &installPlan{collections: cols}
	err := installDryRun(context.Background(), cfg, reportRuntime, state, plan, time.Now(), installRoot, nil)
	assertFailsOfflineClosed(t, err)
}

// TestWarmDryRunProbeIgnoresInstallState pins that warmDryRunProbe ignores
// install state: a valid install whose cached artifact was evicted is still
// reported "Would warm", since warm's product is the cache.
func TestWarmDryRunProbeIgnoresInstallState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())
	if err := installWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("installWithState (perform the real install): %v", err)
	}

	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL()}
	// Evict the artifact while the install and its marker stand: install's
	// probe would call this settled, but warm's product is gone.
	if err := state.backend.Artifacts().Delete(context.Background(), artifactKey(col)); err != nil {
		t.Fatalf("evict cached artifact: %v", err)
	}

	cols := map[string]collection{"acme.app@1.0.0": col}
	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, srv.Client())
	warmed := state.store.WarmedArtifactSHAByKey()
	probe := warmDryRunProbe(cfg, state.backend.Artifacts(), state.extractStore, warmed)
	classifyDryRun(context.Background(), reportRuntime, cfg, cols, warmDryRunVerbs, probe)

	if !printer.hasOkContaining("Would warm: acme.app@1.0.0 (would download)") {
		t.Errorf("expected acme.app reported as would-warm, got okLines %v", printer.okLines())
	}
	if printer.hasPersistentPrintContaining("Already warm") {
		t.Errorf("warmDryRunProbe must never report \"Already warm\" from install state alone, got %v", printer.persists)
	}
}

// TestDryRunBannerOnlyWarns pins that dryRunBanner emits exactly one line,
// through Warnf only: the tier that reaches stderr and survives --quiet.
func TestDryRunBannerOnlyWarns(t *testing.T) {
	t.Parallel()
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	dryRunBanner(runtime)

	if len(printer.warns) != 1 {
		t.Fatalf("expected exactly one Warnf line, got %v", printer.warns)
	}
	if !printer.hasWarnContaining("--dry-run") {
		t.Errorf("expected the banner to mention --dry-run, got %q", printer.warns[0])
	}
	if len(printer.prints) != 0 || len(printer.persists) != 0 || len(printer.oks) != 0 {
		t.Errorf("expected no output on any other tier, got prints=%v persists=%v oks=%v", printer.prints, printer.persists, printer.oks)
	}
}

// TestDryRunBannerEmittedExactlyOnceAcrossCommands pins that initInstall
// prints the dry-run banner exactly once per run, for install and warm alike.
func TestDryRunBannerEmittedExactlyOnceAcrossCommands(t *testing.T) {
	t.Parallel()

	assertBannerOnce := func(t *testing.T, run func(context.Context, *config.Config, *infra.Infra) error) {
		t.Helper()
		root := t.TempDir()
		cacheDir := filepath.Join(root, "cache")
		reqPath := filepath.Join(root, "requirements.yml")
		mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

		srv := fakegalaxy.New(t)
		srv.AddVersion("acme", "app", "1.0.0", nil)

		cfg := &config.Config{
			Server:           srv.URL(),
			CacheDir:         cacheDir,
			DownloadPath:     filepath.Join(root, "install"),
			RequirementsFile: reqPath,
			Workers:          1,
			DryRun:           true,
		}
		printer := &capturingPrinter{}
		runtime := infra.New(printer, srv.Client())

		if err := run(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("run: %v", err)
		}
		count := 0
		for _, w := range printer.warns {
			if strings.Contains(w, "--dry-run is active") {
				count++
			}
		}
		if count != 1 {
			t.Errorf("banner warn count = %d, want exactly 1, warns=%v", count, printer.warns)
		}
	}

	t.Run("install", func(t *testing.T) {
		t.Parallel()
		assertBannerOnce(t, Start)
	})
	t.Run("warm", func(t *testing.T) {
		t.Parallel()
		assertBannerOnce(t, Warm)
	})
}

// TestInitInstallDryRunSkipsClearCache pins that a dry run skips
// --clear-cache with a warning; its positive control is
// TestInitInstallClearCacheWipesArtifactsAndMetadataCaches.
func TestInitInstallDryRunSkipsClearCache(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	// Named like a cached artifact (store.ClearCacheFiles deletes ".tar.gz"),
	// so its survival proves ClearFiles never ran.
	sentinelPath := filepath.Join(cacheDir, "sentinel.acme-app-1.0.0.tar.gz")
	mustWriteFile(t, sentinelPath, []byte("cached bytes"))

	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	cfg := &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		ClearCache:       true,
		DryRun:           true,
		Workers:          1,
	}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	_, state, err := initInstall(context.Background(), cfg, runtime)
	if err != nil {
		t.Fatalf("initInstall: %v", err)
	}
	t.Cleanup(func() {
		if state.release != nil {
			_ = state.release()
		}
		_ = state.backend.Close(context.Background())
	})

	if _, statErr := os.Stat(sentinelPath); statErr != nil {
		t.Errorf("expected the cache sentinel file to survive a dry run's --clear-cache, stat error: %v", statErr)
	}
	if !printer.hasWarnContaining("--clear-cache") {
		t.Errorf("expected a warning naming --clear-cache, got %v", printer.warns)
	}
}

// TestInitInstallDryRunSkipsRecordProject pins that a dry run never records
// the project in the registry, which feeds the destructive cleanup command.
func TestInitInstallDryRunSkipsRecordProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	cfg := &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		DryRun:           true,
		Workers:          1,
	}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)

	_, state, err := initInstall(context.Background(), cfg, runtime)
	if err != nil {
		t.Fatalf("initInstall: %v", err)
	}
	t.Cleanup(func() {
		if state.release != nil {
			_ = state.release()
		}
		_ = state.backend.Close(context.Background())
	})

	registry, err := store.LoadProjectRegistry(cacheDir)
	if err != nil {
		t.Fatalf("store.LoadProjectRegistry: %v", err)
	}
	if len(registry.Projects) != 0 {
		t.Errorf("expected an empty project registry after a dry run, got %d entries: %+v", len(registry.Projects), registry.Projects)
	}
}

// TestWriteRunMetricsDryRunSkipsAndWarns pins writeRunMetrics' own dry-run
// guard: no metrics file and a warning instead, for every command using it.
func TestWriteRunMetricsDryRunSkipsAndWarns(t *testing.T) {
	t.Parallel()
	metricsPath := filepath.Join(t.TempDir(), "metrics.json")
	cfg := &config.Config{MetricsFile: metricsPath, DryRun: true}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	writeRunMetrics(cfg, runtime, "install", time.Now(), runCounts{Collections: 1}, false)

	if _, statErr := os.Stat(metricsPath); !os.IsNotExist(statErr) {
		t.Errorf("expected no metrics file written by a dry run, stat error = %v", statErr)
	}
	if !printer.hasWarnContaining(metricsPath) {
		t.Errorf("expected a warning naming the skipped metrics path, got %v", printer.warns)
	}
}

// countingArtifactMetaCalls is an ArtifactStore stub counting Has and Meta
// calls per key; its other methods return errStubNotImplemented so an
// unexpected call fails loudly.
type countingArtifactMetaCalls struct {
	metaCalls map[string]int
	hasCalls  map[string]int
	present   map[string]bool
	mu        sync.Mutex
}

func (a *countingArtifactMetaCalls) Has(_ context.Context, key string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hasCalls[key]++
	return a.present[key], nil
}

func (a *countingArtifactMetaCalls) Meta(_ context.Context, key string) (map[string]string, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.metaCalls[key]++
	return nil, a.present[key], nil
}

func (a *countingArtifactMetaCalls) Fetch(context.Context, string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (a *countingArtifactMetaCalls) TempFile(context.Context, string) (*os.File, func(), error) {
	return nil, nil, errStubNotImplemented
}

func (a *countingArtifactMetaCalls) Commit(context.Context, string, string, map[string]string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (a *countingArtifactMetaCalls) Delete(context.Context, string) error {
	return errStubNotImplemented
}

// TestClassifyDryRunCallsMetaExactlyOncePerCollectionNeverHas pins that a
// dry run calls ArtifactStore.Meta exactly once per probed collection and
// never Has, so on S3 it costs one HEAD; cached and uncached keys alternate.
func TestClassifyDryRunCallsMetaExactlyOncePerCollectionNeverHas(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Workers: 4}

	const keyCount = 12
	cols := make(map[string]collection, keyCount)
	artifacts := &countingArtifactMetaCalls{
		metaCalls: make(map[string]int),
		hasCalls:  make(map[string]int),
		present:   make(map[string]bool),
	}
	for i := range keyCount {
		name := fmt.Sprintf("c%02d", i)
		col := collection{Namespace: "ns", Name: name, Version: "1.0.0"}
		cols[col.key()] = col
		artifacts.present[artifactKey(col)] = i%2 == 0
	}

	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	// root is nil: cfg has no DownloadPath, so newInstallTarget's own
	// nil-root guard makes every collection report ok=false, keeping this
	// test isolated to the artifact-cache probe this test is about.
	probe := installDryRunProbe(cfg, store.New(), artifacts, nil)
	classifyDryRun(context.Background(), runtime, cfg, cols, installDryRunVerbs, probe)

	artifacts.mu.Lock()
	defer artifacts.mu.Unlock()
	for key, col := range cols {
		ak := artifactKey(col)
		if got := artifacts.metaCalls[ak]; got != 1 {
			t.Errorf("collection %s: Meta call count = %d, want exactly 1", key, got)
		}
		if got := artifacts.hasCalls[ak]; got != 0 {
			t.Errorf("collection %s: Has call count = %d, want 0 (a dry run must never call Has directly)", key, got)
		}
	}
}

// pinVerdictTestPin and pinVerdictTestRecorded are two well-formed, distinct
// sha256 hex digests used as a disagreeing lockfile pin and recorded digest.
const (
	pinVerdictTestPin      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pinVerdictTestRecorded = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestDryRunPinVerdictSuppressedWhenOnline pins that a recorded digest
// disagreeing with the pin yields no verdict online, where a real run can
// still evict and refetch.
func TestDryRunPinVerdictSuppressedWhenOnline(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Offline: false}
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0", SHA256: pinVerdictTestPin}
	meta := map[string]string{"sha256": pinVerdictTestRecorded}

	if err := dryRunPinVerdict(cfg, true, meta, col); err != nil {
		t.Fatalf("expected no verdict while online, got %v", err)
	}
}

// TestDryRunPinVerdictSuppressedForSettledCollection pins that
// installDryRunProbe's settled check returns before dryRunPinVerdict: a
// settled install whose cache sidecar drifted stays settled under --offline.
func TestDryRunPinVerdictSuppressedForSettledCollection(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	version := srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())
	if err := installWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("installWithState (perform the real install): %v", err)
	}

	// --offline is the only config where dryRunPinVerdict can fire, so it is
	// the one that exercises the settled short-circuit.
	cfg.Offline = true
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL(), SHA256: version.SHA256}

	sidecarPath := filepath.Join(cacheDir, artifactKey(col)) + helpers.ArtifactSHASidecarSuffix
	if err := os.WriteFile(sidecarPath, []byte(pinVerdictTestRecorded), helpers.FileMod); err != nil {
		t.Fatalf("drift the cache sidecar: %v", err)
	}

	if err := dryRunPinVerdict(cfg, true, map[string]string{"sha256": pinVerdictTestRecorded}, col); err == nil {
		t.Fatal("fixture sanity: expected dryRunPinVerdict to fire directly for the drifted sidecar under --offline")
	}

	installRoot := newTestCollectionsRoot(t, cfg.DownloadPath)
	probe := installDryRunProbe(cfg, state.store, state.backend.Artifacts(), installRoot)
	got := probe(context.Background(), col)
	if !got.settled {
		t.Errorf("expected the collection to be reported settled despite the drifted sidecar, got %+v", got)
	}
	if got.fail != nil {
		t.Errorf("expected no fail verdict for a settled collection, got %v", got.fail)
	}
}

// TestDryRunPinVerdictNoVerdictArms pins each "no verdict" arm of
// dryRunPinVerdict under --offline, with a disagreeing well-formed digest as
// the positive control row.
func TestDryRunPinVerdictNoVerdictArms(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Offline: true}

	for _, tt := range dryRunPinVerdictNoVerdictCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := dryRunPinVerdict(cfg, tt.cached, tt.meta, tt.col)
			if tt.wantErr && err == nil {
				t.Fatal("expected a verdict, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected no verdict, got %v", err)
			}
		})
	}
}

// dryRunPinVerdictNoVerdictCases is the table
// TestDryRunPinVerdictNoVerdictArms runs, factored out purely to keep that
// function itself short.
func dryRunPinVerdictNoVerdictCases() []struct {
	meta    map[string]string
	name    string
	col     collection
	cached  bool
	wantErr bool
} {
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0", SHA256: pinVerdictTestPin}
	unpinnedCol := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}

	return []struct {
		meta    map[string]string
		name    string
		col     collection
		cached  bool
		wantErr bool
	}{
		{
			name:   "empty pin",
			col:    unpinnedCol,
			cached: true,
			meta:   map[string]string{"sha256": pinVerdictTestRecorded},
		},
		{
			name:   "not cached",
			col:    col,
			cached: false,
			meta:   map[string]string{"sha256": pinVerdictTestRecorded},
		},
		{
			name:   "empty recorded digest",
			col:    col,
			cached: true,
			meta:   map[string]string{"sha256": ""},
		},
		{
			name:   "nil meta map",
			col:    col,
			cached: true,
			meta:   nil,
		},
		{
			name:   "malformed recorded digest",
			col:    col,
			cached: true,
			meta:   map[string]string{"sha256": "not-a-valid-hex-digest"},
		},
		{
			name:   "matching recorded digest",
			col:    col,
			cached: true,
			meta:   map[string]string{"sha256": pinVerdictTestPin},
		},
		{
			// Positive control: the same config with a disagreeing well-formed
			// digest must produce a verdict.
			name:    "disagreeing well-formed digest fires",
			col:     col,
			cached:  true,
			meta:    map[string]string{"sha256": pinVerdictTestRecorded},
			wantErr: true,
		},
	}
}

// TestInstallDryRunProbeReportsUnsafeIdentifierWhenRootExists pins that, with a
// root, an unsafe namespace is a would-fail with ErrUnsafeCollectionIdentifier;
// it calls the probe directly, since buildCollectionsMap rejects it first.
func TestInstallDryRunProbeReportsUnsafeIdentifierWhenRootExists(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{DownloadPath: t.TempDir()}
	root := newTestCollectionsRoot(t, cfg.DownloadPath)
	col := collection{Namespace: "../escape", Name: "app", Version: "1.0.0"}

	probe := installDryRunProbe(cfg, store.New(), nil, root)
	got := probe(context.Background(), col)

	if got.settled {
		t.Errorf("expected the collection not to be reported settled, got %+v", got)
	}
	if got.fail == nil {
		t.Fatal("expected a fail verdict for an unsafe collection identifier, got nil")
	}
	if !errors.Is(got.fail, helpers.ErrUnsafeCollectionIdentifier) {
		t.Errorf("expected errors.Is helpers.ErrUnsafeCollectionIdentifier, got %v", got.fail)
	}
}

// metaFoundWithErrorArtifacts is an ArtifactStore stub whose Meta answers
// found=true with a non-nil error, the only shape that exercises the
// err != nil half of dryRunArtifactMeta's guard; other methods are stubs.
type metaFoundWithErrorArtifacts struct{}

// errStubMetaUnreachable is the error metaFoundWithErrorArtifacts.Meta always
// returns, standing in for a cache backend that could not be consulted at
// all (a transport failure, most concretely).
var errStubMetaUnreachable = errors.New("stub: meta probe unreachable")

func (metaFoundWithErrorArtifacts) Has(context.Context, string) (bool, error) {
	return false, errStubNotImplemented
}

func (metaFoundWithErrorArtifacts) Meta(context.Context, string) (map[string]string, bool, error) {
	return map[string]string{"sha256": pinVerdictTestRecorded}, true, errStubMetaUnreachable
}

func (metaFoundWithErrorArtifacts) Fetch(context.Context, string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (metaFoundWithErrorArtifacts) TempFile(context.Context, string) (*os.File, func(), error) {
	return nil, nil, errStubNotImplemented
}

func (metaFoundWithErrorArtifacts) Commit(context.Context, string, string, map[string]string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (metaFoundWithErrorArtifacts) Delete(context.Context, string) error {
	return errStubNotImplemented
}

// TestDryRunProbeMetaErrorCases pins that a Meta error reads as not cached,
// not failed and not settled, for every probe that reaches dryRunArtifactMeta.
func TestDryRunProbeMetaErrorCases(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}

	for _, tc := range dryRunProbeMetaErrorCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := tc.buildProbe(t)
			got := probe(context.Background(), col)

			if got.cached {
				t.Errorf("expected cached=false when Meta answers found=true alongside a non-nil error, got %+v", got)
			}
			if got.fail != nil {
				t.Errorf("expected no fail verdict from a Meta error alone, got %v", got.fail)
			}
			if got.settled {
				t.Errorf("expected settled=false, got %+v", got)
			}
		})
	}
}

// dryRunProbeMetaErrorCase is one table entry for
// TestDryRunProbeMetaErrorCases.
type dryRunProbeMetaErrorCase struct {
	// buildProbe returns the probe this row exercises, built against
	// metaFoundWithErrorArtifacts and whatever else that probe needs.
	buildProbe func(t *testing.T) dryRunProbe
	name       string
}

// dryRunProbeMetaErrorCases enumerates the probes that reach
// dryRunArtifactMeta, one row each, with the fixture that row's probe is
// built against.
func dryRunProbeMetaErrorCases() []dryRunProbeMetaErrorCase {
	return []dryRunProbeMetaErrorCase{
		{
			// installDryRunProbe: a store that could not be consulted is neither
			// a cache hit nor a would-fail. The nil root isolates the artifact
			// probe.
			name: "install",
			buildProbe: func(t *testing.T) dryRunProbe {
				t.Helper()
				cfg := &config.Config{}
				return installDryRunProbe(cfg, store.New(), metaFoundWithErrorArtifacts{}, nil)
			},
		},
		{
			// warmDryRunProbe: the same Meta failure reads as not cached, before
			// extractStore.Ready is ever reached.
			name: "warm",
			buildProbe: func(t *testing.T) dryRunProbe {
				t.Helper()
				cfg := &config.Config{}
				extractStore := extracted.NewStore(t.TempDir())
				return warmDryRunProbe(cfg, metaFoundWithErrorArtifacts{}, extractStore, map[string]string{})
			},
		},
	}
}
