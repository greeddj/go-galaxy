// Package collections_test drives the real install pipeline end to end
// through the public API (collections.Start, Lock) against an in-memory fake
// Galaxy server, the way a CI job would.
package collections_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
	"github.com/psvmcc/hub/pkg/types"
)

// e2eTimeout is the HTTP client timeout every fixture in this file uses: long
// enough that it is never the reason a test fails, short enough that a real
// hang would still fail fast.
const e2eTimeout = 30 * time.Second

// corruptedAppSHA256 is a well-formed but deliberately wrong sha256 hex
// string, used to simulate a lockfile whose pin no longer matches the
// artifact it names.
const corruptedAppSHA256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// testVersion100 is the version every fixture registers first; it duplicates
// the internal package's constant, unreachable across the package boundary.
const testVersion100 = "1.0.0"

// noopPrinter is an output.Printer that prints nothing, mirroring the
// internal tests' stub across the package boundary.
type noopPrinter struct{}

func (noopPrinter) Printf(string, ...any)                        {}
func (noopPrinter) PersistentPrintf(string, ...any)              {}
func (noopPrinter) Okf(string, ...any)                           {}
func (noopPrinter) OkVersionf(string, string, ...any)            {}
func (noopPrinter) Updatef(string, ...any)                       {}
func (noopPrinter) Errorf(string, ...any)                        {}
func (noopPrinter) ErrorVersionf(string, string, string, ...any) {}
func (noopPrinter) Warnf(string, ...any)                         {}
func (noopPrinter) Debugf(string, ...any)                        {}
func (noopPrinter) DebugSincef(time.Time, string, ...any)        {}

// e2eFixture bundles one scenario's fake server, config and runtime; every
// fixture registers acme.app@1.0.0 (depending on acme.lib>=1.0.0) and
// acme.lib@1.0.0, with requirements.yml asking for acme.app at "*".
type e2eFixture struct {
	server       *fakegalaxy.Server
	cfg          *config.Config
	runtime      *infra.Infra
	appV1        fakegalaxy.Version
	libV1        fakegalaxy.Version
	downloadPath string
}

// newE2EFixture builds a fresh fake server and a matching cold cache/install
// pair rooted under t.TempDir, wired for an online install against the fake
// server's own client.
func newE2EFixture(t *testing.T) *e2eFixture {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme.app")

	s := fakegalaxy.New(t)
	appV1 := s.AddVersion("acme", "app", testVersion100, map[string]string{"acme.lib": ">=1.0.0"})
	libV1 := s.AddVersion("acme", "lib", testVersion100, nil)

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
	}

	return &e2eFixture{
		server:       s,
		cfg:          cfg,
		runtime:      infra.New(noopPrinter{}, s.Client()),
		appV1:        appV1,
		libV1:        libV1,
		downloadPath: downloadPath,
	}
}

// writeRequirements writes a minimal requirements.yml at path requiring name
// at version "*", the map-item shape requirements.Load parses.
func writeRequirements(t *testing.T, path, name string) {
	t.Helper()
	content := "collections:\n  - name: " + name + "\n    version: \"*\"\n"
	if err := os.WriteFile(path, []byte(content), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
}

// installPathFor returns where acme.name lands under downloadPath in the
// ansible_collections layout; every collection here is in the acme namespace.
func installPathFor(downloadPath, name string) string {
	return filepath.Join(downloadPath, "ansible_collections", "acme", name)
}

// manifestPathFor returns acme.name's installed MANIFEST.json path under
// downloadPath.
func manifestPathFor(downloadPath, name string) string {
	return filepath.Join(installPathFor(downloadPath, name), "MANIFEST.json")
}

// assertManifestInstalled fails the test unless acme.name's MANIFEST.json
// exists under downloadPath.
func assertManifestInstalled(t *testing.T, downloadPath, name string) {
	t.Helper()
	path := manifestPathFor(downloadPath, name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected acme.%s installed, MANIFEST.json missing at %s: %v", name, path, err)
	}
}

// assertPathAbsent fails the test if path exists, or if stat-ing it fails
// for any reason other than its absence.
func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected %s to not exist, but it does", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error on %s: %v", path, err)
	}
}

