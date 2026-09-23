package collections_test

// End-to-end tests of the ordered server list over two fake servers:
// first-match ownership, fail-closed errors, source: pinning, per-origin
// credentials, hub-shaped servers and per-server cache scoping.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// msReqSpec is one requirements.yml entry for buildMultiServerRequirements:
// a bare "name: *" entry, plus an optional source: line when source is set.
type msReqSpec struct {
	name   string
	source string
}

// buildMultiServerRequirements renders entries into a requirements.yml body,
// each at the wildcard version constraint (this file never cares about
// version selection, only which server answers).
func buildMultiServerRequirements(entries []msReqSpec) string {
	var b strings.Builder
	b.WriteString("collections:\n")
	for _, e := range entries {
		b.WriteString("  - name: " + e.name + "\n    version: \"*\"\n")
		if e.source != "" {
			b.WriteString("    source: " + e.source + "\n")
		}
	}
	return b.String()
}

// newMultiServerConfig builds a *config.Config with servers and a
// requirements.yml, under a fresh t.TempDir so every call gets its own cache
// and download directories.
func newMultiServerConfig(t *testing.T, servers []config.Server, requirementsYAML string) *config.Config {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	if err := os.WriteFile(reqPath, []byte(requirementsYAML), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
	server := ""
	if len(servers) > 0 {
		server = servers[0].URL
	}
	return &config.Config{
		Servers:          servers,
		Server:           server,
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
	}
}

// multiServerRuntime builds an *infra.Infra on the production fetch.New
// transport wired from cfg.Servers, so per-origin token and TLS dispatch are
// exercised as in a real run.
func multiServerRuntime(cfg *config.Config) *infra.Infra {
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
	return infra.New(noopPrinter{}, fetch.New(cfg.Timeout, auths))
}

// msAssertInstalled fails the test unless ns.name's MANIFEST.json exists
// under downloadPath; every collection in this file is in namespace "ns".
func msAssertInstalled(t *testing.T, downloadPath, name string) {
	t.Helper()
	path := filepath.Join(downloadPath, "ansible_collections", "ns", name, "MANIFEST.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected ns.%s installed, MANIFEST.json missing at %s: %v", name, path, err)
	}
}

