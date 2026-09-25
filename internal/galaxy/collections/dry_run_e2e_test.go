package collections_test

// End-to-end dry-run tests for install, warm and outdated against a live fake
// Galaxy: nothing is downloaded, installed, extracted or recorded, the backend
// lock is still taken, and a cold cache never gains a persisted snapshot.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestInstallDryRunAgainstLiveServerDoesNotMutate pins that a cold-cache
// install --dry-run downloads nothing, creates no install tree or cache entry
// and records no install; with no prior snapshot the save is skipped too.
func TestInstallDryRunAgainstLiveServerDoesNotMutate(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.cfg.DryRun = true

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (dry run): %v", err)
	}

	assertNothingDownloadedOrInstalled(t, f)
	assertSnapshotUnpersistedAndRegistryEmpty(t, f.cfg.CacheDir, "acme.app@1.0.0", "acme.lib@1.0.0")
}

// assertNothingDownloadedOrInstalled checks that no artifact request reached
// the server and that neither the install tree, the artifact cache nor the
// extracted store gained an entry.
func assertNothingDownloadedOrInstalled(t *testing.T, f *e2eFixture) {
	t.Helper()
	if got := f.server.Count(fakegalaxy.EndpointArtifact); got != 0 {
		t.Errorf("EndpointArtifact count = %d, want 0 (a dry run must never download an artifact)", got)
	}
	assertPathAbsent(t, installPathFor(f.downloadPath, "app"))
	assertPathAbsent(t, installPathFor(f.downloadPath, "lib"))

	appKey := helpers.ArtifactKey(f.cfg.Server, "acme-app-1.0.0.tar.gz")
	libKey := helpers.ArtifactKey(f.cfg.Server, "acme-lib-1.0.0.tar.gz")
	assertPathAbsent(t, filepath.Join(f.cfg.CacheDir, appKey))
	assertPathAbsent(t, filepath.Join(f.cfg.CacheDir, libKey))

	// The content-addressable extracted store root is created lazily on the
	// first ingest; a dry run must never trigger that first ingest at all.
	assertPathAbsent(t, filepath.Join(f.cfg.CacheDir, extracted.RootDirName))
}

// TestInstallDryRunDoesNotFabricateASnapshot pins that a dry run against a
// cache with no persisted snapshot leaves none, judged by WasPersisted rather
// than by the Bolt file, which the backend creates whether or not it saves.
func TestInstallDryRunDoesNotFabricateASnapshot(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.cfg.DryRun = true

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (dry run against a cold cache): %v", err)
	}

	ctx := context.Background()
	backend := local.New(f.cfg.CacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()

	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	if st.WasPersisted() {
		t.Fatal("expected WasPersisted() to be false: a dry run against a cache with no persisted snapshot must not create one")
	}
}

// TestInstallDryRunSavesMetadataCachesWhenSnapshotExists pins that a dry run
// over an already persisted snapshot still saves its metadata caches; the
// added acme.extra forces a fresh solve so the old snapshot cannot pass it.
func TestInstallDryRunSavesMetadataCachesWhenSnapshotExists(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (seed a persisted snapshot with a real install): %v", err)
	}

	f.server.AddVersion("acme", "extra", "1.0.0", nil)
	writeRequirementsMulti(t, f.cfg.RequirementsFile, "acme.app", "acme.extra")
	f.cfg.DryRun = true

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (dry run against an already-persisted snapshot): %v", err)
	}

	ctx := context.Background()
	backend := local.New(f.cfg.CacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()

	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	if !st.WasPersisted() {
		t.Error("expected WasPersisted() to still be true: a persisted snapshot already existed before the dry run")
	}
	req := st.RequirementsSnapshot()
	if _, ok := req["acme.extra"]; !ok {
		t.Errorf("expected the dry run's own fresh resolve to have saved acme.extra's requirement spec, got %v", req)
	}
}

// dryRunLockObservationCeiling is a liveness ceiling, not a timing margin:
// every assertion waits on an observed event, so a slow machine makes the
// lock helpers slower, never wrong.
const dryRunLockObservationCeiling = 10 * time.Second