// readManifestVersion reads and parses the collection_info.version field of
// an installed acme.name collection's MANIFEST.json.
func readManifestVersion(t *testing.T, downloadPath, name string) string {
	t.Helper()
	path := manifestPathFor(downloadPath, name)
	data, err := os.ReadFile(path) //nolint:gosec // path is built from this test's own temp dirs.
	if err != nil {
		t.Fatalf("read MANIFEST.json at %s: %v", path, err)
	}
	var manifest types.GalaxyCollectionVersionInfoManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse MANIFEST.json at %s: %v", path, err)
	}
	return manifest.CollectionInfo.Version
}

// TestFreshInstallDownloadsFromNetwork asserts a cold-cache install against a
// fake Galaxy server succeeds, installs every collection in the dependency
// graph, and actually hits the network to do so.
func TestFreshInstallDownloadsFromNetwork(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
	if got := f.server.Count(fakegalaxy.EndpointArtifact); got < 2 {
		t.Errorf("EndpointArtifact count = %d, want at least 2 (a real network install happened)", got)
	}
}

// TestInstallReportNamesTheResolvedVersion pins that the success and failure
// lines name the version the solver chose for "*", placed before the cause so
// it cannot read as part of the error text.
func TestInstallReportNamesTheResolvedVersion(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	printer := &lineCapturingPrinter{}
	f.runtime = infra.New(printer, f.server.Client())
	f.server.Fail(fakegalaxy.EndpointArtifact, "acme", "app", fakegalaxy.Fault{
		Status: http.StatusServiceUnavailable, Count: -1,
	})

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err == nil {
		t.Fatalf("Start: expected the armed artifact fault to fail the run")
	}

	if want := "Installed: acme.lib == " + testVersion100; !printer.hasLineContaining(want) {
		t.Fatalf("install report lacks %q:\n%v", want, printer.snapshot())
	}
	failed := "Failed: acme.app == " + testVersion100 + " error: "
	if !printer.hasLineContaining(failed) {
		t.Fatalf("install report lacks a failure line starting %q:\n%v", failed, printer.snapshot())
	}
}

// TestWarmInstallServesFromCache pins that reinstalling into a wiped download
// path over a populated cache makes no HTTP request at all.
func TestWarmInstallServesFromCache(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (populate the cache): %v", err)
	}

	f.server.ResetCounts()
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath to force a reinstall from cache: %v", err)
	}

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Start (warm cache): %v", err)
	}

	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
	if got := f.server.Total(); got != 0 {
		t.Errorf("Total() after the warm reinstall = %d, want 0 (snapshot- and cache-served, no HTTP at all)", got)
	}
}

// TestFrozenInstallHonorsLockfilePins pins that --frozen installs the locked
// version over a higher one and fails closed on a corrupted pin. The subtests
// share one lockfile, so neither they nor the test run in parallel.
func TestFrozenInstallHonorsLockfilePins(t *testing.T) {
	f := newE2EFixture(t)
	lockPath, lf := newFrozenPinFixture(t, f)

	t.Run("pin overrides the highest available version", func(t *testing.T) {
		assertFrozenPinOverridesHighestVersion(t, f)
	})
	t.Run("a corrupted pin fails the run without installing", func(t *testing.T) {
		assertFrozenCorruptedPinFailsClosed(t, f, lockPath, lf)
	})
}

// newFrozenPinFixture publishes acme.app@2.0.0 so only the pin can hold 1.0.0,
// writes a lockfile pinning acme.app and acme.lib at 1.0.0 to their real
// sha256 sums, and sets f.cfg.Frozen.
func newFrozenPinFixture(t *testing.T, f *e2eFixture) (string, *lockfile.File) {
	t.Helper()
	f.server.AddVersion("acme", "app", "2.0.0", map[string]string{"acme.lib": ">=1.0.0"})

	lockPath := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        f.cfg.Server,
		Collections: []lockfile.Entry{
			{Name: "acme.app", Version: testVersion100, Source: f.cfg.Server, SHA256: f.appV1.SHA256, Deps: []string{"acme.lib"}},
			{Name: "acme.lib", Version: testVersion100, Source: f.cfg.Server, SHA256: f.libV1.SHA256},
		},
	}
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
	f.cfg.Frozen = true
	return lockPath, lf
}