// msArtifactSHA256 hashes the cached tarball of ns.name@version that the
// local artifact backend stored under cacheDir at helpers.ArtifactKey, scoped
// to source, the server that resolved it.
func msArtifactSHA256(t *testing.T, cacheDir, source, name, version string) string {
	t.Helper()
	filename := fmt.Sprintf("ns-%s-%s.tar.gz", name, version)
	key := helpers.ArtifactKey(source, filename)
	data, err := os.ReadFile(filepath.Join(cacheDir, key)) //nolint:gosec // path built from this test's own temp dir and fixture names.
	if err != nil {
		t.Fatalf("read cached artifact %s (key %s): %v", filename, key, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// msLockFile runs collections.Lock and loads the resulting lockfile, so a
// test can inspect the Source and SHA256 recorded for each winning server.
func msLockFile(t *testing.T, cfg *config.Config, runtime *infra.Infra) *lockfile.File {
	t.Helper()
	if err := collections.Lock(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	lockPath := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatalf("load lockfile: %v", err)
	}
	return lf
}

// TestMultiServerFirstMatchOwnership pins first-match ownership: with ns.a on
// A, ns.b on B and ns.both on both with different bytes, ns.both installs and
// locks A's bytes and source.
func TestMultiServerFirstMatchOwnership(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.AddVersion("ns", "a", "1.0.0", nil)
	srvB.AddVersion("ns", "b", "1.0.0", nil)
	bothA := srvA.AddVersion("ns", "both", "1.0.0", nil)
	// An empty deps map renders "{}" rather than "null" in MANIFEST.json, giving
	// B's copy different bytes and sha256 with the same (empty) dependency set.
	srvB.AddVersion("ns", "both", "1.0.0", map[string]string{})

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{
		{name: "ns.a"}, {name: "ns.b"}, {name: "ns.both"},
	}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "a")
	msAssertInstalled(t, cfg.DownloadPath, "b")
	msAssertInstalled(t, cfg.DownloadPath, "both")

	if got := msArtifactSHA256(t, cfg.CacheDir, srvA.URL(), "both", "1.0.0"); got != bothA.SHA256 {
		t.Fatalf("installed ns.both sha = %s, want A's own sha %s (first-match ownership)", got, bothA.SHA256)
	}

	lf := msLockFile(t, cfg, runtime)
	if e := findLockEntry(t, lf, "ns.a"); e.Source != srvA.URL() {
		t.Fatalf("ns.a lockfile source = %q, want %q", e.Source, srvA.URL())
	}
	if e := findLockEntry(t, lf, "ns.b"); e.Source != srvB.URL() {
		t.Fatalf("ns.b lockfile source = %q, want %q", e.Source, srvB.URL())
	}
	entryBoth := findLockEntry(t, lf, "ns.both")
	if entryBoth.Source != srvA.URL() {
		t.Fatalf("ns.both lockfile source = %q, want %q (first match, A owns it)", entryBoth.Source, srvA.URL())
	}
	if entryBoth.SHA256 != bothA.SHA256 {
		t.Fatalf("ns.both lockfile sha = %q, want A's own sha %q", entryBoth.SHA256, bothA.SHA256)
	}
}

// TestMultiServerAdvancesOnPlain404 asserts that ns.b, absent from A but
// present on B, resolves from B: a 404 across every apiRoot candidate of A
// advances the walk to B rather than failing the run.
func TestMultiServerAdvancesOnPlain404(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvB.AddVersion("ns", "b", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.b"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "b")

	lf := msLockFile(t, cfg, runtime)
	if e := findLockEntry(t, lf, "ns.b"); e.Source != srvB.URL() {
		t.Fatalf("ns.b lockfile source = %q, want %q", e.Source, srvB.URL())
	}
}

// TestMultiServerAuthFailureAbortsClosed pins that a 401 from A aborts the
// run with helpers.ErrGalaxyAuthFailed naming A and never falls through to B.
func TestMultiServerAuthFailureAbortsClosed(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.RequireAuth("Token good")
	srvA.AddVersion("ns", "x", "1.0.0", nil)
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{
		{ID: "a", URL: srvA.URL(), Token: config.NewSecret("wrong")},
		{ID: "b", URL: srvB.URL()},
	}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))
	runtime := multiServerRuntime(cfg)

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, helpers.ErrGalaxyAuthFailed) {
		t.Fatalf("expected errors.Is ErrGalaxyAuthFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "server a:") {
		t.Fatalf("expected the error to name server %q, got %v", "a", err)
	}
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0 (fail closed, never falls through on auth failure)", got)
	}
}

// TestMultiServerForbiddenAbortsClosed is TestMultiServerAuthFailureAbortsClosed's
// 403 sibling: AuthFailStatus(403) must classify the same as a 401.
func TestMultiServerForbiddenAbortsClosed(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.RequireAuth("Token good")
	srvA.AuthFailStatus(http.StatusForbidden)
	srvA.AddVersion("ns", "x", "1.0.0", nil)
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{
		{ID: "a", URL: srvA.URL(), Token: config.NewSecret("wrong")},
		{ID: "b", URL: srvB.URL()},
	}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))
	runtime := multiServerRuntime(cfg)

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, helpers.ErrGalaxyAuthFailed) {
		t.Fatalf("expected errors.Is ErrGalaxyAuthFailed, got %v", err)
	}
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0 (fail closed, never falls through on a 403)", got)
	}
}

// TestMultiServerUnavailableAbortsAfterRetryBudget pins that a persistent 503
// from A aborts with helpers.ErrGalaxyServerUnavailable naming A, never reaches
// B, and spends one retry budget, not one per API root candidate.
func TestMultiServerUnavailableAbortsAfterRetryBudget(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.Fail(fakegalaxy.EndpointRootMetadata, "", "", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))
	runtime := multiServerRuntime(cfg)

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, helpers.ErrGalaxyServerUnavailable) {
		t.Fatalf("expected errors.Is ErrGalaxyServerUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "server a:") {
		t.Fatalf("expected the error to name server %q, got %v", "a", err)
	}
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0 (fail closed, never falls through on exhausted retries)", got)
	}
	if got := srvA.Count(fakegalaxy.EndpointRootMetadata); got != helpers.FetchRetryMaxAttempts {
		t.Fatalf("srvA root-metadata count = %d, want %d (the retry budget, not multiplied by the candidate walk)",
			got, helpers.FetchRetryMaxAttempts)
	}
}