// assertDryRunStillTakesBackendLock parks a dry run mid-resolve on a Hang
// fault and requires a concurrent lock attempt to fail, then succeed once it
// unwinds; waiting never probes the lock, since initInstall tries it only once.
func assertDryRunStillTakesBackendLock(t *testing.T, run func(context.Context, *config.Config, *infra.Infra) error) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	// Pre-created so the lock attempts below never race initInstall's own
	// os.MkdirAll: each one is either "lock held elsewhere" or "lock free",
	// never "parent directory missing".
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme.app")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "app", "1.0.0", nil)
	s.Fail(fakegalaxy.EndpointRootMetadata, "acme", "app", fakegalaxy.Fault{Hang: true, Count: -1})

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
		DryRun:           true,
	}
	// No client-side Timeout: only ctx's own cancellation (below) may ever
	// unblock the hung request, mirroring prefetch_cancel_e2e_test.go's own
	// fixture.
	runtime := infra.New(noopPrinter{}, s.Client())

	ctx, cancel := context.WithCancel(context.Background())
	// Deferred as well as called below: a t.Fatal before the explicit cancel
	// would leave the Hang-faulted request parked, and fakegalaxy's cleanup would
	// then block until the whole test binary times out.
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, runtime)
	}()

	waitForResolveInFlight(t, s, done)

	if release, err := store.AcquireLock(cacheDir); err == nil {
		_ = release()
		t.Fatal("expected the single concurrent lock attempt to fail while the dry run holds the backend lock, but it succeeded")
	} else if !errors.Is(err, helpers.ErrAnotherInstanceIsRunning) {
		// Reachable only on an IO failure of the lock path itself; kept to tell
		// that apart from contention.
		t.Fatalf("expected errors.Is ErrAnotherInstanceIsRunning, got %v", err)
	}

	cancel()
	awaitCanceledRun(t, done)

	release, err := store.AcquireLock(cacheDir)
	if err != nil {
		t.Fatalf("expected the lock to be free once the dry run unwound, got %v", err)
	}
	_ = release()
}

// waitForResolveInFlight blocks until the server has seen the run's
// root-metadata request, proof the run is past initInstall's Lock and parked,
// and fails by name if the run returns before reaching its resolve.
func waitForResolveInFlight(t *testing.T, s *fakegalaxy.Server, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(dryRunLockObservationCeiling)
	for s.Count(fakegalaxy.EndpointRootMetadata) < 1 {
		select {
		case runErr := <-done:
			t.Fatalf("the run returned before it reached its resolve, so it never held the lock to observe: %v", runErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the root metadata endpoint received no request within %v", dryRunLockObservationCeiling)
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitCanceledRun joins a canceled run and requires an error; the bounded
// wait fails this test by name instead of timing out the whole binary.
func awaitCanceledRun(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case runErr := <-done:
		if runErr == nil {
			t.Fatal("expected the canceled dry run to return an error")
		}
	case <-time.After(dryRunLockObservationCeiling):
		t.Fatalf("the dry run did not return within %v after its context was canceled", dryRunLockObservationCeiling)
	}
}

// TestInstallDryRunStillTakesBackendLock is assertDryRunStillTakesBackendLock
// driven by collections.Start; see that helper's own doc comment for the
// property being pinned.
func TestInstallDryRunStillTakesBackendLock(t *testing.T) {
	assertDryRunStillTakesBackendLock(t, collections.Start)
}

// TestInstallDryRunOfflineReportsWouldFailAndFailsClosed pins that an offline
// dry run with metadata cached by Lock but no artifact cached reports "Would
// fail:" and returns ErrOfflineMode, as the real offline install would.
func TestInstallDryRunOfflineReportsWouldFailAndFailsClosed(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock (populate the snapshot's metadata caches): %v", err)
	}

	f.cfg.DryRun = true
	f.cfg.Offline = true
	f.runtime.HTTP = fetch.NewOffline(f.cfg.Timeout)

	var startErr error
	_, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		startErr = collections.Start(context.Background(), f.cfg, f.runtime)
	})

	if startErr == nil {
		t.Fatal("expected an error from an offline dry run against an uncached artifact")
	}
	if !errors.Is(startErr, helpers.ErrInstallationFailed) {
		t.Errorf("expected errors.Is ErrInstallationFailed, got %v", startErr)
	}
	if !errors.Is(startErr, helpers.ErrOfflineMode) {
		t.Errorf("expected errors.Is ErrOfflineMode, got %v", startErr)
	}
	if !bytes.Contains(stderr, []byte("Would fail:")) {
		t.Errorf("expected a \"Would fail:\" line on stderr, got %q", stderr)
	}
	assertPathAbsent(t, installPathFor(f.downloadPath, "app"))
	assertPathAbsent(t, installPathFor(f.downloadPath, "lib"))
}

// TestInstallDryRunBannerSurvivesQuiet pins that dryRunBanner reaches stderr
// under --quiet through the real progress.Printer, since an env-sourced
// --dry-run must never turn a run into a silent no-op.
func TestInstallDryRunBannerSurvivesQuiet(t *testing.T) {
	f := newE2EFixture(t)
	f.cfg.DryRun = true
	f.cfg.Quiet = true

	var startErr error
	_, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		startErr = collections.Start(context.Background(), f.cfg, f.runtime)
	})
	if startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}

	if !bytes.Contains(stderr, []byte("--dry-run")) {
		t.Errorf("expected the dry-run banner on stderr despite --quiet, got stderr=%q", stderr)
	}
}

