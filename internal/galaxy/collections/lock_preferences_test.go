package collections

// This file pins what lock keeps from the galaxy.lock it reads: the locked
// Galaxy version on any cache unless --refresh sets it aside, a pin no longer
// published resolved anew with one warning, and a file that does not load.

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

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// unpublishedWidgetsWarning is the one warning a lock run prints when
// galaxy.lock pins acme.widgets at 0.9.0, which the fake server never serves.
const unpublishedWidgetsWarning = "Locked acme.widgets 0.9.0 is no longer published; resolved 1.0.0 instead"

// newListedLockRun is newLockRun with its server in cfg.Servers, as
// resolveServers lists it in production, so no run here warns that a locked
// source matches no configured server.
func newListedLockRun(t *testing.T) lockRun {
	t.Helper()
	f := newLockRun(t)
	f.cfg.Servers = []config.Server{{URL: f.cfg.Server}}
	return f
}

// seedLockThenPublish locks f's requirements (acme.widgets 1.0.0), then
// publishes acme.widgets 2.0.0, and returns the lockfile path and bytes.
func seedLockThenPublish(t *testing.T, f lockRun) (string, []byte) {
	t.Helper()
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	before := mustReadFile(t, path)
	f.server.AddVersion("acme", "widgets", testVersion200, nil)
	return path, before
}

// goCold points f at a new, empty cache directory and resets the server's
// request counts, so the next run has no recorded resolution to replay.
func goCold(t *testing.T, f lockRun) {
	t.Helper()
	f.cfg.CacheDir = filepath.Join(t.TempDir(), "cache")
	f.server.ResetCounts()
}

// assertFileUnchanged fails unless the file at path still holds before.
func assertFileUnchanged(t *testing.T, path string, before []byte) {
	t.Helper()
	if after := mustReadFile(t, path); string(after) != string(before) {
		t.Fatalf("lockfile changed:\n--- before\n%s--- after\n%s", before, after)
	}
}

// assertLockedWidgets fails unless the lockfile at path loads and pins
// acme.widgets alone, at version.
func assertLockedWidgets(t *testing.T, path, version string) {
	t.Helper()
	lf, err := lockfile.Load(path)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", path, err)
	}
	assertSingleWidgetsEntry(t, lf, version)
}

// warnsEqual counts the recorded Warnf lines equal to want.
func (p *capturingPrinter) warnsEqual(want string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, line := range p.warns {
		if line == want {
			count++
		}
	}
	return count
}

// warnsContaining counts the recorded Warnf lines containing substr.
func (p *capturingPrinter) warnsContaining(substr string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, line := range p.warns {
		if strings.Contains(line, substr) {
			count++
		}
	}
	return count
}

// TestLockCheckOnAColdCacheKeepsTheLockedVersion pins that --check on an empty
// cache after a newer upstream release prefers galaxy.lock's version: up to
// date, with two requests (its root and version documents) and the file intact.
func TestLockCheckOnAColdCacheKeepsTheLockedVersion(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, before := seedLockThenPublish(t, f)
	goCold(t, f)
	f.cfg.Check = true

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("check Lock on a cold cache: %v", err)
	}
	if !f.printer.hasPersistentPrintContaining("Check: lockfile is up to date") {
		t.Fatalf("persists = %v", f.printer.persists)
	}
	rootDocs := f.server.Count(fakegalaxy.EndpointRootMetadata)
	versionDocs := f.server.Count(fakegalaxy.EndpointVersionDetail)
	if total := f.server.Total(); rootDocs != 1 || versionDocs != 1 || total != 2 {
		t.Errorf("requests: root %d, version %d, total %d; want 1, 1 and 2", rootDocs, versionDocs, total)
	}
	assertFileUnchanged(t, path, before)
}

// TestLockOnAColdCacheKeepsTheLockedVersion pins that a plain lock on an empty
// cache after a newer upstream release rewrites galaxy.lock byte for byte,
// keeping acme.widgets at 1.0.0, and warns about nothing.
func TestLockOnAColdCacheKeepsTheLockedVersion(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, before := seedLockThenPublish(t, f)
	goCold(t, f)

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock on a cold cache: %v", err)
	}
	assertFileUnchanged(t, path, before)
	if len(f.printer.warns) != 0 {
		t.Errorf("warns = %v, want none", f.printer.warns)
	}
}

// TestLockRefreshOnAColdCacheTakesTheNewerVersion is the control: --refresh
// sets galaxy.lock's pins aside, so --check reports the newer release as drift
// and a plain run writes it.
func TestLockRefreshOnAColdCacheTakesTheNewerVersion(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, _ := seedLockThenPublish(t, f)
	goCold(t, f)
	f.cfg.Refresh = true
	f.cfg.Check = true

	err := Lock(context.Background(), f.cfg, f.runtime)
	if !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("check Lock --refresh error = %v, want errors.Is helpers.ErrLockfileDrift", err)
	}
	if !f.printer.hasOkContaining("version 1.0.0 -> 2.0.0") {
		t.Fatalf("oks = %v", f.printer.oks)
	}

	f.cfg.Check = false
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock --refresh: %v", err)
	}
	assertLockedWidgets(t, path, testVersion200)
}

