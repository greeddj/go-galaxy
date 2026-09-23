package collections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// testVersion100 is this file's dominant test fixture version literal,
// pulled out as a const purely to satisfy goconst - it carries no domain
// meaning beyond "a generic first version".
const testVersion100 = "1.0.0"

// mustSolverVersion parses testVersion100 as a solver.Version or fails the
// test. Every call site needs the same version, so this hardcodes it rather
// than threading an always-identical parameter.
func mustSolverVersion(t *testing.T) solver.Version {
	t.Helper()
	v, err := solver.NewVersion(testVersion100)
	if err != nil {
		t.Fatalf("solver.NewVersion(%q): %v", testVersion100, err)
	}
	return v
}

// countingProvider wraps a solver.Provider and counts calls to each method,
// so a test can assert a fast path really never reached a given method
// (rather than only inferring it from the fake server's own request count).
type countingProvider struct {
	solver.Provider

	universeCalls atomic.Int64
}

func (c *countingProvider) Universe(ctx context.Context, pkg string) ([]solver.Version, error) {
	c.universeCalls.Add(1)
	return c.Provider.Universe(ctx, pkg)
}

// TestProviderLazinessAvoidsVersionsList pins that a root satisfied by the
// registry's highest_version is decided by that probe alone: Universe is
// never called and no versions-list request is made.
func TestProviderLazinessAvoidsVersionsList(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, nil)

	p := &countingProvider{Provider: newTestMetadataProvider(t, srv)}
	reqs := []solver.Requirement{{Package: "acme.widgets", Constraint: "^1.0.0"}}

	result, err := solver.Solve(t.Context(), reqs, p)
	if err != nil {
		t.Fatalf("Solve: unexpected error: %v", err)
	}
	if result.Versions["acme.widgets"] != testVersion100 {
		t.Fatalf("Versions[acme.widgets] = %q, want 1.0.0", result.Versions["acme.widgets"])
	}
	if got := p.universeCalls.Load(); got != 0 {
		t.Fatalf("Universe was called %d times, want 0 (the highest_version probe should have sufficed)", got)
	}
	if got := srv.Count(fakegalaxy.EndpointVersionsList); got != 0 {
		t.Fatalf("versions-list requests = %d, want 0", got)
	}
}

// TestProviderDependenciesWarmPinIsZeroNetwork pins that a deps-cache entry
// under the key scoped to the single configured server is served with no
// request at all, since boundBaseFor needs no network for one candidate.
func TestProviderDependenciesWarmPinIsZeroNetwork(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	st := store.New()
	cacheKey := helpers.ScopedDepsCacheKey(normalizeServerBase(srv.URL()), "acme.widgets@1.0.0")
	st.SetDepsCache(cacheKey, map[string]string{"acme.other": "^2.0.0"})

	p := NewMetadataProvider(testConfig(srv), testRuntime(srv), st, nil)
	deps, err := p.Dependencies(t.Context(), "acme.widgets", mustSolverVersion(t))
	if err != nil {
		t.Fatalf("Dependencies: unexpected error: %v", err)
	}
	if len(deps) != 1 || deps["acme.other"] != "^2.0.0" {
		t.Fatalf("Dependencies = %v, want {acme.other: ^2.0.0}", deps)
	}
	if got := srv.Total(); got != 0 {
		t.Fatalf("server received %d requests, want 0 (warm pin must be zero-network)", got)
	}
}

// TestProviderOfflineMissReturnsErrOfflineMode pins that Dependencies and
// Universe on an offline client with an empty cache both fail with
// helpers.ErrOfflineMode.
func TestProviderOfflineMissReturnsErrOfflineMode(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: "http://offline.example.invalid", Offline: true}
	runtime := infra.New(noopPrinter{}, fetch.NewOffline(0))
	p := NewMetadataProvider(cfg, runtime, store.New(), nil)

	if _, err := p.Universe(t.Context(), "acme.widgets"); !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("Universe error = %v, want errors.Is(err, ErrOfflineMode)", err)
	}
	if _, err := p.Dependencies(t.Context(), "acme.widgets", mustSolverVersion(t)); !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("Dependencies error = %v, want errors.Is(err, ErrOfflineMode)", err)
	}
}