// TestOutdatedDryRunMutatesNothing pins that outdated under --dry-run leaves
// the lockfile byte-identical, creates no cache directory, writes no metrics
// file (writeRunMetrics suppresses it), and succeeds the same without it.
func TestOutdatedDryRunMutatesNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	metricsPath := filepath.Join(root, "metrics.json")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "widgets", "1.0.0", nil)
	s.AddVersion("acme", "widgets", "2.0.0", nil)

	lockPath := lockfile.ResolveDefaultPath(reqPath, "")
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        s.URL(),
		Collections: []lockfile.Entry{{
			Name: "acme.widgets", Version: "1.0.0", Source: s.URL(), DownloadURL: lockedDownloadURLFor(s.URL(), "acme.widgets", "1.0.0"),
		}},
	}
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
	before, err := os.ReadFile(lockPath) //nolint:gosec // path is this test's own temp dir.
	if err != nil {
		t.Fatalf("read lockfile before Outdated: %v", err)
	}

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		DryRun:           true,
		Workers:          2,
	}
	runtime := infra.New(noopPrinter{}, s.Client())

	dryErr := collections.Outdated(context.Background(), cfg, runtime)
	if dryErr != nil {
		t.Fatalf("Outdated (dry run): %v", dryErr)
	}

	after, err := os.ReadFile(lockPath) //nolint:gosec // path is this test's own temp dir.
	if err != nil {
		t.Fatalf("read lockfile after Outdated: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("expected the lockfile to be byte-identical after Outdated, before=%q after=%q", before, after)
	}
	if _, statErr := os.Stat(cacheDir); !os.IsNotExist(statErr) {
		t.Errorf("expected Outdated to never create the configured cache directory, stat error = %v", statErr)
	}
	if _, statErr := os.Stat(metricsPath); !os.IsNotExist(statErr) {
		t.Errorf("expected Outdated to never write the configured metrics file, stat error = %v", statErr)
	}

	// The result itself must not depend on --dry-run: a second, otherwise
	// identical run with cfg.DryRun cleared must succeed exactly the same.
	cfg.DryRun = false
	normalErr := collections.Outdated(context.Background(), cfg, infra.New(noopPrinter{}, s.Client()))
	if normalErr != nil {
		t.Fatalf("Outdated (normal run): %v", normalErr)
	}
}