// TestLockNoCacheKeepsTheLockedVersion pins that --no-cache, which reads no
// recorded resolution, still keeps galaxy.lock's version: the file is no cache.
func TestLockNoCacheKeepsTheLockedVersion(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, before := seedLockThenPublish(t, f)
	f.cfg.NoCache = true

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock --no-cache: %v", err)
	}
	assertFileUnchanged(t, path, before)
}

// TestLockTakesTheNewestAllowedPastATightenedConstraint pins that a pin the
// requirements no longer allow gives way to the newest version they do, with
// no warning: the change is intended, and the file shows it.
func TestLockTakesTheNewestAllowedPastATightenedConstraint(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, _ := seedLockThenPublish(t, f)
	f.server.AddVersion("acme", "widgets", "1.5.0", nil)
	f.server.AddVersion("acme", "widgets", "1.7.0", nil)
	mustWriteFile(t, f.cfg.RequirementsFile, []byte("collections:\n  - name: acme.widgets\n    version: \">=1.1.0,<2.0.0\"\n"))
	goCold(t, f)

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	assertLockedWidgets(t, path, "1.7.0")
	if len(f.printer.warns) != 0 {
		t.Errorf("warns = %v, want none", f.printer.warns)
	}
}

// saveWidgetsPin writes a lockfile pinning acme.widgets at 0.9.0, which the
// fake server never serves, to the default path, shaped as lock writes one,
// and returns the path.
func saveWidgetsPin(t *testing.T, f lockRun) string {
	t.Helper()
	const version = "0.9.0"
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	lf := &lockfile.File{
		Server: f.cfg.Server,
		Collections: []lockfile.Entry{{
			Name: "acme.widgets", Version: version, Source: f.cfg.Server,
			DownloadURL: lockedDownloadURLFor(f.cfg.Server, "acme.widgets", version),
		}},
	}
	if err := lockfile.Save(path, lf); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
	return path
}

// TestLockResolvesAnUnpublishedPinAnewWithOneWarning pins that a locked
// version its server no longer serves is resolved anew with one warning naming
// it, by plain lock and by --check, which then reports the drift.
func TestLockResolvesAnUnpublishedPinAnewWithOneWarning(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path := saveWidgetsPin(t, f)

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	assertLockedWidgets(t, path, testVersion100)
	if got := f.printer.warnsEqual(unpublishedWidgetsWarning); got != 1 || len(f.printer.warns) != 1 {
		t.Fatalf("warns = %v, want exactly %q", f.printer.warns, unpublishedWidgetsWarning)
	}

	saveWidgetsPin(t, f)
	printer := &capturingPrinter{}
	f.cfg.Check = true
	err := Lock(context.Background(), f.cfg, infra.New(printer, f.server.Client()))
	if !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("check Lock error = %v, want errors.Is helpers.ErrLockfileDrift", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitLock {
		t.Errorf("exitcode.FromError(err) = %d, want %d", got, exitcode.ExitLock)
	}
	if got := printer.warnsEqual(unpublishedWidgetsWarning); got != 1 || len(printer.warns) != 1 {
		t.Fatalf("check warns = %v, want exactly %q", printer.warns, unpublishedWidgetsWarning)
	}
}

// recordedVersion returns the version the snapshot in cacheDir records for
// fqdn's resolution, "" when it records none.
func recordedVersion(t *testing.T, cacheDir, fqdn string) string {
	t.Helper()
	ctx := context.Background()
	backend := local.New(cacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()
	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return st.ResolvedSnapshot()[fqdn].Version
}

// TestLockPrefersTheLockfileOverADisagreeingRecordedResolution pins that a
// resolution install --refresh recorded after a newer release loses to
// galaxy.lock: --check passes, asking nothing of a warm cache, and so does lock.
func TestLockPrefersTheLockfileOverADisagreeingRecordedResolution(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, before := seedLockThenPublish(t, f)
	f.cfg.Refresh = true
	if err := Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("install --refresh: %v", err)
	}
	f.cfg.Refresh = false
	if got := recordedVersion(t, f.cfg.CacheDir, "acme.widgets"); got != testVersion200 {
		t.Fatalf("recorded acme.widgets = %q, want 2.0.0 (the premise)", got)
	}

	f.cfg.Check = true
	f.server.ResetCounts()
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("check Lock: %v", err)
	}
	if !f.printer.hasPersistentPrintContaining("Check: lockfile is up to date") {
		t.Fatalf("persists = %v", f.printer.persists)
	}
	if got := f.server.Total(); got != 0 {
		t.Errorf("server.Total() = %d, want 0: the locked version's documents are cached", got)
	}
	f.cfg.Check = false
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	assertFileUnchanged(t, path, before)
}

