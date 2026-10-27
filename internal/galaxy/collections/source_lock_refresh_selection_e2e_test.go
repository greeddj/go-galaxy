package collections_test

// This file pins, through the public Lock, which git, url and role pins a
// narrowed lock --refresh keeps, which move together, and how a kept pin
// replays.

import (
	"fmt"
	"slices"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestLockRefreshKeepsAGitRootItDoesNotName pins a git root whose branch moved
// under a narrowed lock --refresh: unnamed, it keeps its commit, fetching nothing
// over the cache that locked it; named, it moves.
func TestLockRefreshKeepsAGitRootItDoesNotName(t *testing.T) {
	t.Parallel()
	locked, moved := fakeCommit("app-1"), fakeCommit("app-2")
	for _, state := range cacheStates() {
		for _, tc := range []struct{ selected, want string }{
			{selected: "acme.lib", want: locked},
			{selected: "acme.app", want: moved},
		} {
			t.Run(state.name+", "+tc.selected+" named", func(t *testing.T) {
				t.Parallel()
				f := newGitFixture(t)
				git := recordGitCommits(f)
				f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n")
				f.lockfile(t)
				before := sourceLockBytes(t, f.reqPath)
				f.git.repos[gitAppURL].refs[gitMainRef] = moved
				if state.fresh {
					goColdGit(t, f)
				}
				f.git.resetCounts()
				git.take()
				f.cfg.Refresh, f.cfg.RefreshCollections = true, []string{tc.selected}

				if got := findLockEntry(t, f.lockfile(t), "acme.app").Commit; got != tc.want {
					t.Fatalf("acme.app locked at %s, want %s", got, tc.want)
				}
				if tc.want == moved {
					return
				}
				assertSourceLockBytes(t, f.reqPath, before)
				wantAdvertised, wantFetched := 0, []string(nil)
				if state.fresh {
					wantAdvertised, wantFetched = 1, []string{locked}
				}
				if adv, _ := f.git.counts(); adv != wantAdvertised {
					t.Fatalf("lock advertised %d times, want %d", adv, wantAdvertised)
				}
				if got := git.take(); !slices.Equal(got, wantFetched) {
					t.Fatalf("acquisitions = %q, want %q", got, wantFetched)
				}
			})
		}
	}
}

// TestLockRefreshReleasesAGitRootWholeForOneNamedCollection pins that naming
// acme.one moves acme.two, which depends on nothing named, to the root's new
// commit too, while naming acme.lib keeps both.
func TestLockRefreshReleasesAGitRootWholeForOneNamedCollection(t *testing.T) {
	t.Parallel()
	locked, moved := fakeCommit("mono-1"), fakeCommit("mono-2")
	for _, tc := range []struct{ selected, want string }{
		{selected: "acme.one", want: moved},
		{selected: "acme.lib", want: locked},
	} {
		t.Run(tc.selected+" named", func(t *testing.T) {
			t.Parallel()
			f := newGitFixture(t)
			f.writeRequirements(t, "collections:\n"+gitFrozenRoot("collections", gitFrozenParentRef)+"  - acme.lib\n")
			f.lockfile(t)
			repo := f.git.repos[gitMonoURL]
			repo.commits[moved] = []fakeGitCollection{
				{namespace: "acme", name: "one", version: "0.1.1", subdir: "collections/one"},
				{namespace: "acme", name: "two", version: "0.2.1", subdir: "collections/two", deps: map[string]string{"acme.one": "*"}},
			}
			repo.refs["HEAD"], repo.refs[gitMainRef] = moved, moved
			f.cfg.Refresh, f.cfg.RefreshCollections = true, []string{tc.selected}

			lf := f.lockfile(t)
			for _, name := range []string{"acme.one", "acme.two"} {
				if got := findLockEntry(t, lf, name).Commit; got != tc.want {
					t.Errorf("%s locked at %s, want %s", name, got, tc.want)
				}
			}
		})
	}
}

// TestLockRefreshKeepsAURLRootItDoesNotName pins a url root serving new bytes
// under a narrowed --refresh: named, it takes them; unnamed, it replays its pin
// or, on an empty cache, acts as lock.
func TestLockRefreshKeepsAURLRootItDoesNotName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		selected            string
		fresh               bool
		downloads, warnings int
	}{
		{selected: "acme.lib"},
		// Kept on an empty cache, the pin downloads and keeps the new bytes it
		// finds with one warning.
		{selected: "acme.lib", fresh: true, downloads: 1, warnings: 1},
		{selected: "acme.kafka", downloads: 1},
		{selected: "acme.kafka", fresh: true, downloads: 1},
	} {
		t.Run(fmt.Sprintf("%s named, empty cache %t", tc.selected, tc.fresh), func(t *testing.T) {
			t.Parallel()
			f := newURLFixture(t)
			f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
			f.lockfile(t)
			before := sourceLockBytes(t, f.reqPath)
			newSHA, warning := serveNewKafkaBytes(f)
			if tc.fresh {
				goColdURL(t, f)
			}
			downloads := f.galaxy.Count(fakegalaxy.EndpointTarball)
			f.cfg.Refresh, f.cfg.RefreshCollections = true, []string{tc.selected}

			kafka := findLockEntry(t, f.lockfile(t), "acme.kafka")
			if got := f.galaxy.Count(fakegalaxy.EndpointTarball) - downloads; got != tc.downloads {
				t.Fatalf("lock downloaded %d times, want %d", got, tc.downloads)
			}
			if got, all := warnCount(f.printer, warning), warnCount(f.printer, "Locked "); got != tc.warnings || all != got {
				t.Fatalf("locked-pin warnings %q, want %d of %q", f.printer.warns, tc.warnings, warning)
			}
			if tc.downloads == 0 {
				assertSourceLockBytes(t, f.reqPath, before)
			} else if kafka.SHA256 != newSHA || kafka.Version != kafkaNewVersion {
				t.Fatalf("url entry = %+v, want sha256 %s at %s", kafka, newSHA, kafkaNewVersion)
			}
		})
	}
}

