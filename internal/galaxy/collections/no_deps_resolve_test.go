package collections

// Tests of resolveCollectionsInternal under cfg.NoDeps, which resolves through
// the solver wrapped in NewNoDepsProvider rather than a path of its own.

import (
	"context"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestNoDepsUnpinnedRootResolvesConcreteVersion pins that a "*" root under
// NoDeps resolves to the highest version, so "*" never reaches the artifact
// cache key or the lockfile.
func TestNoDepsUnpinnedRootResolvesConcreteVersion(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "solo", "1.0.0", nil)
	srv.AddVersion("acme", "solo", "2.0.0", nil)

	cfg := &config.Config{Server: srv.URL(), Workers: 2, NoDeps: true}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root := collection{Namespace: "acme", Name: "solo", Version: "*", Constraint: "*", Source: srv.URL()}
	resolved, graph, err := resolveCollectionsInternal(context.Background(), deps, []collection{root}, resolveNestedPartial)
	if err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}

	got, ok := resolved["acme.solo"]
	if !ok {
		t.Fatalf("expected a resolved entry for acme.solo, got %#v", resolved)
	}
	if got.Version != "2.0.0" {
		t.Fatalf("resolved version = %q, want %q (the highest registered version)", got.Version, "2.0.0")
	}
	if _, ok := graph[got.key()]; !ok {
		t.Fatalf("expected graph to contain a node for %s, got %#v", got.key(), graph)
	}
}

// TestNoDepsPinnedRootSkipsMetadataFetch pins that an exactly pinned root
// resolves under NoDeps with zero HTTP requests: the solver's exact-pin path
// asks nothing and NewNoDepsProvider never fetches dependencies.
func TestNoDepsPinnedRootSkipsMetadataFetch(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "pinned", "1.0.0", nil)

	cfg := &config.Config{Server: srv.URL(), Workers: 2, NoDeps: true}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root := collection{Namespace: "acme", Name: "pinned", Version: "1.0.0", Constraint: "1.0.0", Source: srv.URL()}
	resolved, _, err := resolveCollectionsInternal(context.Background(), deps, []collection{root}, resolveNestedPartial)
	if err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}

	got, ok := resolved["acme.pinned"]
	if !ok {
		t.Fatalf("expected a resolved entry for acme.pinned, got %#v", resolved)
	}
	if got.Version != "1.0.0" {
		t.Fatalf("resolved version = %q, want %q", got.Version, "1.0.0")
	}
	if total := srv.Total(); total != 0 {
		t.Fatalf("expected 0 HTTP requests for a pinned root, got %d (request counts: root=%d versions=%d detail=%d artifact=%d)",
			total,
			srv.Count(fakegalaxy.EndpointRootMetadata),
			srv.Count(fakegalaxy.EndpointVersionsList),
			srv.Count(fakegalaxy.EndpointVersionDetail),
			srv.Count(fakegalaxy.EndpointArtifact),
		)
	}
}