// assertSnapshotUnpersistedAndRegistryEmpty reopens the backend and requires
// that a cold-cache dry run persisted no snapshot, enrolled no project and
// recorded no install for any key in notInstalled.
func assertSnapshotUnpersistedAndRegistryEmpty(t *testing.T, cacheDir string, notInstalled ...string) {
	t.Helper()
	ctx := context.Background()
	backend := local.New(cacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()

	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	if st.WasPersisted() {
		t.Error("expected WasPersisted() to still be false: a cold-cache dry run must not fabricate a persisted snapshot")
	}
	for _, key := range notInstalled {
		if _, ok := st.GetInstalled(key); ok {
			t.Errorf("expected no recordInstall entry for %s in the reloaded snapshot", key)
		}
	}

	registry, err := backend.LoadProjectRegistry(ctx)
	if err != nil {
		t.Fatalf("backend.LoadProjectRegistry: %v", err)
	}
	if len(registry.Projects) != 0 {
		t.Errorf("expected an empty project registry after a dry run, got %d entries: %+v", len(registry.Projects), registry.Projects)
	}
}

// TestWarmDryRunAgainstLiveServerCachesNothing pins that a cold-cache warm
// --dry-run downloads nothing, creates no artifact entry or extracted store
// root, enrolls no project and leaves the snapshot unpersisted.
func TestWarmDryRunAgainstLiveServerCachesNothing(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.cfg.DryRun = true

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm (dry run): %v", err)
	}

	if got := f.server.Count(fakegalaxy.EndpointArtifact); got != 0 {
		t.Errorf("EndpointArtifact count = %d, want 0 (a dry run must never download an artifact)", got)
	}
	appKey := helpers.ArtifactKey(f.cfg.Server, "acme-app-1.0.0.tar.gz")
	libKey := helpers.ArtifactKey(f.cfg.Server, "acme-lib-1.0.0.tar.gz")
	assertPathAbsent(t, filepath.Join(f.cfg.CacheDir, appKey))
	assertPathAbsent(t, filepath.Join(f.cfg.CacheDir, libKey))
	assertPathAbsent(t, filepath.Join(f.cfg.CacheDir, extracted.RootDirName))

	assertSnapshotUnpersistedAndRegistryEmpty(t, f.cfg.CacheDir)
}

// TestWarmDryRunWritesNoWarmedEntry pins that warm --dry-run never calls
// recordWarmed; a real install seeds a persisted snapshot first so an empty
// warmed set is not the vacuous result of a skipped save.
func TestWarmDryRunWritesNoWarmedEntry(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (seed a persisted snapshot via a real install): %v", err)
	}

	f.server.ResetCounts()
	f.cfg.DryRun = true
	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm (dry run against an already-installed cache): %v", err)
	}

	if got := f.server.Count(fakegalaxy.EndpointArtifact); got != 0 {
		t.Errorf("EndpointArtifact count = %d, want 0 (a dry run must never download an artifact)", got)
	}

	st := loadStoreSnapshot(t, f.cfg, f.runtime)
	if !st.WasPersisted() {
		t.Error("expected WasPersisted() to still be true: a persisted snapshot already existed before the dry run")
	}
	if got := len(st.WarmedArtifactSHAByKey()); got != 0 {
		t.Errorf("warmed set size = %d, want 0 (warm --dry-run must never call recordWarmed)", got)
	}
}

// TestWarmDryRunReportsWouldWarmWhenExtractedStoreIsCold pins that a cached
// artifact alone is not "already warm": with the extracted store wiped (a
// fresh runner over a warm S3 bucket) both collections report "would warm".
func TestWarmDryRunReportsWouldWarmWhenExtractedStoreIsCold(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm (populate both the artifact cache and the extracted store): %v", err)
	}
	if err := os.RemoveAll(filepath.Join(f.cfg.CacheDir, extracted.RootDirName)); err != nil {
		t.Fatalf("remove the extracted store root: %v", err)
	}

	f.cfg.DryRun = true
	var warmErr error
	stdout, _ := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if warmErr != nil {
		t.Fatalf("Warm (dry run against a cold extracted store): %v", warmErr)
	}

	if !bytes.Contains(stdout, []byte("Would warm: acme.app@1.0.0 (artifact cached)")) {
		t.Errorf("expected a \"Would warm: acme.app@1.0.0 (artifact cached)\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Would warm: acme.lib@1.0.0 (artifact cached)")) {
		t.Errorf("expected a \"Would warm: acme.lib@1.0.0 (artifact cached)\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Dry run: 2 would warm, 0 already warm, 0 would fail")) {
		t.Errorf("expected the would-warm summary line, got stdout=%q", stdout)
	}
}

