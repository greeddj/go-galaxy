package collections_test

// This file exercises `outdated` end to end against both a real fake Galaxy
// server (fakegalaxy) and a raw, hand-rolled HTTP/1.1 responder that answers
// a request with an attacker-controlled status-line reason phrase - the one
// shape fakegalaxy cannot produce, since http.ResponseWriter's own status
// line is never operator-influenced. Together with dry_run_e2e_test.go's
// TestOutdatedDryRunMutatesNothing, this is the suite proving outdated's
// report is sanitized on the same boundary as every other operator-facing
// line, honors --metrics-file (including under --dry-run, where the shared
// writeRunMetrics guard suppresses it), discloses every flag it cannot
// honor, and classifies its own failures onto the documented exit codes.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// rawStatusLineServer answers every request on a fresh connection with a
// fixed, verbatim HTTP/1.1 status line - including a reason phrase this test
// controls byte for byte - followed by an empty, non-cached JSON-shaped
// body. It exists because fakegalaxy always renders its status line through
// net/http's own http.ResponseWriter, which never lets a caller put an
// arbitrary byte sequence into the reason phrase; only a raw net.Listen
// responder can produce the exact wire bytes a hostile or badly configured
// real Galaxy deployment could.
type rawStatusLineServer struct {
	listener net.Listener
	url      string
}

// newRawStatusLineServer starts the responder and registers its shutdown via
// t.Cleanup. statusLine is written verbatim as the response's first line
// (e.g. "HTTP/1.1 404 <reason phrase>"), terminated with the server's own
// "\r\n" plus a Content-Length: 0 and Connection: close pair, so every
// accepted connection serves exactly one request before it is closed - a
// new connection is required for each subsequent one, which
// http.Transport's own dialer handles transparently for the client under
// test.
func newRawStatusLineServer(t *testing.T, statusLine string) *rawStatusLineServer {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	srv := &rawStatusLineServer{listener: ln, url: "http://" + ln.Addr().String()}
	go srv.acceptLoop(statusLine)
	t.Cleanup(func() { _ = ln.Close() })
	return srv
}

// acceptLoop serves connections until the listener is closed by the test's
// t.Cleanup, at which point Accept returns an error and the loop exits.
func (s *rawStatusLineServer) acceptLoop(statusLine string) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go serveOneRawStatusLine(conn, statusLine)
	}
}

// serveOneRawStatusLine reads exactly one HTTP request off conn - discarding
// it, since every test using this fixture cares only about the response -
// then writes the fixed status line and closes the connection. A malformed
// or absent request (e.g. the client gave up before sending headers) is
// answered with nothing further; the connection close alone is enough to
// unblock a caller waiting on it.
func serveOneRawStatusLine(conn net.Conn, statusLine string) {
	defer func() { _ = conn.Close() }()
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return
	}
	_ = req.Body.Close()
	_, _ = conn.Write([]byte(statusLine + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
}

// saveOutdatedLockfile writes a minimal lockfile at requirementsFile's
// default lockfile path, containing exactly the given entries, each sourced
// from server - the shape every outdated e2e fixture in this file needs
// before it can call collections.Outdated.
func saveOutdatedLockfile(t *testing.T, requirementsFile, server string, entries ...lockfile.Entry) string {
	t.Helper()
	lockPath := lockfile.ResolveDefaultPath(requirementsFile, "")
	for i := range entries {
		if entries[i].Source == "" {
			entries[i].Source = server
		}
	}
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        server,
		Collections:   entries,
	}
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
	return lockPath
}