// TestProviderMalformedDependencyKeyAborts pins that a dependency key that
// is not "ns.name" fails Dependencies with ErrInvalidDependencyKey and
// aborts a full Solve with it, rather than yielding a *solver.ConflictError.
func TestProviderMalformedDependencyKeyAborts(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, map[string]string{"not-a-fqdn": "^1.0.0"})

	p := newTestMetadataProvider(t, srv)
	_, err := p.Dependencies(t.Context(), "acme.widgets", mustSolverVersion(t))
	if !errors.Is(err, helpers.ErrInvalidDependencyKey) {
		t.Fatalf("Dependencies error = %v, want errors.Is(err, ErrInvalidDependencyKey)", err)
	}

	reqs := []solver.Requirement{{Package: "acme.widgets", Constraint: "^1.0.0"}}
	_, solveErr := solver.Solve(t.Context(), reqs, p)
	if !errors.Is(solveErr, helpers.ErrInvalidDependencyKey) {
		t.Fatalf("Solve error = %v, want errors.Is(err, ErrInvalidDependencyKey)", solveErr)
	}
	if _, ok := errors.AsType[*solver.ConflictError](solveErr); ok {
		t.Fatalf("Solve error is a *solver.ConflictError, want a plain aborting error: %v", solveErr)
	}
}

