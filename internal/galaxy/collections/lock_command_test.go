package collections

// This file pins the lock command's end-to-end behavior, exercised through
// the exported Lock entry point (not lockWithState) so runLock's own
// lifecycle is covered:
//
//   - TestLockHonorsExplicitLockFilePath: an explicit --lock-file path (with a
//     not-yet-existing parent directory) is where the lockfile actually lands,
//     the conventional default path next to requirements.yml is left untouched,
//     and the metrics report's lockfile/lockfile_hash pair describes the file
//     actually written at the overridden path, not the default one.
//   - TestLockOverwritesAnExistingLockfile: a stale lockfile at the default path
//     is replaced wholesale by a fresh resolve, not merged with it, and the
//     resolve itself never consults the stale file's entries.
//
// It also covers lock's --dry-run preview (TestLockDryRun*): a diff of a
// fresh resolve against whatever lockfile is already on disk, reported
// through Okf/PersistentPrintf and never written to disk, with the snapshot
// save and the metrics-skip guard both behaving exactly as they do for
// install and warm.
//
// And it covers lock's --frozen drift gate (TestLockFrozen*, see lockFrozen's
// own doc comment in lock_command.go): lock --frozen still resolves fresh -
// lockWithState always does - but refuses to overwrite the lockfile when
// that fresh resolve disagrees with the one already on disk, reporting the
// disagreement through the same reportLockfileDiff the dry-run preview uses
// and failing the run with helpers.ErrLockfileDrift rather than silently
// rewriting the file. A missing lockfile and an unloadable one are each
// their own sentinel (helpers.ErrLockfileMissing, helpers.ErrLockfileInvalid)
// rather than being folded into drift - see lockFrozen's own doc comment for
// why.

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

// lockRun bundles the cfg/runtime/printer trio every lock test in this file
// drives Lock with, plus the fakegalaxy server and temp root behind them.
// server is exposed, not just wired into runtime's http.Client, because
// TestLockDryRunSavesMetadataCachesWhenSnapshotExists needs to register an
// additional version on it between two Lock calls.
type lockRun struct {
	cfg     *config.Config
	runtime *infra.Infra
	printer *capturingPrinter
	server  *fakegalaxy.Server
	root    string
}

// newLockRun builds a cold cache, a requirements.yml pinning nothing but
// resolving to acme.widgets@1.0.0 against a fresh fakegalaxy server, and a
// matching lockRun fixture ready to drive the exported Lock. Every lock test
// in this file starts from this same fixture so tests differ only in the
// field(s) each one sets or asserts on afterward.
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

// assertSingleWidgetsEntry fails the test unless lf contains exactly one
// entry, named acme.widgets, pinned at wantVersion. Shared by every test in
// this file that performs a real Lock: each one asserts that the lockfile
// Lock actually wrote pins nothing but a fresh resolve's own result, whatever
// staleness or override preceded the run.
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

// readMetricsReport reads the metrics file at path and decodes it into a
// map, not metrics.Report: decoding into the struct would resolve field
// names through the very same json tags the marshal side used, so a renamed
// or swapped tag would round-trip invisibly. Reading the literal wire keys
// pins the on-disk contract a consuming CI dashboard actually parses,
// independent of the Go struct's field names (see
// TestArtifactMetricsWrittenToMetricsFile in e2e_test.go for the same
// reasoning).
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

// TestLockHonorsExplicitLockFilePath asserts that an explicit --lock-file
// path - including one whose parent directory does not exist yet - is where
// the lockfile actually lands, that the conventional default path next to
// requirements.yml is left untouched, and that the metrics report describes
// the file actually written rather than the default location.
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
// cfg.LockFile as the lockfile path and carries the SHA256 hash of the
// lockfile actually written there - not the default path's hash - proving
// the report describes the file the run produced, not a hardcoded default.
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