// TestOutdatedSanitizesServerReasonPhrase proves a server's HTTP reason
// phrase - here carrying a screen-clear sequence, a cursor-home sequence, a
// color-set sequence, and a bell, exactly the payload a real terminal would
// act on - cannot reach the operator's terminal as raw control bytes. It
// lands on stderr specifically (a lookup failure is a diagnostic, kept off
// stdout), and the printable substring inside the payload still appears once
// sanitized - the positive control proving the message was printed and
// cleaned, not merely swallowed.
//
// The two sanitization assertions below are exact, not a sample of the
// injected bytes, and neither can be "every byte this test injects is
// individually absent": progress.Progress decorates every result-tier line
// with its own SGR escape codes unconditionally, not only on a terminal, so
// a blanket "stderr carries zero ESC bytes" assertion cannot hold there -
// see progress.go's ansiRed/ansiGreen/ansiReset. Instead:
//   - bytes.Count(stderr, U+FFFD) == 4 counts the replacement character
//     itself, one per injected C0 byte (the three ESC bytes opening the
//     screen-clear, cursor-home, and color-set sequences, plus the one BEL).
//     No prefix this program's printer emits ever contains U+FFFD, so this
//     single count catches every injected byte at once - including the
//     color-set sequence's own ESC, which a bytes.Contains check for it
//     could never assert on its own, since the printer's own ansiGreen
//     ("\x1b[1m\x1b[32m") contains that exact byte sequence as a legitimate
//     substring.
//   - stdout carries zero ESC bytes at all: in this fixture stdout holds
//     only the markerless PersistentPrintf summary line, which the printer
//     never colors, so this is both achievable and a real assertion.
//
// Deliberately not t.Parallel(): it drives a real progress.Progress through
// captureStdIO, which swaps the process-wide os.Stdout/os.Stderr for its
// duration - see captureStdIO's own doc comment in token_leak_e2e_test.go
// for why every test doing that in this package runs un-parallelized.
//
// Mutation: reverting reportOutdated's failure-line Errorf call to a bare
// fmt.Printf makes the replacement-count assertion fail with `stderr
// carries 0 U+FFFD replacement characters, want 4: ""` (every byte moved to
// stdout, so stderr is empty), the ESC-on-stdout assertion fail with
// `stdout contains a raw ESC byte at index 67: "Lookup failed:
// \"acme.widgets\"@1.0.0: failed to fetch metadata: 404 \x1b[2J\x1b[1;1H
// \x1b[32mEVERYTHING IS UP TO DATE\a (http://127.0.0.1:60058/api/collections/
// acme/widgets)\n.../galaxy.lock: 0 up to date, 0 outdated, 1
// failed\n"`, and the two stream-separation assertions plus the positive
// control fail as well ("expected the failure line to stay off stdout",
// "expected the failure line on stderr, got stderr=\"\"", and "expected the
// sanitized reason phrase text to still be printed, got stderr=\"\"") - run
// and confirmed.
func TestOutdatedSanitizesServerReasonPhrase(t *testing.T) {
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")

	raw := newRawStatusLineServer(t, "HTTP/1.1 404 \x1b[2J\x1b[1;1H\x1b[32mEVERYTHING IS UP TO DATE\a")
	saveOutdatedLockfile(t, reqPath, raw.url, lockfile.Entry{Name: "acme.widgets", Version: "1.0.0"})

	cfg := &config.Config{
		Server:           raw.url,
		RequirementsFile: reqPath,
		Workers:          1,
		Timeout:          e2eTimeout,
	}

	var outErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := infra.New(printer, fetch.New(cfg.Timeout, nil))
		outErr = collections.Outdated(context.Background(), cfg, runtime)
	})
	if outErr == nil {
		t.Fatal("expected an error from the failed lookup")
	}

	const wantReplacements = 4 // three ESC (screen-clear, cursor-home, color-set) plus one BEL
	if got := bytes.Count(stderr, []byte("�")); got != wantReplacements {
		t.Errorf("stderr carries %d U+FFFD replacement characters, want %d: %q", got, wantReplacements, stderr)
	}
	if idx := bytes.IndexByte(stdout, 0x1b); idx >= 0 {
		t.Errorf("stdout contains a raw ESC byte at index %d: %q", idx, stdout)
	}
	if bytes.Contains(stdout, []byte("Lookup failed")) {
		t.Errorf("expected the failure line to stay off stdout, got stdout=%q", stdout)
	}
	if !bytes.Contains(stderr, []byte("Lookup failed")) {
		t.Errorf("expected the failure line on stderr, got stderr=%q", stderr)
	}
	if !bytes.Contains(stderr, []byte("EVERYTHING IS UP TO DATE")) {
		t.Errorf("expected the sanitized reason phrase text to still be printed, got stderr=%q", stderr)
	}
}