// TestMultiServerAll404YieldsUnknownPackage pins that a collection no server
// has fails as a solver.ConflictError classified ExitResolution, not as a
// network abort.
func TestMultiServerAll404YieldsUnknownPackage(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.ghost"}}))
	runtime := multiServerRuntime(cfg)

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if _, ok := errors.AsType[*solver.ConflictError](err); !ok {
		t.Fatalf("expected a *solver.ConflictError, got %T: %v", err, err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitResolution {
		t.Fatalf("exitcode.FromError(err) = %d, want ExitResolution (%d)", got, exitcode.ExitResolution)
	}
}

// TestMultiServerPinnedByID asserts a source: value matching a configured
// server_list id installs from exactly that server, never consulting A.
func TestMultiServerPinnedByID(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x", source: "b"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "x")
	if got := srvA.Total(); got != 0 {
		t.Fatalf("srvA.Total() = %d, want 0 (pinned by id, A is never consulted)", got)
	}
}

// TestMultiServerPinnedByOriginDifferentPath pins that a source: naming another
// path on a configured server's origin adopts that server and its token, since
// fetch dispatches credentials by origin, not by the configured URL string.
func TestMultiServerPinnedByOriginDifferentPath(t *testing.T) {
	t.Parallel()
	const hubPath = "/content/published"
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.NewAtBasePath(t, hubPath)
	srvB.RequireAuth("Token btok")
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{
		{ID: "a", URL: srvA.URL()},
		{ID: "b", URL: srvB.URL(), Token: config.NewSecret("btok")},
	}
	source := srvB.URL() + hubPath
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x", source: source}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "x")

	value, present := srvB.SeenAuth(fakegalaxy.EndpointRootMetadata)
	if !present || value != "Token btok" {
		t.Fatalf("SeenAuth(EndpointRootMetadata) = (%q, %v), want (\"Token btok\", true) - the origin match must still attach B's token",
			value, present)
	}
	if got := srvA.Total(); got != 0 {
		t.Fatalf("srvA.Total() = %d, want 0", got)
	}
}

// TestMultiServerPinnedMatchingNothingSendsNoAuth pins that a source: matching
// no configured id or origin is queried anonymously, fails closed with
// helpers.ErrGalaxyAuthFailed naming that URL, and never consults A or B.
func TestMultiServerPinnedMatchingNothingSendsNoAuth(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvC := fakegalaxy.New(t) // never part of cfg.Servers
	srvC.RequireAuth("Token good")
	srvC.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{
		{ID: "a", URL: srvA.URL(), Token: config.NewSecret("atok")},
		{ID: "b", URL: srvB.URL(), Token: config.NewSecret("btok")},
	}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x", source: srvC.URL()}}))
	runtime := multiServerRuntime(cfg)

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, helpers.ErrGalaxyAuthFailed) {
		t.Fatalf("expected errors.Is ErrGalaxyAuthFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), srvC.URL()) {
		t.Fatalf("expected the error to name the base URL %s, got %v", srvC.URL(), err)
	}
	if value, present := srvC.SeenAuth(fakegalaxy.EndpointRootMetadata); present || value != "" {
		t.Fatalf("SeenAuth = (%q, %v), want no Authorization header sent to an unmatched server", value, present)
	}
	if got := srvA.Total(); got != 0 {
		t.Fatalf("srvA.Total() = %d, want 0", got)
	}
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0", got)
	}
}

// TestMultiServerPinnedNeverFallsThrough asserts a collection pinned to A
// that only exists on B never falls through to B: it fails as an unknown
// package, and B is never consulted.
func TestMultiServerPinnedNeverFallsThrough(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x", source: "a"}}))
	runtime := multiServerRuntime(cfg)

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if _, ok := errors.AsType[*solver.ConflictError](err); !ok {
		t.Fatalf("expected a *solver.ConflictError (unknown package), got %T: %v", err, err)
	}
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0 (a pinned collection never falls through)", got)
	}
}