// TestLockOverwritesAnExistingLockfile asserts that a stale lockfile at the
// default path is replaced wholesale by a fresh resolve, not merged with it,
// and that the resolve itself never consulted the stale entries: the run
// pins only what requirements.yml + the live server resolve to, and the
// stale file's own entries never leak into the result.
func TestLockOverwritesAnExistingLockfile(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)

	defaultPath := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	stale := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        f.cfg.Server,
		Collections: []lockfile.Entry{
			{Name: "acme.widgets", Version: "0.9.0", Source: f.cfg.Server, SHA256: "0000000000000000000000000000000000000000000000000000000000bad"},
			{Name: "acme.legacy", Version: testVersion100, Source: f.cfg.Server},
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
// carries neither the stale acme.legacy entry nor its 0.9.0 pin, proving a
// stale lockfile is replaced wholesale by a fresh resolve rather than merged
// with it.
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

// TestLockFrozenPassesOnAnUpToDateLockfile is the positive control every
// other --frozen gate test below depends on: it proves the gate can pass, not
// only refuse, on the identical newLockRun fixture those tests reuse. A real
// Lock run seeds the lockfile; a second Lock with cfg.Frozen set re-resolves
// the same requirements against the same server and finds no difference, so
// the run succeeds, the file on disk is untouched byte-for-byte, the report
// names the file as already up to date, and the metrics report - written by
// lockFrozen's own writeRunMetrics call, not skipped the way the missing- or
// unloadable-lockfile early returns skip it - claims "frozen": true.
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

// TestLockFrozenFailsOnDrift proves the gate's refusal shape: a requirements
// root added after the lockfile was written makes a fresh resolve disagree
// with what is already on disk, and --frozen fails the run instead of
// silently rewriting the file to match. Assertions are ordered so each is
// reachable as the first failure under a mutation that stops short of it -
// see the two mutations below, both actually run.
//
// Mutation 1 (dropping the `if !diff.Empty() { return ... }` block in
// lockFrozen entirely, leaving `return saveErr`) confirmed to fail this test
// at the first assertion, before errors.Is or the byte-identity check are
// ever reached:
//
//	lock_command_test.go:361: expected an error from a drifted lockfile under
//	--frozen, got nil
//	--- FAIL: TestLockFrozenFailsOnDrift (0.06s)
//
// Mutation 2 (keeping the drift return but adding an unconditional
// `lockfile.Save(path, lf)` write right before it, so the error class stays
// correct but the file is rewritten anyway) confirmed to fail this test at
// the byte-identity check specifically, with errors.Is and the exit-code
// check both still passing:
//
//	lock_command_test.go:371: frozen Lock rewrote the lockfile on drift:
//	server: http://127.0.0.1:PORT
//	collections:
//	  - name: acme.extra
//	    version: 1.0.0
//	    ...
//	--- FAIL: TestLockFrozenFailsOnDrift (0.05s)
//
// which is what proves the byte-identity assertion is load-bearing on its
// own, independent of the sentinel check above it.
//
// Mutation 3 (moving the writeRunMetrics call in lockFrozen to after the
// `if !diff.Empty()` return, so it only runs on the non-drift tail) confirmed
// to fail the trailing metrics assertions:
//
//	lock_command_test.go:386: metrics frozen = <nil>, want true
//	--- FAIL: TestLockFrozenFailsOnDrift (0.05s)
//
// not with a missing-file error: the seeding Lock call earlier in this test
// already wrote a metrics report (command "lock", frozen absent, since that
// call was not frozen), so under this mutation the file still exists but
// carries that stale content - the frozen run's own metrics were never
// written over it. The "command" assertion below cannot be the first failure
// under any mutation confined to lockFrozen, not just this one: the seeding
// Lock call guarantees both the file's existence and its command value
// before lockFrozen ever runs, so nothing lockFrozen does can change what
// that assertion checks. It stays paired with "frozen" anyway - together
// they read as "this is a lock report, and it is the frozen run's", worth
// stating even though only the second half can ever be the one that fails.
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

	// A non-dry-run lock --frozen writes the metrics report on every outcome,
	// success or drift-failure alike - unlike a resolve/build failure, which
	// returns before writeRunMetrics ever runs. A CI dashboard consuming this
	// report depends on it existing even for a run that exits nonzero.
	written := readMetricsReport(t, f.cfg.MetricsFile)
	if got, _ := written["command"].(string); got != metricsCommandLock {
		t.Errorf("metrics command = %q, want %q", got, metricsCommandLock)
	}
	if got, _ := written["frozen"].(bool); !got {
		t.Errorf("metrics frozen = %v, want true", written["frozen"])
	}
}