// TestWarmDryRunReportsAlreadyWarmWhenFullyWarm pins that once both the
// artifact and its extracted tree exist, the dry run reports "already warm"
// without any network request.
func TestWarmDryRunReportsAlreadyWarmWhenFullyWarm(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm (populate the cache): %v", err)
	}

	f.server.ResetCounts()
	f.cfg.DryRun = true
	var warmErr error
	stdout, _ := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if warmErr != nil {
		t.Fatalf("Warm (dry run against a fully warm cache): %v", warmErr)
	}
	if got := f.server.Total(); got != 0 {
		t.Errorf("Total() after the dry run = %d, want 0 (a fully warm cache needs no network access to preview)", got)
	}

	if !bytes.Contains(stdout, []byte("Already warm: acme.app@1.0.0")) {
		t.Errorf("expected an \"Already warm: acme.app@1.0.0\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Already warm: acme.lib@1.0.0")) {
		t.Errorf("expected an \"Already warm: acme.lib@1.0.0\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Dry run: 0 would warm, 2 already warm, 0 would fail")) {
		t.Errorf("expected the already-warm summary line, got stdout=%q", stdout)
	}
}

// TestWarmDryRunUsesLockfilePinAsSHASource pins warmDryRunSHA's precedence:
// under --frozen the lockfile pin names the extracted tree to check, so a
// cache an install populated, with no warmed entry, reports "already warm".
func TestWarmDryRunUsesLockfilePinAsSHASource(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (populate the artifact cache and the extracted store, writing no warmed entry): %v", err)
	}
	newFrozenPinFixture(t, f)

	f.cfg.DryRun = true
	var warmErr error
	stdout, _ := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if warmErr != nil {
		t.Fatalf("Warm --frozen --dry-run: %v", warmErr)
	}

	if !bytes.Contains(stdout, []byte("Already warm: acme.app@1.0.0")) {
		t.Errorf("expected an \"Already warm: acme.app@1.0.0\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Already warm: acme.lib@1.0.0")) {
		t.Errorf("expected an \"Already warm: acme.lib@1.0.0\" line, got stdout=%q", stdout)
	}
}

// TestWarmDryRunReportsWouldWarmWhenNoSHACanBeNamed pins that with no pin and
// no warmed record warmDryRunSHA names no sha and the probe reports "would
// warm"; it must never fall back to the installed record's ArtifactSHA256.
func TestWarmDryRunReportsWouldWarmWhenNoSHACanBeNamed(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (populate the artifact cache and the extracted store, writing no warmed entry): %v", err)
	}

	f.cfg.DryRun = true
	var warmErr error
	stdout, _ := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if warmErr != nil {
		t.Fatalf("Warm --dry-run: %v", warmErr)
	}

	if !bytes.Contains(stdout, []byte("Would warm: acme.app@1.0.0 (artifact cached)")) {
		t.Errorf("expected a \"Would warm: acme.app@1.0.0 (artifact cached)\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Would warm: acme.lib@1.0.0 (artifact cached)")) {
		t.Errorf("expected a \"Would warm: acme.lib@1.0.0 (artifact cached)\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Dry run: 2 would warm, 0 already warm, 0 would fail")) {
		t.Errorf("expected the would-warm summary line, got stdout=%q", stdout)
	}
}

// driftCachedTarballBytes flips every byte of the cached artifact in place,
// keeping its path and size, and leaves its sha256 sidecar and extracted tree
// untouched, so a presence probe still sees an ordinary cache hit.
func driftCachedTarballBytes(t *testing.T, cacheDir, artifactKey string) {
	t.Helper()
	tarPath := filepath.Join(cacheDir, artifactKey)
	original, err := os.ReadFile(tarPath) //nolint:gosec // path is this test's own cache fixture under t.TempDir().
	if err != nil {
		t.Fatalf("read the cached tarball before drifting it: %v", err)
	}
	drifted := make([]byte, len(original))
	for i, b := range original {
		drifted[i] = b ^ 0xFF
	}
	if err := os.WriteFile(tarPath, drifted, helpers.FileMod); err != nil {
		t.Fatalf("drift the cached tarball in place: %v", err)
	}
}