// TestBuildSolverUniverseIsDeterministic pins that buildSolverUniverse yields
// one descending order for every input order, the equal-precedence
// 1.0.0 vs 1.0.0+build tie included.
func TestBuildSolverUniverseIsDeterministic(t *testing.T) {
	t.Parallel()
	orderings := [][]string{
		{testVersion100, "1.0.0+build", "2.0.0", "1.5.0"},
		{"2.0.0", "1.0.0+build", "1.5.0", testVersion100},
		{"1.5.0", "2.0.0", testVersion100, "1.0.0+build"},
		{"1.0.0+build", testVersion100, "1.5.0", "2.0.0"},
	}
	// 1.0.0+build and 1.0.0 have equal semver precedence (build metadata is
	// not significant to Compare), so the tie-break is the original string
	// descending: "1.0.0+build" > "1.0.0" byte-wise, so it sorts first.
	want := []string{"2.0.0", "1.5.0", "1.0.0+build", testVersion100}

	var wantGot []string
	for i, raw := range orderings {
		got := buildSolverUniverse(raw)
		strs := make([]string, len(got))
		for j, v := range got {
			strs[j] = v.Original()
		}
		if i == 0 {
			wantGot = strs
			if !equalStrings(strs, want) {
				t.Fatalf("orderings[0] = %v, want %v", strs, want)
			}
			continue
		}
		if !equalStrings(strs, wantGot) {
			t.Fatalf("orderings[%d] = %v, want %v (must match every other input order)", i, strs, wantGot)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestProviderUnknownPackageIsNotAnError pins that a package with no
// versions reports ok=false and a nil universe with nil errors, and that a
// Solve requiring it fails with a "no published versions" ConflictError.
func TestProviderUnknownPackageIsNotAnError(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	p := newTestMetadataProvider(t, srv)

	v, ok, err := p.Highest(t.Context(), "acme.ghost")
	if err != nil || ok {
		t.Fatalf("Highest = (%v, %v, %v), want (_, false, nil)", v, ok, err)
	}
	versions, err := p.Universe(t.Context(), "acme.ghost")
	if err != nil || versions != nil {
		t.Fatalf("Universe = (%v, %v), want (nil, nil)", versions, err)
	}

	reqs := []solver.Requirement{{Package: "acme.ghost", Constraint: "^1.0.0"}}
	_, solveErr := solver.Solve(t.Context(), reqs, p)
	var conflictErr *solver.ConflictError
	if !errors.As(solveErr, &conflictErr) {
		t.Fatalf("Solve error is not a *solver.ConflictError: %v", solveErr)
	}
	if !errors.Is(solveErr, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("errors.Is(err, ErrNoVersionSatisfiesConstraints) = false")
	}
	proof := conflictErr.Error()
	if !strings.Contains(proof, "acme.ghost") || !strings.Contains(proof, "no published versions") {
		t.Fatalf("proof %q does not mention acme.ghost has no published versions", proof)
	}
}

// TestNoDepsProviderReturnsEmptyDependencies pins that noDepsProvider reports
// no dependencies without delegating, and that a Solve through it resolves
// only the root of a package that does declare a dependency.
func TestNoDepsProviderReturnsEmptyDependencies(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, map[string]string{"acme.other": "^1.0.0"})
	srv.AddVersion("acme", "other", testVersion100, nil)

	inner := &countingProvider{Provider: newTestMetadataProvider(t, srv)}
	wrapped := NewNoDepsProvider(inner)

	deps, err := wrapped.Dependencies(t.Context(), "acme.widgets", mustSolverVersion(t))
	if err != nil {
		t.Fatalf("Dependencies: unexpected error: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("Dependencies = %v, want an empty map", deps)
	}

	reqs := []solver.Requirement{{Package: "acme.widgets", Constraint: "^1.0.0"}}
	result, err := solver.Solve(t.Context(), reqs, wrapped)
	if err != nil {
		t.Fatalf("Solve: unexpected error: %v", err)
	}
	if len(result.Versions) != 1 || result.Versions["acme.widgets"] != testVersion100 {
		t.Fatalf("Versions = %v, want exactly {acme.widgets: 1.0.0} (no-deps must not pull in acme.other)", result.Versions)
	}
	if edges := result.Graph["acme.widgets"]; len(edges) != 0 {
		t.Fatalf("Graph[acme.widgets] = %v, want no edges under --no-deps", edges)
	}
}

// TestProviderHighestEmptyFallsBackToUniverse pins that an empty
// highest_version, a shape fakegalaxy never serves, makes Highest report
// ok=false while Universe still reads the versions list.
func TestProviderHighestEmptyFallsBackToUniverse(t *testing.T) {
	t.Parallel()
	srv := newEmptyHighestVersionServer(t)

	cfg := &config.Config{Server: srv.URL}
	runtime := infra.New(noopPrinter{}, srv.Client())
	p := NewMetadataProvider(cfg, runtime, store.New(), nil)

	_, ok, err := p.Highest(t.Context(), "acme.widgets")
	if err != nil || ok {
		t.Fatalf("Highest = (_, %v, %v), want (_, false, nil)", ok, err)
	}

	versions, err := p.Universe(t.Context(), "acme.widgets")
	if err != nil {
		t.Fatalf("Universe: unexpected error: %v", err)
	}
	if len(versions) != 1 || versions[0].Original() != testVersion100 {
		t.Fatalf("Universe = %v, want exactly [1.0.0]", versions)
	}
}

// newEmptyHighestVersionServer serves acme/widgets' v3 root metadata, with
// highest_version left empty, and its versions list; anything else 404s.
func newEmptyHighestVersionServer(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/collections/acme/widgets/":
			root := map[string]any{
				"versions_url":    srv.URL + "/api/v3/collections/acme/widgets/versions/",
				"highest_version": map[string]any{"href": "", "version": ""},
			}
			_ = json.NewEncoder(w).Encode(root)
		case "/api/v3/collections/acme/widgets/versions/":
			payload := map[string]any{
				"meta": map[string]any{"count": 1},
				"data": []map[string]any{{"version": testVersion100, "href": srv.URL + "/api/v3/collections/acme/widgets/versions/1.0.0/"}},
			}
			_ = json.NewEncoder(w).Encode(payload)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestIsUnknownPackageErrorClassification pins that only a bare 404 and
// ErrLoadMetadataFailed count as an unknown package, while an auth failure
// or an unavailable server must abort the run instead.
func TestIsUnknownPackageErrorClassification(t *testing.T) {
	t.Parallel()
	notFound := &cacheManager.HTTPStatusError{Code: http.StatusNotFound}
	unauthorized := &cacheManager.HTTPStatusError{Code: http.StatusUnauthorized}
	unavailable := &cacheManager.HTTPStatusError{Code: http.StatusServiceUnavailable}

	cases := []struct {
		err  error
		name string
		want bool
	}{
		{name: "bare 404", err: notFound, want: true},
		{name: "ErrLoadMetadataFailed", err: helpers.ErrLoadMetadataFailed, want: true},
		{
			name: "401 wrapped in ErrGalaxyAuthFailed",
			err:  fmt.Errorf("%w: server a: %w", helpers.ErrGalaxyAuthFailed, unauthorized),
			want: false,
		},
		{
			name: "503 wrapped in ErrGalaxyServerUnavailable",
			err:  fmt.Errorf("%w: server a: %w", helpers.ErrGalaxyServerUnavailable, unavailable),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isUnknownPackageError(tc.err); got != tc.want {
				t.Fatalf("isUnknownPackageError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// newTestMetadataProvider builds a MetadataProvider pointed at srv.
func newTestMetadataProvider(t *testing.T, srv *fakegalaxy.Server) *MetadataProvider {
	t.Helper()
	return NewMetadataProvider(testConfig(srv), testRuntime(srv), store.New(), nil)
}

func testConfig(srv *fakegalaxy.Server) *config.Config {
	return &config.Config{Server: srv.URL()}
}

func testRuntime(srv *fakegalaxy.Server) *infra.Infra {
	return infra.New(noopPrinter{}, srv.Client())
}