// TestLockFrozenFailsOnAMissingLockfile proves a lockfile that was never
// written is reported as helpers.ErrLockfileMissing, not as drift:
// lockfile.Compare(nil, fresh) would report every resolved collection as
// Added, which reads exactly like "every pin just changed" even though the
// real fact is "this project has never run lock" - see lockFrozen's own doc
// comment. The gate must also never create the file itself: --frozen exists
// to consume what is already there, not to bootstrap it.
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

// TestLockFrozenFailsClosedOnAnUnloadableLockfile proves the gate fails
// closed - with the specific sentinel helpers.ErrLockfileInvalid, not merely
// "some error" - on a lockfile that exists but cannot be loaded, and leaves
// whatever is on disk at that path untouched. This pins that lockFrozen
// calls lockfile.Load directly rather than reusing lockDryRunBaseline's
// lenient "treat as no baseline" policy, which would instead turn an
// unloadable file into an all-Added drift report.
//
// Its two subtests exercise lockfile.Load's two independent failure arms
// through lockFrozen end to end, not just at the lockfile package's own unit
// level: "unsupported schema version" reaches ErrLockfileInvalid through the
// schema-version check inside Load, after os.ReadFile already succeeded;
// "directory at the lockfile path" reaches the identical sentinel through
// Load's IO arm instead - os.ReadFile itself failing with something other
// than fs.ErrNotExist, wrapped with %s rather than %w so it can never also
// satisfy IsNotExist. Both must land on the same sentinel and exit class for
// lockFrozen's own "any other lockfile.Load failure" claim to be true, not
// merely true for the one shape this file happened to test before the IO arm
// existed.
//
// Mutation (replacing lockFrozen's `lockfile.Load(path)` error-handling block
// with a bare `existing := lockDryRunBaseline(runtime, path)`) confirmed to
// fail the "unsupported schema version" subtest with:
//
//	lock_command_test.go:478: Lock error = lockfile is out of date:
//	/.../galaxy.lock: run `go-galaxy lock` to update it, want
//	errors.Is helpers.ErrLockfileInvalid
//	--- FAIL: TestLockFrozenFailsClosedOnAnUnloadableLockfile (0.03s)
//
// not with a nil error: lockDryRunBaseline warns and returns nil for an
// unloadable file instead of failing, so lockFrozen compares the fresh
// resolve against a nil baseline - reporting every entry as Added - and the
// run still fails, but as ordinary drift (helpers.ErrLockfileDrift) rather
// than as the specific "this file cannot even be read" failure the real
// lockfile.Load path reports. The wrong sentinel is exactly the bug this
// test exists to catch: a corrupt lockfile would silently pass as "you
// haven't run lock yet" instead of failing closed on its own terms.
func TestLockFrozenFailsClosedOnAnUnloadableLockfile(t *testing.T) {
	t.Parallel()
	t.Run("unsupported schema version", assertLockFrozenRejectsBadSchemaVersion)
	t.Run("directory at the lockfile path", assertLockFrozenRejectsADirectoryAtTheLockfilePath)
}

// assertLockFrozenRejectsBadSchemaVersion is
// TestLockFrozenFailsClosedOnAnUnloadableLockfile's "unsupported schema
// version" subtest, factored out to keep the parent test's own cyclomatic
// complexity within budget.
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

// assertLockFrozenRejectsADirectoryAtTheLockfilePath is
// TestLockFrozenFailsClosedOnAnUnloadableLockfile's "directory at the
// lockfile path" subtest, factored out to keep the parent test's own
// cyclomatic complexity within budget.
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

// TestLockFrozenDryRunReportsDriftAndSkipsMetrics proves --frozen and
// --dry-run compose the way lockWithState's own doc comment states: both
// suppress the write, and --frozen additionally supplies the stricter
// verdict, so a drifted lockfile still fails the run under --dry-run - it is
// not merely previewed as a would-be change - while writeRunMetrics's own
// cfg.DryRun guard applies exactly as it does to every other command's dry
// run, so no metrics report is written in either the passing or the failing
// case. The two subtests share one fixture and run in a fixed order, not in
// parallel with each other: the second mutates the same requirements.yml the
// first depends on staying untouched.
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