// TestWarmDryRunAndRunDisagreeOnAFrozenOfflineDriftedCacheHit pins a disclosed
// limit: drifted tarball bytes still preview "Already warm", while the real
// --frozen --offline run re-hashes them and fails; detecting it costs a download.
func TestWarmDryRunAndRunDisagreeOnAFrozenOfflineDriftedCacheHit(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm (populate the artifact cache and the extracted tree, unpinned): %v", err)
	}

	// Drift acme.app's cached tarball bytes in place, leaving its sidecar
	// sha256 and its extracted tree exactly as the honest warm above produced
	// them.
	driftCachedTarballBytes(t, f.cfg.CacheDir, helpers.ArtifactKey(f.cfg.Server, "acme-app-1.0.0.tar.gz"))

	newFrozenPinFixture(t, f)
	f.cfg.Offline = true
	f.runtime.HTTP = fetch.NewOffline(f.cfg.Timeout)

	f.cfg.DryRun = true
	var dryErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		dryErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if dryErr != nil {
		t.Fatalf("warm --frozen --offline --dry-run: %v (expected the preview to report success despite the drift)", dryErr)
	}
	if !bytes.Contains(stdout, []byte("Already warm: acme.app@1.0.0")) {
		t.Errorf("expected \"Already warm: acme.app@1.0.0\" despite the drifted bytes, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("0 would warm, 2 already warm, 0 would fail")) {
		t.Errorf("expected a would-fail count of zero, got stdout=%q", stdout)
	}
	if !bytes.Contains(stderr, []byte("--frozen --offline: this preview checks the cached artifact's recorded digest against the pin")) {
		t.Errorf("expected the --frozen --offline warning on stderr, got stderr=%q", stderr)
	}

	f.cfg.DryRun = false
	var realErr error
	_, realStderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		realErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	// The real run fails closed; its "Failed:" line names the checksum mismatch.
	if !errors.Is(realErr, helpers.ErrInstallationFailed) {
		t.Fatalf("expected the real --frozen --offline warm to fail with errors.Is ErrInstallationFailed, got %v", realErr)
	}
	if !bytes.Contains(realStderr, []byte(helpers.ErrSHA256Mismatch.Error())) {
		t.Errorf("expected the real run's failure line to name %q, got stderr=%q", helpers.ErrSHA256Mismatch.Error(), realStderr)
	}
}

// TestWarmDryRunOfflineFailsClosed pins that an offline warm dry run with no
// cached artifact reports "Would fail:" and exits ExitInstall with
// ErrOfflineMode, as the real offline warm would.
func TestWarmDryRunOfflineFailsClosed(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock (populate the snapshot's metadata caches): %v", err)
	}

	f.cfg.DryRun = true
	f.cfg.Offline = true
	f.runtime.HTTP = fetch.NewOffline(f.cfg.Timeout)

	var warmErr error
	_, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})

	if warmErr == nil {
		t.Fatal("expected an error from an offline dry run against an uncached artifact")
	}
	if !errors.Is(warmErr, helpers.ErrInstallationFailed) {
		t.Errorf("expected errors.Is ErrInstallationFailed, got %v", warmErr)
	}
	if !errors.Is(warmErr, helpers.ErrOfflineMode) {
		t.Errorf("expected errors.Is ErrOfflineMode, got %v", warmErr)
	}
	if got := exitcode.FromError(warmErr); got != exitcode.ExitInstall {
		t.Errorf("exitcode.FromError(err) = %d, want ExitInstall (%d)", got, exitcode.ExitInstall)
	}
	if !bytes.Contains(stderr, []byte("Would fail:")) {
		t.Errorf("expected a \"Would fail:\" line on stderr, got %q", stderr)
	}
}

// TestWarmDryRunStillTakesBackendLock is assertDryRunStillTakesBackendLock
// driven by collections.Warm: a dry-run warm locks like a real one, unlike
// --no-cache, which is rejected before initInstall runs.
func TestWarmDryRunStillTakesBackendLock(t *testing.T) {
	assertDryRunStillTakesBackendLock(t, collections.Warm)
}

// TestWarmDryRunSkipsMetrics pins that writeRunMetrics's dry-run guard covers
// warm: the configured metrics file is not written and a warning names it.
func TestWarmDryRunSkipsMetrics(t *testing.T) {
	f := newE2EFixture(t)
	f.cfg.MetricsFile = filepath.Join(t.TempDir(), "metrics.json")
	f.cfg.DryRun = true

	var warmErr error
	_, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if warmErr != nil {
		t.Fatalf("Warm (dry run): %v", warmErr)
	}
	if _, statErr := os.Stat(f.cfg.MetricsFile); !os.IsNotExist(statErr) {
		t.Errorf("expected no metrics file written by a dry run, stat error = %v", statErr)
	}
	if !bytes.Contains(stderr, []byte(f.cfg.MetricsFile)) {
		t.Errorf("expected a warning naming the skipped metrics path, got stderr=%q", stderr)
	}
}