// TestOutdatedRefusesAHostileLockfileEntryName covers one channel into the
// report - a lockfile entry's own Name - and pins that it never reaches the
// report at all. A name outside the alphabet a Galaxy server itself accepts
// is refused by lockfile.Load, so outdated fails before it builds a URL,
// before it prints a line, and before it touches the network.
//
// The printer boundary itself is untouched and still proven here:
// TestOutdatedSanitizesServerReasonPhrase covers the channel no name
// alphabet can reach, a server's own HTTP reason phrase, and it is that
// test - not this one - which pins that safeout.Clean's replacement
// actually fires for this report. A name refused at load never reaches the
// printer at all, so these rows pin the refusal rather than anything about
// how a value that does reach the report is rendered.
//
// The two rows differ in which check refuses them, and both are kept because
// a single alphabet check replacing two different rejections is exactly the
// kind of change that could silently narrow to one: the first is a name that
// splits into two halves and fails the alphabet, the second fails the split
// itself. The third row is the control: a well-formed name on the identical
// fixture must load, be looked up, and be reported, so the two refusals
// cannot be the fixture failing to reach the report path at all.
func TestOutdatedRefusesAHostileLockfileEntryName(t *testing.T) {
	// t.Run, not t.Parallel(): every row drives captureStdIO, which swaps the
	// process-wide os.Stdout/os.Stderr.
	t.Run("control byte in a two-part name", func(t *testing.T) {
		assertOutdatedRefusesName(t, "acme."+string(rune(0x9b))+"widgets")
	})
	t.Run("three dot-separated parts", func(t *testing.T) {
		assertOutdatedRefusesName(t, "acme."+string(rune(0x9b))+".widgets")
	})
	t.Run("well-formed name is reported", testOutdatedReportsAWellFormedName)
}

// assertOutdatedRefusesName runs outdated against a lockfile holding exactly
// one entry named hostileName and asserts the run is refused at load: the
// lockfile exit class, no request to the server, and nothing printed on
// either stream that carries the name.
func assertOutdatedRefusesName(t *testing.T, hostileName string) {
	t.Helper()

	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	s := fakegalaxy.New(t)
	saveOutdatedLockfile(t, reqPath, s.URL(), lockfile.Entry{Name: hostileName, Version: "1.0.0"})

	cfg := &config.Config{Server: s.URL(), RequirementsFile: reqPath, Workers: 1}

	var outErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := infra.New(printer, s.Client())
		outErr = collections.Outdated(context.Background(), cfg, runtime)
	})

	if !errors.Is(outErr, helpers.ErrLockfileInvalid) {
		t.Fatalf("Outdated error = %v, want errors.Is helpers.ErrLockfileInvalid", outErr)
	}
	if got := exitcode.FromError(outErr); got != exitcode.ExitLock {
		t.Errorf("exitcode.FromError(err) = %d, want ExitLock (%d)", got, exitcode.ExitLock)
	}
	if got := s.Total(); got != 0 {
		t.Errorf("fake server saw %d requests, want 0: the name must be refused before any network call", got)
	}
	// The refusal message names the offending value, quoted, which is the
	// only place it may still appear - a report line built from it must not.
	if bytes.Contains(stdout, []byte("Lookup failed")) || bytes.Contains(stderr, []byte("Lookup failed")) {
		t.Errorf("a refused entry still produced a report line: stdout=%q stderr=%q", stdout, stderr)
	}
}

