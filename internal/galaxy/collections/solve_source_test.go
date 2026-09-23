package collections

import (
	"context"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// testVersion200 is a fixture version literal, a const to satisfy goconst.
const testVersion200 = "2.0.0"

// TestSolveCollectionsMultiSourceRoots pins that two roots pinned to two
// different servers each resolve from, and record as Source, their own server.
func TestSolveCollectionsMultiSourceRoots(t *testing.T) {
	t.Parallel()
	srvX := fakegalaxy.New(t)
	srvY := fakegalaxy.New(t)
	srvX.AddVersion("acme", "a", testVersion100, nil)
	srvY.AddVersion("acme", "b", testVersion200, nil)

	runtime := infra.New(noopPrinter{}, srvX.Client())
	cfg := &config.Config{Server: srvX.URL()}
	roots := []collection{
		{Namespace: "acme", Name: "a", Source: srvX.URL(), Constraint: "^1.0.0"},
		{Namespace: "acme", Name: "b", Source: srvY.URL(), Constraint: "^2.0.0"},
	}

	resolved, _, err := solveCollections(context.Background(), newCollectionDeps(cfg, runtime, store.New()), roots)
	if err != nil {
		t.Fatalf("solveCollections: unexpected error: %v", err)
	}

	a, ok := resolved["acme.a"]
	if !ok || a.Version != testVersion100 || a.Source != srvX.URL() {
		t.Fatalf("resolved[acme.a] = %+v, want version 1.0.0 from %s", a, srvX.URL())
	}
	b, ok := resolved["acme.b"]
	if !ok || b.Version != testVersion200 || b.Source != srvY.URL() {
		t.Fatalf("resolved[acme.b] = %+v, want version 2.0.0 from %s", b, srvY.URL())
	}
}

// TestSolveCollectionsTransitiveDepUsesDefaultServer pins that a transitive
// dependency never inherits its parent's source: a pinned root's dependency
// resolves from, and records as Source, the default server.
func TestSolveCollectionsTransitiveDepUsesDefaultServer(t *testing.T) {
	t.Parallel()
	srvRoot := fakegalaxy.New(t)
	srvDefault := fakegalaxy.New(t)
	srvRoot.AddVersion("acme", "a", testVersion100, map[string]string{"acme.dep": "^1.0.0"})
	srvDefault.AddVersion("acme", "dep", testVersion100, nil)

	runtime := infra.New(noopPrinter{}, srvRoot.Client())
	cfg := &config.Config{Server: srvDefault.URL()}
	roots := []collection{
		{Namespace: "acme", Name: "a", Source: srvRoot.URL(), Constraint: "^1.0.0"},
	}

	resolved, graph, err := solveCollections(context.Background(), newCollectionDeps(cfg, runtime, store.New()), roots)
	if err != nil {
		t.Fatalf("solveCollections: unexpected error: %v", err)
	}

	dep, ok := resolved["acme.dep"]
	if !ok {
		t.Fatalf("resolved is missing acme.dep: %v", resolved)
	}
	if dep.Version != testVersion100 {
		t.Fatalf("resolved[acme.dep].Version = %q, want 1.0.0", dep.Version)
	}
	if dep.Source != srvDefault.URL() {
		t.Fatalf("resolved[acme.dep].Source = %q, want cfg.Server (%s), not the root's own source (%s)",
			dep.Source, srvDefault.URL(), srvRoot.URL())
	}

	rootKey := resolved["acme.a"].key()
	depKey := dep.key()
	edges := graph[rootKey]
	if len(edges) != 1 || edges[0] != depKey {
		t.Fatalf("graph[%s] = %v, want exactly [%s]", rootKey, edges, depKey)
	}
}

// TestSolveCollectionsSharedTransitiveDepUsesDefaultServer pins that a
// dependency shared by roots pinned to two different servers resolves against
// the default server.
func TestSolveCollectionsSharedTransitiveDepUsesDefaultServer(t *testing.T) {
	t.Parallel()
	srvX := fakegalaxy.New(t)
	srvY := fakegalaxy.New(t)
	srvDefault := fakegalaxy.New(t)
	srvX.AddVersion("acme", "a", testVersion100, map[string]string{"acme.shared": "^1.0.0"})
	srvY.AddVersion("acme", "b", testVersion100, map[string]string{"acme.shared": "^1.0.0"})
	srvDefault.AddVersion("acme", "shared", testVersion100, nil)

	runtime := infra.New(noopPrinter{}, srvX.Client())
	cfg := &config.Config{Server: srvDefault.URL()}
	roots := []collection{
		{Namespace: "acme", Name: "a", Source: srvX.URL(), Constraint: "^1.0.0"},
		{Namespace: "acme", Name: "b", Source: srvY.URL(), Constraint: "^1.0.0"},
	}

	resolved, _, err := solveCollections(context.Background(), newCollectionDeps(cfg, runtime, store.New()), roots)
	if err != nil {
		t.Fatalf("solveCollections: unexpected error: %v", err)
	}

	shared, ok := resolved["acme.shared"]
	if !ok {
		t.Fatalf("resolved is missing acme.shared: %v", resolved)
	}
	if shared.Source != srvDefault.URL() {
		t.Fatalf("resolved[acme.shared].Source = %q, want cfg.Server (%s)", shared.Source, srvDefault.URL())
	}
}

// TestRootSourceMap pins that a pinned root maps to its source, an unpinned
// root to "" (not cfg.Server), and a transitive dependency is never a key.
func TestRootSourceMap(t *testing.T) {
	t.Parallel()
	roots := []collection{
		{Namespace: "acme", Name: "a", Source: "https://explicit.example"},
		{Namespace: "acme", Name: "b"},
	}

	sources := rootSourceMap(roots)
	if sources["acme.a"] != "https://explicit.example" {
		t.Fatalf("sources[acme.a] = %q, want the explicit source", sources["acme.a"])
	}
	if v, ok := sources["acme.b"]; !ok || v != "" {
		t.Fatalf("sources[acme.b] = (%q, %v), want (\"\", true) - unpinned", v, ok)
	}
	if _, ok := sources["acme.dep"]; ok {
		t.Fatalf("sources unexpectedly has an entry for a non-root fqdn: %v", sources)
	}
}

// TestMetadataProviderSourceOf pins that sourceOf returns a recorded source
// and "" (unpinned, so the whole server list is walked) for any other fqdn.
func TestMetadataProviderSourceOf(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: "https://default.example"}
	sources := map[string]string{"acme.a": "https://explicit.example"}
	p := NewMetadataProvider(cfg, infra.New(noopPrinter{}, nil), store.New(), sources)

	if got := p.sourceOf("acme.a"); got != "https://explicit.example" {
		t.Fatalf("sourceOf(acme.a) = %q, want the explicit source", got)
	}
	if got := p.sourceOf("acme.dep"); got != "" {
		t.Fatalf("sourceOf(acme.dep) = %q, want \"\" (unpinned)", got)
	}
}

