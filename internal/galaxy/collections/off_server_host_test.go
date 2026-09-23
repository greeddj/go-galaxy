package collections

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// offHostWarnSubstring is the fixed fragment every off-server-host warning
// line contains, used by every test in this file to detect (or rule out) the
// warning without depending on its exact wording.
const offHostWarnSubstring = "differs from the configured server origin"

// offServerHostGuardCase is one table entry for
// TestWarnIfOffServerDownloadHostGuards.
type offServerHostGuardCase struct {
	name        string
	base        string
	downloadURL string
	wantWarn    bool
}

// offServerHostGuardCases is every origin mismatch that must warn, then every
// input that must stay silent: a real origin match, and the blank, unparseable
// and hostless inputs the guard declines to judge, so no false alarm reaches CI.
func offServerHostGuardCases() []offServerHostGuardCase {
	return append(offServerOriginMismatchCases(), offServerOriginSilentCases()...)
}

// offServerOriginMismatchCases holds the rows that must warn: an origin
// differing in each of the three components Origin normalizes over.
func offServerOriginMismatchCases() []offServerHostGuardCase {
	return []offServerHostGuardCase{
		{
			name:        "differing host warns",
			base:        "https://galaxy.example.com",
			downloadURL: "https://cdn.other.example/artifact.tar.gz",
			wantWarn:    true,
		},
		{
			// Another scheme and port on the same host is another origin, and
			// origin decides whether the operator's token and TLS policy apply.
			name:        "same host different scheme and port warns",
			base:        "https://galaxy.example.com:443",
			downloadURL: "http://galaxy.example.com:8080/artifact.tar.gz",
			wantWarn:    true,
		},
		{
			// The host is unchanged but the transport silently stops being
			// TLS, and no credential follows the request.
			name:        "scheme downgrade on the same host warns",
			base:        "https://galaxy.example.com",
			downloadURL: "http://galaxy.example.com/artifact.tar.gz",
			wantWarn:    true,
		},
	}
}

// offServerOriginSilentCases holds the rows that must stay silent: a genuine
// origin match under varying spelling, and every guard branch that declines to
// judge the comparison at all.
func offServerOriginSilentCases() []offServerHostGuardCase {
	return []offServerHostGuardCase{
		{
			name:        "same host no warn",
			base:        "https://galaxy.example.com",
			downloadURL: "https://galaxy.example.com/artifact.tar.gz",
			wantWarn:    false,
		},
		{
			// Origin fills in the scheme's default port, so an explicit :443
			// and an implicit one are the same endpoint.
			name:        "same origin with implicit default port no warn",
			base:        "https://galaxy.example.com",
			downloadURL: "https://galaxy.example.com:443/artifact.tar.gz",
			wantWarn:    false,
		},
		{
			name:        "origin comparison is case-insensitive",
			base:        "https://Galaxy.Example.COM",
			downloadURL: "https://galaxy.example.com/artifact.tar.gz",
			wantWarn:    false,
		},
		{
			name:        "blank base no warn",
			base:        "   ",
			downloadURL: "https://cdn.other.example/artifact.tar.gz",
			wantWarn:    false,
		},
		{
			// A raw control character makes url.Parse fail outright.
			name:        "unparseable base no warn",
			base:        "http://exa\x7fmple.com",
			downloadURL: "https://cdn.other.example/artifact.tar.gz",
			wantWarn:    false,
		},
		{
			name:        "unparseable download URL no warn",
			base:        "https://galaxy.example.com",
			downloadURL: "http://exa\x7fmple.com",
			wantWarn:    false,
		},
		{
			// A hostless URL yields a degenerate origin matching nothing, so
			// it is guarded explicitly rather than treated as a mismatch.
			name:        "download URL without a host no warn",
			base:        "https://galaxy.example.com",
			downloadURL: "/local/artifact.tar.gz",
			wantWarn:    false,
		},
	}
}

