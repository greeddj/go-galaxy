package collections_test

// This file pins, through the public Lock, that adding a root keeps what
// galaxy.lock pins for the collections already locked, on the cache that
// locked them and on an empty one alike.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// lockAndLoad runs Lock over f and returns the lockfile it wrote.
func lockAndLoad(t *testing.T, f *e2eFixture) *lockfile.File {
	t.Helper()
	if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	lf, err := lockfile.Load(path)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", path, err)
	}
	return lf
}

// assertLockedVersions fails unless lf pins exactly the collections in want,
// each at its version.
func assertLockedVersions(t *testing.T, lf *lockfile.File, want map[string]string) {
	t.Helper()
	got := make(map[string]string, len(lf.Collections))
	for _, e := range lf.Collections {
		got[e.Name] = e.Version
	}
	if len(got) != len(want) {
		t.Fatalf("locked %v, want %v", got, want)
	}
	for name, version := range want {
		if got[name] != version {
			t.Fatalf("locked %v, want %v", got, want)
		}
	}
}

// cacheState is one cache a lock that adds a root runs on: the one that wrote
// galaxy.lock, whose metadata is still fresh, or an empty one.
type cacheState struct {
	name  string
	fresh bool
}

// cacheStates is both cacheState values.
func cacheStates() []cacheState {
	return []cacheState{{name: "same cache", fresh: false}, {name: "empty cache", fresh: true}}
}

// lockFixtureThenAdd locks newE2EFixture's acme.app and acme.lib at 1.0.0,
// lets publish add releases, then requires roots, on an empty cache if fresh.
func lockFixtureThenAdd(t *testing.T, fresh bool, publish func(f *e2eFixture), roots ...string) *e2eFixture {
	t.Helper()
	f := newE2EFixture(t)
	assertLockedVersions(t, lockAndLoad(t, f), map[string]string{"acme.app": testVersion100, "acme.lib": testVersion100})
	publish(f)
	writeRequirementsMulti(t, f.cfg.RequirementsFile, roots...)
	if fresh {
		f.cfg.CacheDir = filepath.Join(t.TempDir(), "cache")
	}
	return f
}

// TestLockKeepsALockedTransitiveWhenARootIsAdded pins that a root added after
// a newer acme.lib release leaves acme.lib at its locked 1.0.0, which the new
// root's own constraint allows, rather than taking the release.
func TestLockKeepsALockedTransitiveWhenARootIsAdded(t *testing.T) {
	t.Parallel()
	for _, tc := range cacheStates() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := lockFixtureThenAdd(t, tc.fresh, func(f *e2eFixture) {
				f.server.AddVersion("acme", "lib", "1.1.0", nil)
				f.server.AddVersion("acme", "extra", testVersion100, map[string]string{"acme.lib": ">=1.0.0"})
			}, "acme.app", "acme.extra")

			assertLockedVersions(t, lockAndLoad(t, f), map[string]string{
				"acme.app": testVersion100, "acme.extra": testVersion100, "acme.lib": testVersion100,
			})
		})
	}
}

// TestLockTakesAnOlderNewRootOverMovingALockedCollection pins the pick order
// end to end: acme.alpha, sorting before every locked collection, is resolved
// at 1.0.0, since its 2.0.0 needs acme.lib >=2.0.0 past the locked 1.0.0.
func TestLockTakesAnOlderNewRootOverMovingALockedCollection(t *testing.T) {
	t.Parallel()
	for _, tc := range cacheStates() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := lockFixtureThenAdd(t, tc.fresh, func(f *e2eFixture) {
				f.server.AddVersion("acme", "lib", "2.0.0", nil)
				f.server.AddVersion("acme", "alpha", testVersion100, map[string]string{"acme.lib": ">=1.0.0"})
				f.server.AddVersion("acme", "alpha", "2.0.0", map[string]string{"acme.lib": ">=2.0.0"})
			}, "acme.alpha", "acme.app")

			assertLockedVersions(t, lockAndLoad(t, f), map[string]string{
				"acme.alpha": testVersion100, "acme.app": testVersion100, "acme.lib": testVersion100,
			})
		})
	}
}
