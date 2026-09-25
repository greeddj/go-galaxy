package collections

// This file pins the lock command end to end through the exported Lock, so
// runLock's lifecycle is covered: the --lock-file path, overwriting, the
// --dry-run preview and the --frozen drift gate.

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// lockRun bundles the cfg, runtime, printer, fakegalaxy server and temp root
// every lock test drives Lock with; server is exposed so a test can publish
// another version between two Lock calls.
type lockRun struct {
	cfg     *config.Config
	runtime *infra.Infra
	printer *capturingPrinter
	server  *fakegalaxy.Server
	root    string
}

// newLockRun builds a cold cache and a requirements.yml resolving to
// acme.widgets@1.0.0 against a fresh fakegalaxy server: the fixture every lock
// test in this file starts from.
func newLockRun(t *testing.T) lockRun {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		MetricsFile:      filepath.Join(root, "metrics.json"),
		Workers:          1,
	}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())
	return lockRun{cfg: cfg, runtime: runtime, printer: printer, server: srv, root: root}
}

// assertSingleWidgetsEntry fails the test unless lf pins exactly one entry,
// acme.widgets at wantVersion.
func assertSingleWidgetsEntry(t *testing.T, lf *lockfile.File, wantVersion string) {
	t.Helper()
	if len(lf.Collections) != 1 {
		t.Fatalf("unexpected lockfile collections: %+v, want exactly 1 entry", lf.Collections)
	}
	entry := lf.Collections[0]
	if entry.Name != "acme.widgets" || entry.Version != wantVersion {
		t.Fatalf("unexpected lockfile entry: %+v, want acme.widgets@%s", entry, wantVersion)
	}
}

// readMetricsReport decodes the metrics file at path into a map, not
// metrics.Report, so the test reads the literal wire keys a CI dashboard parses
// and a renamed json tag cannot round-trip invisibly.
func readMetricsReport(t *testing.T, path string) map[string]any {
	t.Helper()
	//nolint:gosec // path is this test's own fixed cfg.MetricsFile, not user input.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read metrics file %s: %v", path, err)
	}
	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("unmarshal metrics file %s: %v", path, err)
	}
	return written
}

// TestLockHonorsExplicitLockFilePath pins that an explicit --lock-file path,
// even under a missing parent directory, is where the lockfile lands, that the
// default path stays untouched and that the metrics report describes the file.
func TestLockHonorsExplicitLockFilePath(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	// A not-yet-existing parent directory ("ci/") on purpose:
	// helpers.WriteFileAtomic MkdirAll's it, so this also proves Lock never
	// requires the caller to pre-create the lockfile's directory.
	f.cfg.LockFile = filepath.Join(f.root, "ci", "pinned.lock.yml")

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	lf, err := lockfile.Load(f.cfg.LockFile)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", f.cfg.LockFile, err)
	}
	assertSingleWidgetsEntry(t, lf, testVersion100)

	defaultPath := filepath.Join(filepath.Dir(f.cfg.RequirementsFile), lockfile.DefaultName)
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Errorf("expected default lockfile path %s to not exist, stat err = %v", defaultPath, err)
	}

	assertLockMetricsDescribeFile(t, f.cfg, lf)
}

// assertLockMetricsDescribeFile asserts that cfg.MetricsFile's report names
// cfg.LockFile and carries the SHA256 of the lockfile actually written there.
func assertLockMetricsDescribeFile(t *testing.T, cfg *config.Config, lf *lockfile.File) {
	t.Helper()
	written := readMetricsReport(t, cfg.MetricsFile)
	if got, _ := written["lockfile"].(string); got != cfg.LockFile {
		t.Errorf("metrics lockfile = %q, want %q", got, cfg.LockFile)
	}
	wantHash, err := lf.Hash()
	if err != nil {
		t.Fatalf("lf.Hash: %v", err)
	}
	gotHash, _ := written["lockfile_hash"].(string)
	if gotHash == "" || gotHash != wantHash {
		t.Errorf("metrics lockfile_hash = %q, want %q (the hash of the file actually written at the overridden path)", gotHash, wantHash)
	}
}