// setAppPin sets acme.app's SHA256 pin in-place within lf.Collections.
// Shared by the install- and warm-side corrupted-pin e2e tests.
func setAppPin(lf *lockfile.File, sha string) {
	for i := range lf.Collections {
		if lf.Collections[i].Name == "acme.app" {
			lf.Collections[i].SHA256 = sha
		}
	}
}

// assertFrozenPinOverridesHighestVersion asserts a frozen install takes the
// pinned acme.app@1.0.0 over 2.0.0 without listing versions, and that its
// metrics report says "frozen": true.
func assertFrozenPinOverridesHighestVersion(t *testing.T, f *e2eFixture) {
	t.Helper()
	// f.cfg is shared with the corrupted-pin subtest, so MetricsFile is set
	// here and restored afterward.
	f.cfg.MetricsFile = filepath.Join(t.TempDir(), "metrics.json")
	defer func() { f.cfg.MetricsFile = "" }()

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := readManifestVersion(t, f.downloadPath, "app"); got != testVersion100 {
		t.Errorf("installed acme.app collection_info.version = %q, want %q (lockfile pin over the 2.0.0 highest)", got, testVersion100)
	}
	if got := f.server.Count(fakegalaxy.EndpointVersionsList); got != 0 {
		t.Errorf("EndpointVersionsList count = %d, want 0 (frozen resolution never consults the versions listing)", got)
	}

	data, err := os.ReadFile(f.cfg.MetricsFile)
	if err != nil {
		t.Fatalf("read metrics file %s: %v", f.cfg.MetricsFile, err)
	}
	// Unmarshal into a map, not metrics.Report, for the same reason
	// TestArtifactMetricsWrittenToMetricsFile above does: the literal wire
	// key is what a CI dashboard actually parses.
	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("unmarshal metrics file %s: %v", f.cfg.MetricsFile, err)
	}
	if got, _ := written["frozen"].(bool); !got {
		t.Errorf("metrics frozen = %v, want true (a successful frozen install honors the lockfile)", written["frozen"])
	}
}

// assertFrozenCorruptedPinFailsClosed corrupts acme.app's pin in lf, saves it
// at lockPath, and asserts the resulting frozen install fails the whole run
// rather than installing bytes that no longer match what the lockfile names.
func assertFrozenCorruptedPinFailsClosed(t *testing.T, f *e2eFixture, lockPath string, lf *lockfile.File) {
	t.Helper()
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath before the corrupted-pin run: %v", err)
	}
	setAppPin(lf, corruptedAppSHA256)
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save corrupted lockfile: %v", err)
	}

	err := collections.Start(context.Background(), f.cfg, f.runtime)
	if err == nil {
		t.Fatal("expected an error from a corrupted lockfile pin, got nil")
	}
	// failureSummary.wrap joins each collection's cause behind
	// ErrInstallationFailed, so ErrSHA256Mismatch stays reachable and exitcode
	// maps the run to the integrity code rather than the install one.
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is ErrSHA256Mismatch, got %v", err)
	}
	assertPathAbsent(t, installPathFor(f.downloadPath, "app"))
}