// assertFrozenDryRunPassesAndSkipsMetrics runs the first subtest of
// TestLockFrozenDryRunReportsDriftAndSkipsMetrics: an up-to-date lockfile
// under --frozen --dry-run succeeds and writes no metrics report. Factored
// out of the parent test to keep its own cyclomatic complexity within
// budget.
func assertFrozenDryRunPassesAndSkipsMetrics(t *testing.T, f lockRun) {
	t.Helper()
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, statErr := os.Stat(f.cfg.MetricsFile); !os.IsNotExist(statErr) {
		t.Fatalf("expected no metrics report, stat err = %v", statErr)
	}
}

// assertFrozenDryRunDriftFailsAndSkipsMetrics runs the second subtest of
// TestLockFrozenDryRunReportsDriftAndSkipsMetrics: adding a root drifts the
// lockfile, and --frozen --dry-run fails with helpers.ErrLockfileDrift,
// leaves the lockfile untouched, and still writes no metrics report.
// Factored out of the parent test to keep its own cyclomatic complexity
// within budget.
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

// TestLockFrozenReportsAServerOnlyChangeAsDrift proves the file-level Server
// field is itself drift, even when every collection entry is untouched:
// lockfile.Compare's Diff.Server is what makes this a difference at all -
// Server is part of the lockfile's identity the same way a collection pin is
// (see File.Server's own doc comment) - and reportLockfileDiff's own "Would
// change: server ..." line - the only line printed, since no collection
// changed - is what the run reports before failing.
//
// Mutation (dropping `diff.Server = serverFieldChange(before, after)` from
// lockfile.Compare, in internal/galaxy/lockfile/compare.go) confirmed to
// fail this test with:
//
//	lock_command_test.go:645: expected an error from a server-only-changed
//	lockfile under --frozen, got nil
//	--- FAIL: TestLockFrozenReportsAServerOnlyChangeAsDrift (0.07s)
//
// because with Diff.Server never populated, diff.Empty() sees no collection
// change and no server change either, so the gate reports the lockfile as
// up to date instead of drifted - exactly the false negative this test
// exists to catch.
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

// TestLockFrozenWithoutRefreshIgnoresUpstreamPublication pins the gate's
// mirroring of lock's own resolution as a deliberate contract, not an
// oversight: lockFrozen's fresh resolve reuses the persisted resolve
// snapshot exactly as a plain `lock` run would whenever requirements.yml
// (and the effective server list) are unchanged, so publishing a newer
// upstream version without touching requirements.yml leaves the gate
// reporting "up to date" rather than drift. That mirroring holds per flag
// set, not per command: what this gate compares against is whatever
// `lock --<those flags>` would write, and plain `lock` (no --refresh)
// writes the pinned 1.0.0 here regardless of what has been published since,
// because it reuses the same unchanged resolve snapshot this frozen run
// does. A future editor "fixing" this into a fresh-resolve-every-time gate
// would have to delete this test first, since it would then also break
// lock's own established snapshot-reuse behavior for every other caller of
// resolveCollectionsInternal.
//
// The up-to-date-verdict assertion is what makes this a pin rather than a
// nil-error smoke test: a plain (unfrozen) `lock` run also returns nil here,
// so asserting only that would still pass if the `if cfg.Frozen` branch were
// deleted from lockWithState entirely. The "Frozen: lockfile is up to date"
// line is emitted only by lockFrozen, via frozenDiffPrefix, so reaching it
// proves the gate actually ran. The server.Total() == 0 check next to it is
// documentary, not itself pinned: no reachable mutation fails it first,
// since anything that would make the gate reach the network here also
// changes what it resolves to, which flips the verdict and fails that
// check first instead (see the branch-deletion mutation below, which the
// count check survives) - but it is still the only assertion proving the
// verdict came from the resolve snapshot rather than a live wire that
// happened to agree with it, so it stays.
//
// TestLockFrozenWithRefreshDetectsUpstreamPublication below is this test's
// mutual positive control: the identical fixture, differing only in
// cfg.Refresh, reports drift instead of "up to date" - proving the verdict
// here comes from --refresh being absent, not from the gate being unable to
// detect a real difference in the first place.
//
// Mutation (deleting the `if cfg.Frozen { return lockFrozen(...) }` branch
// from lockWithState, so a frozen run falls through to lock's ordinary
// write path) confirmed to fail this test with:
//
//	lock_command_test.go:728: persists = [✅ Lockfile written to
//	/.../galaxy.lock (1 collections) ✅ Lockfile written to
//	/.../galaxy.lock (1 collections)]
//	--- FAIL: TestLockFrozenWithoutRefreshIgnoresUpstreamPublication (0.06s)
//
// The server.Total() == 0 check passes even under this mutation - lock's
// ordinary write path also reuses the resolve snapshot and touches no
// network - which is exactly why the verdict-line assertion, not the count
// one, is what catches a deleted gate here.
func TestLockFrozenWithoutRefreshIgnoresUpstreamPublication(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}

	// A higher version published on the server, requirements.yml left
	// untouched: an unpinned resolve against the live server would prefer
	// this one, but the gate must never reach the network for it - the
	// resolve snapshot the seeding Lock above wrote is still valid, since
	// requirements.yml has not changed.
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

