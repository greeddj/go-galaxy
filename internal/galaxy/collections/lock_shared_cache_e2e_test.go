package collections_test

// This file pins that lock records no pin galaxy.lock alone decided in a cache
// other projects share: a locked commit its ref no longer names and a Galaxy
// role's locked repository are fetched for the run, never recorded.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

const (
	// otherRoleURL is a GitHub repository other than the one the v1 API names
	// for geerlingguy.docker, holding a role at the tag galaxy.lock locks.
	otherRoleURL = "https://github.com/attacker/ansible-role-docker"
	// dockerGalaxyPinKey is the Galaxy pin key of geerlingguy.docker asked
	// for at no version.
	dockerGalaxyPinKey = "galaxy\ngeerlingguy.docker\n"
	// movingRoleLabel is the version a role from movingRoleURL installs as at
	// HEAD or main, the branch HEAD names.
	movingRoleLabel = "main"
)

// sharedCacheProject is another project on cfg's cache: its own requirements
// file holding body and its own install paths, with no galaxy.lock, as another
// pipeline on one runner or one S3 bucket.
func sharedCacheProject(t *testing.T, cfg *config.Config, body string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	req := filepath.Join(dir, "requirements.yml")
	if err := os.WriteFile(req, []byte(body), 0o600); err != nil {
		t.Fatalf("write the other project's requirements: %v", err)
	}
	other := *cfg
	other.RequirementsFile = req
	other.DownloadPath = filepath.Join(dir, "install")
	other.RolesPath = filepath.Join(dir, "roles")
	other.Check, other.DryRun, other.Refresh = false, false, false
	return &other
}

// saveLockfile writes lf at the lockfile path beside reqPath.
func saveLockfile(t *testing.T, reqPath string, lf *lockfile.File) {
	t.Helper()
	if err := lockfile.Save(lockfile.ResolveDefaultPath(reqPath, ""), lf); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
}

// installAsAnotherProject runs a plain install of body as a project sharing f's
// cache and returns its configuration.
func installAsAnotherProject(t *testing.T, f *gitFixture, body string) *config.Config {
	t.Helper()
	other := sharedCacheProject(t, f.cfg, body)
	if err := collections.Start(context.Background(), other, f.runtime); err != nil {
		t.Fatalf("the other project's install: %v", err)
	}
	return other
}

// relockAppAt rewrites acme.app's entry in f's galaxy.lock to commit at version,
// leaving every other byte as lock wrote it.
func relockAppAt(t *testing.T, f *gitFixture, commit, version string) {
	t.Helper()
	lf := f.lockfile(t)
	for i := range lf.Collections {
		if lf.Collections[i].Name == "acme.app" {
			lf.Collections[i].Commit, lf.Collections[i].Version = commit, version
		}
	}
	saveLockfile(t, f.reqPath, lf)
}

// TestLockRecordsNoGitPinGalaxyLockAloneDecided pins that a galaxy.lock naming
// dev's commit for main makes no lock run record it: a project sharing the
// cache then installs the commit main names, from its own recorded pin.
func TestLockRecordsNoGitPinGalaxyLockAloneDecided(t *testing.T) {
	t.Parallel()
	const requirement = "collections:\n  - git+" + gitAppURL + ",main\n"
	mainCommit := fakeCommit("app-1")
	for name, tc := range map[string]struct {
		want          error
		version       string
		check, dryRun bool
	}{
		"passing lock --check": {version: appMovedVersion, check: true},
		"failing lock --check": {version: appLockedVersion, check: true, want: helpers.ErrLockfileDrift},
		"lock --dry-run":       {version: appMovedVersion, dryRun: true},
		"lock":                 {version: appMovedVersion},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGitFixture(t)
			installAsAnotherProject(t, f, requirement)
			f.writeRequirements(t, requirement)
			relockAppAt(t, f, fakeCommit("app-2"), tc.version)

			f.cfg.Check, f.cfg.DryRun = tc.check, tc.dryRun
			if err := lockGitRun(f); !errors.Is(err, tc.want) {
				t.Fatalf("lock over a galaxy.lock naming dev's commit for main: %v, want %v", err, tc.want)
			}
			pin, ok := loadStoreSnapshot(t, f.cfg, f.runtime).GetGitPin(gitsource.PinKey(gitAppURL, "main", ""))
			if !ok || pin.Commit != mainCommit {
				t.Fatalf("git pin for main after the lock = %+v (recorded: %t), want main's own commit %s", pin, ok, mainCommit)
			}

			f.git.resetCounts()
			other := installAsAnotherProject(t, f, requirement)
			if got := readManifestVersion(t, other.DownloadPath, "app"); got != appLockedVersion {
				t.Fatalf("the other project installed acme.app %s, want %s, the commit main names", got, appLockedVersion)
			}
			if adv, acq := f.git.counts(); adv != 0 || acq != 0 {
				t.Fatalf("the other project's install reached the remote: advertises=%d acquires=%d", adv, acq)
			}
		})
	}
}

