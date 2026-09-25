package collections_test

// This file drives outdated end to end against fakegalaxy and a raw HTTP/1.1
// responder with a reason phrase fakegalaxy cannot produce: sanitized output,
// --metrics-file, the unhonored-flags warning and exit-code classification.

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

// rawStatusLineServer answers every request with a verbatim HTTP/1.1 status
// line, reason phrase included, which net/http's ResponseWriter (and so
// fakegalaxy) never lets a caller choose byte for byte.
type rawStatusLineServer struct {
	listener net.Listener
	url      string
}

// newRawStatusLineServer starts the responder and closes it via t.Cleanup.
// Each connection serves one request: statusLine, an empty body, and
// Connection: close.
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

// serveOneRawStatusLine discards one request read from conn, writes the fixed
// status line and closes; an unreadable request gets only the close.
func serveOneRawStatusLine(conn net.Conn, statusLine string) {
	defer func() { _ = conn.Close() }()
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return
	}
	_ = req.Body.Close()
	_, _ = conn.Write([]byte(statusLine + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
}

// saveOutdatedLockfile writes requirementsFile's default lockfile holding
// exactly entries, each sourced from server unless it names a source, and
// each Galaxy entry given the download_url it needs to load.
func saveOutdatedLockfile(t *testing.T, requirementsFile, server string, entries ...lockfile.Entry) string {
	t.Helper()
	lockPath := lockfile.ResolveDefaultPath(requirementsFile, "")
	for i := range entries {
		if entries[i].Source == "" {
			entries[i].Source = server
		}
		if entries[i].IsGalaxy() && entries[i].DownloadURL == "" {
			entries[i].DownloadURL = lockedDownloadURLFor(server, entries[i].Name, entries[i].Version)
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

// TestOutdatedSanitizesServerReasonPhrase pins that control bytes in a
// server's reason phrase reach stderr only as U+FFFD, text kept, and never
// stdout. Not parallel: captureStdIO swaps the process-wide os.Stdout/Stderr.
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

// TestOutdatedRefusesAHostileLockfileEntryName pins that lockfile.Load refuses
// a name failing the alphabet and one failing the two-part split before any
// request or report line; the third row is the well-formed control.
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

// assertOutdatedRefusesName runs outdated over one entry named hostileName and
// asserts a refusal at load: ExitLock, no request, no report line.
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

// testOutdatedReportsAWellFormedName is the refusals' control: a registered,
// well-formed name on the same fixture is looked up and reported as outdated,
// so the refusals come from the name rather than the fixture.
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

// TestOutdatedQuietStillReportsAndSplitsStreams pins that --quiet suppresses
// no report line, a failed lookup stays on stderr, and the summary still counts
// the unprinted up-to-date entry. Not parallel: it uses captureStdIO.
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

// TestOutdatedVerboseReportsUpToDate pins that --verbose adds the up-to-date
// line in install's shape: marker, name, then "== <version>". Not parallel:
// it uses captureStdIO.
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

// outdatedMetricsFixture builds the three-entry lockfile (up to date, outdated,
// a 404) the metrics tests share, and returns the server, a config and the
// lockfile path.
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

// TestOutdatedWritesMetricsReport pins outdated's --metrics-file report: three
// lookups, one failure, zero cache traffic, the lockfile and its hash, and no
// frozen field even with cfg.Frozen set.
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

// readMetricsReport decodes the metrics report at path into a generic map, so
// a test can assert a field's absence, which a typed struct would hide.
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

// assertOutdatedMetricsReport checks the fields TestOutdatedWritesMetricsReport
// pins, split out only for the cyclomatic-complexity budget.
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

// TestOutdatedDryRunSuppressesMetricsReport pins that --dry-run suppresses the
// metrics report and warns naming its path, through writeRunMetrics' shared
// guard alone. Not parallel: it uses captureStdIO.
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

// TestOutdatedWarnsAboutUnhonoredFlags pins exactly one warning line naming
// the configured inert flags, none for a clean config, and an unchanged
// request count. Not parallel: it uses captureStdIO.
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

// TestOutdatedFailedLookupExitCode pins the exit class of each failure shape:
// a 404 lookup is ExitNetwork, and a lockfile entry name that is not
// "namespace.name" is ExitLock.
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
