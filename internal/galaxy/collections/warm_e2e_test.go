package collections_test

// These e2e tests drive the warm command's real pipeline against fakegalaxy,
// mirroring the install suite; warm --dry-run's preview is covered in
// dry_run_e2e_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// assertExtractedStorePresent fails the test unless the content-addressable
// extracted store under cacheDir has a ready (fully extracted) entry for sha.
func assertExtractedStorePresent(t *testing.T, cacheDir, sha string) {
	t.Helper()
	path := filepath.Join(cacheDir, extracted.RootDirName, sha, extracted.ReadyMarker)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected extracted store entry for sha %s to exist, stat error: %v", sha, err)
	}
}

// loadStoreSnapshot loads cfg's persisted snapshot and closes the backend.
// Call it only after every Warm/Start run on that cache has returned.
func loadStoreSnapshot(t *testing.T, cfg *config.Config, runtime *infra.Infra) *store.Store {
	t.Helper()
	ctx := context.Background()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("cacheBackend.New: %v", err)
	}
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() {
		_ = backend.Close(ctx)
	}()
	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	return st
}

// assertMetricsCommand reads the metrics file at path and fails the test
// unless its "command" field equals want.
func assertMetricsCommand(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixed cfg.MetricsFile, not user input.
	if err != nil {
		t.Fatalf("read metrics file %s: %v", path, err)
	}
	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("unmarshal metrics file %s: %v", path, err)
	}
	got, _ := written["command"].(string)
	if got != want {
		t.Errorf("metrics file %s command = %q, want %q", path, got, want)
	}
}

// TestWarmColdCachePopulatesCacheWithoutInstalling pins that a cold warm
// fills both caches, creates no ansible_collections tree, records no install,
// and writes a warmed entry per ns.name@version holding its artifact sha.
func TestWarmColdCachePopulatesCacheWithoutInstalling(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm: %v", err)
	}

	assertArtifactFilePresent(t, f.cfg.CacheDir, f.cfg.Server, "acme-app-1.0.0.tar.gz")
	assertArtifactFilePresent(t, f.cfg.CacheDir, f.cfg.Server, "acme-lib-1.0.0.tar.gz")
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.appV1.SHA256)
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.libV1.SHA256)

	assertPathAbsent(t, filepath.Join(f.downloadPath, "ansible_collections"))

	st := loadStoreSnapshot(t, f.cfg, f.runtime)
	if got := len(st.InstalledArtifactSHAByKey()); got != 0 {
		t.Errorf("installed set size = %d, want 0 (warm never calls recordInstall)", got)
	}

	warmed := st.WarmedArtifactSHAByKey()
	if got := warmed[collectionKey(f.appV1)]; got != f.appV1.SHA256 {
		t.Errorf("warmed[%q] = %q, want %q", collectionKey(f.appV1), got, f.appV1.SHA256)
	}
	if got := warmed[collectionKey(f.libV1)]; got != f.libV1.SHA256 {
		t.Errorf("warmed[%q] = %q, want %q", collectionKey(f.libV1), got, f.libV1.SHA256)
	}
}

// TestWarmColdCacheDownloadsEachArtifactOnce pins warm's prefetch handoff: a
// cold warm costs exactly one artifact GET per collection, where a worker
// that re-downloaded instead of taking the handoff would double it.
func TestWarmColdCacheDownloadsEachArtifactOnce(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm: %v", err)
	}

	if got := f.server.Count(fakegalaxy.EndpointArtifact); got != 2 {
		t.Errorf("EndpointArtifact count = %d, want 2 (one download per collection, prefetched then handed off)", got)
	}
}

// TestInstallRecordsNoWarmedEntries pins that install writes no warmed
// entry: its install record already keeps the extracted tree, and a warmed
// one would outlive cleanup's removal of that install.
func TestInstallRecordsNoWarmedEntries(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	st := loadStoreSnapshot(t, f.cfg, f.runtime)
	if got := len(st.WarmedArtifactSHAByKey()); got != 0 {
		t.Errorf("warmed set size = %d, want 0 (install never calls recordWarmed)", got)
	}
}