// TestLockFrozenWithRefreshDetectsUpstreamPublication is
// TestLockFrozenWithoutRefreshIgnoresUpstreamPublication's mutual positive
// control: the identical fixture - seed Lock, then publish
// acme.widgets@2.0.0 upstream with requirements.yml left untouched - reports
// drift instead of "up to date" once cfg.Refresh is also set, because
// lock --frozen --refresh resolves fresh against the live server rather than
// reusing the snapshot, exactly as lock --refresh (no --frozen) itself
// would. This is the concrete instance of the gate's per-flag-set mirroring:
// lock --frozen gates the requirements-to-lockfile relationship; lock
// --frozen --refresh gates upstream publication too, because that is what
// lock --refresh alone would have written into the file this gate compares
// against.
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

// TestLockDryRunWritesNoLockfileAndReportsAdds proves the cold-cache shape of
// lock's --dry-run preview: no lockfile exists yet, so the diff against a nil
// baseline reports every resolved collection as added, nothing is written to
// disk, and no metrics report is produced.
//
// The no-warning assertion below pins the silence half of
// lockDryRunBaseline's own documented policy - "an absent file is silent" -
// which, unlike its "present but unloadable warns" half
// (TestLockDryRunWarnsOnAnUnreadableBaseline), is covered nowhere
// else. It is genuinely pinned, not documentary:
// deleting the `if errors.Is(err, fs.ErrNotExist)` guard in lockfile.Load
// makes lockfile.IsNotExist stop recognizing this fixture's missing-file
// error, so lockDryRunBaseline takes its warn branch instead of its silent
// one, and this assertion fires. It pins that policy half of
// lockDryRunBaseline, not the Load guard itself, which lockfile package's own
// tests already pin.
//
// Three mutations were run against this test, all confirmed to fail it:
//
//   - Dropping the `if cfg.DryRun` branch from lockWithState (so the real
//     lockfile.Save path runs unconditionally) fails the lockfile-absence
//     check with:
//     lock_command_test.go:819: expected no lockfile written, stat err = <nil>
//   - Removing writeRunMetrics's own internal cfg.DryRun guard (leaving
//     lockDryRun's unconditional call to it) fails the metrics-absence check
//     with:
//     lock_command_test.go:835: expected no metrics report, stat err = <nil>
//   - Deleting the `if errors.Is(err, fs.ErrNotExist) { return nil, err }`
//     guard in lockfile.Load fails the no-warning check with:
//     lock_command_test.go:822: unexpected warning on a cold cache: [--dry-run
//     is active: no artifact will be downloaded, installed, or cached; the
//     resolved metadata caches are still saved existing lockfile
//     /.../galaxy.lock cannot be read (lockfile is invalid: open
//     /.../galaxy.lock: no such file or directory); reporting every
//     collection as added no persisted snapshot was found; a dry run will not
//     create one, so the metadata caches this run built are discarded
//     --dry-run: skipping metrics report to /.../metrics.json]
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

