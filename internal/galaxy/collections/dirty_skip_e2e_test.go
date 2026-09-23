package collections_test

// Proves collections.Start's initInstall really wraps its backend with
// cacheManager.WithCleanSaveSkip, end to end against a real local backend;
// the store and cache unit tests never drive Start.

import (
	"context"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
)

// reloadLastSnapshot returns Meta.LastSnapshot as persisted in cacheDir,
// through a fresh local backend so each call observes what is on disk.
func reloadLastSnapshot(t *testing.T, cacheDir string) time.Time {
	t.Helper()
	ctx := context.Background()
	backend := local.New(cacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()

	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	return st.MetaSnapshot().LastSnapshot
}

// TestIdleInstallDoesNotRewriteTheSnapshot pins that an idle second install
// leaves the persisted LastSnapshot untouched, while a real requirements
// change, the positive control, advances it.
func TestIdleInstallDoesNotRewriteTheSnapshot(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (populate the cache): %v", err)
	}
	firstStamp := reloadLastSnapshot(t, f.cfg.CacheDir)
	if firstStamp.IsZero() {
		t.Fatal("expected a non-zero LastSnapshot after the first install")
	}

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Start (fully idle): %v", err)
	}
	idleStamp := reloadLastSnapshot(t, f.cfg.CacheDir)
	if !idleStamp.Equal(firstStamp) {
		t.Fatalf(
			"LastSnapshot after an idle install = %v, want unchanged from %v (an idle run must not rewrite the snapshot)",
			idleStamp, firstStamp,
		)
	}

	// Positive control: a genuine requirements.yml change forces a fresh
	// resolve, which writes into the store through the normal setters and
	// must advance the stamp.
	f.server.AddVersion("acme", "extra", testVersion100, nil)
	writeRequirementsMulti(t, f.cfg.RequirementsFile, "acme.app", "acme.extra")
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("third Start (a real requirements change): %v", err)
	}
	changedStamp := reloadLastSnapshot(t, f.cfg.CacheDir)
	if !changedStamp.After(idleStamp) {
		t.Fatalf("LastSnapshot after a real requirements change = %v, want strictly after %v", changedStamp, idleStamp)
	}
}
