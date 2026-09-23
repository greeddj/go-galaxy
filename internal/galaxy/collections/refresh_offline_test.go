package collections

// Store.snapshotData ages out APICache, DepsCache and Versions but never the
// resolve snapshot, so an old cache routinely holds a resolution with no
// metadata behind it: the state refreshBypassesSnapshot's !cfg.Offline guards.

import (
	"context"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// TestRefreshOfflinePreservesResolveWithStaleMetadataCaches pins that
// --refresh --offline resolves from the resolve snapshot with empty metadata
// caches, over fetch.NewOffline as production wires it, not cfg.Offline alone.
func TestRefreshOfflinePreservesResolveWithStaleMetadataCaches(t *testing.T) {
	t.Parallel()
	cfg, _ := poisonedVersionFixture(t)
	cfg.Refresh = true
	cfg.Offline = true
	runtime := infra.New(noopPrinter{}, fetch.NewOffline(0))

	roots, _, err := loadRoots(cfg, runtime)
	if err != nil {
		t.Fatalf("loadRoots: %v", err)
	}
	reqSpec := buildRequirementsSpec(roots)
	reqHash := requirementsSignatureFromSpec(reqSpec, cfg.NoDeps, serversSignature(cfg))

	st := store.New()
	st.SetResolvedAll(map[string]store.ResolvedEntry{
		"acme.widgets": {Version: testVersion100, Source: cfg.Server},
	})
	st.SetGraphSnapshot(map[string][]string{"acme.widgets@" + testVersion100: {}})
	st.SetMetaRequirements(reqHash, cfg.Server)
	st.SetRequirements(reqSpec)
	st.ClearCaches()

	deps := newCollectionDeps(cfg, runtime, st)
	resolved, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveTopLevel)
	if err != nil {
		t.Fatalf("resolveCollectionsInternal (refresh + offline, resolve snapshot only, no metadata cache): %v", err)
	}
	got, ok := resolved["acme.widgets"]
	if !ok || got.Version != testVersion100 {
		t.Fatalf(`resolved["acme.widgets"] = %+v, ok=%v, want Version=%s (served from the resolve snapshot)`,
			got, ok, testVersion100)
	}
}