// TestLockPrefersTheLockfileOverADisagreeingIncrementalReplay pins the same
// for a root added after install --refresh: the replayed part of the merge
// moved acme.widgets, so lock drops it and keeps the locked 1.0.0.
func TestLockPrefersTheLockfileOverADisagreeingIncrementalReplay(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, _ := seedLockThenPublish(t, f)
	f.cfg.Refresh = true
	if err := Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("install --refresh: %v", err)
	}
	f.cfg.Refresh = false
	f.server.AddVersion("acme", "extra", testVersion100, nil)
	mustWriteFile(t, f.cfg.RequirementsFile,
		[]byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n  - name: acme.extra\n    version: \"*\"\n"))

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	lf, err := lockfile.Load(path)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", path, err)
	}
	got := make(map[string]string, len(lf.Collections))
	for _, e := range lf.Collections {
		got[e.Name] = e.Version
	}
	if len(got) != 2 || got["acme.widgets"] != testVersion100 || got["acme.extra"] != testVersion100 {
		t.Fatalf("locked %v, want acme.widgets and acme.extra at 1.0.0", got)
	}
}

// TestLockReplaysAnAgreeingResolutionWithNoRequest pins that a plain lock
// whose recorded resolution agrees with galaxy.lock replays it, asking the
// server nothing although a newer release exists.
func TestLockReplaysAnAgreeingResolutionWithNoRequest(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, before := seedLockThenPublish(t, f)
	f.server.ResetCounts()

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if got := f.server.Total(); got != 0 {
		t.Errorf("server.Total() = %d, want 0", got)
	}
	assertFileUnchanged(t, path, before)
}

// TestLockNoDepsKeepsRootPins pins that --no-deps on an empty cache keeps a
// root at its locked version: the solver reaches the preference through
// noDepsProvider.
func TestLockNoDepsKeepsRootPins(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, before := seedLockThenPublish(t, f)
	goCold(t, f)
	f.cfg.NoDeps = true

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock --no-deps: %v", err)
	}
	assertFileUnchanged(t, path, before)
}

// TestLockOfflineFailsWithoutTheLockedVersionDocuments pins that --offline
// over a cache holding the newer release's documents but not the locked
// version's fails with ErrOfflineMode, exit 4, and leaves galaxy.lock alone.
func TestLockOfflineFailsWithoutTheLockedVersionDocuments(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, before := seedLockThenPublish(t, f)
	goCold(t, f)
	f.cfg.LockFile = filepath.Join(f.root, "refreshed.lock")
	f.cfg.Refresh = true
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock --refresh into another file: %v", err)
	}
	assertLockedWidgets(t, f.cfg.LockFile, testVersion200)

	f.cfg.LockFile = ""
	f.cfg.Refresh = false
	f.cfg.Offline = true
	offline := infra.New(&capturingPrinter{}, fetch.NewOffline(f.cfg.Timeout))
	err := Lock(context.Background(), f.cfg, offline)
	if !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("Lock --offline error = %v, want errors.Is helpers.ErrOfflineMode", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitNetwork {
		t.Errorf("exitcode.FromError(err) = %d, want %d", got, exitcode.ExitNetwork)
	}
	assertFileUnchanged(t, path, before)
}

// oldSchemaWidgetsLockfile is a schema 4 file pinning acme.widgets 1.0.0 with
// no download_url, the shape an older release wrote and Load refuses.
func oldSchemaWidgetsLockfile(server string) []byte {
	return fmt.Appendf(nil, "schema_version: 4\ncollections:\n  - name: acme.widgets\n    version: 1.0.0\n    source: %s\n", server)
}

// TestLockWarnsOnceAndResolvesWithoutPinsOverAnUnloadableLockfile pins the
// repair an older file's refusal names: a plain lock warns once, keeps none of
// its pins (so 2.0.0 is taken) and writes a file that loads.
func TestLockWarnsOnceAndResolvesWithoutPinsOverAnUnloadableLockfile(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	f.server.AddVersion("acme", "widgets", testVersion200, nil)
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	mustWriteFile(t, path, oldSchemaWidgetsLockfile(f.cfg.Server))

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if got := f.printer.warnsContaining("cannot be read"); got != 1 || len(f.printer.warns) != 1 {
		t.Fatalf("warns = %v, want exactly one saying the lockfile cannot be read", f.printer.warns)
	}
	if !f.printer.hasWarnContaining("; resolving without its pins") {
		t.Fatalf("warns = %v, want one saying the run resolves without the file's pins", f.printer.warns)
	}
	assertLockedWidgets(t, path, testVersion200)
}

