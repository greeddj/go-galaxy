package collections

// This file pins sourcesMatchRoots and the two replays applying it: a replay
// holds a git or url collection only where a root of this run expanded into
// one, at the same locator; otherwise it is refused and the run solves.

import (
	"context"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

const (
	// replayAppURL and replayKafkaURL are the git and url sources of the
	// collections these tests replay; replayGalaxy is a Galaxy server base.
	replayAppURL   = "https://git.example/acme/app.git"
	replayKafkaURL = "https://downloads.example/acme-kafka-0.24.0.tar.gz"
	replayGalaxy   = "https://galaxy.example"
	// replayAppVersion is the version acme.app is built as at both commits.
	replayAppVersion = "1.2.3"
	// replayCommit and replayMovedCommit are two commits of replayAppURL.
	replayCommit      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	replayMovedCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// replayGitApp is acme.app as discovery expands a git root of replayAppURL
// at main, which names commit.
func replayGitApp(commit string) collection {
	return collection{
		Namespace:  "acme",
		Name:       "app",
		Version:    replayAppVersion,
		Constraint: replayAppVersion,
		Source:     gitsource.Locator{URL: replayAppURL, Commit: commit}.String(),
		Type:       typeGit,
		Ref:        "main",
	}
}

// replayGalaxyRoot is name as an unpinned Galaxy root, before any resolve.
func replayGalaxyRoot(name string) collection {
	return collection{Namespace: "acme", Name: name}
}

// TestSourcesMatchRoots pins the rule over both source kinds and both ways:
// a replayed git or url collection must be a root's own, and a git or url
// root's collection must be replayed at its own locator.
func TestSourcesMatchRoots(t *testing.T) {
	t.Parallel()
	gitApp, movedApp := replayGitApp(replayCommit), replayGitApp(replayMovedCommit)
	kafka := collection{
		Namespace: "acme", Name: "kafka", Version: "0.24.0", Constraint: "0.24.0", Type: typeURL,
		Source: urlsource.Locator{URL: replayKafkaURL, SHA256: strings.Repeat("ab", 32)}.String(),
	}
	base := collection{Namespace: "acme", Name: "base", Version: testVersion100, Source: replayGalaxy}
	galaxyApp := collection{Namespace: "acme", Name: "app", Version: testVersion100, Source: replayGalaxy}
	baseRoot := replayGalaxyRoot("base")
	for _, tc := range []struct {
		name     string
		roots    []collection
		resolved []collection
		want     bool
	}{
		{name: "Galaxy collections only", roots: []collection{baseRoot}, resolved: []collection{base, galaxyApp}, want: true},
		{name: "a git root's own collection", roots: []collection{baseRoot, gitApp}, resolved: []collection{base, gitApp}, want: true},
		{name: "a url root's own collection", roots: []collection{baseRoot, kafka}, resolved: []collection{base, kafka}, want: true},
		{name: "a git collection no root expanded into", roots: []collection{baseRoot}, resolved: []collection{base, gitApp}},
		{name: "a url collection no root expanded into", roots: []collection{baseRoot}, resolved: []collection{base, kafka}},
		{name: "a git root's collection at another commit", roots: []collection{baseRoot, movedApp}, resolved: []collection{base, gitApp}},
		{name: "a git root's collection from Galaxy", roots: []collection{baseRoot, gitApp}, resolved: []collection{base, galaxyApp}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resolved := make(map[string]collection, len(tc.resolved))
			for _, col := range tc.resolved {
				resolved[col.fqdn()] = col
			}
			if got := sourcesMatchRoots(tc.roots, resolved); got != tc.want {
				t.Fatalf("sourcesMatchRoots = %t, want %t", got, tc.want)
			}
		})
	}
}

// replaySourcesFixture is a Galaxy server publishing acme.base, which depends
// on acme.app, Galaxy's own acme.app and acme.other, with a run over it.
type replaySourcesFixture struct {
	cfg     *config.Config
	runtime *infra.Infra
}

func newReplaySourcesFixture(t *testing.T) replaySourcesFixture {
	t.Helper()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "base", testVersion100, map[string]string{"acme.app": ">=1.0.0"})
	srv.AddVersion("acme", "app", testVersion100, nil)
	srv.AddVersion("acme", "other", testVersion100, nil)
	return replaySourcesFixture{cfg: &config.Config{Server: srv.URL(), Workers: 2}, runtime: infra.New(noopPrinter{}, srv.Client())}
}

