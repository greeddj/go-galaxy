package collections_test

// This file pins that a replayed resolution hands a run no git or url
// collection its own requirements did not expand into: one another project on
// the shared cache, or a requirement this project dropped, brought into it.

import (
	"context"
	"errors"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

const (
	// withGitApp asks Galaxy for acme.base, which depends on acme.app, beside
	// the git requirement supplying acme.app; withoutGitApp asks Galaxy for
	// acme.base and acme.other alone, so acme.app has to come from Galaxy.
	withGitApp    = "collections:\n  - acme.base\n  - git+" + gitAppURL + ",main\n"
	withoutGitApp = "collections:\n  - acme.base\n  - acme.other\n"
	// otherAppURL is a repository one project alone asks for, holding an
	// acme.app at otherAppVersion, which no other source publishes.
	otherAppURL     = "https://git.example/other/app.git"
	otherAppVersion = "9.9.9"
	// galaxyKafkaVersion is the acme.kafka Galaxy publishes beside the one the
	// url fixture's tarball holds.
	galaxyKafkaVersion = "0.10.0"
	// appFQDN is the collection both the git requirement and Galaxy supply.
	appFQDN = "acme.app"
)

// newReplayFixture is the git fixture plus Galaxy's acme.base, which depends
// on acme.app, Galaxy's own acme.app at testVersion100, and acme.other.
func newReplayFixture(t *testing.T) *gitFixture {
	t.Helper()
	f := newGitFixture(t)
	f.galaxy.AddVersion("acme", "base", testVersion100, map[string]string{appFQDN: ">=1.0.0"})
	f.galaxy.AddVersion("acme", "app", testVersion100, map[string]string{"acme.lib": ">=1.0.0"})
	f.galaxy.AddVersion("acme", "other", testVersion100, nil)
	return f
}

// assertAppFromGalaxy fails unless cfg's project installed Galaxy's acme.app
// and no git remote was reached since the last count reset.
func assertAppFromGalaxy(t *testing.T, f *gitFixture, cfg *config.Config) {
	t.Helper()
	if got := readManifestVersion(t, cfg.DownloadPath, "app"); got != testVersion100 {
		t.Fatalf("installed acme.app %s, want Galaxy's %s", got, testVersion100)
	}
	if adv, acq := f.git.counts(); adv != 0 || acq != 0 {
		t.Fatalf("the install reached a git remote: advertises=%d acquires=%d", adv, acq)
	}
}

// assertAppLockedFromGalaxy fails unless lock, run for cfg's project, writes
// acme.app as Galaxy's testVersion100.
func assertAppLockedFromGalaxy(t *testing.T, f *gitFixture, cfg *config.Config) {
	t.Helper()
	if err := collections.Lock(context.Background(), cfg, f.runtime); err != nil {
		t.Fatalf("lock: %v", err)
	}
	lf, err := lockfile.Load(lockfile.ResolveDefaultPath(cfg.RequirementsFile, ""))
	if err != nil {
		t.Fatalf("load lockfile: %v", err)
	}
	for _, e := range lf.Collections {
		if e.Name != appFQDN {
			continue
		}
		if !e.IsGalaxy() || e.Version != testVersion100 {
			t.Fatalf("lock wrote acme.app as %+v, want Galaxy's %s", e, testVersion100)
		}
		return
	}
	t.Fatalf("lock wrote no acme.app entry: %+v", lf.Collections)
}

// renameGraphNode returns graph with the node from renamed to, as a key and
// wherever it is a dependency.
func renameGraphNode(graph map[string][]string, from, to string) map[string][]string {
	out := make(map[string][]string, len(graph))
	for key, deps := range graph {
		if key == from {
			key = to
		}
		renamed := make([]string, len(deps))
		for i, dep := range deps {
			if dep == from {
				dep = to
			}
			renamed[i] = dep
		}
		out[key] = renamed
	}
	return out
}

// TestReplayHandsNoProjectACommitAnotherProjectLocked pins that once a lock
// keeps dev's commit for main, passing or failing, previewed or uncached, a
// project asking Galaxy for acme.base installs and locks Galaxy's acme.app.
func TestReplayHandsNoProjectACommitAnotherProjectLocked(t *testing.T) {
	t.Parallel()
	devLocator := gitsource.Locator{URL: gitAppURL, Commit: fakeCommit("app-2")}.String()
	for name, tc := range map[string]struct {
		want                   error
		version                string
		check, dryRun, noCache bool
	}{
		"passing lock --check":    {version: appMovedVersion, check: true},
		"failing lock --check":    {version: appLockedVersion, check: true, want: helpers.ErrLockfileDrift},
		"lock --dry-run":          {version: appMovedVersion, dryRun: true},
		"lock --check --no-cache": {version: appMovedVersion, check: true, noCache: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newReplayFixture(t)
			f.writeRequirements(t, withGitApp)
			relockAppAt(t, f, fakeCommit("app-2"), tc.version)

			f.cfg.Check, f.cfg.DryRun, f.cfg.NoCache = tc.check, tc.dryRun, tc.noCache
			if err := lockGitRun(f); !errors.Is(err, tc.want) {
				t.Fatalf("lock over a galaxy.lock naming dev's commit for main: %v, want %v", err, tc.want)
			}
			f.cfg.Check, f.cfg.DryRun, f.cfg.NoCache = false, false, false
			if got := loadStoreSnapshot(t, f.cfg, f.runtime).ResolvedSnapshot()[appFQDN].Source; got != devLocator {
				t.Fatalf("the lock recorded acme.app from %q, want dev's commit %q", got, devLocator)
			}

			f.git.resetCounts()
			other := installAsAnotherProject(t, f, withoutGitApp)
			assertAppFromGalaxy(t, f, other)
			assertAppLockedFromGalaxy(t, f, other)
		})
	}
}

