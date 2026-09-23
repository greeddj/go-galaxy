package collections

// A failing SaveStore in installWithState, warmWithState or lockWithState
// still writes the metrics report and keeps the run's own failure class, with
// the save error joined behind it rather than replacing it.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// errSaveFailSentinel stands in for a backend's SaveStore failure (disk full,
// an S3 partition) unrelated to whether any collection succeeded.
var errSaveFailSentinel = errors.New("simulated SaveStore failure")

// errPrimarySentinel stands in for the run's primary error that
// annotateSaveFailure is asked to preserve.
var errPrimarySentinel = errors.New("simulated primary failure")

// metricsCommandLock is the metrics report's "command" value for a lock run,
// shared by every test in the package that asserts on it.
const metricsCommandLock = "lock"

// saveFailBackend wraps a real cacheManager.Backend and overrides only
// SaveStore to always fail; every other method is the real backend's.
type saveFailBackend struct {
	cacheManager.Backend
}

// SaveStore always fails, simulating a persistence failure at the very last
// step of a run.
func (saveFailBackend) SaveStore(context.Context, *store.Store) error {
	return errSaveFailSentinel
}

// readMetricsCommand returns the "command" field of the metrics file at path,
// failing the test when it cannot be read or decoded (readMetricsReport).
func readMetricsCommand(t *testing.T, path string) string {
	t.Helper()
	command, _ := readMetricsReport(t, path)["command"].(string)
	return command
}

// TestAnnotateSaveFailurePassesPrimaryUnchangedWhenSaveSucceeds pins that a
// nil save error returns the primary error itself, not a rewrap; errors.Is
// would hold for an unconditional wrap, so only identity catches it.
func TestAnnotateSaveFailurePassesPrimaryUnchangedWhenSaveSucceeds(t *testing.T) {
	t.Parallel()

	//nolint:err113,errorlint // identity is the assertion: errors.Is would also
	// hold for an unconditional wrap, which is exactly the regression this test
	// exists to catch, so comparing the values is the only check that bites.
	if got := annotateSaveFailure(errPrimarySentinel, nil); got != errPrimarySentinel {
		t.Fatalf("expected the primary error returned unchanged, got %v", got)
	}
}

// TestWarmWithStateSaveFailureWritesMetricsNoCollections pins that a warm with
// nothing to do returns the save error and still writes the metrics report,
// since writeRunMetrics is not gated on the save succeeding.
func TestWarmWithStateSaveFailureWritesMetricsNoCollections(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))
	metricsPath := filepath.Join(root, "metrics.json")

	cfg := &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		Workers:          1,
		Offline:          true,
	}
	state := newSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, http.DefaultClient)

	err := warmWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}

	if got := readMetricsCommand(t, metricsPath); got != "warm" {
		t.Errorf("metrics file not written despite the save failure: command = %q, want %q", got, "warm")
	}
}

// TestWarmWithStateSaveFailureKeepsWarmFailureClass pins that a warm with a
// failed download and a failed save still matches ErrInstallationFailed, so
// the exit class survives, with the save error joined as context.
func TestWarmWithStateSaveFailureKeepsWarmFailureClass(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)
	srv.Fail(fakegalaxy.EndpointArtifact, "acme", "widgets", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())

	err := warmWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is helpers.ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}
	// The 503 is the per-collection cause: the headline, that cause and the
	// save error must all stay matchable through the real pipeline.
	if !errors.Is(err, helpers.ErrDownloadFailed) {
		t.Fatalf("expected errors.Is helpers.ErrDownloadFailed, got %v", err)
	}
}

// TestInstallWithStateSaveFailureWritesMetrics pins that installWithState
// writes the metrics report after finalizeInstall's save attempt whether or
// not that save succeeded.
func TestInstallWithStateSaveFailureWritesMetrics(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))
	metricsPath := filepath.Join(root, "metrics.json")

	cfg := &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
		Offline:          true,
	}
	state := newSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, http.DefaultClient)

	err := installWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}

	if got := readMetricsCommand(t, metricsPath); got != "install" {
		t.Errorf("metrics file not written despite the save failure: command = %q, want %q", got, "install")
	}
}

// TestInstallWithStateSaveFailureKeepsInstallFailureClass pins that a failed
// collection outranks a failed save in finalizeInstall, so the run keeps
// ErrInstallationFailed instead of degrading to the generic exit code.
func TestInstallWithStateSaveFailureKeepsInstallFailureClass(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)
	srv.Fail(fakegalaxy.EndpointArtifact, "acme", "widgets", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
	}
	state := newSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())

	err := installWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is helpers.ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}
	// The 503 is the per-collection cause: the headline, that cause and the
	// save error must all stay matchable through the real pipeline.
	if !errors.Is(err, helpers.ErrDownloadFailed) {
		t.Fatalf("expected errors.Is helpers.ErrDownloadFailed, got %v", err)
	}
}