// TestWarmDryRunBannerSurvivesQuiet pins that the dry-run banner reaches
// stderr under --quiet for warm too, since initInstall emits it for every
// dry-run command.
func TestWarmDryRunBannerSurvivesQuiet(t *testing.T) {
	f := newE2EFixture(t)
	f.cfg.DryRun = true
	f.cfg.Quiet = true

	var warmErr error
	_, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if warmErr != nil {
		t.Fatalf("Warm: %v", warmErr)
	}

	if !bytes.Contains(stderr, []byte("--dry-run")) {
		t.Errorf("expected the dry-run banner on stderr despite --quiet, got stderr=%q", stderr)
	}
}

// assertFrozenOfflinePinMismatchExitsIntegrity seeds the cache by a real call
// to run, then under --frozen --offline --dry-run requires a pin that disagrees
// with the cached digest to fail with ExitIntegrity, and a corrected pin to pass.
func assertFrozenOfflinePinMismatchExitsIntegrity(t *testing.T, run func(context.Context, *config.Config, *infra.Infra) error) {
	t.Helper()
	f := newE2EFixture(t)

	if err := run(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed (populate the cache with the real, correctly-hashed artifact): %v", err)
	}

	lockPath, lf := newFrozenPinFixture(t, f)
	f.cfg.Offline = true
	f.runtime.HTTP = fetch.NewOffline(f.cfg.Timeout)
	f.cfg.DryRun = true

	assertFrozenOfflinePreviewFailsOnCorruptedPin(t, f, run, lockPath, lf)
	assertFrozenOfflinePreviewSucceedsOnCorrectedPin(t, f, run, lockPath, lf)
}

// assertFrozenOfflinePreviewFailsOnCorruptedPin corrupts acme.app's pin and
// requires the preview to fail with ErrSHA256Mismatch, ExitIntegrity, a
// "Would fail:" line and a would-fail count of one.
func assertFrozenOfflinePreviewFailsOnCorruptedPin(
	t *testing.T, f *e2eFixture, run func(context.Context, *config.Config, *infra.Infra) error, lockPath string, lf *lockfile.File,
) {
	t.Helper()
	setAppPin(lf, corruptedAppSHA256)
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save corrupted lockfile: %v", err)
	}

	var dryErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		dryErr = run(context.Background(), f.cfg, f.runtime)
	})

	if dryErr == nil {
		t.Fatal("expected the preview to fail closed on a pin/digest mismatch, got nil")
	}
	if !errors.Is(dryErr, helpers.ErrSHA256Mismatch) {
		t.Errorf("expected errors.Is ErrSHA256Mismatch, got %v", dryErr)
	}
	if got := exitcode.FromError(dryErr); got != exitcode.ExitIntegrity {
		t.Errorf("exitcode.FromError(err) = %d, want ExitIntegrity (%d)", got, exitcode.ExitIntegrity)
	}
	if !bytes.Contains(stderr, []byte("Would fail: acme.app@1.0.0")) {
		t.Errorf("expected a \"Would fail: acme.app@1.0.0\" line on stderr, got stderr=%q", stderr)
	}
	if !bytes.Contains(stdout, []byte("1 would fail")) {
		t.Errorf("expected the summary line to count exactly one would-fail collection, got stdout=%q", stdout)
	}
}

// assertFrozenOfflinePreviewSucceedsOnCorrectedPin is the positive control:
// with the pin back on the artifact's real digest the same preview succeeds
// with no would-fail, so the refusal above is genuine.
func assertFrozenOfflinePreviewSucceedsOnCorrectedPin(
	t *testing.T, f *e2eFixture, run func(context.Context, *config.Config, *infra.Infra) error, lockPath string, lf *lockfile.File,
) {
	t.Helper()
	setAppPin(lf, f.appV1.SHA256)
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save corrected lockfile: %v", err)
	}

	var okErr error
	okStdout, _ := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		okErr = run(context.Background(), f.cfg, f.runtime)
	})
	if okErr != nil {
		t.Fatalf("expected the preview to succeed once the pin matches the cached artifact, got %v", okErr)
	}
	if !bytes.Contains(okStdout, []byte("0 would fail")) {
		t.Errorf("expected a would-fail count of zero once the pin is corrected, got stdout=%q", okStdout)
	}
}