// TestWarnIfOffServerDownloadHostGuards drives warnIfOffServerDownloadHost
// directly for every guard branch listed in offServerHostGuardCases.
func TestWarnIfOffServerDownloadHostGuards(t *testing.T) {
	t.Parallel()

	for _, tt := range offServerHostGuardCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			printer := &capturingPrinter{}
			runtime := infra.New(printer, http.DefaultClient)

			warnIfOffServerDownloadHost(runtime, tt.base, tt.downloadURL)

			// The warning goes out through Warnf, not Printf, so it survives
			// --quiet; assert against that channel specifically.
			if got := printer.hasWarnContaining(offHostWarnSubstring); got != tt.wantWarn {
				t.Fatalf("downloadURL %q against base %q: warned=%v, want %v (warns=%v)",
					tt.downloadURL, tt.base, got, tt.wantWarn, printer.warns)
			}
		})
	}
}

// TestOffServerDownloadHostWarningCutsPresignedQuery pins that the warning,
// printed even under --quiet, names the download URL but not its presigned
// query, which is a bearer capability for the artifact.
func TestOffServerDownloadHostWarningCutsPresignedQuery(t *testing.T) {
	t.Parallel()

	const base = "https://galaxy.example.com"
	const artifact = "https://cdn.other.example/artifact.tar.gz"
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	warnIfOffServerDownloadHost(runtime, base, artifact+presignedQuery)

	if !printer.hasWarnContaining(offHostWarnSubstring) {
		t.Fatalf("expected an off-server-host warning, got %v", printer.warns)
	}
	if printer.hasWarnContaining("X-Amz-Signature") {
		t.Errorf("warning line carries the presigned query: %v", printer.warns)
	}
	if !printer.hasWarnContaining(artifact) {
		t.Errorf("warning line does not name the download URL it is about: %v", printer.warns)
	}
}

// TestDownloadCollectionPrintsNoCapability pins that the request carries the
// presigned query whole, since the object store authenticates by it, while the
// printed download line names the artifact without it.
func TestDownloadCollectionPrintsNoCapability(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seenQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenQuery = r.URL.RawQuery
		mu.Unlock()
		_, _ = w.Write([]byte("artifact bytes"))
	}))
	t.Cleanup(srv.Close)

	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())
	artifact := srv.URL + "/artifact.tar.gz"

	resp, err := downloadCollection(context.Background(), runtime, artifact+presignedQuery)
	if err != nil {
		t.Fatalf("downloadCollection: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	mu.Lock()
	got := seenQuery
	mu.Unlock()
	if want := strings.TrimPrefix(presignedQuery, "?"); got != want {
		t.Errorf("server saw query %q, want the whole presigned query %q", got, want)
	}
	if printer.hasPrintContaining("X-Amz-Signature") {
		t.Errorf("printed line carries the presigned query: %v", printer.prints)
	}
	if !printer.hasPrintContaining(artifact) {
		t.Errorf("printed line does not name the artifact being downloaded: %v", printer.prints)
	}
}

// TestDownloadCollectionErrorNamesNoCapability pins that the non-200 error
// from downloadCollection names the artifact and the status but not the
// presigned query.
func TestDownloadCollectionErrorNamesNoCapability(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	runtime := infra.New(&capturingPrinter{}, srv.Client())
	artifact := srv.URL + "/artifact.tar.gz"

	resp, err := downloadCollection(context.Background(), runtime, artifact+presignedQuery)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatalf("downloadCollection() error = nil, want a non-200 failure")
	}
	msg := err.Error()
	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("error text carries the presigned query: %s", msg)
	}
	if !strings.Contains(msg, artifact) {
		t.Errorf("error text does not name the artifact: %s", msg)
	}
	if !strings.Contains(msg, "403") {
		t.Errorf("error text does not name the status: %s", msg)
	}
}

// unreachableArtifactHost is loopback port 1, which no unprivileged test can
// bind, so the dial is refused at once with no packet leaving the host.
const unreachableArtifactHost = "https://127.0.0.1:1"