// TestLockRecordsNoGalaxyRolePinFromGalaxyLock pins that lock records no Galaxy
// pin from a galaxy.lock entry, genuine or moved to another GitHub repository,
// so a project sharing the cache asks the v1 API and installs the genuine role.
func TestLockRecordsNoGalaxyRolePinFromGalaxyLock(t *testing.T) {
	t.Parallel()
	const docker = "roles:\n  - geerlingguy.docker\n"
	genuine, other := fakeCommit("docker-3"), fakeCommit("other-1")
	for name, tc := range map[string]struct {
		want                       error
		requirements, repo, commit string
	}{
		"the genuine entry":                        {requirements: docker, repo: galaxyRoleURL, commit: genuine},
		"another repository, passing lock --check": {requirements: docker, repo: otherRoleURL, commit: other},
		"another repository, failing lock --check": {
			requirements: docker + "  - src: git+" + roleAppURL + "\n    name: app\n", repo: otherRoleURL, commit: other,
			want: helpers.ErrLockfileDrift,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGalaxyRoleFixture(t)
			repo := &fakeGitRepo{refs: map[string]string{"HEAD": other, "refs/heads/master": other, "refs/tags/" + dockerLocked: other}}
			repo.addRole(other, fakeGitRole{roleName: "docker", files: map[string]string{"COMMIT": "OTHER " + other + "\n"}})
			f.git.add(otherRoleURL, repo)
			f.writeRequirements(t, docker)
			lf := f.lockfile(t)
			for i := range lf.Roles {
				lf.Roles[i].Repository, lf.Roles[i].Commit = tc.repo, tc.commit
			}
			saveLockfile(t, f.reqPath, lf)
			f.writeRequirements(t, tc.requirements)

			goColdRoles(t, f)
			f.cfg.Check = true
			if err := lockRoles(f); !errors.Is(err, tc.want) {
				t.Fatalf("lock --check on a cold cache: %v, want %v", err, tc.want)
			}
			st := loadStoreSnapshot(t, f.cfg, f.runtime)
			if pin, ok := st.GetRolePin(dockerGalaxyPinKey); ok {
				t.Fatalf("lock --check recorded the Galaxy pin %+v from galaxy.lock", pin)
			}
			// The tag still names the locked commit there, as any resolve would record.
			pin, ok := st.GetRolePin(gitsource.PinKey(tc.repo, "refs/tags/"+dockerLocked, ""))
			if !ok || pin.Commit != tc.commit || pin.Version != dockerLocked {
				t.Fatalf("git pin of the locked tag = %+v (recorded: %t), want %s at %s", pin, ok, dockerLocked, tc.commit)
			}

			project := sharedCacheProject(t, f.cfg, docker)
			f.galaxy.ResetCounts()
			if err := collections.Start(context.Background(), project, f.runtime); err != nil {
				t.Fatalf("the other project's install: %v", err)
			}
			assertFileContains(t, filepath.Join(project.RolesPath, "geerlingguy.docker", "COMMIT"), genuine)
			if got := f.galaxy.Count(fakegalaxy.EndpointRoleLookup); got != 1 {
				t.Fatalf("the other project's install made %d v1 lookups, want 1: no Galaxy pin to replay", got)
			}
		})
	}
}

// TestLockReplaysTheGitHalfOfAGalaxyRoleItRecorded is the Galaxy role's
// control: the tag's pin a lock on an empty cache recorded serves the next
// lock, which asks no Galaxy API and reaches no remote.
func TestLockReplaysTheGitHalfOfAGalaxyRoleItRecorded(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	before := lockRoleBytes(t, f)
	goColdRoles(t, f)
	f.lockfile(t)

	f.galaxy.ResetCounts()
	f.git.resetCounts()
	acquires := f.git.roleAcquireCount()
	f.lockfile(t)
	assertRoleLockBytes(t, f, before)
	if adv, _ := f.git.counts(); adv != 0 || f.galaxy.Total() != 0 || f.git.roleAcquireCount() != acquires {
		t.Fatalf("the next lock reached the network: advertises=%d Galaxy requests=%d role acquires=%d",
			adv, f.galaxy.Total(), f.git.roleAcquireCount()-acquires)
	}
}

// assertLockFetches fails unless lock run, since the last count reset, made
// the fetches a lock of locked makes: none when it replays, else one
// advertisement and one acquisition by locked.
func assertLockFetches(t *testing.T, f *gitFixture, take func() []string, run int, replays bool, locked string) {
	t.Helper()
	adv, commits := 1, []string{locked}
	if replays {
		adv, commits = 0, nil
	}
	if got, _ := f.git.counts(); got != adv {
		t.Fatalf("lock %d advertised %d times, want %d", run, got, adv)
	}
	if got := take(); !slices.Equal(got, commits) {
		t.Fatalf("lock %d acquired %q, want %q", run, got, commits)
	}
}