// TestMultiServerTransitiveDepWalksList pins that a dependency does not
// inherit its root's source: ns.root is pinned to A, and its dependency
// ns.dep, present only on B, resolves from B.
func TestMultiServerTransitiveDepWalksList(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.AddVersion("ns", "root", "1.0.0", map[string]string{"ns.dep": ">=1.0.0"})
	srvB.AddVersion("ns", "dep", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.root", source: "a"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "root")
	msAssertInstalled(t, cfg.DownloadPath, "dep")

	lf := msLockFile(t, cfg, runtime)
	if e := findLockEntry(t, lf, "ns.dep"); e.Source != srvB.URL() {
		t.Fatalf("ns.dep lockfile source = %q, want %q (no source inheritance from the pinned root)", e.Source, srvB.URL())
	}
}

// TestMultiServerHubShapeInList pins that two collections on a Galaxy NG /
// Automation Hub shaped server first in the list both resolve, each with one
// counted root-metadata hit (unmatched probes are not counted).
func TestMultiServerHubShapeInList(t *testing.T) {
	t.Parallel()
	const hubPath = "/api/automation-hub"
	srvHub := fakegalaxy.NewAtBasePath(t, hubPath)
	srvB := fakegalaxy.New(t)
	srvHub.AddVersion("ns", "one", "1.0.0", nil)
	srvHub.AddVersion("ns", "two", "1.0.0", nil)

	servers := []config.Server{
		{ID: "hub", URL: srvHub.URL() + hubPath},
		{ID: "b", URL: srvB.URL()},
	}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.one"}, {name: "ns.two"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "one")
	msAssertInstalled(t, cfg.DownloadPath, "two")

	if got := srvHub.Count(fakegalaxy.EndpointRootMetadata); got != 2 {
		t.Fatalf("srvHub root-metadata count = %d, want 2 (one successful hit per collection)", got)
	}
}

// TestMultiServerAuthReachesOwnServerOnly asserts that when A and B both
// require auth with different expected headers, each one's SeenAuth matches
// its own configured token and never the other's.
func TestMultiServerAuthReachesOwnServerOnly(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.RequireAuth("Token atok")
	srvB.RequireAuth("Token btok")
	srvA.AddVersion("ns", "a", "1.0.0", nil)
	srvB.AddVersion("ns", "b", "1.0.0", nil)

	servers := []config.Server{
		{ID: "a", URL: srvA.URL(), Token: config.NewSecret("atok")},
		{ID: "b", URL: srvB.URL(), Token: config.NewSecret("btok")},
	}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{
		{name: "ns.a", source: "a"}, {name: "ns.b", source: "b"},
	}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if v, ok := srvA.SeenAuth(fakegalaxy.EndpointRootMetadata); !ok || v != "Token atok" {
		t.Fatalf("srvA SeenAuth(EndpointRootMetadata) = (%q, %v), want (\"Token atok\", true)", v, ok)
	}
	if v, ok := srvB.SeenAuth(fakegalaxy.EndpointRootMetadata); !ok || v != "Token btok" {
		t.Fatalf("srvB SeenAuth(EndpointRootMetadata) = (%q, %v), want (\"Token btok\", true)", v, ok)
	}
}

// TestMultiServerResolutionDeterministicAcrossWorkerCounts pins that the
// locked Source and SHA256 of twelve collections split over two servers are
// identical with 1 and 8 workers: server ownership is decided at resolve time.
func TestMultiServerResolutionDeterministicAcrossWorkerCounts(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	names := []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7", "c8", "c9", "c10", "c11", "c12"}
	for i, name := range names {
		switch {
		case i < 4: // A only
			srvA.AddVersion("ns", name, "1.0.0", nil)
		case i < 8: // B only
			srvB.AddVersion("ns", name, "1.0.0", nil)
		default: // both, with different artifact bytes (see TestMultiServerFirstMatchOwnership)
			srvA.AddVersion("ns", name, "1.0.0", nil)
			srvB.AddVersion("ns", name, "1.0.0", map[string]string{})
		}
	}
	entries := make([]msReqSpec, len(names))
	for i, name := range names {
		entries[i] = msReqSpec{name: "ns." + name}
	}
	requirementsYAML := buildMultiServerRequirements(entries)
	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}

	runOnce := func(t *testing.T, workers int) (map[string]string, map[string]string) {
		t.Helper()
		cfg := newMultiServerConfig(t, servers, requirementsYAML)
		cfg.Workers = workers
		runtime := multiServerRuntime(cfg)
		if err := collections.Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start (workers=%d): %v", workers, err)
		}
		lf := msLockFile(t, cfg, runtime)
		sources := make(map[string]string, len(lf.Collections))
		shas := make(map[string]string, len(lf.Collections))
		for _, e := range lf.Collections {
			sources[e.Name] = e.Source
			shas[e.Name] = e.SHA256
		}
		return sources, shas
	}

	sources1, shas1 := runOnce(t, 1)
	sources8, shas8 := runOnce(t, 8)

	if !reflect.DeepEqual(sources1, sources8) {
		t.Fatalf("Source map differs between workers=1 and workers=8:\n1: %v\n8: %v", sources1, sources8)
	}
	if !reflect.DeepEqual(shas1, shas8) {
		t.Fatalf("SHA256 map differs between workers=1 and workers=8:\n1: %v\n8: %v", shas1, shas8)
	}
}