// TestDownloadCollectionTransportErrorNamesNoCapability pins that a transport
// error drops the userinfo and presigned query net/http leaves in, yet keeps
// host, path, the cause, *url.Error reachability and downloadRetryable.
func TestDownloadCollectionTransportErrorNamesNoCapability(t *testing.T) {
	t.Parallel()

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	artifact := unreachableArtifactHost + "/artifact.tar.gz"
	// #nosec G101 -- test fixture literal, not a real credential
	const userinfo = "u:s3cr3t@"
	requested := strings.Replace(artifact, "https://", "https://"+userinfo, 1) + presignedQuery

	resp, err := downloadCollection(context.Background(), runtime, requested)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatalf("downloadCollection(%q) error = nil, want a transport failure", requested)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("the transport failure is no longer reachable as *url.Error, which is what "+
			"downloadRetryable and exitcode classify through: %v", err)
	}

	msg := err.Error()
	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("transport error text carries the presigned query: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("transport error text carries the userinfo prefix %q: %s", "u:", msg)
	}
	// Host and path without the scheme: the uncut message carries them too,
	// behind "u:***@", so this positive control holds with or without the cut.
	if !strings.Contains(msg, "127.0.0.1:1/artifact.tar.gz") {
		t.Errorf("transport error text does not name the artifact: %s", msg)
	}
	if !strings.Contains(msg, urlErr.Err.Error()) {
		t.Errorf("transport error text does not carry the transport cause %q: %s", urlErr.Err, msg)
	}
	if !downloadRetryable(err) {
		t.Errorf("downloadRetryable(transport failure) = false, want the transport arm unchanged: %v", err)
	}
}

// newOffHostTestServer starts an httptest server that always serves content
// (a minimal valid tar.gz built by buildMinimalTarGz), and returns it
// alongside content's sha256, ready to be wired as an artifact's DownloadURL.
func newOffHostTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	content := buildMinimalTarGz(t)
	sha := sha256Hex(content)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)
	return server, sha
}

// runOffHostInstall installs col against server's artifact using a fresh
// capturingPrinter-backed runtime and cfg.Server as given, returning the
// printer (to inspect for a warning) and the resulting install path.
func runOffHostInstall(t *testing.T, cfgServer string, server *httptest.Server, sha string) (*capturingPrinter, string) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	// Source mirrors what solverResultToResolvedGraph stamps on a resolved
	// collection: the warning compares against the collection's own server.
	col := collection{Namespace: "acme", Name: "offhost", Version: "1.0.0", Source: cfgServer}
	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: server.URL}
	meta.Artifact.Sha256 = sha

	cfg := &config.Config{
		Server:       cfgServer,
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		// NoDeps matches the other buildMinimalTarGz fixtures, whose artifact
		// carries no dependencies.
		NoDeps: true,
	}

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	deps := installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, store.New()),
		artifacts:      local.NewArtifacts(cfg.CacheDir),
		root:           newTestCollectionsRoot(t, downloadPath),
	}

	if err := installCollection(context.Background(), col, deps, nil, meta, downloadResult{}); err != nil {
		t.Fatalf("installCollection: %v", err)
	}
	return printer, filepath.Join(downloadPath, "ansible_collections", col.Namespace, col.Name)
}

// TestOffServerDownloadHostWarns pins that a download URL on another origin
// than the collection's server, passed in as a cached snapshot would be, warns
// through Warnf (which survives --quiet) and the install still succeeds.
func TestOffServerDownloadHostWarns(t *testing.T) {
	t.Parallel()
	server, sha := newOffHostTestServer(t)

	printer, installPath := runOffHostInstall(t, "https://galaxy.example.invalid", server, sha)

	if !printer.hasWarnContaining(offHostWarnSubstring) {
		t.Fatalf("expected an off-server-host warning to be recorded via Warnf, got %v", printer.warns)
	}
	// Warnf prefixes its own marker at render time, so the format string
	// itself must not also carry the emoji - that would double it.
	if printer.hasWarnContaining("⚠️") {
		t.Fatalf("expected no emoji marker in the Warnf-recorded line, got %v", printer.warns)
	}
	if _, statErr := os.Stat(installPath); statErr != nil {
		t.Fatalf("expected the collection to be installed despite the host-mismatch warning, stat error: %v", statErr)
	}
}

// TestSameHostDownloadNoWarn proves the ordinary case - the download URL's
// host matches the configured server - never emits the off-host warning,
// alongside a successful install.
func TestSameHostDownloadNoWarn(t *testing.T) {
	t.Parallel()
	server, sha := newOffHostTestServer(t)

	printer, installPath := runOffHostInstall(t, server.URL, server, sha)

	if printer.hasWarnContaining(offHostWarnSubstring) {
		t.Fatalf("expected no off-server-host warning for a same-host download, got %v", printer.warns)
	}
	if _, statErr := os.Stat(installPath); statErr != nil {
		t.Fatalf("expected install path %s to exist, stat error: %v", installPath, statErr)
	}
}