// testOutdatedReportsAWellFormedName is the control for the two refusals
// above: the identical fixture with a name inside the alphabet must reach the
// report. It is registered on the fake server so the lookup succeeds and the
// run exits cleanly, which is what proves the refusals come from the name and
// not from the fixture. A newer version is registered beside the locked one
// so the entry is outdated, the verdict a default run prints a line for.
func testOutdatedReportsAWellFormedName(t *testing.T) {
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	s := fakegalaxy.New(t)
	s.AddVersion("acme", "widgets", "1.0.0", nil)
	s.AddVersion("acme", "widgets", "2.0.0", nil)
	saveOutdatedLockfile(t, reqPath, s.URL(), lockfile.Entry{Name: "acme.widgets", Version: "1.0.0"})

	cfg := &config.Config{Server: s.URL(), RequirementsFile: reqPath, Workers: 1}

	var outErr error
	stdout, _ := captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := infra.New(printer, s.Client())
		outErr = collections.Outdated(context.Background(), cfg, runtime)
	})
	if outErr != nil {
		t.Fatalf("Outdated with a well-formed name: %v", outErr)
	}
	if !bytes.Contains(stdout, []byte("acme.widgets")) {
		t.Errorf("expected the well-formed name in the report, got stdout=%q", stdout)
	}
}

// outdatedVerdictsRun is what runOutdatedVerdicts observed: each stream's
// bytes, the lockfile path the summary line names, and the run's error.
type outdatedVerdictsRun struct {
	err      error
	lockPath string
	stdout   []byte
	stderr   []byte
}

// runOutdatedVerdicts runs outdated through a real progress.Progress over a
// lockfile holding one entry of each verdict - acme.current up to date,
// acme.stale outdated, acme.missing a lookup that 404s.
func runOutdatedVerdicts(t *testing.T, verbose, quiet bool) outdatedVerdictsRun {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "current", "1.0.0", nil)
	s.AddVersion("acme", "stale", "1.0.0", nil)
	s.AddVersion("acme", "stale", "2.0.0", nil)
	// acme.missing is never registered, so its root-metadata lookup 404s.

	var run outdatedVerdictsRun
	run.lockPath = saveOutdatedLockfile(t, reqPath, s.URL(),
		lockfile.Entry{Name: "acme.current", Version: "1.0.0"},
		lockfile.Entry{Name: "acme.stale", Version: "1.0.0"},
		lockfile.Entry{Name: "acme.missing", Version: "1.0.0"},
	)

	cfg := &config.Config{
		Server:           s.URL(),
		RequirementsFile: reqPath,
		Workers:          2,
		Verbose:          verbose,
		Quiet:            quiet,
	}

	run.stdout, run.stderr = captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := infra.New(printer, s.Client())
		run.err = collections.Outdated(context.Background(), cfg, runtime)
	})
	return run
}