// TestLockDryRunReportsUpdateAndRemoval proves the preview reports both an
// updated and a removed collection - the two Diff cases
// TestLockDryRunWritesNoLockfileAndReportsAdds never exercises - against a
// stale lockfile pinning acme.widgets at an old version with no sha256 (a
// version -> version and a (none) -> sha256 change in the same line) plus a
// phantom acme.legacy entry the fresh resolve no longer has a root for. It
// also doubles as TestLockDryRunWritesNoLockfileAndReportsAdds's positive
// control on the printer: the same fixture that reports zero updates there
// reports exactly one here.
//
// Mutation (dropping the `if cfg.DryRun` branch from lockWithState) fails
// with:
//
//	lock_command_test.go:897: dry run rewrote the lockfile:
//	server: http://127.0.0.1:PORT
//	collections:
//	  - name: acme.widgets
//	    version: 1.0.0
//	    source: http://127.0.0.1:PORT
//	    sha256: c101ba4cb889e4daaf158fdf58276ac2aa15d109e2508d602207d63c3317ae15
//	schema_version: 1
func TestLockDryRunReportsUpdateAndRemoval(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.DryRun = true
	// Re-registers the same acme.widgets@1.0.0 newLockRun already added, purely
	// to capture its deterministic sha256: AddVersion computes the sha from
	// namespace/name/version/deps alone, so a repeat call overwrites the
	// existing map entry idempotently rather than registering a second one,
	// and deriving the expected value this way beats hardcoding it.
	wantVersion := f.server.AddVersion("acme", "widgets", testVersion100, nil)

	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	stale := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        f.cfg.Server,
		Collections: []lockfile.Entry{
			{Name: "acme.widgets", Version: "0.9.0", Source: f.cfg.Server},
			{Name: "acme.legacy", Version: "2.0.0", Source: f.cfg.Server},
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
	wantUpdate := "Would update: acme.widgets (version 0.9.0 -> 1.0.0; sha256 (none) -> " + wantVersion.SHA256 + ")"
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

// TestLockDryRunNoChangeReportsAllUnchanged proves an up-to-date lockfile
// reports zero adds/updates/removals and exactly one unchanged collection,
// with no per-entry line at all - the fixture is first driven through a real
// Lock (this test's own positive control, proving the fixture can produce a
// real lockfile, not just refuse to touch one) so the dry run that follows
// diffs against genuinely current content rather than an empty or synthetic
// baseline.
//
// This test is also TestLockDryRunReportsAServerOnlyChange's positive
// control: both drive the identical fixture to the identical add/update/
// remove/unchanged counts and differ only in the file-level Server field -
// this one leaves it alone and reports "lockfile is up to date", the other
// changes only it and reports "lockfile would change". That pairing is what
// proves the verdict term discriminates on the server field alone rather
// than on the counts, which are identical in both tests.
//
// Mutation (dropping the `if cfg.DryRun` branch from lockWithState) fails
// with:
//
//	lock_command_test.go:959: persists = [✅ Lockfile written to /.../galaxy.lock (1 collections)]
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

// TestLockDryRunReportsAServerOnlyChange proves the file-level Server field
// is itself reported, and itself flips the summary's verdict, even though
// every collection is untouched and every add/update/remove/unchanged count
// stays exactly what TestLockDryRunNoChangeReportsAllUnchanged's identical
// fixture reports - see that test's own doc comment for the full pairing.
// Without this, a diff that changes only Diff.Server renders zero per-entry
// lines and a count-only summary indistinguishable from "nothing to do",
// which is exactly the bug this test exists to catch: the fixture stages a
// lockfile whose Server disagrees with cfg.Server (rewritten through
// lockfile.Save, not a hand-built struct, so the staged file is exactly what
// a real, older lock run would have left behind) while every entry stays
// untouched.
//
// The two assertions below are pinned by two different mutations - see each
// one's own comment - because they exercise two independent halves of
// reportLockfileDiff's own logic (the server line itself, and the
// Empty()-derived verdict term), and because the first assertion's own
// t.Fatalf would mask the second one's failure under any mutation that also
// breaks the first: the second assertion is reachable only under a mutation
// that leaves the server line intact.
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

	// Mutation (dropping reportLockfileDiff's `if diff.Server != nil` block
	// entirely, leaving no server line at all) fails this check with:
	//
	//	lock_command_test.go:1020: oks = []
	if !dryPrinter.hasOkContaining("Would change: server " + staleServer + " -> " + f.cfg.Server) {
		t.Fatalf("oks = %v", dryPrinter.oks)
	}
	// Mutation (keeping the server line, but dropping the
	// `verdict := ...; if diff.Empty() { ... }` logic and its use in the
	// PersistentPrintf format string, leaving a count-only summary) leaves
	// the check above passing, since the server line survives this mutation
	// untouched, and fails this one with:
	//
	//	lock_command_test.go:1032: persists = [Dry run: 0 would be added, 0
	//	would be updated, 0 would be removed, 1 unchanged (/.../galaxy.lock)]
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

// TestLockDryRunWarnsOnAnUnreadableBaseline proves lockDryRunBaseline's own
// disclosure: a lockfile already on disk that fails lockfile.Load (here, an
// unsupported schema version) is warned about by name rather than silently
// treated the same as "no lockfile at all", and the run still succeeds and
// still reports every collection as added - the positive control proving the
// preview did not merely abort on the unreadable file.
//
// Mutation (dropping the Warnf call in lockDryRunBaseline while keeping its
// nil return) fails with:
//
//	lock_command_test.go:1071: warns = [--dry-run is active: no artifact will
//	be downloaded, installed, or cached; the resolved metadata caches are
//	still saved no persisted snapshot was found; a dry run will not create
//	one, so the metadata caches this run built are discarded --dry-run:
//	skipping metrics report to /.../metrics.json]
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

// TestLockDryRunDoesNotFabricateASnapshot proves a dry run against a cold
// cache - no persisted snapshot yet - never creates one, mirroring
// TestInstallDryRunDoesNotFabricateASnapshot: saveDryRunSnapshotIfPersisted's
// WasPersisted() guard must refuse to save here, or a later cleanup run would
// read the resulting persisted-and-empty snapshot as "nothing is installed or
// warmed anywhere" and sweep the whole extracted store.
//
// Mutation (replacing lockDryRun's saveDryRunSnapshotIfPersisted call with a
// bare state.backend.SaveStore(ctx, state.store)) fails with:
//
//	lock_command_test.go:1108: lock --dry-run against a cold cache must not create a persisted snapshot
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

// TestLockDryRunSavesMetadataCachesWhenSnapshotExists is
// TestLockDryRunDoesNotFabricateASnapshot's positive control on the same
// fixture: once a real Lock has seeded a persisted snapshot, a later dry run
// still saves the metadata caches a genuinely fresh resolve builds. A second
// collection is registered on the server and added to requirements.yml
// between the seeding Lock and the dry run specifically so the dry run's
// resolve is not just replaying an already-cached result.
//
// The WasPersisted() check below is a PRECONDITION GUARD, not a pin: it is
// satisfied by the seeding Lock call alone, before the dry run ever runs, so
// no mutation of the dry-run path can make it fail while the requirement-spec
// check after it still passes - that property is already pinned by
// TestLockDryRunDoesNotFabricateASnapshot, on the cold-cache fixture where
// WasPersisted() genuinely depends on what the dry run itself did. What
// actually pins THIS test's own claim - that the dry run's own fresh resolve
// was saved, not just that a persisted snapshot exists from some earlier
// write - is the RequirementsSnapshot() check: acme.extra only enters that
// map through this run's own resolve, never through the seeding Lock, which
// never heard of it.
//
// Killing mutation: replacing lockDryRun's saveDryRunSnapshotIfPersisted call
// with a no-op (var saveErr error) leaves WasPersisted() true - the seed
// Lock's own save already made it true - but fails the requirement-spec
// check with:
//
//	lock_command_test.go:1168: expected the dry run's own fresh resolve to
//	have saved acme.extra's requirement spec, got map[acme.widgets:{*  galaxy []}]
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

// TestLockDryRunSkipsMetricsAndSaysSo proves writeRunMetrics's own
// cfg.DryRun guard applies to lock exactly as it does to install and warm:
// no metrics file is written, and the operator is told why on stderr. The
// two assertions are pinned by two different mutations - see each one's own
// comment below - because the call-site mutation is unobservable through the
// filesystem alone.
func TestLockDryRunSkipsMetricsAndSaysSo(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.DryRun = true
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Mutation (removing writeRunMetrics's own internal cfg.DryRun guard)
	// fails this check with:
	//
	//	lock_command_test.go:1191: expected no metrics report, stat err = <nil>
	if _, err := os.Stat(f.cfg.MetricsFile); !os.IsNotExist(err) {
		t.Fatalf("expected no metrics report, stat err = %v", err)
	}
	// Mutation (removing the writeRunMetrics call from lockDryRun entirely,
	// leaving the file-absence check above still passing) fails this check
	// with:
	//
	//	lock_command_test.go:1208: warns = [--dry-run is active: no artifact
	//	will be downloaded, installed, or cached; the resolved metadata
	//	caches are still saved no persisted snapshot was found; a dry run
	//	will not create one, so the metadata caches this run built are
	//	discarded]
	//
	// A file-absence-only version of this test does not catch that second
	// mutation at all: dropping the call site is unobservable through the
	// filesystem, since writeRunMetrics's own guard already means the file
	// would be absent either way.
	if !f.printer.hasWarnContaining("--dry-run: skipping metrics report to " + f.cfg.MetricsFile) {
		t.Fatalf("warns = %v", f.printer.warns)
	}
}

// TestLockDryRunRefusesAHostileBaselineAndSaysSo proves lock's --dry-run
// preview renders no adversarial baseline lockfile - it never loads one. A
// lockfile entry whose name is not <namespace>.<name> in the alphabet
// a Galaxy server itself accepts is refused by lockfile.Load, and
// lockDryRunBaseline's documented policy takes over from there: warn on
// stderr, then report every collection as added, exactly as it does for any
// other baseline it cannot read.
//
// Refusing such a name is where this boundary belongs, rather than rendering
// it safely: printing a forged line is the harm, and the printer's
// sanitization deliberately keeps "\n" so it cannot be the thing that
// prevents it. That printer boundary still covers what no name alphabet can
// reach - a server's error text, a filesystem path, a manifest.
//
// Assertion (1) is what keeps this a production-path test rather than a
// lockfile unit test wearing this file's name: the refusal is observed
// through lockfile.Load on a file lockfile.Save itself wrote, so the round
// trip is real. (2) and (3) then prove the preview degraded into its
// documented no-baseline behavior instead of failing the run or acting on the
// entry.
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

	// (3) the run still succeeded and still reported the legitimate
	// collection as added, which is lockDryRunBaseline's documented behavior
	// for a baseline it cannot read - and the control proving the refusal did
	// not simply abort the preview.
	if !f.printer.hasOkContaining("Would add: acme.widgets@1.0.0") {
		t.Fatalf("oks = %v", f.printer.oks)
	}
	if !f.printer.hasWarnContaining("cannot be read") {
		t.Fatalf("expected a warning naming the unreadable baseline, warns = %v", f.printer.warns)
	}
}

// assertHostileNameIsRefusedByLoad is assertion (1) from
// TestLockDryRunRefusesAHostileBaselineAndSaysSo's own doc comment: the
// production path (lockfile.Save then lockfile.Load) refuses the hostile name
// on the way back in, so nothing downstream ever holds it. Factored out to
// keep the caller's own cyclomatic complexity within budget.
func assertHostileNameIsRefusedByLoad(t *testing.T, path string) {
	t.Helper()
	loaded, err := lockfile.Load(path)
	if !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("lockfile.Load(%s) = (%+v, %v), want errors.Is helpers.ErrLockfileInvalid", path, loaded, err)
	}
}

// assertNoPathMatchesHostileName is assertion (3) from
// TestLockDryRunRefusesAHostileBaselineAndSaysSo's own doc comment:
// the preview never touched the filesystem on the hostile entry's behalf -
// no path anywhere under root resolves to, or is even named after, the
// traversal target "passwd". Factored out to keep the caller's own
// cyclomatic complexity within budget; the WalkDir callback records the
// first match into a closure variable rather than returning a dynamically
// constructed error, since err113 forbids exactly that.
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