// TestLockWithStateSaveFailureWritesMetricsAndKeepsLockfile pins that lock
// writes and announces the lockfile before the save, and writes metrics after
// it; the fatal metrics read runs last so it cannot mask (b) or (c).
func TestLockWithStateSaveFailureWritesMetricsAndKeepsLockfile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))
	metricsPath := filepath.Join(root, "metrics.json")

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		Workers:          1,
	}
	state := newSaveFailState(t, cfg)
	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())

	err := lockWithState(context.Background(), cfg, runtime, state, time.Now())

	// (a) the returned error is the save failure.
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}

	// (b) the lockfile itself was written and is valid, independent of the
	// snapshot save's outcome.
	path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, loadErr := lockfile.Load(path)
	if loadErr != nil {
		t.Errorf("expected lockfile.Load to succeed despite the save failure, got %v", loadErr)
	} else if len(lf.Collections) != 1 {
		t.Errorf("expected 1 lockfile entry, got %d", len(lf.Collections))
	}

	// (c) the operator was told the file landed, even though the run still
	// exits nonzero.
	if !printer.hasOkContaining("Lockfile written") {
		t.Errorf("expected a result-tier \"Lockfile written\" line, got %v", printer.okLines())
	}

	// (d) last: readMetricsCommand t.Fatalf's on a missing file, which would
	// otherwise mask a real failure in (b) or (c) above.
	if got := readMetricsCommand(t, metricsPath); got != metricsCommandLock {
		t.Errorf("metrics file not written despite the save failure: command = %q, want %q", got, metricsCommandLock)
	}
}

// TestLockFrozenSaveFailureJoinsBehindDrift pins that lockFrozen with drift and
// a failed save returns ErrLockfileDrift with the save error joined behind it,
// writes metrics, and leaves the stale lockfile byte for byte untouched.
func TestLockFrozenSaveFailureJoinsBehindDrift(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))
	metricsPath := filepath.Join(root, "metrics.json")

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		Workers:          1,
		Frozen:           true,
	}

	// A stale pin the fresh resolve (1.0.0, the only version) disagrees with,
	// which is the drift lockFrozen must report.
	path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	stale := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        cfg.Server,
		Collections:   []lockfile.Entry{{Name: "acme.widgets", Version: "0.9.0", Source: cfg.Server}},
	}
	if err := lockfile.Save(path, stale); err != nil {
		t.Fatalf("save stale lockfile: %v", err)
	}
	before := mustReadFile(t, path)

	state := newSaveFailState(t, cfg)
	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())

	err := lockWithState(context.Background(), cfg, runtime, state, time.Now())

	// (a) both the drift verdict and the save failure are reachable through
	// the same returned error.
	if !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("expected errors.Is helpers.ErrLockfileDrift, got %v", err)
	}
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}

	// (b) the lockfile on disk is untouched: lockFrozen never writes it,
	// drift or not, save failure or not.
	after := mustReadFile(t, path)
	if string(before) != string(after) {
		t.Fatalf("lockFrozen rewrote the lockfile despite drift:\n%s", after)
	}

	// (c) writeRunMetrics still ran despite both failures.
	if got := readMetricsCommand(t, metricsPath); got != metricsCommandLock {
		t.Errorf("metrics file not written despite drift and the save failure: command = %q, want %q", got, metricsCommandLock)
	}
}

// TestLockWithStateWritesLockfileAndMetrics exercises lockWithState's happy
// path with a real (not save-failing) backend, confirming a successful lock
// run writes both the lockfile and the metrics report.
func TestLockWithStateWritesLockfileAndMetrics(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))
	metricsPath := filepath.Join(root, "metrics.json")

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())

	err := lockWithState(context.Background(), cfg, runtime, state, time.Now())
	if err != nil {
		t.Fatalf("expected lockWithState to succeed, got %v", err)
	}

	path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, loadErr := lockfile.Load(path)
	if loadErr != nil {
		t.Fatalf("lockfile.Load: %v", loadErr)
	}
	if len(lf.Collections) != 1 || lf.Collections[0].Name != "acme.widgets" {
		t.Fatalf("unexpected lockfile collections: %+v", lf.Collections)
	}

	if got := readMetricsCommand(t, metricsPath); got != metricsCommandLock {
		t.Errorf("metrics command = %q, want %q", got, metricsCommandLock)
	}
	if !printer.hasOkContaining("Lockfile written") {
		t.Errorf("expected a result-tier \"Lockfile written\" line, got %v", printer.okLines())
	}
}

// newLocalState builds an installState over a real *local.Backend opened and
// loaded like initInstall, without the lock: the *WithState functions under
// test neither release it nor close the backend.
func newLocalState(t *testing.T, cfg *config.Config) *installState {
	t.Helper()
	backend := local.New(cfg.CacheDir)
	ctx := context.Background()
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close(ctx) })
	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	return &installState{
		backend:      backend,
		store:        st,
		extractStore: newExtractStore(cfg),
	}
}

// newSaveFailState is newLocalState with its backend wrapped in
// saveFailBackend, so only SaveStore fails.
func newSaveFailState(t *testing.T, cfg *config.Config) *installState {
	t.Helper()
	state := newLocalState(t, cfg)
	state.backend = saveFailBackend{Backend: state.backend}
	return state
}