// TestLockOverwritesAnExistingLockfile pins that a stale lockfile at the
// default path is replaced wholesale by a fresh resolve: none of its entries
// is merged in or consulted.
func TestLockOverwritesAnExistingLockfile(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)

	defaultPath := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	stale := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        f.cfg.Server,
		Collections: []lockfile.Entry{
			{
				Name: "acme.widgets", Version: "0.9.0", Source: f.cfg.Server,
				DownloadURL: lockedDownloadURLFor(f.cfg.Server, "acme.widgets", "0.9.0"),
				SHA256:      "0000000000000000000000000000000000000000000000000000000000bad",
			},
			{
				Name: "acme.legacy", Version: testVersion100, Source: f.cfg.Server,
				DownloadURL: lockedDownloadURLFor(f.cfg.Server, "acme.legacy", testVersion100),
			},
		},
	}
	if err := lockfile.Save(defaultPath, stale); err != nil {
		t.Fatalf("save stale lockfile: %v", err)
	}
	staleBytes, err := os.ReadFile(defaultPath) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read stale lockfile: %v", err)
	}

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	lf, err := lockfile.Load(defaultPath)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", defaultPath, err)
	}
	assertSingleWidgetsEntry(t, lf, testVersion100)

	freshBytes, err := os.ReadFile(defaultPath) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read fresh lockfile: %v", err)
	}
	assertReplacedNotMerged(t, string(freshBytes), string(staleBytes))
}

// assertReplacedNotMerged fails the test unless fresh differs from stale and
// carries neither the stale acme.legacy entry nor its 0.9.0 pin.
func assertReplacedNotMerged(t *testing.T, fresh, stale string) {
	t.Helper()
	if fresh == stale {
		t.Fatalf("expected the fresh lockfile bytes to differ from the stale ones, both were:\n%s", fresh)
	}
	if strings.Contains(fresh, "acme.legacy") {
		t.Errorf("fresh lockfile still contains the stale acme.legacy entry: %s", fresh)
	}
	if strings.Contains(fresh, "0.9.0") {
		t.Errorf("fresh lockfile still contains the stale 0.9.0 pin: %s", fresh)
	}
}

// mustReadFile reads path, failing the test on any error. Used by the
// --frozen gate tests below to prove the lockfile on disk is byte-identical
// before and after a run that must not have written it.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	//nolint:gosec // path is this test's own fixture, not user input.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// TestLockFrozenPassesOnAnUpToDateLockfile is the --frozen gate's positive
// control: an unchanged resolve passes, leaves the file byte-identical, reports
// it up to date and writes a metrics report with "frozen": true.
func TestLockFrozenPassesOnAnUpToDateLockfile(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	before := mustReadFile(t, path)

	f.cfg.Frozen = true
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("frozen Lock: %v", err)
	}

	after := mustReadFile(t, path)
	if string(before) != string(after) {
		t.Fatalf("frozen Lock rewrote an up-to-date lockfile:\n%s", after)
	}
	if !f.printer.hasPersistentPrintContaining("lockfile is up to date") {
		t.Fatalf("persists = %v", f.printer.persists)
	}
	written := readMetricsReport(t, f.cfg.MetricsFile)
	if got, _ := written["frozen"].(bool); !got {
		t.Errorf("metrics frozen = %v, want true (a lock --frozen run honors the flag)", written["frozen"])
	}
}