// TestLockDryRunWarnsOnceOverAnUnloadableLockfile pins that --dry-run warns
// once about a file that does not load, saying the run resolves without its
// pins and reports every entry as added.
func TestLockDryRunWarnsOnceOverAnUnloadableLockfile(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	f.cfg.DryRun = true
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	mustWriteFile(t, path, oldSchemaWidgetsLockfile(f.cfg.Server))

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock --dry-run: %v", err)
	}
	if got := f.printer.warnsContaining("cannot be read"); got != 1 {
		t.Fatalf("warns = %v, want exactly one saying the lockfile cannot be read", f.printer.warns)
	}
	if !f.printer.hasWarnContaining("; resolving without its pins and reporting every entry as added") {
		t.Fatalf("warns = %v", f.printer.warns)
	}
	if !f.printer.hasOkContaining("Would add: acme.widgets@1.0.0") {
		t.Fatalf("oks = %v", f.printer.oks)
	}
}

// TestLockCheckFailsOnAnUnloadableLockfileAfterResolving pins that --check,
// --dry-run beside it or not, resolves first and then fails with the error
// Load gave, exit 6, warning nothing about the file: the failure names it.
func TestLockCheckFailsOnAnUnloadableLockfileAfterResolving(t *testing.T) {
	t.Parallel()
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry-run %t", dryRun), func(t *testing.T) {
			t.Parallel()
			f := newListedLockRun(t)
			f.cfg.Check, f.cfg.DryRun = true, dryRun
			path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
			mustWriteFile(t, path, oldSchemaWidgetsLockfile(f.cfg.Server))
			before := mustReadFile(t, path)

			err := Lock(context.Background(), f.cfg, f.runtime)
			if !errors.Is(err, helpers.ErrLockfileInvalid) {
				t.Fatalf("check Lock error = %v, want errors.Is helpers.ErrLockfileInvalid", err)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitLock {
				t.Errorf("exitcode.FromError(err) = %d, want %d", got, exitcode.ExitLock)
			}
			if f.printer.hasWarnContaining("cannot be read") {
				t.Errorf("warns = %v, want no warning about the file under --check", f.printer.warns)
			}
			if got := f.server.Count(fakegalaxy.EndpointRootMetadata); got != 1 {
				t.Errorf("root metadata requests = %d, want 1: --check resolves before it judges the file", got)
			}
			assertFileUnchanged(t, path, before)
		})
	}
}

// TestLockPreferencesNilPrefersNothing pins the nil index every command but
// lock carries: no pin, every resolution agrees, and nothing is recorded.
func TestLockPreferencesNilPrefersNothing(t *testing.T) {
	t.Parallel()
	var prefs *lockPreferences
	if version, ok := prefs.galaxyVersion("acme.widgets"); ok || version != "" {
		t.Errorf("galaxyVersion = %q, %v; want \"\", false", version, ok)
	}
	resolved := map[string]collection{"acme.widgets": {Namespace: "acme", Name: "widgets", Version: testVersion200}}
	if !prefs.agreesWith(nil, resolved) {
		t.Error("agreesWith = false, want true")
	}
	prefs.recordUnpublished("acme.widgets")
	if prefs.isUnpublished("acme.widgets") {
		t.Error("isUnpublished = true after recording on nil, want false")
	}
	printer := &capturingPrinter{}
	prefs.warnUnpublished(printer, resolved)
	if len(printer.warns) != 0 {
		t.Errorf("warns = %v, want none", printer.warns)
	}
}

// mixedLockfile pins acme.galaxy as a Galaxy entry, acme.git as a git entry
// and acme.url as a url entry, each at 1.0.0.
func mixedLockfile() *lockfile.File {
	return &lockfile.File{Collections: []lockfile.Entry{
		{Name: "acme.galaxy", Version: testVersion100},
		{Name: "acme.git", Type: lockfile.TypeGit, Version: testVersion100},
		{Name: "acme.url", Type: lockfile.TypeURL, Version: testVersion100},
	}}
}

// TestNewLockPreferencesKeepsGalaxyPinsUnlessRefreshSetsThemAside pins that
// only Galaxy entries pin a Galaxy version, and that --refresh drops every pin
// unless --offline, which skips --refresh, is set too.
func TestNewLockPreferencesKeepsGalaxyPinsUnlessRefreshSetsThemAside(t *testing.T) {
	t.Parallel()
	prefs := newLockPreferences(&config.Config{}, mixedLockfile())
	if version, ok := prefs.galaxyVersion("acme.galaxy"); !ok || version != testVersion100 {
		t.Errorf("galaxyVersion(acme.galaxy) = %q, %v; want 1.0.0, true", version, ok)
	}
	for _, fqdn := range []string{"acme.git", "acme.url", "acme.absent"} {
		if version, ok := prefs.galaxyVersion(fqdn); ok {
			t.Errorf("galaxyVersion(%s) = %q, true; want no Galaxy pin", fqdn, version)
		}
	}
	if got := newLockPreferences(&config.Config{Refresh: true}, mixedLockfile()); got != nil {
		t.Error("newLockPreferences under --refresh is not nil")
	}
	if got := newLockPreferences(&config.Config{Refresh: true, Offline: true}, mixedLockfile()); got == nil {
		t.Error("newLockPreferences under --refresh --offline is nil, want the pins")
	}
	if got := newLockPreferences(&config.Config{}, nil); got != nil {
		t.Error("newLockPreferences without a file is not nil")
	}
}