// TestLockRefreshNoCacheFetchesKeptSourcesAgain pins that --no-cache under a
// narrowed lock --refresh fetches every kept url and git source once more, as
// plain lock --no-cache does, rewriting nothing.
func TestLockRefreshNoCacheFetchesKeptSourcesAgain(t *testing.T) {
	t.Parallel()
	f := newAllSourcesFixture(t)
	before := lockRoleBytes(t, f)
	downloads := f.galaxy.Count(fakegalaxy.EndpointTarball)
	f.git.resetCounts()
	roleAcquires := f.git.roleAcquireCount()
	f.cfg.NoCache, f.cfg.Refresh, f.cfg.RefreshCollections = true, true, []string{"acme.lib"}

	f.lockfile(t)
	assertRoleLockBytes(t, f, before)
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball) - downloads; got != 2 {
		t.Fatalf("lock downloaded %d tarballs, want 2, one per kept url source", got)
	}
	// The kept git collection and both git roles are fetched again too, by commit.
	if _, acquires := f.git.counts(); acquires != 1 {
		t.Errorf("lock acquired the git collection %d times, want 1", acquires)
	}
	if got := f.git.roleAcquireCount() - roleAcquires; got != 2 {
		t.Errorf("lock acquired %d git roles, want 2", got)
	}
}

// monoCommit is the mono repository's acme.one and acme.two at versions one
// and two, acme.two needing acme.one and acme.lib from Galaxy.
func monoCommit(one, two string) []fakeGitCollection {
	return []fakeGitCollection{
		{namespace: "acme", name: "one", version: one, subdir: "collections/one"},
		{
			namespace: "acme", name: "two", version: two, subdir: "collections/two",
			deps: map[string]string{"acme.one": "*", "acme.lib": ">=1.0.0"},
		},
	}
}