// TestFrozenInstallRejectsWildcardLockfilePin pins that install --frozen
// refuses a "*" lockfile pin with ErrLockfileInvalid and exit 6, creating no
// manifest or glob-named ".info" directory; the subtests share one lockfile.
func TestFrozenInstallRejectsWildcardLockfilePin(t *testing.T) {
	f := newE2EFixture(t)
	f.server.AddVersion("acme", "app", "2.0.0", nil)
	lockPath := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	f.cfg.Frozen = true
	wildcardInfoDir := filepath.Join(f.downloadPath, "ansible_collections", "acme.app-*.info")

	t.Run("wildcard pin fails closed and creates nothing", func(t *testing.T) {
		lf := &lockfile.File{
			SchemaVersion: lockfile.SchemaVersion,
			Server:        f.cfg.Server,
			Collections:   []lockfile.Entry{{Name: "acme.app", Version: "*", Source: f.cfg.Server}},
		}
		if err := lockfile.Save(lockPath, lf); err != nil {
			t.Fatalf("save wildcard-pinned lockfile: %v", err)
		}

		err := collections.Start(context.Background(), f.cfg, f.runtime)
		if err == nil {
			t.Fatal("expected an error from a wildcard-pinned lockfile under --frozen, got nil")
		}
		if !errors.Is(err, helpers.ErrLockfileInvalid) {
			t.Fatalf("Start error = %v, want errors.Is helpers.ErrLockfileInvalid", err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitLock {
			t.Errorf("exitcode.FromError(err) = %d, want %d", got, exitcode.ExitLock)
		}
		assertPathAbsent(t, manifestPathFor(f.downloadPath, "app"))
		// The glob-shaped sidecar an unguarded binary would create from the
		// literal "*" version text - see newInstallTarget's ".info" naming.
		assertPathAbsent(t, wildcardInfoDir)
	})

	t.Run("exact pin on the identical fixture installs cleanly", func(t *testing.T) {
		lf := &lockfile.File{
			SchemaVersion: lockfile.SchemaVersion,
			Server:        f.cfg.Server,
			Collections: []lockfile.Entry{
				{Name: "acme.app", Version: testVersion100, Source: f.cfg.Server, SHA256: f.appV1.SHA256},
			},
		}
		if err := lockfile.Save(lockPath, lf); err != nil {
			t.Fatalf("save exact-pinned lockfile: %v", err)
		}

		if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("Start: %v", err)
		}
		assertManifestInstalled(t, f.downloadPath, "app")
		if got := readManifestVersion(t, f.downloadPath, "app"); got != testVersion100 {
			t.Errorf("installed acme.app collection_info.version = %q, want %q", got, testVersion100)
		}
	})
}

// warnCapturingPrinter is a no-op output.Printer that records Warnf lines;
// the internal package's capturingPrinter is unreachable from this package.
type warnCapturingPrinter struct {
	noopPrinter

	warns []string
}

func (p *warnCapturingPrinter) Warnf(format string, args ...any) {
	p.warns = append(p.warns, fmt.Sprintf(format, args...))
}

// hasWarnContaining reports whether any recorded Warnf line contains substr.
func (p *warnCapturingPrinter) hasWarnContaining(substr string) bool {
	for _, line := range p.warns {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// installOnceAndPublishNewerVersion installs acme.app@1.0.0 cold, publishes
// 2.0.0 with the same dependency, wipes the download path and resets the
// server counts, leaving a warm cache whose snapshot still names 1.0.0.
func installOnceAndPublishNewerVersion(t *testing.T) *e2eFixture {
	t.Helper()
	f := newE2EFixture(t)
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (populate the cache): %v", err)
	}
	f.server.AddVersion("acme", "app", "2.0.0", map[string]string{"acme.lib": ">=1.0.0"})
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath before the refresh run: %v", err)
	}
	f.server.ResetCounts()
	return f
}

// TestRefreshReSolvesInsteadOfReplayingTheSnapshot pins that without
// --refresh a warm rerun replays the snapshot with no HTTP, and with it the
// run re-resolves and installs the newly published 2.0.0.
func TestRefreshReSolvesInsteadOfReplayingTheSnapshot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		wantVersion string
		refresh     bool
		wantNetwork bool
	}{
		{name: "without refresh: replays the snapshot, no network", wantVersion: testVersion100, refresh: false, wantNetwork: false},
		{name: "with refresh: re-resolves, picks up the new version", wantVersion: "2.0.0", refresh: true, wantNetwork: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := installOnceAndPublishNewerVersion(t)
			f.cfg.Refresh = tc.refresh

			if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
				t.Fatalf("second Start (refresh=%v): %v", tc.refresh, err)
			}

			if got := readManifestVersion(t, f.downloadPath, "app"); got != tc.wantVersion {
				t.Errorf("installed acme.app collection_info.version = %q, want %q", got, tc.wantVersion)
			}
			gotNetwork := f.server.Total() > 0
			if gotNetwork != tc.wantNetwork {
				t.Errorf("server.Total() > 0 = %v, want %v (Total() = %d)", gotNetwork, tc.wantNetwork, f.server.Total())
			}
		})
	}
}