// TestResolvePinnedExactRootKeepsSourceUnderPrewarm pins that exactly pinned
// roots warmed by prewarmRootMetadata still resolve with their source: server
// as Source, and the default server is never asked.
func TestResolvePinnedExactRootKeepsSourceUnderPrewarm(t *testing.T) {
	t.Parallel()
	srvPinned := fakegalaxy.New(t)
	srvDefault := fakegalaxy.New(t)
	srvPinned.AddVersion("acme", "one", testVersion100, nil)
	srvPinned.AddVersion("acme", "two", testVersion100, nil)

	cfg := &config.Config{Server: srvDefault.URL(), Workers: 2}
	runtime := infra.New(noopPrinter{}, srvPinned.Client())
	roots := []collection{
		{Namespace: "acme", Name: "one", Version: testVersion100, Constraint: testVersion100, Source: srvPinned.URL()},
		{Namespace: "acme", Name: "two", Version: testVersion100, Constraint: testVersion100, Source: srvPinned.URL()},
	}

	resolved, _, err := resolveCollectionsInternal(
		context.Background(), newCollectionDeps(cfg, runtime, store.New()), roots, resolveNestedPartial)
	if err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}

	for _, fqdn := range []string{"acme.one", "acme.two"} {
		got, ok := resolved[fqdn]
		if !ok {
			t.Fatalf("expected a resolved entry for %s, got %#v", fqdn, resolved)
		}
		if got.Source != srvPinned.URL() {
			t.Fatalf("resolved[%s].Source = %q, want the pinned server %q, not cfg.Server (%s)",
				fqdn, got.Source, srvPinned.URL(), srvDefault.URL())
		}
	}
	if n := srvDefault.Total(); n != 0 {
		t.Fatalf("srvDefault.Total() = %d, want 0 - a pinned root never consults cfg.Server", n)
	}
}