// TestLockRefreshAnnouncesTheGitSiblingsOfANamedCollection pins that naming
// acme.one sets aside acme.two, which its commit holds, and acme.lib, which
// acme.two needs: one report line names all three, and each moves.
func TestLockRefreshAnnouncesTheGitSiblingsOfANamedCollection(t *testing.T) {
	t.Parallel()
	locked, moved := fakeCommit("mono-1"), fakeCommit("mono-2")
	for _, tc := range []struct {
		want      map[string]string
		selected  string
		commit    string
		announced string
	}{
		{
			selected: "acme.one", commit: moved, announced: "Setting aside the pins of acme.lib, acme.one, acme.two",
			want: map[string]string{"acme.one": "0.1.1", "acme.two": "0.2.1", "acme.lib": selectionLibV210},
		},
		{
			selected: "acme.lib", commit: locked, announced: "Setting aside the pins of acme.lib",
			want: map[string]string{"acme.one": "0.1.0", "acme.two": "0.2.0", "acme.lib": selectionLibV210},
		},
	} {
		t.Run(tc.selected+" named", func(t *testing.T) {
			t.Parallel()
			f := newGitFixture(t)
			repo := f.git.repos[gitMonoURL]
			repo.commits[locked] = monoCommit("0.1.0", "0.2.0")
			f.writeRequirements(t, "collections:\n"+gitFrozenRoot("collections", gitFrozenParentRef))
			assertLockedVersions(t, f.lockfile(t), map[string]string{"acme.one": "0.1.0", "acme.two": "0.2.0", "acme.lib": e2eVersion200})
			repo.commits[moved] = monoCommit("0.1.1", "0.2.1")
			repo.refs["HEAD"], repo.refs[gitMainRef] = moved, moved
			f.galaxy.AddVersion("acme", "lib", selectionLibV210, nil)
			printer := newTierPrinter(func() int {
				advertises, acquires := f.git.counts()
				return f.galaxy.Total() + advertises + acquires
			})
			f.runtime.Output = printer
			f.cfg.Refresh, f.cfg.RefreshCollections = true, []string{tc.selected}

			lf := f.lockfile(t)
			assertLockedVersions(t, lf, tc.want)
			for _, name := range []string{"acme.one", "acme.two"} {
				if got := findLockEntry(t, lf, name).Commit; got != tc.commit {
					t.Errorf("%s locked at %s, want %s", name, got, tc.commit)
				}
			}
			assertAnnounced(t, printer, tc.announced)
		})
	}
}

// TestLockRefreshMovesEveryURLRoleOfOneURL pins two url roles with one src,
// whose pin is the URL's: naming either moves both without a warning in any
// resolve order, and keeps another URL's role.
func TestLockRefreshMovesEveryURLRoleOfOneURL(t *testing.T) {
	t.Parallel()
	for _, selected := range []string{"first", "second"} {
		for run := range 3 {
			t.Run(fmt.Sprintf("%s named, run %d", selected, run), func(t *testing.T) {
				t.Parallel()
				f := newURLRoleFixture(t)
				shared, _ := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=shared\n"})
				sharedURL := f.galaxy.AddTarball("dl/shared.tar.gz", shared)
				lone, _ := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=lone\n"})
				loneURL := f.galaxy.AddTarball("dl/lone.tar.gz", lone)
				f.writeRequirements(t, "roles:\n  - src: "+sharedURL+"\n    name: first\n  - src: "+sharedURL+"\n    name: second\n"+
					"  - src: "+loneURL+"\n    name: lone\n")
				before := f.lockfile(t)
				movedShared, sha := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=shared moved\n"})
				f.galaxy.AddTarball("dl/shared.tar.gz", movedShared)
				movedLone, _ := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=lone moved\n"})
				f.galaxy.AddTarball("dl/lone.tar.gz", movedLone)
				printer := newTierPrinter(f.galaxy.Total)
				f.runtime.Output = printer
				f.cfg.DownloadWorkers = 4
				f.cfg.Refresh, f.cfg.RefreshRoles = true, []string{selected}

				after := f.lockfile(t)
				for _, name := range []string{"first", "second"} {
					want := findLockRole(t, before, name)
					want.SHA256, want.Version = sha, sha[:12]
					assertLockedRole(t, after, want)
				}
				assertLockedRole(t, after, findLockRole(t, before, "lone"))
				if got := printer.matching(tierWarn, "Locked role"); len(got) != 0 {
					t.Errorf("locked-role warnings %+v, want none", got)
				}
				assertAnnounced(t, printer, "Setting aside the pins of role first, role second")
			})
		}
	}
}