// TestLockFrozenFailsOnDrift pins that a root added after locking fails
// --frozen with helpers.ErrLockfileDrift and the lock exit code, leaves the
// file byte-identical and still writes the frozen run's metrics report.
func TestLockFrozenFailsOnDrift(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	before := mustReadFile(t, path)

	f.server.AddVersion("acme", "extra", testVersion100, nil)
	mustWriteFile(t, f.cfg.RequirementsFile,
		[]byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n  - name: acme.extra\n    version: \"*\"\n"))
	f.cfg.Frozen = true

	err := Lock(context.Background(), f.cfg, f.runtime)
	if err == nil {
		t.Fatal("expected an error from a drifted lockfile under --frozen, got nil")
	}
	if !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("Lock error = %v, want errors.Is helpers.ErrLockfileDrift", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitLock {
		t.Errorf("exitcode.FromError(err) = %d, want %d", got, exitcode.ExitLock)
	}
	after := mustReadFile(t, path)
	if string(before) != string(after) {
		t.Fatalf("frozen Lock rewrote the lockfile on drift:\n%s", after)
	}
	if !f.printer.hasOkContaining("Would add: acme.extra@" + testVersion100) {
		t.Fatalf("oks = %v", f.printer.oks)
	}

	// lock --frozen writes its metrics report on drift too, unlike a resolve
	// failure: a CI dashboard needs it even for a run that exits nonzero.
	written := readMetricsReport(t, f.cfg.MetricsFile)
	if got, _ := written["command"].(string); got != metricsCommandLock {
		t.Errorf("metrics command = %q, want %q", got, metricsCommandLock)
	}
	if got, _ := written["frozen"].(bool); !got {
		t.Errorf("metrics frozen = %v, want true", written["frozen"])
	}
}