// TestLockPreferencesAgreesWithGalaxyVersionsOnly pins the replay gate: only a
// Galaxy collection off its entry's version disagrees, unless its root excludes
// that version; not a git entry's, an unlocked one, or one now built from git.
func TestLockPreferencesAgreesWithGalaxyVersionsOnly(t *testing.T) {
	t.Parallel()
	prefs := newLockPreferences(&config.Config{}, mixedLockfile())
	gitLocator := "git+https://git.example/acme.git#@" + strings.Repeat("a", 40)
	agreeing := map[string]collection{
		"acme.galaxy": {Namespace: "acme", Name: "galaxy", Version: testVersion100},
		"acme.git":    {Namespace: "acme", Name: "git", Version: testVersion200, Source: gitLocator},
		"acme.other":  {Namespace: "acme", Name: "other", Version: "3.0.0"},
	}
	if !prefs.agreesWith(nil, agreeing) {
		t.Error("agreesWith = false for a resolution keeping the Galaxy pin")
	}
	nowGit := map[string]collection{"acme.galaxy": {Namespace: "acme", Name: "galaxy", Version: testVersion200, Source: gitLocator}}
	if !prefs.agreesWith(nil, nowGit) {
		t.Error("agreesWith = false for a Galaxy entry's collection now built from git")
	}
	moved := map[string]collection{"acme.galaxy": {Namespace: "acme", Name: "galaxy", Version: testVersion200}}
	if prefs.agreesWith(nil, moved) {
		t.Error("agreesWith = true for a resolution moving the Galaxy pin")
	}
	for constraint, want := range map[string]bool{">=2.0.0": true, testVersion200: true, "*": false, ">=1.0.0": false} {
		roots := []collection{{Namespace: "acme", Name: "galaxy", Constraint: constraint}}
		if got := prefs.agreesWith(roots, moved); got != want {
			t.Errorf("agreesWith under a root asking %q = %t, want %t", constraint, got, want)
		}
	}
	excludedAsDependency := []collection{{Namespace: "acme", Name: "other", Constraint: ">=2.0.0"}}
	if prefs.agreesWith(excludedAsDependency, moved) {
		t.Error("agreesWith = true for a moved pin no root asks for")
	}
}

// TestLockPreferencesWarnOncefWarnsOncePerEntry pins the warn-once set under
// concurrent callers: one line per entry however often it is raised.
func TestLockPreferencesWarnOncefWarnsOncePerEntry(t *testing.T) {
	t.Parallel()
	prefs := newLockPreferences(&config.Config{}, mixedLockfile())
	printer := &capturingPrinter{}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for _, name := range []string{"acme.galaxy", "acme.git"} {
				prefs.warnOncef(printer, lockEntryRef{name: name, kind: lockedCollection}, "warned about %s", name)
			}
		})
	}
	wg.Wait()
	if len(printer.warns) != 2 || printer.warnsEqual("warned about acme.galaxy") != 1 || printer.warnsEqual("warned about acme.git") != 1 {
		t.Fatalf("warns = %v, want one line per entry", printer.warns)
	}
}

// mutateLocalStore loads the local snapshot at cacheDir under the backend's
// lock, hands it to mutate and saves it back, as state an earlier run left
// would be. Safe only between runs.
func mutateLocalStore(t *testing.T, cacheDir string, mutate func(st *store.Store)) {
	t.Helper()
	ctx := context.Background()
	backend := local.New(cacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()
	lockCtx, release, err := backend.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer func() { _ = release() }()
	st, err := backend.LoadStore(lockCtx)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	mutate(st)
	if err := backend.SaveStore(lockCtx, st); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}
}

// ageAPICache moves every server document the cache at cacheDir holds back by
// age, as though each had been fetched that long ago.
func ageAPICache(t *testing.T, cacheDir string, age time.Duration) {
	t.Helper()
	mutateLocalStore(t, cacheDir, func(st *store.Store) {
		for key, entry := range st.APICache {
			entry.FetchedAt = entry.FetchedAt.Add(-age)
			st.SetAPICache(key, entry)
		}
	})
}

// lockedWidgetsEntry loads the lockfile at path and returns its one entry,
// failing unless it pins acme.widgets alone.
func lockedWidgetsEntry(t *testing.T, path string) lockfile.Entry {
	t.Helper()
	lf, err := lockfile.Load(path)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", path, err)
	}
	if len(lf.Collections) != 1 || lf.Collections[0].Name != testWidgetsFQDN {
		t.Fatalf("locked %+v, want acme.widgets alone", lf.Collections)
	}
	return lf.Collections[0]
}

// writeWidgetsConstraint asks for acme.widgets at constraint.
func writeWidgetsConstraint(t *testing.T, f lockRun, constraint string) {
	t.Helper()
	mustWriteFile(t, f.cfg.RequirementsFile, []byte("collections:\n  - name: acme.widgets\n    version: \""+constraint+"\"\n"))
}