// TestRefreshDoesNotRedownloadCachedArtifacts pins that --refresh re-fetches
// root metadata but never re-downloads an artifact already cached for the
// version it resolves to: isCacheHit must not consult cfg.Refresh.
func TestRefreshDoesNotRedownloadCachedArtifacts(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (populate the cache): %v", err)
	}
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath before the refresh run: %v", err)
	}
	f.server.ResetCounts()
	f.cfg.Refresh = true

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Start (refresh): %v", err)
	}

	if got := f.server.Count(fakegalaxy.EndpointRootMetadata); got == 0 {
		t.Errorf("EndpointRootMetadata count = %d, want > 0 (refresh must bypass the version-free cached answer)", got)
	}
	if got := f.server.Count(fakegalaxy.EndpointArtifact); got != 0 {
		t.Errorf("EndpointArtifact count = %d, want 0 (a cached artifact must not be re-downloaded)", got)
	}
	if got := readManifestVersion(t, f.downloadPath, "app"); got != testVersion100 {
		t.Errorf("installed acme.app collection_info.version = %q, want %q", got, testVersion100)
	}
}

// TestOfflineOutranksRefresh pins that --offline wins over --refresh: no HTTP,
// the snapshot's 1.0.0 installed and a skipped-flag warning; the snapshot veto
// itself is pinned by TestRefreshOfflinePreservesResolveWithStaleMetadataCaches.
func TestOfflineOutranksRefresh(t *testing.T) {
	t.Parallel()
	f := installOnceAndPublishNewerVersion(t)
	f.cfg.Refresh = true
	f.cfg.Offline = true
	printer := &warnCapturingPrinter{}
	runtime := infra.New(printer, f.server.Client())

	if err := collections.Start(context.Background(), f.cfg, runtime); err != nil {
		t.Fatalf("Start (refresh + offline): %v", err)
	}

	if got := f.server.Total(); got != 0 {
		t.Errorf("server.Total() = %d, want 0 (offline must never reach the network)", got)
	}
	if got := readManifestVersion(t, f.downloadPath, "app"); got != testVersion100 {
		t.Errorf("installed acme.app collection_info.version = %q, want %q", got, testVersion100)
	}
	if !printer.hasWarnContaining("--offline: skipping --refresh") {
		t.Errorf("expected a --refresh-skipped warning on stderr, got %v", printer.warns)
	}
}

// TestOfflineInstall asserts --offline refuses to reach the network on a
// cold cache, and installs entirely from a warm cache with the network
// transport hard-disabled.
func TestOfflineInstall(t *testing.T) {
	t.Parallel()

	t.Run("cold cache rejects the network", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)
		f.cfg.Offline = true
		f.runtime = infra.New(noopPrinter{}, fetch.NewOffline(f.cfg.Timeout))

		err := collections.Start(context.Background(), f.cfg, f.runtime)
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

	t.Run("warm cache installs with the network hard disabled", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)

		if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("first Start (populate the cache online): %v", err)
		}

		f.server.ResetCounts()
		f.runtime.HTTP = fetch.NewOffline(f.cfg.Timeout)
		f.cfg.Offline = true
		if err := os.RemoveAll(f.downloadPath); err != nil {
			t.Fatalf("remove downloadPath to force a reinstall: %v", err)
		}

		if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("second Start (offline warm reinstall): %v", err)
		}

		assertManifestInstalled(t, f.downloadPath, "app")
		assertManifestInstalled(t, f.downloadPath, "lib")
		if got := f.server.Total(); got != 0 {
			t.Errorf("Total() after the offline warm reinstall = %d, want 0", got)
		}
	})
}