// TestInstallPreservesWarmedEntries pins that install neither removes nor
// rewrites a warmed entry: the install path writes the whole snapshot back,
// so pruning warmed keys it installed would expose warmed trees to cleanup.
func TestInstallPreservesWarmedEntries(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm (populate the warmed set): %v", err)
	}

	warmedBefore := loadStoreSnapshot(t, f.cfg, f.runtime).WarmedArtifactSHAByKey()
	appKey, libKey := collectionKey(f.appV1), collectionKey(f.libV1)
	if got := warmedBefore[appKey]; got != f.appV1.SHA256 {
		t.Fatalf("warmedBefore[%q] = %q, want %q", appKey, got, f.appV1.SHA256)
	}
	if got := warmedBefore[libKey]; got != f.libV1.SHA256 {
		t.Fatalf("warmedBefore[%q] = %q, want %q", libKey, got, f.libV1.SHA256)
	}

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	warmedAfter := loadStoreSnapshot(t, f.cfg, f.runtime).WarmedArtifactSHAByKey()
	// A plain len() comparison would also pass if install rewrote both entries
	// with different (but still two) values, so each key is checked against
	// the exact sha recorded before install ran.
	if got := warmedAfter[appKey]; got != warmedBefore[appKey] {
		t.Errorf("warmedAfter[%q] = %q, want unchanged %q", appKey, got, warmedBefore[appKey])
	}
	if got := warmedAfter[libKey]; got != warmedBefore[libKey] {
		t.Errorf("warmedAfter[%q] = %q, want unchanged %q", libKey, got, warmedBefore[libKey])
	}
}

// TestWarmRewarmIsFullyCacheServed asserts that warming an already-warm cache
// a second time never touches the network: both the artifact cache and the
// extracted store are served straight from disk.
func TestWarmRewarmIsFullyCacheServed(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Warm (populate the cache): %v", err)
	}

	f.server.ResetCounts()
	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Warm (re-warm): %v", err)
	}

	if got := f.server.Total(); got != 0 {
		t.Errorf("Total() after re-warm = %d, want 0 (cache-served, no HTTP at all)", got)
	}
}

// TestWarmFrozenHonorsLockfilePinAndFailsClosedOnCorruption pins that a
// frozen warm caches the pinned version over a higher one, and that a
// corrupted pin fails the run without poisoning the cached tarball.
func TestWarmFrozenHonorsLockfilePinAndFailsClosedOnCorruption(t *testing.T) {
	f := newE2EFixture(t)
	lockPath, lf := newFrozenPinFixture(t, f)

	// Sequential, not parallel subtests: the second reuses and mutates the
	// same lockfile the first already wrote, mirroring
	// TestFrozenInstallHonorsLockfilePins's own explicit ordering dependency.
	t.Run("pin overrides the highest available version", func(t *testing.T) {
		assertWarmFrozenHonorsPin(t, f)
	})
	t.Run("a corrupted pin fails closed without poisoning the cache", func(t *testing.T) {
		assertWarmFrozenCorruptedPinFailsClosed(t, f, lockPath, lf)
	})
}

// assertWarmFrozenHonorsPin runs a frozen warm and asserts it warms the
// lockfile's pinned acme.app@1.0.0 - not the higher 2.0.0 also registered on
// the server - without ever listing versions.
func assertWarmFrozenHonorsPin(t *testing.T, f *e2eFixture) {
	t.Helper()
	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("frozen Warm honoring the pin: %v", err)
	}
	assertArtifactFilePresent(t, f.cfg.CacheDir, f.cfg.Server, "acme-app-1.0.0.tar.gz")
	if got := f.server.Count(fakegalaxy.EndpointVersionsList); got != 0 {
		t.Errorf("EndpointVersionsList count = %d, want 0 (frozen resolution never consults the versions listing)", got)
	}
}

// assertWarmFrozenCorruptedPinFailsClosed fails a frozen warm on a corrupted
// pin, then restores the pin and re-warms with no HTTP at all, proving the
// failed run left the cache intact.
func assertWarmFrozenCorruptedPinFailsClosed(t *testing.T, f *e2eFixture, lockPath string, lf *lockfile.File) {
	t.Helper()
	setAppPin(lf, corruptedAppSHA256)
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save corrupted lockfile: %v", err)
	}

	err := collections.Warm(context.Background(), f.cfg, f.runtime)
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed for the corrupted pin, got %v", err)
	}
	// Each worker's cause is joined behind the headline, so the triggering
	// sentinel is reachable too, not just the aggregate classification.
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is ErrSHA256Mismatch for the corrupted pin, got %v", err)
	}

	setAppPin(lf, f.appV1.SHA256)
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("restore the true pin in the lockfile: %v", err)
	}

	f.server.ResetCounts()
	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("recovery Warm after the corrupted-pin failure: %v", err)
	}
	if got := f.server.Total(); got != 0 {
		t.Errorf("Total() after the recovery Warm = %d, want 0 (cache un-poisoned by the failed pin check)", got)
	}
}