// TestLockFrozenFailsOnAMissingLockfile pins that a never-written lockfile is
// helpers.ErrLockfileMissing, not an all-Added drift, and that the gate does
// not create it: --frozen consumes a lockfile, it never bootstraps one.
func TestLockFrozenFailsOnAMissingLockfile(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.Frozen = true

	err := Lock(context.Background(), f.cfg, f.runtime)
	if err == nil {
		t.Fatal("expected an error from a missing lockfile under --frozen, got nil")
	}
	if !errors.Is(err, helpers.ErrLockfileMissing) {
		t.Fatalf("Lock error = %v, want errors.Is helpers.ErrLockfileMissing", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitLock {
		t.Errorf("exitcode.FromError(err) = %d, want %d", got, exitcode.ExitLock)
	}
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected the gate to create no lockfile, stat err = %v", statErr)
	}
}

// TestLockFrozenFailsClosedOnAnUnloadableLockfile pins that lockFrozen fails
// closed with helpers.ErrLockfileInvalid, not drift, on both lockfile.Load
// failure arms (schema version, IO) and leaves the path untouched.
func TestLockFrozenFailsClosedOnAnUnloadableLockfile(t *testing.T) {
	t.Parallel()
	t.Run("unsupported schema version", assertLockFrozenRejectsBadSchemaVersion)
	t.Run("directory at the lockfile path", assertLockFrozenRejectsADirectoryAtTheLockfilePath)
}

// assertLockFrozenRejectsBadSchemaVersion is the "unsupported schema version"
// subtest of TestLockFrozenFailsClosedOnAnUnloadableLockfile.
func assertLockFrozenRejectsBadSchemaVersion(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.Frozen = true
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	mustWriteFile(t, path, []byte("schema_version: 9999\ncollections: []\n"))
	before := mustReadFile(t, path)

	err := Lock(context.Background(), f.cfg, f.runtime)
	if err == nil {
		t.Fatal("expected an error from an unloadable lockfile under --frozen, got nil")
	}
	if !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("Lock error = %v, want errors.Is helpers.ErrLockfileInvalid", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitLock {
		t.Errorf("exitcode.FromError(err) = %d, want %d", got, exitcode.ExitLock)
	}
	after := mustReadFile(t, path)
	if string(before) != string(after) {
		t.Fatalf("frozen Lock rewrote an unloadable lockfile:\n%s", after)
	}
}

// assertLockFrozenRejectsADirectoryAtTheLockfilePath is the "directory at the
// lockfile path" subtest of TestLockFrozenFailsClosedOnAnUnloadableLockfile.
func assertLockFrozenRejectsADirectoryAtTheLockfilePath(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.Frozen = true
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	if err := os.Mkdir(path, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}

	err := Lock(context.Background(), f.cfg, f.runtime)
	if err == nil {
		t.Fatal("expected an error from a directory at the lockfile path under --frozen, got nil")
	}
	if !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("Lock error = %v, want errors.Is helpers.ErrLockfileInvalid", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitLock {
		t.Errorf("exitcode.FromError(err) = %d, want %d", got, exitcode.ExitLock)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("stat %s after frozen Lock: %v", path, statErr)
	}
	if !info.IsDir() {
		t.Errorf("expected %s to still be a directory after frozen Lock, found a regular file instead", path)
	}
}

// TestLockFrozenDryRunReportsDriftAndSkipsMetrics pins that --frozen --dry-run
// still fails on drift and writes no metrics report either way. The subtests
// share one fixture and run in order, since the second edits requirements.yml.
func TestLockFrozenDryRunReportsDriftAndSkipsMetrics(t *testing.T) {
	f := newLockRun(t)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	before := mustReadFile(t, path)
	// The seed Lock above already wrote f.cfg.MetricsFile (--dry-run is not
	// yet set); removed here so the "no metrics report" checks below observe
	// what THIS run did, not a leftover from the seed.
	if err := os.Remove(f.cfg.MetricsFile); err != nil {
		t.Fatalf("remove seed metrics file: %v", err)
	}
	f.cfg.Frozen = true
	f.cfg.DryRun = true

	t.Run("up to date lockfile passes and writes no metrics", func(t *testing.T) {
		assertFrozenDryRunPassesAndSkipsMetrics(t, f)
	})
	t.Run("drifted lockfile fails and still writes no metrics", func(t *testing.T) {
		assertFrozenDryRunDriftFailsAndSkipsMetrics(t, f, path, before)
	})

	if !f.printer.hasWarnContaining("--dry-run: skipping metrics report to " + f.cfg.MetricsFile) {
		t.Fatalf("warns = %v", f.printer.warns)
	}
}

// assertFrozenDryRunPassesAndSkipsMetrics is the first subtest of
// TestLockFrozenDryRunReportsDriftAndSkipsMetrics: an up-to-date lockfile
// passes under --frozen --dry-run and no metrics report is written.
func assertFrozenDryRunPassesAndSkipsMetrics(t *testing.T, f lockRun) {
	t.Helper()
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, statErr := os.Stat(f.cfg.MetricsFile); !os.IsNotExist(statErr) {
		t.Fatalf("expected no metrics report, stat err = %v", statErr)
	}
}

// assertFrozenDryRunDriftFailsAndSkipsMetrics is the second subtest: an added
// root fails --frozen --dry-run with helpers.ErrLockfileDrift, leaves the
// lockfile untouched and writes no metrics report.
func assertFrozenDryRunDriftFailsAndSkipsMetrics(t *testing.T, f lockRun, path string, before []byte) {
	t.Helper()
	f.server.AddVersion("acme", "extra", testVersion100, nil)
	mustWriteFile(t, f.cfg.RequirementsFile,
		[]byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n  - name: acme.extra\n    version: \"*\"\n"))

	err := Lock(context.Background(), f.cfg, f.runtime)
	if err == nil {
		t.Fatal("expected an error from a drifted lockfile under --frozen --dry-run, got nil")
	}
	if !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("Lock error = %v, want errors.Is helpers.ErrLockfileDrift", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitLock {
		t.Errorf("exitcode.FromError(err) = %d, want %d", got, exitcode.ExitLock)
	}
	after := mustReadFile(t, path)
	if string(before) != string(after) {
		t.Fatalf("--frozen --dry-run rewrote the lockfile:\n%s", after)
	}
	if _, statErr := os.Stat(f.cfg.MetricsFile); !os.IsNotExist(statErr) {
		t.Fatalf("expected no metrics report, stat err = %v", statErr)
	}
}

// TestLockFrozenReportsAServerOnlyChangeAsDrift pins that a change to only the
// file-level Server field is drift (lockfile.Compare's Diff.Server) and is
// reported through its "Would change: server" line.
func TestLockFrozenReportsAServerOnlyChangeAsDrift(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	lf, err := lockfile.Load(path)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", path, err)
	}
	staleServer := "https://old-server.example"
	lf.Server = staleServer
	if err := lockfile.Save(path, lf); err != nil {
		t.Fatalf("save server-only-stale lockfile: %v", err)
	}
	f.cfg.Frozen = true

	err = Lock(context.Background(), f.cfg, f.runtime)
	if err == nil {
		t.Fatal("expected an error from a server-only-changed lockfile under --frozen, got nil")
	}
	if !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("Lock error = %v, want errors.Is helpers.ErrLockfileDrift", err)
	}
	want := "Would change: server " + staleServer + " -> " + f.cfg.Server
	if !f.printer.hasOkContaining(want) {
		t.Fatalf("Okf lines = %v, want one to contain %q", f.printer.okLines(), want)
	}
}

// TestLockFrozenWithoutRefreshIgnoresUpstreamPublication pins that the gate
// mirrors plain lock: with requirements.yml unchanged it reuses the resolve
// snapshot, so a newer upstream version is not drift and no request is made.
func TestLockFrozenWithoutRefreshIgnoresUpstreamPublication(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}

	// A newer version upstream with requirements.yml untouched: the seeding
	// Lock's resolve snapshot still applies, so the gate must not see it.
	f.server.AddVersion("acme", "widgets", "2.0.0", nil)
	f.cfg.Frozen = true
	f.server.ResetCounts()

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("frozen Lock: %v", err)
	}
	if got := f.server.Total(); got != 0 {
		t.Errorf("server.Total() = %d, want 0 (the gate must never reach the network here)", got)
	}
	if !f.printer.hasPersistentPrintContaining("Frozen: lockfile is up to date") {
		t.Fatalf("persists = %v", f.printer.persists)
	}
}