// lockDockerBesideApp locks geerlingguy.docker, installed as docker, beside the
// git role app, publishes docker's newer tag, goes to an empty cache if fresh,
// and returns the bytes locked.
func lockDockerBesideApp(t *testing.T, f *roleFixture, git *roleCommitRecorder, fresh bool) []byte {
	t.Helper()
	f.writeRequirements(t, "roles:\n  - name: docker\n    src: geerlingguy.docker\n  - src: git+"+roleAppURL+"\n    name: app\n")
	before := lockRoleBytes(t, f)
	publishDockerTag(f)
	if fresh {
		goColdRoles(t, f)
	}
	f.galaxy.ResetCounts()
	git.take()
	return before
}

// TestLockRefreshMovesAGalaxyRoleNamedByEitherName pins that lock --refresh
// --role takes a Galaxy role's newer tag when named by its install name or by
// its Galaxy name.
func TestLockRefreshMovesAGalaxyRoleNamedByEitherName(t *testing.T) {
	t.Parallel()
	for _, state := range cacheStates() {
		for _, selected := range []string{"docker", "geerlingguy.docker"} {
			t.Run(state.name+", "+selected+" named", func(t *testing.T) {
				t.Parallel()
				f := newGalaxyRoleFixture(t)
				lockDockerBesideApp(t, f, recordRoleCommits(f), state.fresh)
				f.cfg.Refresh, f.cfg.RefreshRoles = true, []string{selected}

				docker := findLockRole(t, f.lockfile(t), "docker")
				if docker.Version != dockerPublished || docker.Commit != fakeCommit("docker-4") {
					t.Fatalf("docker locked as %+v, want %s at its new commit", docker, dockerPublished)
				}
			})
		}
	}
}

// TestLockRefreshKeepsAGalaxyRoleItDoesNotName pins a Galaxy role with a newer
// tag kept by lock --refresh --role app, asking no v1 API, and fetched only
// on an empty cache.
func TestLockRefreshKeepsAGalaxyRoleItDoesNotName(t *testing.T) {
	t.Parallel()
	for _, state := range cacheStates() {
		t.Run(state.name, func(t *testing.T) {
			t.Parallel()
			f := newGalaxyRoleFixture(t)
			git := recordRoleCommits(f)
			before := lockDockerBesideApp(t, f, git, state.fresh)
			f.cfg.Refresh, f.cfg.RefreshRoles = true, []string{"app"}

			f.lockfile(t)
			assertRoleLockBytes(t, f, before)
			if got := f.galaxy.Total(); got != 0 {
				t.Fatalf("lock made %d Galaxy requests, want 0", got)
			}
			// On an empty cache app and base, set aside, are fetched by ref.
			var want []string
			if state.fresh {
				want = []string{gitNoCommit, gitNoCommit, fakeCommit("docker-3")}
			}
			if got := git.take(); !slices.Equal(got, want) {
				t.Fatalf("role acquisitions = %q, want %q", got, want)
			}
		})
	}
}

// TestLockRefreshReleasesTheRolesANamedRoleDependsOn pins a narrowed lock
// --refresh once base and moving moved: naming app moves base, its dependency,
// naming moving keeps both, and a kept role fetches nothing.
func TestLockRefreshReleasesTheRolesANamedRoleDependsOn(t *testing.T) {
	t.Parallel()
	appLocked := fakeCommit("role-app-1")
	baseLocked, baseMoved := fakeCommit("role-base-1"), fakeCommit("role-base-2")
	movingLocked, movingMoved := fakeCommit("moving-1"), fakeCommit("moving-2")
	for _, tc := range []struct {
		want     map[string]string
		selected string
		fetched  []string
	}{
		{
			selected: "app", fetched: []string{baseMoved},
			want: map[string]string{"app": appLocked, "base": baseMoved, "moving": movingLocked},
		},
		{
			selected: "moving", fetched: []string{movingMoved},
			want: map[string]string{"app": appLocked, "base": baseLocked, "moving": movingMoved},
		},
	} {
		t.Run(tc.selected+" named", func(t *testing.T) {
			t.Parallel()
			f := newRoleFixture(t)
			moving := addMovingRepo(f, "moving-1", "moving-2")
			git := recordRoleCommits(f)
			f.writeRequirements(t, "roles:\n  - src: git+"+roleAppURL+"\n    name: app\n"+
				"  - src: git+"+movingRoleURL+"\n    version: main\n    name: moving\n")
			f.lockfile(t)
			base := f.git.repos[roleBaseURL]
			base.addRole(baseMoved, fakeGitRole{roleName: "base"})
			base.refs["HEAD"], base.refs[roleMainRef] = baseMoved, baseMoved
			moveMain(moving, "moving-2")
			git.take()
			f.cfg.Refresh, f.cfg.RefreshRoles = true, []string{tc.selected}

			lf := f.lockfile(t)
			for name, commit := range tc.want {
				if got := findLockRole(t, lf, name).Commit; got != commit {
					t.Errorf("role %s locked at %s, want %s", name, got, commit)
				}
			}
			if got := git.take(); !slices.Equal(got, tc.fetched) {
				t.Fatalf("role acquisitions = %q, want %q", got, tc.fetched)
			}
		})
	}
}