// TestLockBindsTheServerThatStillServesAKeptVersion pins that a kept version's
// collection document is revalidated past its 10-minute window as any solve
// revalidates it, so a first server that dropped the collection gives way.
func TestLockBindsTheServerThatStillServesAKeptVersion(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	second := fakegalaxy.New(t)
	second.AddVersion("acme", "widgets", testVersion100, nil)
	f.cfg.Servers = []config.Server{{URL: f.server.URL()}, {URL: second.URL()}}
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}
	if entry := lockedWidgetsEntry(t, path); entry.Source != f.server.URL() {
		t.Fatalf("seed lock bound acme.widgets to %s, want the first server %s", entry.Source, f.server.URL())
	}
	for _, ep := range []fakegalaxy.Endpoint{
		fakegalaxy.EndpointRootMetadata, fakegalaxy.EndpointVersionsList, fakegalaxy.EndpointVersionDetail, fakegalaxy.EndpointArtifact,
	} {
		f.server.Fail(ep, "acme", "widgets", fakegalaxy.Fault{Status: http.StatusNotFound, Count: -1})
	}
	ageAPICache(t, f.cfg.CacheDir, time.Hour)
	writeWidgetsConstraint(t, f, ">=1.0.0")

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock after the first server dropped acme.widgets: %v", err)
	}
	entry := lockedWidgetsEntry(t, path)
	wantURL := lockedDownloadURLFor(second.URL(), "acme.widgets", testVersion100)
	if entry.Version != testVersion100 || entry.Source != second.URL() || entry.DownloadURL != wantURL {
		t.Fatalf("locked %+v, want 1.0.0 from %s at %s", entry, second.URL(), wantURL)
	}
}

// TestLockOfflineReplaysPastAPinTheRequirementsExclude pins that a pin the
// tightened requirements exclude does not stop an offline lock from replaying
// the resolution recorded for them: --check reports drift and lock writes it.
func TestLockOfflineReplaysPastAPinTheRequirementsExclude(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, _ := seedLockThenPublish(t, f)
	writeWidgetsConstraint(t, f, ">=2.0.0")
	goCold(t, f)
	f.cfg.LockFile = filepath.Join(f.root, "other.lock")
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock into another file: %v", err)
	}

	f.cfg.LockFile, f.cfg.Offline, f.cfg.Check = "", true, true
	offline := infra.New(&capturingPrinter{}, fetch.NewOffline(f.cfg.Timeout))
	if err := Lock(context.Background(), f.cfg, offline); !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("lock --check --offline = %v, want errors.Is helpers.ErrLockfileDrift", err)
	}
	f.cfg.Check = false
	if err := Lock(context.Background(), f.cfg, offline); err != nil {
		t.Fatalf("lock --offline: %v", err)
	}
	assertLockedWidgets(t, path, testVersion200)
}

// TestLockOfflineSolvesPastExcludedPinsItHasNoMetadataFor pins that a locked
// version the requirements exclude is never checked for publication: an offline
// solve without its metadata passes, for a root and a dependency alike.
func TestLockOfflineSolvesPastExcludedPinsItHasNoMetadataFor(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	f.server.AddVersion("acme", "app", testVersion100, map[string]string{"acme.lib": ">=1.0.0"})
	f.server.AddVersion("acme", "lib", testVersion100, nil)
	mustWriteFile(t, f.cfg.RequirementsFile, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}
	f.server.AddVersion("acme", "app", testVersion200, map[string]string{"acme.lib": ">=2.0.0"})
	f.server.AddVersion("acme", "lib", testVersion200, nil)
	mustWriteFile(t, f.cfg.RequirementsFile, []byte("collections:\n  - name: acme.app\n    version: \">=2.0.0\"\n"))
	goCold(t, f)
	f.cfg.LockFile = filepath.Join(f.root, "other.lock")
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock into another file: %v", err)
	}

	f.cfg.LockFile, f.cfg.Offline = "", true
	offline := infra.New(&capturingPrinter{}, fetch.NewOffline(f.cfg.Timeout))
	if err := Lock(context.Background(), f.cfg, offline); err != nil {
		t.Fatalf("lock --offline past the excluded pins: %v", err)
	}
	lf, err := lockfile.Load(path)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", path, err)
	}
	got := make(map[string]string, len(lf.Collections))
	for _, e := range lf.Collections {
		got[e.Name] = e.Version
	}
	if len(got) != 2 || got["acme.app"] != testVersion200 || got["acme.lib"] != testVersion200 {
		t.Fatalf("locked %v, want acme.app and acme.lib at 2.0.0", got)
	}
}