// TestLockFrozenWithRefreshDetectsUpstreamPublication is the positive control
// of the no-refresh case: with --refresh the fresh resolve reaches the live
// server, so the newly published version is reported as drift.
func TestLockFrozenWithRefreshDetectsUpstreamPublication(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}

	f.server.AddVersion("acme", "widgets", "2.0.0", nil)
	f.cfg.Frozen = true
	f.cfg.Refresh = true

	err := Lock(context.Background(), f.cfg, f.runtime)
	if err == nil {
		t.Fatal("expected an error from upstream drift under --frozen --refresh, got nil")
	}
	if !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("Lock error = %v, want errors.Is helpers.ErrLockfileDrift", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitLock {
		t.Errorf("exitcode.FromError(err) = %d, want %d", got, exitcode.ExitLock)
	}
	wantVersionChange := "version " + testVersion100 + " -> 2.0.0"
	if !f.printer.hasOkContaining(wantVersionChange) {
		t.Fatalf("expected an Okf line containing %q, got oks = %v", wantVersionChange, f.printer.oks)
	}
}

// TestLockDryRunWritesNoLockfileAndReportsAdds pins the cold-cache preview:
// every collection reports as added against no baseline, an absent lockfile
// draws no warning, and neither a lockfile nor a metrics report is written.
func TestLockDryRunWritesNoLockfileAndReportsAdds(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.DryRun = true

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no lockfile written, stat err = %v", err)
	}
	if f.printer.hasWarnContaining("cannot be read") {
		t.Fatalf("unexpected warning on a cold cache: %v", f.printer.warns)
	}
	if !f.printer.hasOkContaining("Would add: acme.widgets@1.0.0") {
		t.Fatalf("oks = %v", f.printer.oks)
	}
	wantSummary := "Dry run: lockfile would change; 1 would be added, 0 would be updated, 0 would be removed, 0 unchanged (" + path + ")"
	if !f.printer.hasPersistentPrintContaining(wantSummary) {
		t.Fatalf("persists = %v", f.printer.persists)
	}
	if f.printer.hasPersistentPrintContaining("Lockfile written") {
		t.Fatalf("dry run announced a write: %v", f.printer.persists)
	}
	if _, err := os.Stat(f.cfg.MetricsFile); !os.IsNotExist(err) {
		t.Fatalf("expected no metrics report, stat err = %v", err)
	}
}