// moveEverySource moves each git and url source newAllSourcesFixture requires
// past galaxy.lock and returns the sha256 the url role now serves.
func moveEverySource(t *testing.T, f *roleFixture) string {
	t.Helper()
	f.git.repos[gitAppURL].refs[gitMainRef] = fakeCommit("app-2")
	app := f.git.repos[roleAppURL]
	app.refs["HEAD"], app.refs[roleMainRef] = fakeCommit("role-app-2"), fakeCommit("role-app-2")
	kafka, _ := fakegalaxy.BuildArtifact("acme", "kafka", kafkaNewVersion, map[string]string{"acme.lib": ">=1.0.0"})
	f.galaxy.AddTarball(urlKafkaPath, kafka)
	held, sha := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=moved\n"})
	f.galaxy.AddTarball("dl/held.tar.gz", held)
	return sha
}

// TestLockRefreshReplaysEveryUnnamedSource pins a narrowed lock --refresh over
// the cache that locked every source, each moved since: what it keeps it
// replays, fetching nothing, and only the named pin moves.
func TestLockRefreshReplaysEveryUnnamedSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		collections, roles []string
		downloads          int
	}{
		{name: "acme.lib named", collections: []string{"acme.lib"}},
		{name: "the url role named", roles: []string{"held"}, downloads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAllSourcesFixture(t)
			lockRoleBytes(t, f)
			before, err := lockfile.Load(roleLockPath(f))
			if err != nil {
				t.Fatalf("load lockfile: %v", err)
			}
			heldSHA := moveEverySource(t, f)
			f.git.resetCounts()
			roleAcquires, downloads := f.git.roleAcquireCount(), f.galaxy.Count(fakegalaxy.EndpointTarball)
			f.cfg.Refresh, f.cfg.RefreshCollections, f.cfg.RefreshRoles = true, tc.collections, tc.roles

			after := f.lockfile(t)
			if adv, acq := f.git.counts(); adv != 0 || acq != 0 || f.git.roleAcquireCount() != roleAcquires {
				t.Fatalf("lock reached a git remote: advertises=%d acquires=%d role acquires=%d",
					adv, acq, f.git.roleAcquireCount()-roleAcquires)
			}
			if got := f.galaxy.Count(fakegalaxy.EndpointTarball) - downloads; got != tc.downloads {
				t.Fatalf("lock downloaded %d tarballs, want %d", got, tc.downloads)
			}
			if got := warnCount(f.printer, "Locked "); got != 0 {
				t.Fatalf("lock drew a locked-pin warning: %q", f.printer.warns)
			}
			assertLockedAsBefore(t, before, after, tc.roles, heldSHA)
		})
	}
}

// assertLockedAsBefore fails unless after holds every entry of before, except
// that each url role in moved is locked at sha under its default label.
func assertLockedAsBefore(t *testing.T, before, after *lockfile.File, moved []string, sha string) {
	t.Helper()
	for _, e := range before.Collections {
		if got := findLockEntry(t, after, e.Name); fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", e) {
			t.Errorf("entry %s = %+v, want %+v", e.Name, got, e)
		}
	}
	for _, want := range before.Roles {
		if slices.Contains(moved, want.Name) {
			want.SHA256, want.Version = sha, sha[:12]
		}
		assertLockedRole(t, after, want)
	}
}