// TestNoDepsUnpinnedInstallsResolvedVersion pins that --no-deps resolves a
// "*" root to the highest version, which then names the installed manifest,
// the artifact cache key and the lockfile entry.
func TestNoDepsUnpinnedInstallsResolvedVersion(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme.solo")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "solo", testVersion100, nil)
	s.AddVersion("acme", "solo", "2.0.0", nil)

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
		NoDeps:           true,
	}
	runtime := infra.New(noopPrinter{}, s.Client())

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := readManifestVersion(t, downloadPath, "solo"); got != "2.0.0" {
		t.Errorf("installed acme.solo collection_info.version = %q, want %q (the highest registered)", got, "2.0.0")
	}

	assertNoWildcardArtifactFilenames(t, cacheDir)
	assertArtifactFilePresent(t, cacheDir, s.URL(), "acme-solo-2.0.0.tar.gz")

	if err := collections.Lock(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	lockPath := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatalf("load lockfile: %v", err)
	}
	entry := findLockEntry(t, lf, "acme.solo")
	if entry.Version != "2.0.0" {
		t.Errorf("lockfile entry version = %q, want %q, not the literal %q constraint", entry.Version, "2.0.0", "*")
	}
}

// assertNoWildcardArtifactFilenames fails if any file under cacheDir carries a
// literal or escaped "*", the sign of an unresolved version in an artifact key.
func assertNoWildcardArtifactFilenames(t *testing.T, cacheDir string) {
	t.Helper()
	err := filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.Contains(name, "%2A") || strings.Contains(name, "*") {
			t.Errorf("found wildcard artifact filename %q under %s", name, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cacheDir %s: %v", cacheDir, err)
	}
}

// assertArtifactFilePresent fails the test unless a cache entry for filename,
// scoped to source (the server the collection resolved from - see
// helpers.ArtifactKey), exists directly under cacheDir.
func assertArtifactFilePresent(t *testing.T, cacheDir, source, filename string) {
	t.Helper()
	path := filepath.Join(cacheDir, helpers.ArtifactKey(source, filename))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected cached artifact %s to exist, stat error: %v", path, err)
	}
}