// record returns a store holding a resolution recorded for roots: acme.base
// from the server, depending on app, and acme.other when withOther is set.
func (f replaySourcesFixture) record(roots []collection, app collection, withOther bool) *store.Store {
	base := collection{Namespace: "acme", Name: "base", Version: testVersion100, Source: f.cfg.Server}
	resolved := map[string]collection{base.fqdn(): base, app.fqdn(): app}
	graph := map[string][]string{base.key(): {app.key()}, app.key(): nil}
	if withOther {
		other := collection{Namespace: "acme", Name: "other", Version: testVersion100, Source: f.cfg.Server}
		resolved[other.fqdn()], graph[other.key()] = other, nil
	}
	spec := buildRequirementsSpec(roots)
	st := store.New()
	recordResolution(st, resolved, graph, requirementsSignatureFromSpec(spec, false, serversSignature(f.cfg)), f.cfg.Server, spec)
	return st
}

// replay runs the full, then the incremental replay of roots over st, the git
// roots among them discovered at their locators, and reports whether either
// was taken and the source it gives acme.app.
func (f replaySourcesFixture) replay(t *testing.T, st *store.Store, roots []collection) (string, bool) {
	t.Helper()
	deps := newCollectionDeps(f.cfg, f.runtime, st)
	deps.gitMemo = newGitDiscoveryMemo()
	for _, root := range roots {
		if root.isGit() {
			deps.gitMemo.put(root.fqdn(), gitPin{locator: root.Source, ref: root.Ref, version: root.Version, deps: map[string]string{}})
		}
	}
	spec := buildRequirementsSpec(roots)
	hash := requirementsSignatureFromSpec(spec, false, serversSignature(f.cfg))
	resolved, _, ok, err := resolveFromSnapshots(context.Background(), deps, roots, spec, hash)
	if err != nil {
		t.Fatalf("resolveFromSnapshots: %v", err)
	}
	return resolved["acme.app"].Source, ok
}

// TestReplaysHoldOnlySourcesTheirRootsExpandedInto pins which replays are
// taken: one keeping a project's own git root, unchanged or moved within one
// version, is; one holding a git collection none of its roots expanded into is not.
func TestReplaysHoldOnlySourcesTheirRootsExpandedInto(t *testing.T) {
	t.Parallel()
	gitApp, movedApp := replayGitApp(replayCommit), replayGitApp(replayMovedCommit)
	base, other := replayGalaxyRoot("base"), replayGalaxyRoot("other")
	for _, tc := range []struct {
		name       string
		wantSource string
		recorded   []collection
		roots      []collection
		withOther  bool
		wantOK     bool
	}{
		{
			name: "full replay of the project's own git root", recorded: []collection{base, gitApp},
			roots: []collection{base, gitApp}, wantSource: gitApp.Source, wantOK: true,
		},
		{
			name: "full replay recorded with a git collection no root asks for", recorded: []collection{base, other},
			withOther: true, roots: []collection{base, other},
		},
		{
			name: "incremental replay beside the project's own git root", recorded: []collection{base, gitApp},
			roots: []collection{base, gitApp, other}, wantSource: gitApp.Source, wantOK: true,
		},
		{
			name: "incremental replay after the git root moved within its version", recorded: []collection{base, gitApp},
			roots: []collection{base, movedApp, other}, wantSource: movedApp.Source, wantOK: true,
		},
		{
			name: "incremental replay holding another project's git collection", recorded: []collection{base, gitApp},
			roots: []collection{base, other},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newReplaySourcesFixture(t)
			st := f.record(tc.recorded, gitApp, tc.withOther)
			source, ok := f.replay(t, st, tc.roots)
			if ok != tc.wantOK || (ok && source != tc.wantSource) {
				t.Fatalf("replay taken: %t with acme.app from %q, want taken: %t with acme.app from %q", ok, source, tc.wantOK, tc.wantSource)
			}
		})
	}
}

// TestRefusedReplayResolvesFromTheServer pins what follows a refused replay:
// the run solves, so acme.app, recorded from another project's git root,
// comes from the server, and the recorded resolution becomes this run's.
func TestRefusedReplayResolvesFromTheServer(t *testing.T) {
	t.Parallel()
	f := newReplaySourcesFixture(t)
	gitApp := replayGitApp(replayCommit)
	st := f.record([]collection{replayGalaxyRoot("base"), gitApp}, gitApp, false)

	roots := []collection{replayGalaxyRoot("base"), replayGalaxyRoot("other")}
	resolved, _, err := resolveCollectionsInternal(context.Background(), newCollectionDeps(f.cfg, f.runtime, st), roots, resolveTopLevel)
	if err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}
	if app := resolved["acme.app"]; app.Version != testVersion100 || app.Source != f.cfg.Server {
		t.Fatalf("acme.app resolved as %+v, want %s from %s", app, testVersion100, f.cfg.Server)
	}
	if recorded := st.ResolvedSnapshot()["acme.app"]; recorded.Source != f.cfg.Server {
		t.Fatalf("the recorded resolution holds acme.app from %q, want %q", recorded.Source, f.cfg.Server)
	}
}