// TestWarmOffline asserts --offline warm refuses to reach the network on a
// cold cache, and warms entirely from a warm cache with the network
// transport hard-disabled, mirroring TestOfflineInstall.
func TestWarmOffline(t *testing.T) {
	t.Parallel()

	t.Run("cold cache rejects the network", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)
		f.cfg.Offline = true
		f.runtime = infra.New(noopPrinter{}, fetch.NewOffline(f.cfg.Timeout))

		err := collections.Warm(context.Background(), f.cfg, f.runtime)
		if err == nil {
			t.Fatal("expected an offline-mode error on a cold cache, got nil")
		}
		if !errors.Is(err, helpers.ErrOfflineMode) {
			t.Fatalf("expected errors.Is ErrOfflineMode, got %v", err)
		}
		if got := f.server.Total(); got != 0 {
			t.Errorf("Total() = %d, want 0 (the offline transport never dials)", got)
		}
	})

	t.Run("warm cache re-warms with the network hard disabled", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)

		if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("first Warm (populate the cache online): %v", err)
		}

		f.server.ResetCounts()
		f.runtime.HTTP = fetch.NewOffline(f.cfg.Timeout)
		f.cfg.Offline = true

		if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("second Warm (offline re-warm): %v", err)
		}
		if got := f.server.Total(); got != 0 {
			t.Errorf("Total() after the offline re-warm = %d, want 0", got)
		}
	})
}

// TestWarmPartialFailureKeepsSuccessfulCollectionCached pins that one failed
// download still leaves the other collection cached and warmed, the snapshot
// saved, and no warmed entry for the failed one.
func TestWarmPartialFailureKeepsSuccessfulCollectionCached(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.server.Fail(fakegalaxy.EndpointArtifact, "acme", "lib", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

	err := collections.Warm(context.Background(), f.cfg, f.runtime)
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}

	assertArtifactFilePresent(t, f.cfg.CacheDir, f.cfg.Server, "acme-app-1.0.0.tar.gz")
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.appV1.SHA256)

	st := loadStoreSnapshot(t, f.cfg, f.runtime)
	// Resolution always records requirements, so a non-empty set proves the
	// save ran despite the partial failure.
	if got := len(st.RequirementsSnapshot()); got == 0 {
		t.Errorf("RequirementsSnapshot is empty, want the snapshot to have been saved despite the partial failure")
	}
	if got := len(st.InstalledArtifactSHAByKey()); got != 0 {
		t.Errorf("installed set size = %d, want 0 (warm never calls recordInstall)", got)
	}

	warmed := st.WarmedArtifactSHAByKey()
	if got := warmed[collectionKey(f.appV1)]; got != f.appV1.SHA256 {
		t.Errorf("warmed[%q] = %q, want %q (the successful collection)", collectionKey(f.appV1), got, f.appV1.SHA256)
	}
	if _, ok := warmed[collectionKey(f.libV1)]; ok {
		t.Errorf("warmed[%q] present, want absent (the collection whose download persistently failed)", collectionKey(f.libV1))
	}
}

// collectionKey builds the ns.name@version snapshot key for a fakegalaxy
// Version, matching the unexported collection.key() format the production
// code stamps into the store.
func collectionKey(v fakegalaxy.Version) string {
	return v.Namespace + "." + v.Name + "@" + v.Version
}

// TestWarmLockReleasedAfterFailingRun pins that a failing warm releases the
// exclusive lock, so a second Warm on the same cache is not blocked.
func TestWarmLockReleasedAfterFailingRun(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	// The fault covers the prefetch and fallback downloads' retries exactly,
	// so the first Warm exhausts it and the second hits a clean server.
	f.server.Fail(fakegalaxy.EndpointArtifact, "acme", "lib", fakegalaxy.Fault{
		Status: http.StatusServiceUnavailable,
		Count:  2 * helpers.FetchRetryMaxAttempts,
	})

	err := collections.Warm(context.Background(), f.cfg, f.runtime)
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("first Warm: expected errors.Is ErrInstallationFailed, got %v", err)
	}

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Warm after the failing run: %v (the first run's lock was not released)", err)
	}
}

// TestWarmNoCacheRejectsBeforeResolving pins that warm --no-cache exits as a
// usage error before any network call, with or without --dry-run, and with
// --dry-run also before the backend is opened.
func TestWarmNoCacheRejectsBeforeResolving(t *testing.T) {
	t.Parallel()

	t.Run("without dry-run", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)
		f.cfg.NoCache = true

		err := collections.Warm(context.Background(), f.cfg, f.runtime)
		if !errors.Is(err, helpers.ErrWarmCacheDisabled) {
			t.Fatalf("expected errors.Is ErrWarmCacheDisabled, got %v", err)
		}
		if got := f.server.Total(); got != 0 {
			t.Errorf("Total() = %d, want 0 (--no-cache must reject before resolving anything)", got)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitUsage {
			t.Errorf("exitcode.FromError(err) = %d, want ExitUsage (%d)", got, exitcode.ExitUsage)
		}
	})

	t.Run("with dry-run", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)
		f.cfg.NoCache = true
		f.cfg.DryRun = true

		err := collections.Warm(context.Background(), f.cfg, f.runtime)
		if !errors.Is(err, helpers.ErrWarmCacheDisabled) {
			t.Fatalf("expected errors.Is ErrWarmCacheDisabled, got %v", err)
		}
		if got := f.server.Total(); got != 0 {
			t.Errorf("Total() = %d, want 0 (--no-cache must reject before any network call)", got)
		}
		// Opening the backend creates the cache directory, so its absence
		// proves the backend was never opened.
		if _, statErr := os.Stat(f.cfg.CacheDir); !os.IsNotExist(statErr) {
			t.Errorf("expected cacheDir to never be created, stat error = %v", statErr)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitUsage {
			t.Errorf("exitcode.FromError(err) = %d, want ExitUsage (%d)", got, exitcode.ExitUsage)
		}
	})
}