// assertLockedPinRecorded fails unless lock run left a pin recorded exactly
// when its ref did not move, the locked one (isLocked) when it did not.
func assertLockedPinRecorded(t *testing.T, run int, moved, recorded, isLocked bool, pin any) {
	t.Helper()
	switch {
	case moved && recorded:
		t.Fatalf("lock %d recorded the pin %+v, which its ref no longer names", run, pin)
	case !moved && (!recorded || !isLocked):
		t.Fatalf("lock %d left the pin %+v (recorded: %t), want the locked one", run, pin, recorded)
	}
}

// assertCachedArtifact fails unless lock run left key in cacheDir.
func assertCachedArtifact(t *testing.T, run int, cacheDir, key string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(cacheDir, key)); err != nil {
		t.Fatalf("lock %d left no artifact of the locked commit: %v", run, err)
	}
}

// TestLockRecordsALockedGitPinOnlyWhileItsRefNamesIt pins what a lock on an
// empty cache records for a git root: the locked commit's pin while main names
// it, which the next lock replays, else none, so each lock fetches it again.
func TestLockRecordsALockedGitPinOnlyWhileItsRefNamesIt(t *testing.T) {
	t.Parallel()
	locked := fakeCommit("app-1")
	artifact := helpers.ArtifactKey(gitsource.Locator{URL: gitAppURL, Commit: locked}.String(),
		helpers.ArtifactFilename("acme", "app", appLockedVersion))
	for name, moved := range map[string]bool{"unmoved branch": false, "moved branch": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGitFixture(t)
			git := recordGitCommits(f)
			f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n")
			f.lockfile(t)
			if moved {
				f.git.repos[gitAppURL].refs[gitMainRef] = fakeCommit("app-2")
			}
			goColdGit(t, f)
			git.take()

			for run := range 2 {
				f.git.resetCounts()
				if err := lockGitRun(f); err != nil {
					t.Fatalf("lock %d: %v", run, err)
				}
				pin, ok := loadStoreSnapshot(t, f.cfg, f.runtime).GetGitPin(gitsource.PinKey(gitAppURL, "main", ""))
				assertLockedPinRecorded(t, run, moved, ok, pin.Commit == locked, pin)
				assertCachedArtifact(t, run, f.cfg.CacheDir, artifact)
				assertLockFetches(t, f, git.take, run, run == 1 && !moved, locked)
			}
		})
	}
}

// TestLockRecordsALockedRolePinOnlyWhileItsRefNamesIt is the git role's
// counterpart: the locked commit's pin while HEAD names it, which the next
// lock replays, else none, so each lock fetches it again.
func TestLockRecordsALockedRolePinOnlyWhileItsRefNamesIt(t *testing.T) {
	t.Parallel()
	locked := fakeCommit("recorded-1")
	for name, moved := range map[string]bool{"unmoved HEAD": false, "moved HEAD": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRoleFixture(t)
			repo := addMovingRepo(f, "recorded-1", "recorded-2")
			git := recordRoleCommits(f)
			f.writeRequirements(t, "roles:\n  - src: git+"+movingRoleURL+"\n    name: moving\n")
			f.lockfile(t)
			if moved {
				moveMain(repo, "recorded-2")
			}
			goColdRoles(t, f)
			git.take()
			artifact := helpers.ArtifactKey(gitsource.Locator{URL: movingRoleURL, Commit: locked}.String(),
				helpers.RoleArtifactFilename("moving", movingRoleLabel))

			for run := range 2 {
				f.git.resetCounts()
				if err := lockRoles(f); err != nil {
					t.Fatalf("lock %d: %v", run, err)
				}
				pin, ok := loadStoreSnapshot(t, f.cfg, f.runtime).GetRolePin(gitsource.PinKey(movingRoleURL, "HEAD", ""))
				assertLockedPinRecorded(t, run, moved, ok, pin.Commit == locked && pin.Version == movingRoleLabel, pin)
				assertCachedArtifact(t, run, f.cfg.CacheDir, artifact)
				assertLockFetches(t, f.gitFixture, git.take, run, run == 1 && !moved, locked)
			}
		})
	}
}

// TestLockRecordsNoRolePinUnderALabelOnlyGalaxyLockNames pins the label half of
// the rule: a git role locked at main's own commit under a label main does not
// give keeps that label, rewriting nothing, but records no pin under it.
func TestLockRecordsNoRolePinUnderALabelOnlyGalaxyLockNames(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	addMovingRepo(f, "labeled-1")
	f.writeRequirements(t, "roles:\n  - src: git+"+movingRoleURL+"\n    version: main\n    name: moving\n")
	lf := f.lockfile(t)
	for i := range lf.Roles {
		lf.Roles[i].Version = "edited"
	}
	saveLockfile(t, f.reqPath, lf)
	before := sourceLockBytes(t, f.reqPath)

	goColdRoles(t, f)
	if err := lockRoles(f); err != nil {
		t.Fatalf("lock over a label main does not give: %v", err)
	}
	assertSourceLockBytes(t, f.reqPath, before)
	if pin, ok := loadStoreSnapshot(t, f.cfg, f.runtime).GetRolePin(gitsource.PinKey(movingRoleURL, "main", "")); ok {
		t.Fatalf("lock recorded the role pin %+v under a label only galaxy.lock names", pin)
	}
}
