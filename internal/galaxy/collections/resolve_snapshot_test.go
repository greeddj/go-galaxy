package collections

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

const (
	snapshotServer  = "https://hub.example/"
	snapshotTarball = "https://dl.example/acme-app-1.0.0.tar.gz"
)

// snapGitPinned is refetchRepoURL's locator pinned to refetchCommit.
func snapGitPinned() string {
	return gitsource.Locator{URL: refetchRepoURL, Commit: refetchCommit}.String()
}

// snapGitUnpinned is refetchRepoURL's locator with no commit.
func snapGitUnpinned() string { return gitsource.Locator{URL: refetchRepoURL}.String() }

// snapURLPinned is snapshotTarball's locator pinned to a stand-in digest.
func snapURLPinned() string {
	return urlsource.Locator{URL: snapshotTarball, SHA256: strings.Repeat("c", 64)}.String()
}

// snapURLUnpinned is snapshotTarball's locator with no digest.
func snapURLUnpinned() string { return urlsource.Locator{URL: snapshotTarball}.String() }

// TestCollectionFromResolvedEntryRefusals pins that every snapshot reader
// refuses an unversioned, unnamed, torn or unpinned entry.
func TestCollectionFromResolvedEntryRefusals(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: snapshotServer}
	refused := map[string]struct {
		entry store.ResolvedEntry
		fqdn  string
	}{
		"no version":           {fqdn: "acme.app", entry: store.ResolvedEntry{Source: snapshotServer}},
		"not an fqdn":          {fqdn: "acme", entry: store.ResolvedEntry{Version: "1.0.0"}},
		"unpinned git locator": {fqdn: "acme.app", entry: store.ResolvedEntry{Version: "1.0.0", Source: snapGitUnpinned()}},
		"torn git locator":     {fqdn: "acme.app", entry: store.ResolvedEntry{Version: "1.0.0", Source: gitsource.LocatorPrefix + "x"}},
		"unpinned url locator": {fqdn: "acme.app", entry: store.ResolvedEntry{Version: "1.0.0", Source: snapURLUnpinned()}},
	}
	for name, tt := range refused {
		if col, ok := collectionFromResolvedEntry(cfg, tt.fqdn, tt.entry); ok {
			t.Errorf("%s: collectionFromResolvedEntry() = %+v, want a refusal", name, col)
		}
	}

	snapshot := map[string]store.ResolvedEntry{"acme.app": {Version: "1.0.0"}, "acme.lib": {}}
	if got, ok := buildResolvedSnapshot(cfg, snapshot); ok || got != nil {
		t.Errorf("buildResolvedSnapshot() with one unversioned entry = (%v, %v), want (nil, false)", got, ok)
	}
}

// TestCollectionFromResolvedEntryRebuilds pins the accepted shapes: a locator
// keeps its kind and pin, and only a sourceless entry takes the run's server.
func TestCollectionFromResolvedEntryRebuilds(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: snapshotServer}
	accepted := []struct {
		entry store.ResolvedEntry
		want  collection
	}{
		{
			entry: store.ResolvedEntry{Version: "1.0.0", Source: snapGitPinned(), Ref: refetchRef},
			want:  collection{Namespace: "acme", Name: "app", Version: "1.0.0", Source: snapGitPinned(), Type: typeGit, Ref: refetchRef},
		},
		{
			entry: store.ResolvedEntry{Version: "1.0.0", Source: snapURLPinned()},
			want: collection{
				Namespace: "acme", Name: "app", Version: "1.0.0", Source: snapURLPinned(), Type: typeURL, SHA256: strings.Repeat("c", 64),
			},
		},
		{
			entry: store.ResolvedEntry{Version: "1.0.0"},
			want:  collection{Namespace: "acme", Name: "app", Version: "1.0.0", Source: snapshotServer},
		},
		{
			entry: store.ResolvedEntry{Version: "1.0.0", Source: "https://mirror.example/"},
			want:  collection{Namespace: "acme", Name: "app", Version: "1.0.0", Source: "https://mirror.example/"},
		},
	}
	for _, tt := range accepted {
		if got, ok := collectionFromResolvedEntry(cfg, "acme.app", tt.entry); !ok || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("collectionFromResolvedEntry(%+v) = (%+v, %v), want %+v", tt.entry, got, ok, tt.want)
		}
	}
}