// TestReplayHandsNoProjectAnotherProjectsGitSource pins the same with no
// galaxy.lock anywhere: once a project installs acme.base beside its own git
// requirement for acme.app, another asking Galaxy installs Galaxy's acme.app.
func TestReplayHandsNoProjectAnotherProjectsGitSource(t *testing.T) {
	t.Parallel()
	f := newReplayFixture(t)
	commit := fakeCommit("other-app-1")
	f.git.add(otherAppURL, &fakeGitRepo{
		refs:    map[string]string{"HEAD": commit, gitMainRef: commit},
		commits: map[string][]fakeGitCollection{commit: {{namespace: "acme", name: "app", version: otherAppVersion}}},
	})
	first := installAsAnotherProject(t, f, "collections:\n  - acme.base\n  - git+"+otherAppURL+",main\n")
	if got := readManifestVersion(t, first.DownloadPath, "app"); got != otherAppVersion {
		t.Fatalf("the first project installed acme.app %s, want its repository's %s", got, otherAppVersion)
	}

	f.git.resetCounts()
	other := installAsAnotherProject(t, f, withoutGitApp)
	assertAppFromGalaxy(t, f, other)
}

// TestReplayHandsNoProjectAnotherProjectsURLSource pins the url half: once a
// project installs acme.stream beside the url requirement supplying
// acme.kafka, another asking Galaxy alone takes Galaxy's, never the tarball.
func TestReplayHandsNoProjectAnotherProjectsURLSource(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.galaxy.AddVersion("acme", "stream", testVersion100, map[string]string{"acme.kafka": ">=0.1.0"})
	f.galaxy.AddVersion("acme", "kafka", galaxyKafkaVersion, map[string]string{"acme.lib": ">=1.0.0"})
	f.galaxy.AddVersion("acme", "other", testVersion100, nil)
	f.writeRequirements(t, "collections:\n  - acme.stream\n  - "+f.tarballURL+"\n")
	f.mustInstall(t)
	if got := readManifestVersion(t, f.downloadPath, "kafka"); got != urlKafkaVersion {
		t.Fatalf("the first project installed acme.kafka %s, want the tarball's %s", got, urlKafkaVersion)
	}

	other := sharedCacheProject(t, f.cfg, "collections:\n  - acme.stream\n  - acme.other\n")
	f.galaxy.ResetCounts()
	if err := collections.Start(context.Background(), other, f.runtime); err != nil {
		t.Fatalf("the other project's install: %v", err)
	}
	if got := readManifestVersion(t, other.DownloadPath, "kafka"); got != galaxyKafkaVersion {
		t.Fatalf("the other project installed acme.kafka %s, want Galaxy's %s", got, galaxyKafkaVersion)
	}
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 0 {
		t.Fatalf("the other project's install requested the tarball %d times, want 0", got)
	}
}

