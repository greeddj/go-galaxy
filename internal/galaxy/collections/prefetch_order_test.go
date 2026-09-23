package collections

// Tests that the prefetch queue is ordered by install level, both in
// sortTasksByLevel alone and through startPrefetcher, where one worker's FIFO
// drain makes the Commit order observable.

import (
	"context"
	"slices"
	"testing"

	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestSortTasksByLevelOrdersByLevelThenKey pins ascending level with a key
// tie-break, and that a key absent from levelIndex sorts at level 0 rather
// than arbitrarily.
func TestSortTasksByLevelOrdersByLevelThenKey(t *testing.T) {
	t.Parallel()
	base := collection{Namespace: "acme", Name: "base", Version: "1.0.0"}
	lib1 := collection{Namespace: "acme", Name: "lib1", Version: "1.0.0"}
	lib2 := collection{Namespace: "acme", Name: "lib2", Version: "1.0.0"}
	app := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}

	// lib2 is listed before lib1 within level 1 on purpose, to prove the key
	// tie-break re-sorts within a level rather than preserving input order.
	levels := [][]string{{base.key()}, {lib2.key(), lib1.key()}, {app.key()}}
	levelIndex := buildLevelIndex(levels)

	tasks := []collection{app, lib2, base, lib1}
	sortTasksByLevel(tasks, levelIndex)

	want := []string{base.key(), lib1.key(), lib2.key(), app.key()}
	got := make([]string, len(tasks))
	for i, col := range tasks {
		got[i] = col.key()
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sortTasksByLevel order = %v, want %v", got, want)
	}

	for i := 0; i+1 < len(tasks); i++ {
		li, lj := levelIndex[tasks[i].key()], levelIndex[tasks[i+1].key()]
		if li > lj {
			t.Fatalf("level not ascending at index %d: %d > %d", i, li, lj)
		}
	}

	// A key absent from levelIndex must default to level 0 and sort
	// deterministically alongside the explicit level-0 entries, tie-broken by
	// key: "acme.base@1.0.0" < "acme.missing@1.0.0" < the level-2 app.
	missing := collection{Namespace: "acme", Name: "missing", Version: "1.0.0"}
	tasks2 := []collection{app, missing, base}
	sortTasksByLevel(tasks2, levelIndex)

	want2 := []string{base.key(), missing.key(), app.key()}
	got2 := make([]string, len(tasks2))
	for i, col := range tasks2 {
		got2[i] = col.key()
	}
	if !slices.Equal(got2, want2) {
		t.Fatalf("sortTasksByLevel with absent key order = %v, want %v", got2, want2)
	}
}

// waitAll blocks until every key's prefetch completes, as installLevels does
// through Wait. It must precede Close, whose cancel would otherwise abort
// in-flight downloads and make the commit order nondeterministic.
func waitAll(t *testing.T, prefetch *prefetcher, keys []string) {
	t.Helper()
	for _, key := range keys {
		if _, _, ok, err := prefetch.Wait(key); !ok {
			t.Fatalf("Wait(%s): key was never registered with the prefetcher", key)
		} else if err != nil {
			t.Fatalf("Wait(%s): unexpected prefetch error: %v", key, err)
		}
	}
}

// TestPrefetchQueueOrderedByLevel pins that with one prefetch worker the
// chain app -> lib -> base commits leaf-first, so the queue follows install
// level rather than map order.
func TestPrefetchQueueOrderedByLevel(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "base", "1.0.0", nil)
	srv.AddVersion("acme", "lib", "1.0.0", map[string]string{"acme.base": ">=1.0.0"})
	srv.AddVersion("acme", "app", "1.0.0", map[string]string{"acme.lib": ">=1.0.0"})

	base := collection{Namespace: "acme", Name: "base", Version: "1.0.0"}
	lib := collection{Namespace: "acme", Name: "lib", Version: "1.0.0"}
	app := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}
	collections := map[string]collection{base.key(): base, lib.key(): lib, app.key(): app}
	graph := map[string][]string{
		app.key():  {lib.key()},
		lib.key():  {base.key()},
		base.key(): {},
	}

	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels: %v", err)
	}
	if len(levels) != 3 || len(levels[0]) != 1 || len(levels[1]) != 1 || len(levels[2]) != 1 ||
		levels[0][0] != base.key() || levels[1][0] != lib.key() || levels[2][0] != app.key() {
		t.Fatalf("unexpected levels, want [[base],[lib],[app]]: %#v", levels)
	}

	fx := newPrefetchHandoffFixture(t, srv, 1)
	prefetch := startPrefetcher(
		context.Background(),
		newPrefetchDeps(fx.cfg, fx.runtime, fx.st, fx.artifacts, fx.root),
		collections,
		levels,
	)
	// Wait before Close, whose cancel would race the downloads still running.
	waitAll(t, prefetch, []string{base.key(), lib.key(), app.key()})
	prefetch.Close()

	want := []string{artifactKey(base), artifactKey(lib), artifactKey(app)}
	got := fx.artifacts.commitOrderSnapshot()
	if !slices.Equal(got, want) {
		t.Fatalf("commit order = %v, want %v (leaf-first enqueue matching level order)", got, want)
	}
}