// rootsMatchSnapshotCases are one root each against a fixed snapshot: the
// matches a replay needs, then every miss that sends the run to a solve.
func rootsMatchSnapshotCases() []struct {
	name string
	root collection
	want bool
} {
	app := func(constraint, version string) collection {
		return collection{Namespace: "acme", Name: "app", Type: typeGalaxy, Constraint: constraint, Version: version}
	}
	otherCommit := gitsource.Locator{URL: refetchRepoURL, Commit: refetchOther}.String()
	return []struct {
		name string
		root collection
		want bool
	}{
		{name: "constraint satisfied", root: app(">=1.0.0", ""), want: true},
		{name: "exact version stands in for a constraint", root: app("", "1.2.0"), want: true},
		{name: "git root at its locator", root: collection{Namespace: "acme", Name: "git", Type: typeGit, Source: snapGitPinned()}, want: true},
		{name: "unsupported type", root: collection{Namespace: "acme", Name: "app", Type: "file"}},
		{name: "root absent from the snapshot", root: collection{Namespace: "acme", Name: "new", Type: typeGalaxy}},
		{name: "git root at another commit", root: collection{Namespace: "acme", Name: "git", Type: typeGit, Source: otherCommit}},
		{name: "constraint excludes the snapshot version", root: app(">="+testVersion200, "")},
		{name: "snapshot version is not semver", root: collection{Namespace: "acme", Name: "bad", Type: typeGalaxy, Constraint: ">=1.0.0"}},
		{name: "no graph node", root: collection{Namespace: "acme", Name: "lib", Type: typeGalaxy}},
	}
}