// findLockEntry returns the lockfile.Entry named name from lf, failing the
// test if no such entry exists.
func findLockEntry(t *testing.T, lf *lockfile.File, name string) lockfile.Entry {
	t.Helper()
	for _, e := range lf.Collections {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("lockfile has no entry named %q", name)
	return lockfile.Entry{}
}

// writeRequirementsMulti writes a requirements.yml listing every name at
// version "*", to change the requirements between two runs sharing a cache.
func writeRequirementsMulti(t *testing.T, path string, names ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("collections:\n")
	for _, name := range names {
		b.WriteString("  - name: ")
		b.WriteString(name)
		b.WriteString("\n    version: \"*\"\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
}

// TestNoDepsSnapshotNotReusedByDepsRun pins that a --no-deps snapshot is not
// replayed by a later deps-following run: the mode is part of RequirementsHash,
// so acme.lib is resolved rather than silently skipped.
func TestNoDepsSnapshotNotReusedByDepsRun(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.cfg.NoDeps = true

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (--no-deps, populate the snapshot): %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
	assertPathAbsent(t, installPathFor(f.downloadPath, "lib"))

	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath before the deps-following run: %v", err)
	}
	f.cfg.NoDeps = false

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Start (deps-following, must not reuse the --no-deps snapshot): %v", err)
	}

	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
}

// TestArtifactMetricsColdInstallCountsMisses pins that a cold install counts
// one miss per downloaded artifact, no hits, and a positive byte count.
func TestArtifactMetricsColdInstallCountsMisses(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	totals := f.runtime.Metrics.Totals()
	if totals.CacheMisses != 2 {
		t.Errorf("CacheMisses = %d, want 2 (acme.app and acme.lib, both fetched from the origin)", totals.CacheMisses)
	}
	if totals.CacheHits != 0 {
		t.Errorf("CacheHits = %d, want 0 (a cold cache serves no hits)", totals.CacheHits)
	}
	if totals.BytesDownloaded <= 0 {
		t.Errorf("BytesDownloaded = %d, want > 0 (both artifacts were actually read off the network)", totals.BytesDownloaded)
	}
}

// TestArtifactMetricsWrittenToMetricsFile pins the metrics file's wire keys
// against the in-process counters; a cold install is required because only
// there do cache_hits (0) and cache_misses (2) differ, exposing a swap.
func TestArtifactMetricsWrittenToMetricsFile(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	// Set for this test only: the other counter tests read no metrics file.
	f.cfg.MetricsFile = filepath.Join(t.TempDir(), "metrics.json")

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	data, err := os.ReadFile(f.cfg.MetricsFile)
	if err != nil {
		t.Fatalf("read metrics file %s: %v", f.cfg.MetricsFile, err)
	}
	// A map, not metrics.Report: decoding through the same json tags would hide
	// a renamed or swapped tag; the literal keys are the on-disk contract.
	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("unmarshal metrics file %s: %v", f.cfg.MetricsFile, err)
	}

	totals := f.runtime.Metrics.Totals()
	// Each check compares the file with Totals (a stale read) and with an
	// absolute value (a swapped mapping, which Totals alone would mirror).
	assertMetricCounter(t, written, "cache_hits", totals.CacheHits, 0)
	assertMetricCounter(t, written, "cache_misses", totals.CacheMisses, 2)

	gotBytes := metricFloat(t, written, "bytes_downloaded")
	if int64(gotBytes) != totals.BytesDownloaded {
		t.Errorf("metrics file bytes_downloaded = %v, want %d (runtime.Metrics.Totals().BytesDownloaded)", gotBytes, totals.BytesDownloaded)
	}
	if gotBytes <= 0 {
		t.Errorf("metrics file bytes_downloaded = %v, want > 0 (both artifacts were actually read off the network)", gotBytes)
	}
}

// metricFloat returns the JSON number at key, failing if it is missing or not
// a number; the counts here are far below 2^53, so float64 is exact.
func metricFloat(t *testing.T, written map[string]any, key string) float64 {
	t.Helper()
	v, ok := written[key].(float64)
	if !ok {
		t.Fatalf("metrics file %s = %v (%T), want a JSON number", key, written[key], written[key])
	}
	return v
}

// assertMetricCounter cross-checks the JSON metrics file's value at key
// against both the in-process totals value (a stale-read/wrong-runtime
// check) and its expected absolute value (a swapped-field-mapping check).
func assertMetricCounter(t *testing.T, written map[string]any, key string, wantTotals, wantAbsolute int64) {
	t.Helper()
	got := int64(metricFloat(t, written, key))
	if got != wantTotals {
		t.Errorf("metrics file %s = %d, want %d (runtime.Metrics.Totals())", key, got, wantTotals)
	}
	if got != wantAbsolute {
		t.Errorf("metrics file %s = %d, want %d", key, got, wantAbsolute)
	}
}

// TestArtifactMetricsWarmInstallCountsHits pins that a warm reinstall counts
// one hit per cached artifact, no misses, and zero bytes downloaded.
func TestArtifactMetricsWarmInstallCountsHits(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (populate the cache): %v", err)
	}

	f.server.ResetCounts()
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath to force a reinstall from cache: %v", err)
	}
	// Rebuild the runtime so its Metrics starts fresh for the warm reinstall:
	// there is no Reset method, and reusing f.runtime across two Start calls
	// would let the cold run's misses bleed into this run's totals.
	f.runtime = infra.New(noopPrinter{}, f.server.Client())

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Start (warm cache): %v", err)
	}

	totals := f.runtime.Metrics.Totals()
	if totals.CacheHits != 2 {
		t.Errorf("CacheHits = %d, want 2 (acme.app and acme.lib, both served from the artifact cache)", totals.CacheHits)
	}
	if totals.CacheMisses != 0 {
		t.Errorf("CacheMisses = %d, want 0 (a warm reinstall never reaches the origin)", totals.CacheMisses)
	}
	if totals.BytesDownloaded != 0 {
		t.Errorf("BytesDownloaded = %d, want 0 (a cache hit reads no bytes from a Galaxy origin's artifact response body)",
			totals.BytesDownloaded)
	}
}