// TestOutdatedQuietStillReportsAndSplitsStreams proves every line a default
// report prints is result tier: --quiet suppresses none of them, and a
// failed lookup still lands on stderr while the outdated line and the
// summary stay on stdout. The up-to-date entry has no line to suppress -
// only --verbose prints one - and is still counted in the summary.
//
// Deliberately not t.Parallel(); see
// TestOutdatedSanitizesServerReasonPhrase's own doc comment for why.
func TestOutdatedQuietStillReportsAndSplitsStreams(t *testing.T) {
	run := runOutdatedVerdicts(t, false, true)
	stdout, stderr, lockPath := run.stdout, run.stderr, run.lockPath
	if run.err == nil {
		t.Fatal("expected an error from the failed acme.missing lookup")
	}

	if bytes.Contains(stdout, []byte("Up to date")) {
		t.Errorf("expected no up-to-date line without --verbose, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte(lockPath+": 1 up to date, 1 outdated, 1 failed")) {
		t.Errorf("expected the summary to count the unprinted up-to-date entry, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Outdated: acme.stale 1.0.0 -> 2.0.0")) {
		t.Errorf("expected the outdated line on stdout despite --quiet, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte(lockPath)) {
		t.Errorf("expected the summary line on stdout despite --quiet, got stdout=%q", stdout)
	}
	if bytes.Contains(stdout, []byte("Lookup failed")) {
		t.Errorf("expected the failure line to stay off stdout even under --quiet, got stdout=%q", stdout)
	}
	if !bytes.Contains(stderr, []byte(`Lookup failed: "acme.missing"@1.0.0`)) {
		t.Errorf("expected the failure line on stderr, got stderr=%q", stderr)
	}
}

// TestOutdatedVerboseReportsUpToDate proves --verbose adds the up-to-date
// line on stdout in the shape install prints a settled subject in: the
// success marker, the name, then the version as "== <version>" rather than
// joined to the name with an @.
//
// Deliberately not t.Parallel(); see
// TestOutdatedSanitizesServerReasonPhrase's own doc comment for why.
func TestOutdatedVerboseReportsUpToDate(t *testing.T) {
	run := runOutdatedVerdicts(t, true, false)
	stdout, lockPath := run.stdout, run.lockPath
	if run.err == nil {
		t.Fatal("expected an error from the failed acme.missing lookup")
	}
	if !bytes.Contains(stdout, []byte("✔ Up to date: acme.current == 1.0.0\n")) {
		t.Errorf("expected the up-to-date line on stdout under --verbose, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte(lockPath+": 1 up to date, 1 outdated, 1 failed")) {
		t.Errorf("expected the summary line on stdout, got stdout=%q", stdout)
	}
}

// outdatedMetricsFixture builds the three-entry lockfile (one up to date,
// one outdated, one that 404s) shared by TestOutdatedWritesMetricsReport and
// its dry-run counterpart, returning the lockfile path and a *config.Config
// with RequirementsFile and Server already set.
func outdatedMetricsFixture(t *testing.T) (*fakegalaxy.Server, *config.Config, string) {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "current", "1.0.0", nil)
	s.AddVersion("acme", "stale", "1.0.0", nil)
	s.AddVersion("acme", "stale", "2.0.0", nil)

	lockPath := saveOutdatedLockfile(t, reqPath, s.URL(),
		lockfile.Entry{Name: "acme.current", Version: "1.0.0"},
		lockfile.Entry{Name: "acme.stale", Version: "1.0.0"},
		lockfile.Entry{Name: "acme.missing", Version: "1.0.0"},
	)

	cfg := &config.Config{
		Server:           s.URL(),
		RequirementsFile: reqPath,
		Workers:          2,
	}
	return s, cfg, lockPath
}

// TestOutdatedWritesMetricsReport proves outdated honors --metrics-file: the
// report's collections/failures fields describe the run's own work (three
// lookups, one failure), cache_hits/cache_misses/bytes_downloaded are a
// truthful 0/0/0 since outdated never touches an ArtifactStore, lockfile and
// lockfile_hash are populated, and frozen is absent even though cfg.Frozen
// is set - proving writeRunMetrics is called with a literal false rather
// than cfg.Frozen.
//
// Mutation: passing cfg.Frozen instead of a literal false to writeRunMetrics
// makes the frozen-absence assertion below fail with `expected "frozen" to
// be absent from the report despite cfg.Frozen, got true` - run and
// confirmed.
func TestOutdatedWritesMetricsReport(t *testing.T) {
	t.Parallel()
	s, cfg, lockPath := outdatedMetricsFixture(t)
	metricsPath := filepath.Join(t.TempDir(), "metrics.json")
	cfg.MetricsFile = metricsPath
	// Set despite outdated never honoring it, specifically to prove the
	// report's own "frozen" field stays absent regardless.
	cfg.Frozen = true

	runtime := infra.New(noopPrinter{}, s.Client())
	if err := collections.Outdated(context.Background(), cfg, runtime); err == nil {
		t.Fatal("expected an error from the failed acme.missing lookup")
	}

	report := readMetricsReport(t, metricsPath)
	assertOutdatedMetricsReport(t, report, lockPath)
}

// readMetricsReport reads and decodes the JSON metrics report at path into a
// generic map, so a test can assert individual fields (including a field's
// deliberate absence, which a typed metrics.Report struct would hide behind
// its own zero value) without depending on the full Report shape.
func readMetricsReport(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // path is this test's own temp dir.
	if err != nil {
		t.Fatalf("read metrics file: %v", err)
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal metrics report: %v", err)
	}
	return report
}

// assertOutdatedMetricsReport checks every field TestOutdatedWritesMetricsReport
// cares about, split out of that test purely to stay under the
// cyclomatic-complexity budget: the two together still cover the same set of
// assertions.
func assertOutdatedMetricsReport(t *testing.T, report map[string]any, lockPath string) {
	t.Helper()
	if got := report["command"]; got != "outdated" {
		t.Errorf("command = %v, want %q", got, "outdated")
	}
	if got := report["collections"]; got != float64(3) {
		t.Errorf("collections = %v, want 3", got)
	}
	if got := report["failures"]; got != float64(1) {
		t.Errorf("failures = %v, want 1", got)
	}
	if got, ok := report["frozen"]; ok {
		t.Errorf("expected \"frozen\" to be absent from the report despite cfg.Frozen, got %v", got)
	}
	if got := report["cache_hits"]; got != float64(0) {
		t.Errorf("cache_hits = %v, want 0", got)
	}
	if got := report["cache_misses"]; got != float64(0) {
		t.Errorf("cache_misses = %v, want 0", got)
	}
	if got := report["bytes_downloaded"]; got != float64(0) {
		t.Errorf("bytes_downloaded = %v, want 0", got)
	}
	if got, _ := report["lockfile"].(string); got != lockPath {
		t.Errorf("lockfile = %q, want %q", got, lockPath)
	}
	if got, _ := report["lockfile_hash"].(string); got == "" {
		t.Error("expected a non-empty lockfile_hash")
	}
}

// TestOutdatedDryRunSuppressesMetricsReport proves --dry-run's only real
// effect on outdated: writeRunMetrics' own shared cfg.DryRun guard suppresses
// the report and warns naming the skipped path, exactly as it does for
// install/warm/lock - outdated grows no cfg.DryRun branch of its own.
//
// Deliberately not t.Parallel(); see
// TestOutdatedSanitizesServerReasonPhrase's own doc comment for why.
func TestOutdatedDryRunSuppressesMetricsReport(t *testing.T) {
	s, cfg, _ := outdatedMetricsFixture(t)
	metricsPath := filepath.Join(t.TempDir(), "metrics.json")
	cfg.MetricsFile = metricsPath
	cfg.DryRun = true

	var outErr error
	_, stderr := captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := infra.New(printer, s.Client())
		outErr = collections.Outdated(context.Background(), cfg, runtime)
	})
	if outErr == nil {
		t.Fatal("expected an error from the failed acme.missing lookup")
	}

	if _, statErr := os.Stat(metricsPath); !os.IsNotExist(statErr) {
		t.Errorf("expected no metrics file written under --dry-run, stat error = %v", statErr)
	}
	if !bytes.Contains(stderr, []byte(metricsPath)) {
		t.Errorf("expected a stderr warning naming the skipped metrics path, got stderr=%q", stderr)
	}
}

