package collections_test

// These e2e tests drive collections.Warm then cleanup.Start on a warm-only
// cache: warm writes no installed entry, so only the snapshot's warmed set
// keeps its extracted trees alive through cleanup.

import (
	"context"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/cleanup"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
)

// TestWarmThenCleanupKeepsExtractedTrees pins that warm's extracted trees
// survive repeated cleanup runs, which see no installed workspace for a
// warm-only project, and that a later install still hardlinks from them.
func TestWarmThenCleanupKeepsExtractedTrees(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.appV1.SHA256)
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.libV1.SHA256)

	if err := cleanup.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("cleanup.Start: %v", err)
	}
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.appV1.SHA256)
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.libV1.SHA256)

	// Cleanup keeps a warmed entry in the snapshot, so a second run is just
	// as non-destructive as the first.
	if err := cleanup.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second cleanup.Start: %v", err)
	}
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.appV1.SHA256)
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.libV1.SHA256)

	// An install afterwards must still find the warmed trees intact to
	// hardlink from.
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (install after warm+cleanup): %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
}
