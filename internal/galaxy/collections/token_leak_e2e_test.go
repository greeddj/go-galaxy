package collections_test

// These tests pin end to end that a Galaxy token never reaches stdout, stderr
// or any written file, against fakegalaxy through the real fetch.New
// transport and progress.Printer.

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// leakToken is distinctive enough that no byte the pipeline emits on its own
// can match it, so any match is the token itself.
const leakToken = "tok3n-must-not-appear-anywhere"

// tokenLeakConfig builds a *config.Config for a single fakegalaxy server
// requiring leakToken, rooted under a fresh t.TempDir() so cache, install,
// lockfile, and metrics paths are all isolated per test.
func tokenLeakConfig(t *testing.T, srv *fakegalaxy.Server) *config.Config {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - name: ns.a\n    version: \"*\"\n"), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
	return &config.Config{
		Servers:          []config.Server{{URL: srv.URL(), Token: config.NewSecret(leakToken)}},
		Server:           srv.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		MetricsFile:      filepath.Join(root, "metrics.json"),
		Workers:          4,
		Timeout:          e2eTimeout,
		Verbose:          true,
	}
}

// tokenLeakRuntime builds an *infra.Infra wired like the CLI's serverAuths:
// the real fetch.New transport, so per-origin token attachment is exercised
// for real, paired with the caller's printer.
func tokenLeakRuntime(cfg *config.Config, printer *progress.Progress) *infra.Infra {
	auths := make([]fetch.ServerAuth, 0, len(cfg.Servers))
	for _, s := range cfg.Servers {
		u, err := url.Parse(s.URL)
		if err != nil {
			continue
		}
		auths = append(auths, fetch.ServerAuth{
			Origin:      helpers.Origin(u),
			Token:       s.Token.Reveal(),
			InsecureTLS: s.InsecureSkipTLSVerify,
		})
	}
	return infra.New(printer, fetch.New(cfg.Timeout, auths))
}

// captureStdIO swaps the process-wide os.Stdout/os.Stderr for pipes while fn
// runs, draining both concurrently so fn cannot block, and returns what each
// received. progress.New reads those vars at construction, its only seam.
func captureStdIO(t *testing.T, fn func()) ([]byte, []byte) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stdout): %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stderr): %v", err)
	}
	os.Stdout, os.Stderr = outW, errW
	defer func() {
		os.Stdout, os.Stderr = origOut, origErr
	}()

	var outBuf, errBuf bytes.Buffer
	outDone := make(chan struct{})
	errDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&outBuf, outR)
		close(outDone)
	}()
	go func() {
		_, _ = io.Copy(&errBuf, errR)
		close(errDone)
	}()

	fn()

	_ = outW.Close()
	_ = errW.Close()
	<-outDone
	<-errDone
	_ = outR.Close()
	_ = errR.Close()
	return outBuf.Bytes(), errBuf.Bytes()
}

// assertNoTokenInTree fails the test for every file under root that contains
// token; walking the tree rather than naming files covers any file added later.
func assertNoTokenInTree(t *testing.T, root, token string) {
	t.Helper()
	if _, err := os.Stat(root); err != nil {
		return // nothing was written under root at all; nothing to check.
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // path comes from this test's own WalkDir over its own temp tree.
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(token)) {
			t.Errorf("token leaked into %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// assertNoTokenInFile fails the test if the file at path contains token. A
// missing file passes: whether it is written at all is other tests' concern.
func assertNoTokenInFile(t *testing.T, path, token string) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixed cfg field, not user input.
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read %s: %v", path, err)
	}
	if bytes.Contains(data, []byte(token)) {
		t.Errorf("token leaked into %s", path)
	}
}

// TestTokenNeverLeaksDuringVerboseInstall pins that a verbose install and lock
// leak the token into no output, cache file, install file, metrics, lockfile
// or S3 snapshot payload. Serial: captureStdIO swaps process-wide stdio.
func TestTokenNeverLeaksDuringVerboseInstall(t *testing.T) {
	srv := fakegalaxy.New(t)
	srv.RequireAuth("Token " + leakToken)
	srv.AddVersion("ns", "a", "1.0.0", nil)

	cfg := tokenLeakConfig(t, srv)
	ctx := context.Background()

	var installErr, lockErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := tokenLeakRuntime(cfg, printer)
		runtime.DebugConfigSources(cfg)
		runtime.WarnConfig(cfg)

		installErr = collections.Start(ctx, cfg, runtime)
		// Start never writes a lockfile, so Lock covers that path too.
		lockErr = collections.Lock(ctx, cfg, runtime)
	})
	if installErr != nil {
		t.Fatalf("Start: %v", installErr)
	}
	if lockErr != nil {
		t.Fatalf("Lock: %v", lockErr)
	}

	if bytes.Contains(stdout, []byte(leakToken)) {
		t.Errorf("token leaked into stdout: %q", stdout)
	}
	if bytes.Contains(stderr, []byte(leakToken)) {
		t.Errorf("token leaked into stderr: %q", stderr)
	}

	assertNoTokenInTree(t, cfg.CacheDir, leakToken)
	assertNoTokenInTree(t, cfg.DownloadPath, leakToken)
	assertNoTokenInFile(t, cfg.MetricsFile, leakToken)

	lockPath := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	assertNoTokenInFile(t, lockPath, leakToken)

	// The S3 backend's SaveStore sends store.MarshalSnapshot's bytes, so
	// marshaling the store this run wrote checks the S3 payload without S3.
	backend, err := cacheBackend.New(cfg, tokenLeakRuntime(cfg, progress.New(false, true)))
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
	payload, err := st.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot: %v", err)
	}
	if bytes.Contains(payload, []byte(leakToken)) {
		t.Errorf("token leaked into the marshaled snapshot payload: %s", payload)
	}
}

// TestTokenNeverLeaksOnAuthFailure pins that a rejected token stays out of the
// output that reports the authentication failure. Serial for the same
// stdio-swap reason as TestTokenNeverLeaksDuringVerboseInstall.
func TestTokenNeverLeaksOnAuthFailure(t *testing.T) {
	srv := fakegalaxy.New(t)
	srv.RequireAuth("Token correct-token-value")
	srv.AddVersion("ns", "a", "1.0.0", nil)

	cfg := tokenLeakConfig(t, srv)
	cfg.Servers[0].Token = config.NewSecret(leakToken) // deliberately wrong

	var installErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := tokenLeakRuntime(cfg, printer)
		runtime.DebugConfigSources(cfg)
		runtime.WarnConfig(cfg)
		installErr = collections.Start(context.Background(), cfg, runtime)
	})
	if installErr == nil {
		t.Fatalf("expected an auth failure, got nil")
	}
	if bytes.Contains(stdout, []byte(leakToken)) {
		t.Errorf("token leaked into stdout on auth failure: %q", stdout)
	}
	if bytes.Contains(stderr, []byte(leakToken)) {
		t.Errorf("token leaked into stderr on auth failure: %q", stderr)
	}
}