// TestWarmMetricsWrittenForSuccessAndFailure asserts a metrics file is
// produced with command == "warm" both when warm succeeds and when it fails,
// mirroring writeRunMetrics's unconditional call on both outcomes.
func TestWarmMetricsWrittenForSuccessAndFailure(t *testing.T) {
	t.Parallel()

	t.Run("successful warm", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)
		f.cfg.MetricsFile = filepath.Join(t.TempDir(), "metrics.json")

		if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("Warm: %v", err)
		}
		assertMetricsCommand(t, f.cfg.MetricsFile, "warm")
	})

	t.Run("failing warm", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)
		f.cfg.MetricsFile = filepath.Join(t.TempDir(), "metrics.json")
		f.server.Fail(fakegalaxy.EndpointArtifact, "acme", "lib", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

		err := collections.Warm(context.Background(), f.cfg, f.runtime)
		if !errors.Is(err, helpers.ErrInstallationFailed) {
			t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
		}
		assertMetricsCommand(t, f.cfg.MetricsFile, "warm")
	})
}

// newCycleFixture makes acme.app and acme.lib depend on each other: frozen,
// through the deps of a written lockfile, else through an acme.lib@2.0.0 the
// solver picks for acme.app's ">=1.0.0" that depends on acme.app in turn.
func newCycleFixture(t *testing.T, frozen bool) *e2eFixture {
	t.Helper()
	f := newE2EFixture(t)
	if !frozen {
		f.server.AddVersion("acme", "lib", "2.0.0", map[string]string{"acme.app": ">=1.0.0"})
		return f
	}
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        f.cfg.Server,
		Collections: []lockfile.Entry{
			{
				Name: "acme.app", Version: testVersion100, Source: f.cfg.Server, DownloadURL: f.appV1.DownloadURL,
				SHA256: f.appV1.SHA256, Deps: []string{"acme.lib"},
			},
			{
				Name: "acme.lib", Version: testVersion100, Source: f.cfg.Server, DownloadURL: f.libV1.DownloadURL,
				SHA256: f.libV1.SHA256, Deps: []string{"acme.app"},
			},
		},
	}
	if err := lockfile.Save(lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile), lf); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
	f.cfg.Frozen = true
	return f
}

// TestWarmRefusesADependencyCycleAsInstallDoes pins that warm, like install,
// fails a cyclic graph with the resolution code, from a fresh solve or the
// lockfile and under --dry-run too, before a single artifact is fetched.
func TestWarmRefusesADependencyCycleAsInstallDoes(t *testing.T) {
	t.Parallel()
	commands := []struct {
		run  func(context.Context, *config.Config, *infra.Infra) error
		name string
	}{
		{run: collections.Start, name: "install"},
		{run: collections.Warm, name: "warm"},
	}
	modes := []struct {
		name   string
		frozen bool
		dryRun bool
	}{
		{name: "fresh solve"},
		{name: "fresh solve dry run", dryRun: true},
		{name: "frozen", frozen: true},
		{name: "frozen dry run", frozen: true, dryRun: true},
	}
	for _, command := range commands {
		for _, mode := range modes {
			t.Run(command.name+" "+mode.name, func(t *testing.T) {
				t.Parallel()
				f := newCycleFixture(t, mode.frozen)
				f.cfg.DryRun = mode.dryRun

				err := command.run(context.Background(), f.cfg, f.runtime)
				if !errors.Is(err, helpers.ErrDependencyGraphHasACycle) {
					t.Fatalf("%s: expected errors.Is ErrDependencyGraphHasACycle, got %v", command.name, err)
				}
				if got := exitcode.FromError(err); got != exitcode.ExitResolution {
					t.Errorf("exitcode.FromError(err) = %d, want ExitResolution (%d)", got, exitcode.ExitResolution)
				}
				if got := f.server.Count(fakegalaxy.EndpointArtifact); got != 0 {
					t.Errorf("EndpointArtifact count = %d, want 0 (the cycle fails the plan before any fetch)", got)
				}
			})
		}
	}
}