// TestLockDryRunReportsUpdateAndRemoval pins the preview's update and remove
// lines against a stale lockfile: acme.widgets at an old version with no
// sha256, plus an acme.legacy entry no root reaches any more.
func TestLockDryRunReportsUpdateAndRemoval(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.DryRun = true
	// Re-registering the same version is idempotent and returns its
	// deterministic sha256, so the expectation is derived, not hardcoded.
	wantVersion := f.server.AddVersion("acme", "widgets", testVersion100, nil)

	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	stale := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        f.cfg.Server,
		Collections: []lockfile.Entry{
			{
				Name: "acme.widgets", Version: "0.9.0", Source: f.cfg.Server,
				DownloadURL: lockedDownloadURLFor(f.cfg.Server, "acme.widgets", "0.9.0"),
			},
			{
				Name: "acme.legacy", Version: "2.0.0", Source: f.cfg.Server,
				DownloadURL: lockedDownloadURLFor(f.cfg.Server, "acme.legacy", "2.0.0"),
			},
		},
	}
	if err := lockfile.Save(path, stale); err != nil {
		t.Fatalf("save stale lockfile: %v", err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read stale lockfile: %v", err)
	}

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	after, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read lockfile after dry run: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("dry run rewrote the lockfile:\n%s", after)
	}
	wantUpdate := "Would update: acme.widgets (version 0.9.0 -> 1.0.0; download_url " +
		lockedDownloadURLFor(f.cfg.Server, "acme.widgets", "0.9.0") + " -> " + wantVersion.DownloadURL +
		"; sha256 (none) -> " + wantVersion.SHA256 + ")"
	if !f.printer.hasOkContaining(wantUpdate) {
		t.Fatalf("expected %q, got oks = %v", wantUpdate, f.printer.oks)
	}
	if !f.printer.hasOkContaining("Would remove: acme.legacy@2.0.0") {
		t.Fatalf("oks = %v", f.printer.oks)
	}
	if f.printer.hasOkContaining("Would add:") {
		t.Fatalf("expected no Added line for an already-pinned collection, got %v", f.printer.oks)
	}
}

// TestLockDryRunNoChangeReportsAllUnchanged pins that a dry run over the file
// a real Lock just wrote reports up to date with one unchanged entry; it is the
// control for TestLockDryRunReportsAServerOnlyChange, which changes only Server.
func TestLockDryRunNoChangeReportsAllUnchanged(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	before, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read seeded lockfile: %v", err)
	}

	dryPrinter := &capturingPrinter{}
	dryRuntime := infra.New(dryPrinter, f.server.Client())
	f.cfg.DryRun = true
	if err := Lock(context.Background(), f.cfg, dryRuntime); err != nil {
		t.Fatalf("dry Lock: %v", err)
	}

	after, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read lockfile after dry run: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("dry run rewrote the lockfile:\n%s", after)
	}
	wantSummary := "Dry run: lockfile is up to date; 0 would be added, 0 would be updated, 0 would be removed, 1 unchanged (" + path + ")"
	if !dryPrinter.hasPersistentPrintContaining(wantSummary) {
		t.Fatalf("persists = %v", dryPrinter.persists)
	}
	if len(dryPrinter.oks) != 0 {
		t.Fatalf("expected no per-entry lines, got %v", dryPrinter.oks)
	}
}