// TestDroppedGitRequirementTakesItsCollectionFromGalaxy pins that a project
// dropping its git requirement for acme.app, while acme.base still depends on
// it, installs Galaxy's acme.app although it only added another requirement.
func TestDroppedGitRequirementTakesItsCollectionFromGalaxy(t *testing.T) {
	t.Parallel()
	f := newReplayFixture(t)
	f.writeRequirements(t, withGitApp)
	f.mustInstall(t)
	if got := readManifestVersion(t, f.downloadPath, "app"); got != appLockedVersion {
		t.Fatalf("installed acme.app %s, want the git requirement's %s", got, appLockedVersion)
	}

	f.writeRequirements(t, withoutGitApp)
	f.git.resetCounts()
	f.mustInstall(t)
	assertAppFromGalaxy(t, f, f.cfg)
}

// TestOwnGitRequirementStillReplays is the control: a project's own git
// requirement replays with no git request, on an unchanged rerun, which asks
// Galaxy nothing and skips its save as a replay does, and beside a new one.
func TestOwnGitRequirementStillReplays(t *testing.T) {
	t.Parallel()
	f := newReplayFixture(t)
	f.writeRequirements(t, withGitApp)
	f.mustInstall(t)

	f.git.resetCounts()
	f.galaxy.ResetCounts()
	stamp := reloadLastSnapshot(t, f.cfg.CacheDir)
	f.mustInstall(t)
	if adv, acq := f.git.counts(); adv != 0 || acq != 0 || f.galaxy.Total() != 0 {
		t.Fatalf("an unchanged rerun reached the network: advertises=%d acquires=%d Galaxy requests=%d",
			adv, acq, f.galaxy.Total())
	}
	if got := reloadLastSnapshot(t, f.cfg.CacheDir); !got.Equal(stamp) {
		t.Fatalf("an unchanged rerun saved its snapshot (%v, was %v), so it solved instead of replaying", got, stamp)
	}

	f.writeRequirements(t, withGitApp+"  - acme.other\n")
	f.git.resetCounts()
	f.mustInstall(t)
	if got := readManifestVersion(t, f.downloadPath, "app"); got != appLockedVersion {
		t.Fatalf("installed acme.app %s beside a new requirement, want the git requirement's %s", got, appLockedVersion)
	}
	if adv, acq := f.git.counts(); adv != 0 || acq != 0 {
		t.Fatalf("the rerun beside a new requirement reached the git remote: advertises=%d acquires=%d", adv, acq)
	}
}

// TestFullReplayRefusesASourceItsRequirementsDoNotAsk pins the full replay's
// half: a resolution recorded under these very requirements with a git
// acme.app, as an older release could record one, resolves from Galaxy again.
func TestFullReplayRefusesASourceItsRequirementsDoNotAsk(t *testing.T) {
	t.Parallel()
	f := newReplayFixture(t)
	installAsAnotherProject(t, f, withGitApp)
	f.writeRequirements(t, withoutGitApp)
	f.mustInstall(t)

	gitApp := gitsource.Locator{URL: gitAppURL, Commit: fakeCommit("app-1")}.String()
	mutateStoreSnapshot(t, f, func(st *store.Store) {
		resolved := st.ResolvedSnapshot()
		resolved[appFQDN] = store.ResolvedEntry{Version: appLockedVersion, Source: gitApp, Ref: "main"}
		st.SetResolvedAll(resolved)
		st.SetGraphSnapshot(renameGraphNode(st.GraphSnapshot(), "acme.app@"+testVersion100, "acme.app@"+appLockedVersion))
	})

	f.git.resetCounts()
	other := installAsAnotherProject(t, f, withoutGitApp)
	assertAppFromGalaxy(t, f, other)
}