// TestRootsMatchSnapshot pins the replay gate per root: a supported type, an
// entry at the root's locator, a version inside its constraint, and a graph
// node.
func TestRootsMatchSnapshot(t *testing.T) {
	t.Parallel()
	resolved := map[string]collection{
		"acme.app": {Namespace: "acme", Name: "app", Version: "1.2.0", Source: snapshotServer},
		"acme.git": {Namespace: "acme", Name: "git", Version: "0.1.0", Source: snapGitPinned()},
		"acme.bad": {Namespace: "acme", Name: "bad", Version: "not-semver", Source: snapshotServer},
		"acme.lib": {Namespace: "acme", Name: "lib", Version: "1.0.0", Source: snapshotServer},
	}
	graph := map[string][]string{"acme.app@1.2.0": nil, "acme.git@0.1.0": nil, "acme.bad@not-semver": nil}
	for _, tt := range rootsMatchSnapshotCases() {
		if got := rootsMatchSnapshot([]collection{tt.root}, resolved, graph); got != tt.want {
			t.Errorf("%s: rootsMatchSnapshot() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// expansionSnapshot is a snapshot whose closure from acme.lib holds
// acme.util, plus nodes the snapshot cannot rebuild.
func expansionSnapshot() (map[string]store.ResolvedEntry, map[string][]string) {
	return map[string]store.ResolvedEntry{
		"acme.lib":  {Version: "1.0.0"},
		"acme.util": {Version: testVersion200},
		"acme.torn": {Version: "1.0.0", Source: snapGitUnpinned()},
	}, map[string][]string{
		"acme.lib@1.0.0":  {"acme.util@2.0.0"},
		"acme.util@2.0.0": nil,
		"acme.util@9.0.0": nil,
		"acme.torn@1.0.0": nil,
		"bogus":           nil,
	}
}

// mergedFromApp is an incremental merge holding acme.app@1.0.0 whose one
// edge is dep.
func mergedFromApp(dep string) (map[string]collection, map[string][]string) {
	return map[string]collection{"acme.app": {Namespace: "acme", Name: "app", Version: "1.0.0"}},
		map[string][]string{"acme.app@1.0.0": {dep}}
}

// TestExpandGraphFromSnapshot pins the incremental merge's closure walk: a
// dependency is pulled from the snapshot with its entry and edges.
func TestExpandGraphFromSnapshot(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: snapshotServer}
	resolvedSnap, graphSnap := expansionSnapshot()

	resolved, graph := mergedFromApp("acme.lib@1.0.0")
	if !expandGraphFromSnapshot(cfg, resolved, graph, resolvedSnap, graphSnap) {
		t.Fatal("expandGraphFromSnapshot() = false for a closure the snapshot holds")
	}
	if util := resolved["acme.util"]; util.Version != testVersion200 || util.Source != snapshotServer {
		t.Fatalf("expanded acme.util = %+v, want %s from the run's server", util, testVersion200)
	}
	if !slices.Equal(graph["acme.lib@1.0.0"], []string{"acme.util@2.0.0"}) || !validateMergedGraph(resolved, graph) {
		t.Fatalf("expanded graph = %v, want acme.lib's snapshot edges and a valid merge", graph)
	}
}

// TestExpandGraphFromSnapshotRefusals pins that a dependency the snapshot
// lacks, holds at another version or cannot rebuild fails the merge, and
// that validation refuses a node the merge holds at another version.
func TestExpandGraphFromSnapshotRefusals(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: snapshotServer}
	resolvedSnap, graphSnap := expansionSnapshot()
	for _, dep := range []string{"acme.gone@1.0.0", "acme.util@9.0.0", "acme.torn@1.0.0", "bogus"} {
		resolved, graph := mergedFromApp(dep)
		if expandGraphFromSnapshot(cfg, resolved, graph, resolvedSnap, graphSnap) {
			t.Errorf("expandGraphFromSnapshot() through %s = true, want false", dep)
		}
	}

	resolved, graph := mergedFromApp("acme.app@2.0.0")
	graph["acme.app@2.0.0"] = nil
	if !expandGraphFromSnapshot(cfg, resolved, graph, resolvedSnap, graphSnap) || validateMergedGraph(resolved, graph) {
		t.Fatal("a graph naming the merged root at another version must expand yet fail validation")
	}
	if validateMergedGraph(resolved, map[string][]string{"bogus": nil}) {
		t.Fatal("validateMergedGraph() over a malformed key = true, want false")
	}
	if ensureResolvedFromSnapshot(cfg, resolved, resolvedSnap, "acme.app@2.0.0") {
		t.Fatal("ensureResolvedFromSnapshot() over a merged entry at another version = true, want false")
	}
}

// TestBuildPreservedSnapshot pins the preserved half of an incremental
// resolve: unchanged roots carry their snapshot closure, and any hole in the
// snapshot preserves nothing.
func TestBuildPreservedSnapshot(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: snapshotServer}
	roots := []collection{{Namespace: "acme", Name: "app"}}
	resolvedSnap := map[string]store.ResolvedEntry{"acme.app": {Version: "1.0.0"}, "acme.lib": {Version: "1.0.0"}}
	appToLib := map[string][]string{"acme.app@1.0.0": {"acme.lib@1.0.0"}, "acme.lib@1.0.0": nil}

	resolved, graph, ok := buildPreservedSnapshot(cfg, roots, resolvedSnap, appToLib)
	if !ok || len(resolved) != 2 || len(graph) != 2 || resolved["acme.lib"].Source != snapshotServer {
		t.Fatalf("buildPreservedSnapshot() = (%v, %v, %v), want app and lib preserved", resolved, graph, ok)
	}

	holes := map[string]struct {
		resolvedSnap map[string]store.ResolvedEntry
		graphSnap    map[string][]string
	}{
		"root not in the snapshot": {
			resolvedSnap: map[string]store.ResolvedEntry{"acme.lib": {Version: "1.0.0"}},
			graphSnap:    map[string][]string{"acme.lib@1.0.0": nil},
		},
		"root node missing from the graph": {resolvedSnap: resolvedSnap, graphSnap: map[string][]string{"acme.lib@1.0.0": nil}},
		"dependency at another version": {
			resolvedSnap: map[string]store.ResolvedEntry{"acme.app": {Version: "1.0.0"}, "acme.lib": {Version: testVersion200}},
			graphSnap:    appToLib,
		},
		"dependency entry unpinned": {
			resolvedSnap: map[string]store.ResolvedEntry{"acme.app": {Version: "1.0.0"}, "acme.lib": {Version: "1.0.0", Source: snapURLUnpinned()}},
			graphSnap:    appToLib,
		},
		"malformed dependency key": {
			resolvedSnap: resolvedSnap,
			graphSnap:    map[string][]string{"acme.app@1.0.0": {"acme.lib"}, "acme.lib": nil},
		},
	}
	for name, tt := range holes {
		if resolved, graph, ok := buildPreservedSnapshot(cfg, roots, tt.resolvedSnap, tt.graphSnap); ok || resolved != nil || graph != nil {
			t.Errorf("%s: buildPreservedSnapshot() = (%v, %v, %v), want nothing preserved", name, resolved, graph, ok)
		}
	}
}

// TestMergeResolvedGraphsRefusesDifferentEdges pins that both halves naming
// one node with different dependencies is a conflict, while the same edges
// in another order merge.
func TestMergeResolvedGraphsRefusesDifferentEdges(t *testing.T) {
	t.Parallel()
	app := map[string]collection{"acme.app": {Namespace: "acme", Name: "app", Version: "1.0.0"}}
	preserved := map[string][]string{"acme.app@1.0.0": {"acme.a@1.0.0", "acme.b@1.0.0"}}

	for _, deps := range [][]string{{"acme.a@1.0.0"}, {"acme.a@1.0.0", "acme.c@1.0.0"}, {"acme.a@1.0.0", "acme.a@1.0.0"}} {
		if _, _, ok := mergeResolvedGraphs(app, preserved, app, map[string][]string{"acme.app@1.0.0": deps}); ok {
			t.Errorf("mergeResolvedGraphs() with edges %v against %v = ok, want a conflict", deps, preserved["acme.app@1.0.0"])
		}
	}
	_, graph, ok := mergeResolvedGraphs(app, preserved, app, map[string][]string{"acme.app@1.0.0": {"acme.b@1.0.0", "acme.a@1.0.0"}})
	if !ok || !slices.Equal(graph["acme.app@1.0.0"], preserved["acme.app@1.0.0"]) {
		t.Fatalf("mergeResolvedGraphs() with reordered edges = (%v, %v), want the preserved edges", graph, ok)
	}
}

// TestBuildGraphFromDepsNamesTheMissingNode pins the two sentinels a graph
// build fails with when the resolution lacks a parent or a dependency.
func TestBuildGraphFromDepsNamesTheMissingNode(t *testing.T) {
	t.Parallel()
	resolved := map[string]collection{"acme.app": {Namespace: "acme", Name: "app", Version: "1.0.0"}}

	_, err := buildGraphFromDeps(resolved, map[string]map[string]string{"acme.ghost": {}})
	if !errors.Is(err, helpers.ErrMissingResolvedParent) || !strings.Contains(err.Error(), "acme.ghost") {
		t.Fatalf("buildGraphFromDeps(missing parent) error = %v, want ErrMissingResolvedParent naming acme.ghost", err)
	}
	_, err = buildGraphFromDeps(resolved, map[string]map[string]string{"acme.app": {"acme.lib": "*"}})
	if !errors.Is(err, helpers.ErrMissingResolvedDependency) || !strings.Contains(err.Error(), "acme.lib") {
		t.Fatalf("buildGraphFromDeps(missing dependency) error = %v, want ErrMissingResolvedDependency naming acme.lib", err)
	}
	for _, key := range []string{"acme.app", "@1.0.0", "acme.app@"} {
		if _, _, err := splitCollectionKey(key); !errors.Is(err, helpers.ErrInvalidCollectionKey) {
			t.Errorf("splitCollectionKey(%q) error = %v, want ErrInvalidCollectionKey", key, err)
		}
	}
}

// TestFilterGraphSnapshotDropsUnresolvedNodes pins that a replayed graph keeps
// only nodes and edges the replayed resolution holds.
func TestFilterGraphSnapshotDropsUnresolvedNodes(t *testing.T) {
	t.Parallel()
	resolved := map[string]collection{
		"acme.app": {Namespace: "acme", Name: "app", Version: "1.0.0"},
		"acme.lib": {Namespace: "acme", Name: "lib", Version: "1.0.0"},
	}
	got := filterGraphSnapshot(map[string][]string{
		"acme.app@1.0.0": {"acme.lib@1.0.0", "acme.old@1.0.0"},
		"acme.lib@1.0.0": nil,
		"acme.old@1.0.0": nil,
	}, resolved)
	if len(got) != 2 || !slices.Equal(got["acme.app@1.0.0"], []string{"acme.lib@1.0.0"}) {
		t.Fatalf("filterGraphSnapshot() = %v, want app -> lib and lib alone", got)
	}
}

// TestRequirementsKeyNormalization pins the spellings the replay key folds:
// blank signatures vanish, a query is cut and order sorted, and an empty
// constraint keys like "*".
func TestRequirementsKeyNormalization(t *testing.T) {
	t.Parallel()
	if got := normalizeSignatures([]string{"  ", ""}); got != nil {
		t.Fatalf("normalizeSignatures(blank) = %v, want nil", got)
	}
	got := normalizeSignatures([]string{" https://sig.example/b.asc?X-Amz-Signature=s ", "", "https://sig.example/a.asc"})
	if want := []string{"https://sig.example/a.asc", "https://sig.example/b.asc"}; !slices.Equal(got, want) {
		t.Fatalf("normalizeSignatures() = %v, want %v", got, want)
	}

	empty := requirementsSignatureFromSpec(map[string]store.RequirementSpec{"acme.app": {}}, false, "s")
	star := requirementsSignatureFromSpec(map[string]store.RequirementSpec{"acme.app": {Constraint: "*"}}, false, "s")
	if empty != star {
		t.Fatalf("requirementsSignatureFromSpec(empty) = %s, want the \"*\" key %s", empty, star)
	}
}

// TestVersionsListCacheNeedsAnUnboundedPolicy pins the versions list cache:
// it is read and written only under a policy with no TTL, and a nil store
// records nothing.
func TestVersionsListCacheNeedsAnUnboundedPolicy(t *testing.T) {
	t.Parallel()
	const versionsURL = "https://hub.example/api/v3/collections/acme/app/versions/"
	st := store.New()
	versions := []string{"1.0.0", "1.1.0"}

	cacheVersionsList(st, cacheManager.Policy{Read: true, Write: true, TTL: time.Hour}, versionsURL, versions)
	if _, ok := cachedVersionsList(st, cacheManager.Policy{Read: true}, versionsURL); ok {
		t.Fatal("a TTL-bounded policy wrote the versions cache")
	}
	cacheVersionsList(st, cacheManager.Policy{Read: true, Write: true}, versionsURL, versions)
	if _, ok := cachedVersionsList(st, cacheManager.Policy{Read: true, TTL: time.Hour}, versionsURL); ok {
		t.Fatal("a TTL-bounded policy read the versions cache")
	}
	got, ok := cachedVersionsList(st, cacheManager.Policy{Read: true}, versionsURL)
	if !ok || !slices.Equal(got, versions) {
		t.Fatalf("cachedVersionsList() = (%v, %v), want %v", got, ok, versions)
	}

	cacheVersionsList(nil, cacheManager.Policy{Write: true}, versionsURL, versions)
	setResolvedAll(nil, map[string]collection{"acme.app": {Version: "1.0.0"}})
}