// TestLockDryRunReportsAServerOnlyChange pins that a change to only the
// file-level Server prints its own line and flips the summary verdict to
// "would change", though every count matches an unchanged lockfile.
func TestLockDryRunReportsAServerOnlyChange(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}

	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	lf, err := lockfile.Load(path)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", path, err)
	}
	staleServer := "https://old-server.example"
	lf.Server = staleServer
	if err := lockfile.Save(path, lf); err != nil {
		t.Fatalf("save server-only-stale lockfile: %v", err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read staged lockfile: %v", err)
	}

	dryPrinter := &capturingPrinter{}
	dryRuntime := infra.New(dryPrinter, f.server.Client())
	f.cfg.DryRun = true
	if err := Lock(context.Background(), f.cfg, dryRuntime); err != nil {
		t.Fatalf("dry Lock: %v", err)
	}

	// The server line, printed though no collection changed.
	if !dryPrinter.hasOkContaining("Would change: server " + staleServer + " -> " + f.cfg.Server) {
		t.Fatalf("oks = %v", dryPrinter.oks)
	}
	// The verdict, which only the server change can flip: the counts are the
	// ones an unchanged lockfile reports.
	wantSummary := "Dry run: lockfile would change; 0 would be added, 0 would be updated, 0 would be removed, 1 unchanged (" + path + ")"
	if !dryPrinter.hasPersistentPrintContaining(wantSummary) {
		t.Fatalf("persists = %v", dryPrinter.persists)
	}

	after, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read lockfile after dry run: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("dry run rewrote the lockfile:\n%s", after)
	}
}

// TestLockDryRunWarnsOnAnUnreadableBaseline pins that a lockfile failing
// lockfile.Load is warned about rather than silently treated as absent, and
// that the preview still succeeds and reports every collection as added.
func TestLockDryRunWarnsOnAnUnreadableBaseline(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.DryRun = true
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	mustWriteFile(t, path, []byte("schema_version: 9999\ncollections: []\n"))

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	if !f.printer.hasWarnContaining("cannot be read") {
		t.Fatalf("warns = %v", f.printer.warns)
	}
	if !f.printer.hasOkContaining("Would add: acme.widgets@1.0.0") {
		t.Fatalf("oks = %v", f.printer.oks)
	}
}

// TestLockDryRunDoesNotFabricateASnapshot pins that a dry run on a cold cache
// persists no snapshot, as TestInstallDryRunDoesNotFabricateASnapshot does for
// install: saveDryRunSnapshotIfPersisted saves only over an existing one.
func TestLockDryRunDoesNotFabricateASnapshot(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.DryRun = true
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	ctx := context.Background()
	backend := local.New(f.cfg.CacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()
	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if st.WasPersisted() {
		t.Fatal("lock --dry-run against a cold cache must not create a persisted snapshot")
	}
}

// TestLockDryRunSavesMetadataCachesWhenSnapshotExists pins that a dry run over
// an existing snapshot saves its fresh resolve: only the dry run can record
// acme.extra's requirement spec, while WasPersisted is already set by the seed.
func TestLockDryRunSavesMetadataCachesWhenSnapshotExists(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}

	f.server.AddVersion("acme", "extra", testVersion100, nil)
	mustWriteFile(t, f.cfg.RequirementsFile,
		[]byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n  - name: acme.extra\n    version: \"*\"\n"))
	f.cfg.DryRun = true
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("dry Lock: %v", err)
	}

	ctx := context.Background()
	backend := local.New(f.cfg.CacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()
	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if !st.WasPersisted() {
		t.Fatal("expected WasPersisted() true: a dry run must still save the metadata caches when a persisted snapshot already existed")
	}
	if _, ok := st.RequirementsSnapshot()["acme.extra"]; !ok {
		t.Fatalf("expected the dry run's own fresh resolve to have saved acme.extra's requirement spec, got %v", st.RequirementsSnapshot())
	}
}