// TestOutdatedWarnsAboutUnhonoredFlags proves warnUnhonoredFlags fires
// exactly once, naming every configured flag outdated cannot honor, and
// changes nothing about the run itself: the same server sees the identical
// number of requests whether or not the flags are set. The "none set" run
// is this test's positive control, proving the warning is conditional
// rather than unconditionally printed.
//
// Deliberately not t.Parallel(); see
// TestOutdatedSanitizesServerReasonPhrase's own doc comment for why.
func TestOutdatedWarnsAboutUnhonoredFlags(t *testing.T) {
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "widgets", "1.0.0", nil)
	saveOutdatedLockfile(t, reqPath, s.URL(), lockfile.Entry{Name: "acme.widgets", Version: "1.0.0"})

	newCfg := func() *config.Config {
		return &config.Config{Server: s.URL(), RequirementsFile: reqPath, Workers: 1}
	}
	run := func(cfg *config.Config) ([]byte, []byte) {
		var err error
		stdout, stderr := captureStdIO(t, func() {
			printer := progress.New(cfg.Verbose, cfg.Quiet)
			defer printer.Close()
			runtime := infra.New(printer, s.Client())
			err = collections.Outdated(context.Background(), cfg, runtime)
		})
		if err != nil {
			t.Fatalf("Outdated: %v", err)
		}
		return stdout, stderr
	}

	_, cleanStderr := run(newCfg())
	cleanTotal := s.Total()
	if bytes.Contains(cleanStderr, []byte("does not honor")) {
		t.Errorf("expected no unhonored-flags warning for a clean config, got stderr=%q", cleanStderr)
	}

	s.ResetCounts()
	flaggedCfg := newCfg()
	flaggedCfg.Refresh = true
	flaggedCfg.NoCache = true
	_, flaggedStderr := run(flaggedCfg)
	flaggedTotal := s.Total()

	warningLines := 0
	for line := range bytes.SplitSeq(flaggedStderr, []byte("\n")) {
		if bytes.Contains(line, []byte("does not honor")) {
			warningLines++
		}
	}
	if warningLines != 1 {
		t.Errorf("expected exactly one unhonored-flags warning line, got %d in stderr=%q", warningLines, flaggedStderr)
	}
	if !bytes.Contains(flaggedStderr, []byte("--refresh")) {
		t.Errorf("expected the warning to name --refresh, got stderr=%q", flaggedStderr)
	}
	if !bytes.Contains(flaggedStderr, []byte("--no-cache")) {
		t.Errorf("expected the warning to name --no-cache, got stderr=%q", flaggedStderr)
	}
	if flaggedTotal != cleanTotal {
		t.Errorf("server saw %d requests with --refresh --no-cache set, %d with neither; expected them equal", flaggedTotal, cleanTotal)
	}
}