// TestArtifactMetricsPrefetchHandoffCountedOnce pins that a consumed prefetch
// handoff adds no miss of its own and no hit: misses equal artifact requests,
// and zero hits proves the handed-off bytes, not a cache read, were installed.
func TestArtifactMetricsPrefetchHandoffCountedOnce(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	wantMisses := int64(f.server.Count(fakegalaxy.EndpointArtifact))
	totals := f.runtime.Metrics.Totals()
	if totals.CacheMisses != wantMisses {
		t.Errorf("CacheMisses = %d, want %d (one per EndpointArtifact request; the prefetch handoff must not add a second one)",
			totals.CacheMisses, wantMisses)
	}
	if totals.CacheHits != 0 {
		t.Errorf("CacheHits = %d, want 0 (a consumed prefetch handoff never calls ArtifactStore.Fetch, so it can never register a hit)",
			totals.CacheHits)
	}
}

// TestArtifactMetricsLockCountsNothing pins that Lock leaves all three
// artifact counters at zero: it never touches an ArtifactStore.
func TestArtifactMetricsLockCountsNothing(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	totals := f.runtime.Metrics.Totals()
	if totals.CacheHits != 0 {
		t.Errorf("CacheHits = %d, want 0 (Lock never touches an ArtifactStore)", totals.CacheHits)
	}
	if totals.CacheMisses != 0 {
		t.Errorf("CacheMisses = %d, want 0 (Lock never touches an ArtifactStore)", totals.CacheMisses)
	}
	if totals.BytesDownloaded != 0 {
		t.Errorf("BytesDownloaded = %d, want 0 (Lock never touches an ArtifactStore)", totals.BytesDownloaded)
	}
}

// TestNoDepsSnapshotNotReusedIncrementally pins that adding a root after a
// --no-deps run cannot make tryIncrementalResolve keep acme.app's nil-deps
// entry, so acme.lib is resolved rather than silently skipped.
func TestNoDepsSnapshotNotReusedIncrementally(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.server.AddVersion("acme", "tool", testVersion100, nil)
	f.cfg.NoDeps = true

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (--no-deps, populate the snapshot): %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
	assertPathAbsent(t, installPathFor(f.downloadPath, "lib"))

	writeRequirementsMulti(t, f.cfg.RequirementsFile, "acme.app", "acme.tool")
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath before the deps-following run: %v", err)
	}
	f.cfg.NoDeps = false

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Start (deps-following, must not incrementally reuse the --no-deps snapshot): %v", err)
	}

	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
	assertManifestInstalled(t, f.downloadPath, "tool")
}

// assertInstalledProvenance asserts acme.<name>-<version>.info/GALAXY.yml names
// server and lacks provenanceLine's key (ansible discards a document with
// unknown keys), while go-galaxy.yml beside it holds exactly that line.
func assertInstalledProvenance(t *testing.T, downloadPath, name, version, server, provenanceLine string) {
	t.Helper()
	infoDir := filepath.Join(downloadPath, "ansible_collections", "acme."+name+"-"+version+".info")
	data, err := os.ReadFile(filepath.Join(infoDir, "GALAXY.yml")) //nolint:gosec // path is built from this test's own temp dirs.
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	key, _, _ := strings.Cut(provenanceLine, ":")
	if !strings.Contains(string(data), "server: "+server) || strings.Contains(string(data), key) {
		t.Fatalf("sidecar does not name %s, or carries %s outside ansible's schema:\n%s", server, key, data)
	}
	provenance, err := os.ReadFile(filepath.Join(infoDir, "go-galaxy.yml")) //nolint:gosec // path is built from this test's own temp dirs.
	if err != nil || string(provenance) != provenanceLine+"\n" {
		t.Fatalf("go-galaxy.yml = %q (%v), want %q", provenance, err, provenanceLine+"\n")
	}
}