// TestLockDryRunSkipsMetricsAndSaysSo pins that lock's dry run writes no
// metrics file and says so on stderr; only the warning catches a dropped
// writeRunMetrics call, since the file is absent either way.
func TestLockDryRunSkipsMetricsAndSaysSo(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.DryRun = true
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	if _, err := os.Stat(f.cfg.MetricsFile); !os.IsNotExist(err) {
		t.Fatalf("expected no metrics report, stat err = %v", err)
	}
	if !f.printer.hasWarnContaining("--dry-run: skipping metrics report to " + f.cfg.MetricsFile) {
		t.Fatalf("warns = %v", f.printer.warns)
	}
}

// TestLockDryRunRefusesAHostileBaselineAndSaysSo pins that lockfile.Load refuses
// a baseline entry named outside the collection alphabet (safeout keeps "\n",
// so only the refusal stops a forged line) and the preview runs baseline-free.
func TestLockDryRunRefusesAHostileBaselineAndSaysSo(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	const hostileName = "../../../../etc/passwd"
	hostileSource := "https://x.example\x00\x1b[31m\r\nInstalled: totally.fine"
	oversizedDep := strings.Repeat("d", 10000)

	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	stale := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        f.cfg.Server,
		Collections: []lockfile.Entry{
			{Name: hostileName, Version: "9.9.9", Source: hostileSource, Deps: []string{oversizedDep}},
		},
	}
	if err := lockfile.Save(path, stale); err != nil {
		t.Fatalf("save hostile baseline lockfile: %v", err)
	}
	// (1) the refusal happens on the real round trip, not on a hand-built
	// *File: Save wrote this file and Load is what rejects it.
	assertHostileNameIsRefusedByLoad(t, path)

	f.cfg.DryRun = true
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// (2) the hostile name reaches no report line at all. Its own bytes are
	// what would have forged one, so the check is that nothing printed
	// contains it rather than that it printed in some safe form.
	if f.printer.hasOkContaining(hostileName) {
		t.Fatalf("the refused baseline's name still reached the report: oks = %v", f.printer.oks)
	}
	assertNoPathMatchesHostileName(t, f.root)

	// (3) the run still succeeds and reports the legitimate collection as
	// added, the documented behavior for a baseline it cannot read.
	if !f.printer.hasOkContaining("Would add: acme.widgets@1.0.0") {
		t.Fatalf("oks = %v", f.printer.oks)
	}
	if !f.printer.hasWarnContaining("cannot be read") {
		t.Fatalf("expected a warning naming the unreadable baseline, warns = %v", f.printer.warns)
	}
}

// assertHostileNameIsRefusedByLoad is check (1) of
// TestLockDryRunRefusesAHostileBaselineAndSaysSo: lockfile.Load refuses the
// hostile name in a file lockfile.Save wrote, so nothing downstream holds it.
func assertHostileNameIsRefusedByLoad(t *testing.T, path string) {
	t.Helper()
	loaded, err := lockfile.Load(path)
	if !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("lockfile.Load(%s) = (%+v, %v), want errors.Is helpers.ErrLockfileInvalid", path, loaded, err)
	}
}

// assertNoPathMatchesHostileName fails the test if any path under root is
// named after the traversal target "passwd"; the walk records the first match
// rather than returning a constructed error, which err113 forbids.
func assertNoPathMatchesHostileName(t *testing.T, root string) {
	t.Helper()
	var match string
	walkErr := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if match == "" && strings.Contains(filepath.Base(p), "passwd") {
			match = p
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("filesystem walk: %v", walkErr)
	}
	if match != "" {
		t.Fatalf("found a path matching the hostile name: %s", match)
	}
}

// lockedDownloadURLFor is the download_url a hand-written Galaxy lockfile
// entry for name at version carries: the fake server's own download route.
func lockedDownloadURLFor(server, name, version string) string {
	return strings.TrimSuffix(server, "/") + "/download/" + strings.ReplaceAll(name, ".", "-") + "-" + version + ".tar.gz"
}