// TestOutdatedFailedLookupExitCode pins outdated's exit-code classification
// for its two distinct lookup-failure shapes: a plain 404 (the network
// class) and a lockfile entry whose name is not a "namespace.name" FQDN
// (the lockfile class, which must win even though both failures are joined
// behind the identical helpers.ErrLatestVersionLookupFailed headline).
func TestOutdatedFailedLookupExitCode(t *testing.T) {
	t.Parallel()

	t.Run("404 lookup classifies ExitNetwork", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		reqPath := filepath.Join(root, "requirements.yml")
		s := fakegalaxy.New(t)
		saveOutdatedLockfile(t, reqPath, s.URL(), lockfile.Entry{Name: "acme.missing", Version: "1.0.0"})

		cfg := &config.Config{Server: s.URL(), RequirementsFile: reqPath, Workers: 1}
		runtime := infra.New(noopPrinter{}, s.Client())
		err := collections.Outdated(context.Background(), cfg, runtime)
		if err == nil {
			t.Fatal("expected an error from the 404 lookup")
		}
		if got := exitcode.FromError(err); got != exitcode.ExitNetwork {
			t.Errorf("exitcode.FromError(err) = %d, want ExitNetwork (%d); err=%v", got, exitcode.ExitNetwork, err)
		}
	})

	t.Run("invalid lockfile entry name classifies ExitLock", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		reqPath := filepath.Join(root, "requirements.yml")
		s := fakegalaxy.New(t)
		saveOutdatedLockfile(t, reqPath, s.URL(), lockfile.Entry{Name: "nodothere", Version: "1.0.0"})

		cfg := &config.Config{Server: s.URL(), RequirementsFile: reqPath, Workers: 1}
		runtime := infra.New(noopPrinter{}, s.Client())
		err := collections.Outdated(context.Background(), cfg, runtime)
		if err == nil {
			t.Fatal("expected an error from the invalid lockfile entry name")
		}
		if got := exitcode.FromError(err); got != exitcode.ExitLock {
			t.Errorf("exitcode.FromError(err) = %d, want ExitLock (%d); err=%v", got, exitcode.ExitLock, err)
		}
	})
}