// msReusedConfig clones cfg with the same cache, install and requirements
// paths but a different server list, so a second run reuses the first run's
// persisted snapshot if and only if the signature says it may.
func msReusedConfig(cfg *config.Config, servers []config.Server) *config.Config {
	clone := *cfg
	clone.Servers = servers
	if len(servers) > 0 {
		clone.Server = servers[0].URL
	}
	return &clone
}

// TestMultiServerSnapshotReuseIsPartitionedByServerList pins that resolution
// reuse keys on the ordered server list: the same list touches no server, the
// reversed list re-resolves, since order decides first-match ownership.
func TestMultiServerSnapshotReuseIsPartitionedByServerList(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.AddVersion("ns", "shared", "1.0.0", nil)
	srvB.AddVersion("ns", "shared", "1.0.0", nil)

	forward := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, forward, buildMultiServerRequirements([]msReqSpec{{name: "ns.shared"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("first install: %v", err)
	}

	srvA.ResetCounts()
	srvB.ResetCounts()
	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("second install with an identical server list: %v", err)
	}
	if got := srvA.Count(fakegalaxy.EndpointRootMetadata); got != 0 {
		t.Fatalf("srvA root metadata requests on reuse = %d, want 0 (the snapshot should have been reused)", got)
	}

	reversed := []config.Server{{ID: "b", URL: srvB.URL()}, {ID: "a", URL: srvA.URL()}}
	reorderedCfg := msReusedConfig(cfg, reversed)
	reorderedRuntime := multiServerRuntime(reorderedCfg)

	srvA.ResetCounts()
	srvB.ResetCounts()
	if err := collections.Start(context.Background(), reorderedCfg, reorderedRuntime); err != nil {
		t.Fatalf("third install with a reordered server list: %v", err)
	}
	if srvA.Count(fakegalaxy.EndpointRootMetadata)+srvB.Count(fakegalaxy.EndpointRootMetadata) == 0 {
		t.Fatal("expected a reordered server list to re-resolve, but no server was contacted")
	}
}

// TestMultiServerArtifactCacheKeyIsScopedPerServer pins that two projects on
// one cache, pinning ns.shared@1.0.0 to servers with different bytes, each get
// their own server's tarball: helpers.ArtifactKey folds in the server.
func TestMultiServerArtifactCacheKeyIsScopedPerServer(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	verA := srvA.AddVersion("ns", "shared", "1.0.0", nil)
	// An empty deps map renders differently from nil in MANIFEST.json, giving
	// B's copy different bytes and sha256 with the same dependency set.
	verB := srvB.AddVersion("ns", "shared", "1.0.0", map[string]string{})
	if verA.SHA256 == verB.SHA256 {
		t.Fatalf("fixture bug: A and B must have distinct artifact bytes to prove no collision, both hash to %s", verA.SHA256)
	}

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfgA := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.shared", source: "a"}}))
	cfgB := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.shared", source: "b"}}))
	cfgB.CacheDir = cfgA.CacheDir // force both projects onto one shared cache dir

	runtimeA := multiServerRuntime(cfgA)
	runtimeB := multiServerRuntime(cfgB)

	if err := collections.Start(context.Background(), cfgA, runtimeA); err != nil {
		t.Fatalf("install pinned to A: %v", err)
	}
	if err := collections.Start(context.Background(), cfgB, runtimeB); err != nil {
		t.Fatalf("install pinned to B: %v", err)
	}
	msAssertInstalled(t, cfgA.DownloadPath, "shared")
	msAssertInstalled(t, cfgB.DownloadPath, "shared")

	if got := msArtifactSHA256(t, cfgA.CacheDir, srvA.URL(), "shared", "1.0.0"); got != verA.SHA256 {
		t.Fatalf("A's cached artifact sha = %s, want A's own sha %s", got, verA.SHA256)
	}
	if got := msArtifactSHA256(t, cfgB.CacheDir, srvB.URL(), "shared", "1.0.0"); got != verB.SHA256 {
		t.Fatalf("B's cached artifact sha = %s, want B's own sha %s", got, verB.SHA256)
	}
}

// TestMultiServerDepsCacheKeyIsScopedPerServer pins that cached dependency
// maps are scoped per server: two projects on one cache pin ns.shared@1.0.0 to
// servers declaring different deps, and each installs its own server's dep.
func TestMultiServerDepsCacheKeyIsScopedPerServer(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.AddVersion("ns", "shared", "1.0.0", map[string]string{"ns.depa": "*"})
	srvA.AddVersion("ns", "depa", "1.0.0", nil)
	srvB.AddVersion("ns", "shared", "1.0.0", map[string]string{"ns.depb": "*"})
	srvB.AddVersion("ns", "depb", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfgA := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.shared", source: "a"}}))
	cfgB := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.shared", source: "b"}}))
	cfgB.CacheDir = cfgA.CacheDir // force both projects onto one shared cache dir (and deps cache)

	runtimeA := multiServerRuntime(cfgA)
	runtimeB := multiServerRuntime(cfgB)

	if err := collections.Start(context.Background(), cfgA, runtimeA); err != nil {
		t.Fatalf("install pinned to A: %v", err)
	}
	if err := collections.Start(context.Background(), cfgB, runtimeB); err != nil {
		t.Fatalf("install pinned to B: %v", err)
	}

	msAssertInstalled(t, cfgA.DownloadPath, "shared")
	msAssertInstalled(t, cfgA.DownloadPath, "depa")
	msAssertInstalled(t, cfgB.DownloadPath, "shared")
	msAssertInstalled(t, cfgB.DownloadPath, "depb")
}

// TestMultiServerSourceSwitchForcesReinstall pins that re-pinning an installed
// collection from A to B forces a reinstall from B: installEntryMatches
// compares the recorded install's source with the newly resolved one.
func TestMultiServerSourceSwitchForcesReinstall(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.AddVersion("ns", "x", "1.0.0", nil)
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x", source: "a"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("first install (pinned to A): %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "x")
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0 before switching source", got)
	}

	switchedCfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x", source: "b"}}))
	switchedCfg.CacheDir = cfg.CacheDir         // reuse the snapshot recording A's install
	switchedCfg.DownloadPath = cfg.DownloadPath // reuse the on-disk install tree
	switchedRuntime := multiServerRuntime(switchedCfg)

	if err := collections.Start(context.Background(), switchedCfg, switchedRuntime); err != nil {
		t.Fatalf("second install (pinned to B): %v", err)
	}
	if got := srvB.Total(); got == 0 {
		t.Fatal("srvB.Total() = 0, want > 0 (a source switch must force a real reinstall from B, not a silent skip)")
	}
}