// TestLockDrawsNoWarningForAnExcludedPinNoLongerPublished pins that a locked
// version the requirements exclude is never asked about: its absence from the
// server warns nothing and costs no request.
func TestLockDrawsNoWarningForAnExcludedPinNoLongerPublished(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path := saveWidgetsPin(t, f)
	writeWidgetsConstraint(t, f, ">=1.0.0")

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	assertLockedWidgets(t, path, testVersion100)
	if len(f.printer.warns) != 0 {
		t.Errorf("warns = %v, want none for a pin the requirements exclude", f.printer.warns)
	}
	if got := f.server.Count(fakegalaxy.EndpointVersionDetail); got != 1 {
		t.Errorf("version document requests = %d, want 1, for 1.0.0 alone", got)
	}
}

// TestLockResolvesAKeptPinItsReplayNamedOnceGone pins that a recorded
// resolution agreeing with galaxy.lock on a version its server no longer has,
// whose metadata --clear-cache dropped, is resolved anew with one warning.
func TestLockResolvesAKeptPinItsReplayNamedOnceGone(t *testing.T) {
	t.Parallel()
	for _, check := range []bool{true, false} {
		t.Run(fmt.Sprintf("check %t", check), func(t *testing.T) {
			t.Parallel()
			f := newListedLockRun(t)
			if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
				t.Fatalf("seed Lock: %v", err)
			}
			mutateLocalStore(t, f.cfg.CacheDir, func(st *store.Store) {
				entry := st.ResolvedSnapshot()["acme.widgets"]
				entry.Version = "0.9.0"
				st.SetResolvedAll(map[string]store.ResolvedEntry{"acme.widgets": entry})
				st.SetGraphSnapshot(map[string][]string{"acme.widgets@0.9.0": nil})
			})
			path := saveWidgetsPin(t, f)
			f.cfg.ClearCache, f.cfg.Check = true, check

			err := Lock(context.Background(), f.cfg, f.runtime)
			if check {
				if !errors.Is(err, helpers.ErrLockfileDrift) {
					t.Fatalf("lock --check = %v, want errors.Is helpers.ErrLockfileDrift", err)
				}
			} else {
				if err != nil {
					t.Fatalf("Lock: %v", err)
				}
				assertLockedWidgets(t, path, testVersion100)
			}
			if got := f.printer.warnsEqual(unpublishedWidgetsWarning); got != 1 || len(f.printer.warns) != 1 {
				t.Fatalf("warns = %v, want exactly %q", f.printer.warns, unpublishedWidgetsWarning)
			}
		})
	}
}

// TestLockDryRunKeepsTheLockedVersion pins that --dry-run on an empty cache
// after a newer release previews no change: it keeps galaxy.lock's pins.
func TestLockDryRunKeepsTheLockedVersion(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, before := seedLockThenPublish(t, f)
	goCold(t, f)
	f.cfg.DryRun = true

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock --dry-run: %v", err)
	}
	const upToDate = "Dry run: lockfile is up to date; 0 would be added, 0 would be updated, 0 would be removed, 1 unchanged"
	if !f.printer.hasPersistentPrintContaining(upToDate) {
		t.Fatalf("persists = %v", f.printer.persists)
	}
	if f.printer.hasOkContaining("Would ") {
		t.Fatalf("oks = %v, want no Would line", f.printer.oks)
	}
	assertFileUnchanged(t, path, before)
}

// TestLockClearCacheKeepsTheLockedVersion pins that --clear-cache over the
// resolution install --refresh recorded after a newer release still keeps
// galaxy.lock's version: it clears the cache, never the file.
func TestLockClearCacheKeepsTheLockedVersion(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, before := seedLockThenPublish(t, f)
	f.cfg.Refresh = true
	if err := Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("install --refresh: %v", err)
	}
	f.cfg.Refresh, f.cfg.ClearCache = false, true

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock --clear-cache: %v", err)
	}
	assertFileUnchanged(t, path, before)
}

// TestInstallAndWarmTakeTheNewestPastTheLockfile is the control for lock's
// pins: install without --frozen and warm read no galaxy.lock, so on an empty
// cache both take the release published after the lock.
func TestInstallAndWarmTakeTheNewestPastTheLockfile(t *testing.T) {
	t.Parallel()
	for name, run := range map[string]func(context.Context, *config.Config, *infra.Infra) error{"install": Start, "warm": Warm} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newListedLockRun(t)
			seedLockThenPublish(t, f)
			goCold(t, f)
			if err := run(context.Background(), f.cfg, f.runtime); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got := recordedVersion(t, f.cfg.CacheDir, "acme.widgets"); got != testVersion200 {
				t.Fatalf("%s resolved acme.widgets %q, want 2.0.0", name, got)
			}
		})
	}
}

// TestLockKeepsThePinsOfTheFileLockFileNames pins that --lock-file names the
// one file lock reads its pins and its --check verdict from, with no
// galaxy.lock beside the requirements, on an empty cache after a newer release.
func TestLockKeepsThePinsOfTheFileLockFileNames(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	f.cfg.LockFile = filepath.Join(f.root, "custom.lock")
	path, before := seedLockThenPublish(t, f)
	defaultPath := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, "")

	for _, check := range []bool{true, false} {
		goCold(t, f)
		f.cfg.Check = check
		if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("lock (check %t) --lock-file: %v", check, err)
		}
		assertFileUnchanged(t, path, before)
	}
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Fatalf("stat %s = %v, want no default lockfile written", defaultPath, err)
	}
}