// TestInstallDryRunFrozenOfflinePinMismatchExitsIntegrity is
// assertFrozenOfflinePinMismatchExitsIntegrity driven by collections.Start;
// see that helper's own doc comment for the property being pinned.
func TestInstallDryRunFrozenOfflinePinMismatchExitsIntegrity(t *testing.T) {
	assertFrozenOfflinePinMismatchExitsIntegrity(t, collections.Start)
}

// TestWarmDryRunFrozenOfflinePinMismatchExitsIntegrity is
// assertFrozenOfflinePinMismatchExitsIntegrity driven by collections.Warm,
// pinning warmDryRunProbe's dryRunPinVerdict call.
func TestWarmDryRunFrozenOfflinePinMismatchExitsIntegrity(t *testing.T) {
	assertFrozenOfflinePinMismatchExitsIntegrity(t, collections.Warm)
}

// driftCachedSidecarDigest overwrites the cached artifact's sha256 sidecar
// with sha and leaves the tarball bytes untouched: the recorded-digest-only
// drift, the converse of driftCachedTarballBytes.
func driftCachedSidecarDigest(t *testing.T, cacheDir, artifactKey, sha string) {
	t.Helper()
	sidecarPath := filepath.Join(cacheDir, artifactKey) + helpers.ArtifactSHASidecarSuffix
	if err := os.WriteFile(sidecarPath, []byte(sha), helpers.FileMod); err != nil {
		t.Fatalf("drift the cached artifact's sha256 sidecar: %v", err)
	}
}

// TestInstallDryRunAndRunDisagreeOnAFrozenOfflineRecordedDigestDrift pins a
// disclosed asymmetry: a drifted sidecar fails the preview's dryRunPinVerdict,
// while the real install re-hashes the bytes under the pin and succeeds.
func TestInstallDryRunAndRunDisagreeOnAFrozenOfflineRecordedDigestDrift(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm (seed the artifact cache with the real, correctly-hashed artifact): %v", err)
	}

	newFrozenPinFixture(t, f) // pin stays correct; only the recorded digest drifts below.
	driftCachedSidecarDigest(
		t, f.cfg.CacheDir, helpers.ArtifactKey(f.cfg.Server, "acme-app-1.0.0.tar.gz"), corruptedAppSHA256,
	)

	f.cfg.Offline = true
	f.runtime.HTTP = fetch.NewOffline(f.cfg.Timeout)

	f.cfg.DryRun = true
	var dryErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		dryErr = collections.Start(context.Background(), f.cfg, f.runtime)
	})
	if dryErr == nil {
		t.Fatal("expected the preview to report a would-fail on the drifted recorded digest, got nil")
	}
	if !errors.Is(dryErr, helpers.ErrSHA256Mismatch) {
		t.Errorf("expected errors.Is ErrSHA256Mismatch, got %v", dryErr)
	}
	if got := exitcode.FromError(dryErr); got != exitcode.ExitIntegrity {
		t.Errorf("exitcode.FromError(err) = %d, want ExitIntegrity (%d)", got, exitcode.ExitIntegrity)
	}
	if !bytes.Contains(stderr, []byte("Would fail: acme.app@1.0.0")) {
		t.Errorf("expected a \"Would fail: acme.app@1.0.0\" line on stderr, got stderr=%q", stderr)
	}
	if !bytes.Contains(stdout, []byte("1 would fail")) {
		t.Errorf("expected the summary line to count exactly one would-fail collection, got stdout=%q", stdout)
	}

	f.cfg.DryRun = false
	var realErr error
	_, realStderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		realErr = collections.Start(context.Background(), f.cfg, f.runtime)
	})
	if realErr != nil {
		t.Fatalf(
			"expected the real --frozen --offline install to succeed despite the drifted recorded digest, got %v (stderr=%q)",
			realErr, realStderr,
		)
	}
}