// TestResolvePinnedExactRootKeepsSourceUnderNoDeps pins that under NoDeps,
// where nothing binds an exactly pinned root, its source: is still the
// resolved Source.
func TestResolvePinnedExactRootKeepsSourceUnderNoDeps(t *testing.T) {
	t.Parallel()
	srvPinned := fakegalaxy.New(t)
	srvDefault := fakegalaxy.New(t)
	srvPinned.AddVersion("acme", "pinned", testVersion100, nil)

	cfg := &config.Config{Server: srvDefault.URL(), Workers: 2, NoDeps: true}
	runtime := infra.New(noopPrinter{}, srvPinned.Client())
	roots := []collection{
		{Namespace: "acme", Name: "pinned", Version: testVersion100, Constraint: testVersion100, Source: srvPinned.URL()},
	}

	resolved, _, err := resolveCollectionsInternal(
		context.Background(), newCollectionDeps(cfg, runtime, store.New()), roots, resolveNestedPartial)
	if err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}

	got, ok := resolved["acme.pinned"]
	if !ok {
		t.Fatalf("expected a resolved entry for acme.pinned, got %#v", resolved)
	}
	if got.Source != srvPinned.URL() {
		t.Fatalf("resolved[acme.pinned].Source = %q, want the pinned server %q, not cfg.Server (%s)",
			got.Source, srvPinned.URL(), srvDefault.URL())
	}
}

// TestSourceForPrefersBindingOverSource pins sourceFor's tier order: binding,
// then source: (a server_list id recorded as its server's URL), then the
// first configured server.
func TestSourceForPrefersBindingOverSource(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Servers: []config.Server{
			{ID: "first", URL: "https://first.example"},
			{ID: "pinned", URL: "https://pinned.example"},
		},
		Server: "https://first.example",
	}
	bindings := map[string]string{"acme.bound": "https://bound.example"}
	sources := map[string]string{
		"acme.bound":    "pinned",
		"acme.byid":     "pinned",
		"acme.byurl":    "https://anonymous.example/content",
		"acme.unpinned": "",
	}

	if got := sourceFor("acme.bound", bindings, sources, cfg); got != "https://bound.example" {
		t.Fatalf("sourceFor(acme.bound) = %q, want the binding to outrank the source:", got)
	}
	if got := sourceFor("acme.byid", bindings, sources, cfg); got != "https://pinned.example" {
		t.Fatalf("sourceFor(acme.byid) = %q, want the id resolved to its server URL", got)
	}
	if got := sourceFor("acme.byurl", bindings, sources, cfg); got != "https://anonymous.example/content" {
		t.Fatalf("sourceFor(acme.byurl) = %q, want the pinned URL itself", got)
	}
	if got := sourceFor("acme.unpinned", bindings, sources, cfg); got != "https://first.example" {
		t.Fatalf("sourceFor(acme.unpinned) = %q, want the first configured server", got)
	}
	if got := sourceFor("acme.dep", bindings, sources, cfg); got != "https://first.example" {
		t.Fatalf("sourceFor(acme.dep) = %q, want the first configured server", got)
	}
}