// TestLockCheckReportsAResolveFailureBeforeAMissingLockfile pins that --check
// resolves first: with no lockfile, a requirement no server has exits 3 for
// the resolve, not 6 for the missing file.
func TestLockCheckReportsAResolveFailureBeforeAMissingLockfile(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	mustWriteFile(t, f.cfg.RequirementsFile, []byte("collections:\n  - name: acme.absent\n    version: \"*\"\n"))
	f.cfg.Check = true

	err := Lock(context.Background(), f.cfg, f.runtime)
	if got := exitcode.FromError(err); got != exitcode.ExitResolution || errors.Is(err, helpers.ErrLockfileMissing) {
		t.Fatalf("lock --check = %v (exit %d), want the resolve failure, exit %d", err, got, exitcode.ExitResolution)
	}
}

// TestLockReadsNoLockfileBesideRefusedRequirements pins that lock reads
// galaxy.lock only once the requirements load: a refused file exits 2 with no
// warning about a lockfile beside it that does not load.
func TestLockReadsNoLockfileBesideRefusedRequirements(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	mustWriteFile(t, lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile), oldSchemaWidgetsLockfile(f.cfg.Server))
	mustWriteFile(t, f.cfg.RequirementsFile, []byte("collections:\n  - name: acme.widgets\n    version: [1]\n"))

	err := Lock(context.Background(), f.cfg, f.runtime)
	if got := exitcode.FromError(err); got != exitcode.ExitUsage {
		t.Fatalf("Lock = %v (exit %d), want exit %d", err, got, exitcode.ExitUsage)
	}
	if len(f.printer.warns) != 0 {
		t.Fatalf("warns = %v, want none: the lockfile is never read", f.printer.warns)
	}
}

// TestLockDrawsNoWarningForAnUnpublishedPinTheResultLeavesOut pins the
// warn-after-solve rule: locked acme.b, asked about and found gone, then left
// out once acme.a backjumps to 1.0.0, draws no warning.
func TestLockDrawsNoWarningForAnUnpublishedPinTheResultLeavesOut(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	f.server.AddVersion("acme", "a", testVersion100, nil)
	f.server.AddVersion("acme", "a", testVersion200, map[string]string{"acme.b": "*"})
	f.server.AddVersion("acme", "b", testVersion100, map[string]string{"acme.c": "<1.0.0"})
	f.server.AddVersion("acme", "c", "0.9.0", nil)
	f.server.AddVersion("acme", "c", testVersion100, nil)
	mustWriteFile(t, f.cfg.RequirementsFile,
		[]byte("collections:\n  - name: acme.a\n    version: \"*\"\n  - name: acme.c\n    version: \">=1.0.0\"\n"))
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	lf := &lockfile.File{Server: f.cfg.Server, Collections: []lockfile.Entry{{
		Name: "acme.b", Version: "0.5.0", Source: f.cfg.Server,
		DownloadURL: lockedDownloadURLFor(f.cfg.Server, "acme.b", "0.5.0"),
	}}}
	if err := lockfile.Save(path, lf); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	locked, err := lockfile.Load(path)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", path, err)
	}
	got := make(map[string]string, len(locked.Collections))
	for _, e := range locked.Collections {
		got[e.Name] = e.Version
	}
	if len(got) != 2 || got["acme.a"] != testVersion100 || got["acme.c"] != testVersion100 {
		t.Fatalf("locked %v, want acme.a and acme.c at 1.0.0", got)
	}
	if len(f.printer.warns) != 0 {
		t.Fatalf("warns = %v, want none for a package the result leaves out", f.printer.warns)
	}
}

// TestLockRevalidatesOnlyTheCollectionDocumentOfAKeptPin pins the policies a
// kept version is checked under: past the 10-minute window, a solving lock
// revalidates its collection document once and reads its exact version's.
func TestLockRevalidatesOnlyTheCollectionDocumentOfAKeptPin(t *testing.T) {
	t.Parallel()
	f := newListedLockRun(t)
	path, before := seedLockThenPublish(t, f)
	f.cfg.Refresh = true
	if err := Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("install --refresh: %v", err)
	}
	f.cfg.Refresh = false
	ageAPICache(t, f.cfg.CacheDir, time.Hour)
	f.server.ResetCounts()

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	assertFileUnchanged(t, path, before)
	rootDocs := f.server.Count(fakegalaxy.EndpointRootMetadata)
	versionDocs := f.server.Count(fakegalaxy.EndpointVersionDetail)
	if total := f.server.Total(); rootDocs != 1 || versionDocs != 0 || total != 1 {
		t.Fatalf("requests: root %d, version %d, total %d; want 1, 0 and 1", rootDocs, versionDocs, total)
	}
}
