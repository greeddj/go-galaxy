package collections_test

// This file pins, through the public Lock, that lock keeps the pin galaxy.lock
// holds for every role a request still matches - a Galaxy role's repository,
// tag and commit, a git role's commit and label, a url role's bytes.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

const (
	// movingRoleURL is a repository whose HEAD and main the tests below move.
	movingRoleURL = "https://git.example/acme/ansible-role-moving.git"
	// dockerLocked is the tag geerlingguy.docker locks at in its fixture.
	dockerLocked = "1.10.0"
	// dockerPublished is the tag publishDockerTag publishes after the lock.
	dockerPublished = "1.11.0"
)

// roleCommitRecorder is the fake git client recording the commit each role
// acquisition asked for, "" for one that resolved its ref instead.
type roleCommitRecorder struct {
	*fakeGitClient

	commits []string
	mu      sync.Mutex
}

// recordRoleCommits puts a roleCommitRecorder in front of f's git client.
func recordRoleCommits(f *roleFixture) *roleCommitRecorder {
	r := &roleCommitRecorder{fakeGitClient: f.git}
	f.runtime.Git = r
	return r
}

// AcquireRole records the commit req asks for, then serves it as the fake does.
func (r *roleCommitRecorder) AcquireRole(ctx context.Context, req gitsource.RoleRequest) (gitsource.RoleResult, error) {
	r.mu.Lock()
	r.commits = append(r.commits, req.Commit)
	r.mu.Unlock()
	return r.fakeGitClient.AcquireRole(ctx, req)
}

// take returns the commits recorded since the last call, sorted, and forgets
// them; a level's roles are acquired concurrently.
func (r *roleCommitRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.commits
	r.commits = nil
	slices.Sort(out)
	return out
}

// lockRoles runs Lock over f's configuration.
func lockRoles(f *roleFixture) error {
	return collections.Lock(context.Background(), f.cfg, f.runtime)
}

// roleLockPath is where lock writes f's lockfile.
func roleLockPath(f *roleFixture) string {
	return lockfile.ResolveDefaultPath(f.reqPath, "")
}

// lockRoleBytes locks f and returns the bytes it wrote.
func lockRoleBytes(t *testing.T, f *roleFixture) []byte {
	t.Helper()
	f.lockfile(t)
	data, err := os.ReadFile(roleLockPath(f))
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	return data
}

// assertRoleLockBytes fails unless f's lockfile still holds want.
func assertRoleLockBytes(t *testing.T, f *roleFixture, want []byte) {
	t.Helper()
	got, err := os.ReadFile(roleLockPath(f))
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("lockfile changed:\n--- before\n%s--- after\n%s", want, got)
	}
}

// goColdRoles points f at a new, empty cache and resets the request counts.
func goColdRoles(t *testing.T, f *roleFixture) {
	t.Helper()
	f.cfg.CacheDir = filepath.Join(t.TempDir(), "cache")
	f.galaxy.ResetCounts()
	f.git.resetCounts()
}

// warnCount counts the recorded warnings containing substr.
func warnCount(p *warnCapturingPrinter, substr string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, line := range p.warns {
		if strings.Contains(line, substr) {
			count++
		}
	}
	return count
}

// publishDockerTag publishes geerlingguy.docker 1.11.0 at a new commit, on
// the repository and in the v1 version list.
func publishDockerTag(f *roleFixture) {
	newer := fakeCommit("docker-4")
	repo := f.git.repos[galaxyRoleURL]
	repo.refs["refs/tags/1.11.0"] = newer
	repo.addRole(newer, fakeGitRole{roleName: "docker", files: map[string]string{"COMMIT": newer + "\n"}})
	f.galaxy.AddRole("geerlingguy", "docker", "geerlingguy", "ansible-role-docker", "master", []fakegalaxy.RoleVersion{
		{Name: "1.0.0"}, {Name: "1.10.0", CommitSHA: fakeCommit("docker-3")}, {Name: "1.9.0"}, {Name: "1.11.0", CommitSHA: newer},
	})
}

// addMovingRepo registers movingRoleURL with HEAD and main at the commit of
// the first label, and a role at the commit of every label.
func addMovingRepo(f *roleFixture, labels ...string) *fakeGitRepo {
	first := fakeCommit(labels[0])
	repo := &fakeGitRepo{refs: map[string]string{"HEAD": first, roleMainRef: first}}
	for _, label := range labels {
		c := fakeCommit(label)
		repo.addRole(c, fakeGitRole{roleName: "moving", files: map[string]string{"COMMIT": c + "\n"}})
	}
	f.git.add(movingRoleURL, repo)
	return repo
}

// moveMain points repo's HEAD and main at the commit of label.
func moveMain(repo *fakeGitRepo, label string) {
	repo.refs["HEAD"] = fakeCommit(label)
	repo.refs[roleMainRef] = fakeCommit(label)
}

// TestLockKeepsAGalaxyRoleOnAColdCache pins that a tag published after lock
// leaves a Galaxy role at its locked tag on an empty cache: --check and lock
// ask no Galaxy API, fetch the locked commit once and rewrite nothing.
func TestLockKeepsAGalaxyRoleOnAColdCache(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	git := recordRoleCommits(f)
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	before := lockRoleBytes(t, f)
	publishDockerTag(f)
	locked := fakeCommit("docker-3")

	for _, check := range []bool{true, false} {
		goColdRoles(t, f)
		git.take()
		f.cfg.Check = check
		if err := lockRoles(f); err != nil {
			t.Fatalf("lock (check %t) on a cold cache after a newer tag: %v", check, err)
		}
		if got := f.galaxy.Total(); got != 0 {
			t.Fatalf("lock (check %t) made %d Galaxy requests, want 0", check, got)
		}
		if got := git.take(); !slices.Equal(got, []string{locked}) {
			t.Fatalf("role acquisitions (check %t) = %q, want one by the locked commit %s", check, got, locked)
		}
		assertRoleLockBytes(t, f, before)
	}

	// Positive control: --refresh sets the pin aside and sees the newer tag.
	f.cfg.Check, f.cfg.Refresh = true, true
	if err := lockRoles(f); !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("lock --check --refresh after a newer tag: %v, want ErrLockfileDrift", err)
	}
}

// TestLockLeavesTheGalaxyRolePinInstallRefreshMoved pins that lock keeps
// galaxy.lock's tag over the Galaxy pin install --refresh moved without
// recording it: the pin still names the newer tag, which plain install replays.
func TestLockLeavesTheGalaxyRolePinInstallRefreshMoved(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	before := lockRoleBytes(t, f)
	publishDockerTag(f)
	f.cfg.Refresh = true
	f.mustInstall(t)
	installed := filepath.Join(f.rolePath("geerlingguy.docker"), "COMMIT")
	assertFileContains(t, installed, fakeCommit("docker-4"))

	f.cfg.Refresh = false
	f.galaxy.ResetCounts()
	if err := lockRoles(f); err != nil {
		t.Fatalf("lock over an install --refresh pin: %v", err)
	}
	assertRoleLockBytes(t, f, before)
	if got := f.galaxy.Total(); got != 0 {
		t.Fatalf("lock made %d Galaxy requests, want 0", got)
	}
	pin, ok := loadStoreSnapshot(t, f.cfg, f.runtime).GetRolePin("galaxy\ngeerlingguy.docker\n")
	if !ok || pin.Version != dockerPublished || pin.Commit != fakeCommit("docker-4") {
		t.Fatalf("Galaxy pin after lock = %+v (recorded: %t), want %s, as install --refresh recorded it", pin, ok, dockerPublished)
	}

	f.mustInstall(t)
	assertFileContains(t, installed, fakeCommit("docker-4"))
	if got := f.galaxy.Total(); got != 0 {
		t.Fatalf("install after lock made %d Galaxy requests, want 0", got)
	}
}

// TestLockOverAnAgreeingCacheSkipsItsSave pins that a lock whose roles replay
// what galaxy.lock pins leaves the store clean, so its save is skipped: lock
// records no Galaxy pin from the file, and the v1 API's stands as recorded.
func TestLockOverAnAgreeingCacheSkipsItsSave(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n  - src: git+"+roleAppURL+"\n    name: app\n")
	f.lockfile(t)
	stamp := reloadLastSnapshot(t, f.cfg.CacheDir)
	f.lockfile(t)
	if got := reloadLastSnapshot(t, f.cfg.CacheDir); !got.Equal(stamp) {
		t.Fatalf("LastSnapshot after an idle lock = %v, want unchanged %v", got, stamp)
	}
}

// TestLockKeepsAGitRoleWhoseRefMoved pins that a git role keeps its locked
// commit and label once its branch, or HEAD for one asked at no version,
// moved: on an empty cache, and over the pins install --refresh recorded.
func TestLockKeepsAGitRoleWhoseRefMoved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		refresh bool
	}{{name: "empty cache"}, {name: "install --refresh pins", refresh: true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRoleFixture(t)
			repo := addMovingRepo(f, "moving-1", "moving-2")
			git := recordRoleCommits(f)
			f.writeRequirements(t, "roles:\n  - src: git+"+movingRoleURL+"\n    name: head\n"+
				"  - src: git+"+movingRoleURL+"\n    version: main\n    name: branch\n")
			before := lockRoleBytes(t, f)
			locked := fakeCommit("moving-1")
			lf, err := lockfile.Load(roleLockPath(f))
			if err != nil {
				t.Fatalf("load lockfile: %v", err)
			}
			assertLockedRole(t, lf, lockfile.RoleEntry{
				Name: "head", Type: lockfile.RoleTypeGit, Version: "main", Source: movingRoleURL, Ref: "HEAD", Commit: locked,
			})
			assertLockedRole(t, lf, lockfile.RoleEntry{
				Name: "branch", Type: lockfile.RoleTypeGit, Version: "main", Source: movingRoleURL, Ref: "main", Commit: locked,
			})

			moveMain(repo, "moving-2")
			if tc.refresh {
				f.cfg.Refresh = true
				f.mustInstall(t)
				assertFileContains(t, filepath.Join(f.rolePath("head"), "COMMIT"), fakeCommit("moving-2"))
				f.cfg.Refresh = false
			} else {
				goColdRoles(t, f)
			}
			git.take()
			if err := lockRoles(f); err != nil {
				t.Fatalf("lock after the ref moved: %v", err)
			}
			assertRoleLockBytes(t, f, before)
			if got := git.take(); !slices.Equal(got, []string{locked, locked}) {
				t.Fatalf("role acquisitions = %q, want two by the locked commit %s", got, locked)
			}
		})
	}
}

// TestLockKeepsALockedDependencyRole pins that a role reached only through
// another role's meta keeps its locked commit once its branch moved, and that
// the dependency list comes from the role's own meta, never from galaxy.lock.
func TestLockKeepsALockedDependencyRole(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	git := recordRoleCommits(f)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleAppURL+"\n    name: app\n")
	before := lockRoleBytes(t, f)
	base := f.git.repos[roleBaseURL]
	moved := fakeCommit("role-base-2")
	base.addRole(moved, fakeGitRole{roleName: "base"})
	base.refs["HEAD"], base.refs[roleMainRef] = moved, moved

	goColdRoles(t, f)
	git.take()
	if err := lockRoles(f); err != nil {
		t.Fatalf("lock after the dependency's branch moved: %v", err)
	}
	assertRoleLockBytes(t, f, before)
	want := []string{fakeCommit("role-app-1"), fakeCommit("role-base-1")}
	slices.Sort(want)
	if got := git.take(); !slices.Equal(got, want) {
		t.Fatalf("role acquisitions = %q, want the locked commits %q", got, want)
	}

	// galaxy.lock's deps list is not read: app's meta at its commit names base.
	lf, err := lockfile.Load(roleLockPath(f))
	if err != nil {
		t.Fatalf("load lockfile: %v", err)
	}
	app, dep := findLockRole(t, lf, "app"), findLockRole(t, lf, "base")
	app.Deps = nil
	writeRoleLockfile(t, f, app, dep)
	goColdRoles(t, f)
	if err := lockRoles(f); err != nil {
		t.Fatalf("lock over a lockfile without app's deps: %v", err)
	}
	assertRoleLockBytes(t, f, before)
}

// TestLockResolvesARoleWhoseLockedCommitIsGone pins that a locked commit the
// repository no longer serves warns once per run and the role is resolved
// anew: --check reports the drift, and lock writes what HEAD names now.
func TestLockResolvesARoleWhoseLockedCommitIsGone(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	repo := addMovingRepo(f, "gone-1", "gone-2")
	f.writeRequirements(t, "roles:\n  - src: git+"+movingRoleURL+"\n    name: moving\n")
	f.lockfile(t)
	moveMain(repo, "gone-2")
	delete(repo.roles, fakeCommit("gone-1"))
	warning := "Locked role moving: commit " + fakeCommit("gone-1") + " is no longer served by " + movingRoleURL + "; resolving the role anew"

	goColdRoles(t, f)
	f.cfg.Check = true
	if err := lockRoles(f); !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("lock --check over a commit no longer served: %v, want ErrLockfileDrift", err)
	}
	if got := warnCount(f.printer, warning); got != 1 {
		t.Fatalf("warnings %q after --check = %d, want 1: %q", warning, got, f.printer.warns)
	}

	goColdRoles(t, f)
	f.cfg.Check = false
	assertLockedRole(t, f.lockfile(t), lockfile.RoleEntry{
		Name: "moving", Type: lockfile.RoleTypeGit, Version: "main", Source: movingRoleURL, Ref: "HEAD", Commit: fakeCommit("gone-2"),
	})
	if got := warnCount(f.printer, warning); got != 2 {
		t.Fatalf("warnings %q after lock = %d, want 2, one per run: %q", warning, got, f.printer.warns)
	}
}

// TestLockResolvesAGalaxyRoleEntryItCannotUse pins that a locked Galaxy role
// entry naming a repository off GitHub, a ref no v1 answer names or a server
// the run does not use warns once and is resolved through the v1 API anew.
func TestLockResolvesAGalaxyRoleEntryItCannotUse(t *testing.T) {
	t.Parallel()
	const prefix, suffix = "Locked role geerlingguy.docker: ", "; resolving the role anew"
	for name, tc := range map[string]struct {
		edit    func(e *lockfile.RoleEntry)
		warning string
	}{
		"repository off GitHub": {
			edit:    func(e *lockfile.RoleEntry) { e.Repository = roleBaseURL },
			warning: prefix + "repository " + roleBaseURL + " is not a GitHub repository" + suffix,
		},
		"unqualified ref": {
			edit:    func(e *lockfile.RoleEntry) { e.Ref = dockerLocked },
			warning: prefix + "ref 1.10.0 is not a refs/tags/ or refs/heads/ ref" + suffix,
		},
		"server outside the run": {
			edit:    func(e *lockfile.RoleEntry) { e.Source = "https://old-galaxy.example" },
			warning: prefix + "server https://old-galaxy.example is not one this run uses" + suffix,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGalaxyRoleFixture(t)
			f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
			entry := findLockRole(t, f.lockfile(t), "geerlingguy.docker")
			tc.edit(&entry)
			writeRoleLockfile(t, f, entry)

			goColdRoles(t, f)
			f.cfg.Check = true
			if err := lockRoles(f); !errors.Is(err, helpers.ErrLockfileDrift) {
				t.Fatalf("lock --check over an entry lock cannot use: %v, want ErrLockfileDrift", err)
			}
			if got := f.galaxy.Count(fakegalaxy.EndpointRoleLookup); got != 1 {
				t.Fatalf("v1 lookups = %d, want 1", got)
			}
			if got := warnCount(f.printer, tc.warning); got != 1 {
				t.Fatalf("warnings %q = %d, want 1: %q", tc.warning, got, f.printer.warns)
			}
		})
	}
}

// TestLockOfflineRefusesARoleWhoseLockedCommitIsNotCached pins --offline over
// a cache install --refresh moved past galaxy.lock: the locked commit is not
// there to keep, and the cache's other pin is not taken instead.
func TestLockOfflineRefusesARoleWhoseLockedCommitIsNotCached(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	repo := addMovingRepo(f, "offline-1", "offline-2")
	f.writeRequirements(t, "roles:\n  - src: git+"+movingRoleURL+"\n    name: moving\n")
	f.lockfile(t)
	moveMain(repo, "offline-2")
	f.cfg.Refresh = true
	f.mustInstall(t)

	f.cfg.Refresh = false
	f.goOffline()
	before := f.git.roleAcquireCount()
	err := lockRoles(f)
	if !errors.Is(err, helpers.ErrOfflineMode) || !strings.Contains(err.Error(), "locked commit "+fakeCommit("offline-1")) {
		t.Fatalf("lock --offline without the locked commit: %v, want ErrOfflineMode naming it", err)
	}
	if adv, acq := f.git.counts(); adv != 0 || acq != 0 || f.git.roleAcquireCount() != before {
		t.Fatalf("offline lock reached the remote: advertises=%d acquires=%d role acquires=%d",
			adv, acq, f.git.roleAcquireCount()-before)
	}
}

// TestLockResolvesAChangedRoleRequestAsBefore pins that a request galaxy.lock
// no longer matches - another ref or Galaxy version - resolves as without the
// file: --check reports the drift and no locked-role warning prints.
func TestLockResolvesAChangedRoleRequestAsBefore(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ locked, changed string }{
		"git ref": {
			locked:  "roles:\n  - src: git+" + roleAppURL + "\n    name: app\n",
			changed: "roles:\n  - src: git+" + roleAppURL + "\n    version: dev\n    name: app\n",
		},
		"Galaxy version": {
			locked:  "roles:\n  - geerlingguy.docker\n",
			changed: "roles:\n  - src: geerlingguy.docker\n    version: 1.9.0\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGalaxyRoleFixture(t)
			f.writeRequirements(t, tc.locked)
			f.lockfile(t)
			f.writeRequirements(t, tc.changed)
			goColdRoles(t, f)
			f.cfg.Check = true
			if err := lockRoles(f); !errors.Is(err, helpers.ErrLockfileDrift) {
				t.Fatalf("lock --check after the request changed: %v, want ErrLockfileDrift", err)
			}
			if got := warnCount(f.printer, "Locked role"); got != 0 {
				t.Fatalf("a changed request drew a locked-role warning: %q", f.printer.warns)
			}
		})
	}
}

// goColdURLRoles points f at a new, empty cache directory.
func goColdURLRoles(t *testing.T, f *urlRoleFixture) {
	t.Helper()
	f.cfg.CacheDir = filepath.Join(t.TempDir(), "cache")
}

// urlRoleLockBytes locks f and returns the bytes it wrote.
func urlRoleLockBytes(t *testing.T, f *urlRoleFixture) []byte {
	t.Helper()
	f.lockfile(t)
	data, err := os.ReadFile(lockfile.ResolveDefaultPath(f.reqPath, ""))
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	return data
}

// TestLockKeepsAURLRoleWhoseBytesHold pins a url role on an empty cache whose
// URL still serves the locked bytes: one download per run, no rewrite and no
// warning, under a label the requirement names or the default one.
func TestLockKeepsAURLRoleWhoseBytesHold(t *testing.T) {
	t.Parallel()
	for name, label := range map[string]string{"default label": "", "named label": "1.2.3"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newURLRoleFixture(t)
			data, _ := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=held\n"})
			roleURL := f.galaxy.AddTarball("dl/held.tar.gz", data)
			f.writeRequirements(t, urlRoleRequirements(roleURL, "held", label))
			before := urlRoleLockBytes(t, f)

			for _, check := range []bool{true, false} {
				goColdURLRoles(t, f)
				downloads := f.galaxy.Count(fakegalaxy.EndpointTarball)
				f.cfg.Check = check
				if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
					t.Fatalf("lock (check %t) on a cold cache: %v", check, err)
				}
				if got := f.galaxy.Count(fakegalaxy.EndpointTarball) - downloads; got != 1 {
					t.Fatalf("lock (check %t) downloaded %d times, want 1", check, got)
				}
				got, err := os.ReadFile(lockfile.ResolveDefaultPath(f.reqPath, ""))
				if err != nil {
					t.Fatalf("read lockfile: %v", err)
				}
				if string(got) != string(before) {
					t.Fatalf("lockfile changed:\n--- before\n%s--- after\n%s", before, got)
				}
			}
			if got := warnCount(f.printer, "Locked role"); got != 0 {
				t.Fatalf("unchanged bytes drew a locked-role warning: %q", f.printer.warns)
			}
		})
	}
}

// TestLockTakesTheNewBytesOfAURLRole pins a url role whose URL now serves
// other bytes: one warning per run, --check reports the drift, and lock
// writes the new sha256 under the label those bytes take.
func TestLockTakesTheNewBytesOfAURLRole(t *testing.T) {
	t.Parallel()
	f := newURLRoleFixture(t)
	data, oldSHA := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=old\n"})
	roleURL := f.galaxy.AddTarball("dl/moved.tar.gz", data)
	f.writeRequirements(t, urlRoleRequirements(roleURL, "moved", ""))
	f.lockfile(t)
	newData, newSHA := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=new\n"})
	f.galaxy.AddTarball("dl/moved.tar.gz", newData)
	warning := "Locked role moved: sha256 " + oldSHA + " is no longer served by " + roleURL +
		", which now serves " + newSHA + "; keeping the new bytes"

	goColdURLRoles(t, f)
	f.cfg.Check = true
	if err := collections.Lock(context.Background(), f.cfg, f.runtime); !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("lock --check over other bytes: %v, want ErrLockfileDrift", err)
	}
	if got := warnCount(f.printer, warning); got != 1 {
		t.Fatalf("warnings %q after --check = %d, want 1: %q", warning, got, f.printer.warns)
	}

	goColdURLRoles(t, f)
	f.cfg.Check = false
	lf := f.lockfile(t)
	if got := lf.Roles[0]; got.SHA256 != newSHA || got.Version != newSHA[:12] {
		t.Fatalf("url role entry = %+v, want sha256 %s labeled %s", got, newSHA, newSHA[:12])
	}
	if got := warnCount(f.printer, warning); got != 2 {
		t.Fatalf("warnings %q after lock = %d, want 2, one per run: %q", warning, got, f.printer.warns)
	}
}

// TestLockOfflineRefusesAURLRoleWhoseLockedBytesAreNotCached pins --offline
// over a cache install --refresh moved to other bytes: the locked sha256 is
// not recorded, and the recorded one is not taken instead.
func TestLockOfflineRefusesAURLRoleWhoseLockedBytesAreNotCached(t *testing.T) {
	t.Parallel()
	f := newURLRoleFixture(t)
	data, oldSHA := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=old\n"})
	roleURL := f.galaxy.AddTarball("dl/offline.tar.gz", data)
	f.writeRequirements(t, urlRoleRequirements(roleURL, "offline", ""))
	f.lockfile(t)
	newData, _ := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=new\n"})
	f.galaxy.AddTarball("dl/offline.tar.gz", newData)
	f.cfg.Refresh = true
	f.mustInstall(t)

	f.cfg.Refresh, f.cfg.Offline = false, true
	f.runtime.HTTP = fetch.NewOffline(e2eTimeout)
	f.runtime.URLHTTP = fetch.NewURLDownload(e2eTimeout, true, nil)
	downloads := f.galaxy.Count(fakegalaxy.EndpointTarball)
	err := collections.Lock(context.Background(), f.cfg, f.runtime)
	if !errors.Is(err, helpers.ErrOfflineMode) || !strings.Contains(err.Error(), "locked sha256 "+oldSHA) {
		t.Fatalf("lock --offline without the locked bytes: %v, want ErrOfflineMode naming them", err)
	}
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != downloads {
		t.Fatalf("offline lock reached the origin: downloads = %d, want %d", got, downloads)
	}
}

// TestLockRelabelsAURLRoleWhoseLabelWasDropped pins that a url role keeps its
// locked bytes under the label its request now takes, the bytes' default once
// version: is dropped, so the pin lock records is the one install replays.
func TestLockRelabelsAURLRoleWhoseLabelWasDropped(t *testing.T) {
	t.Parallel()
	f := newURLRoleFixture(t)
	data, sha := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=held\n"})
	roleURL := f.galaxy.AddTarball("dl/held.tar.gz", data)
	f.writeRequirements(t, urlRoleRequirements(roleURL, "held", "1.2.3"))
	f.lockfile(t)
	f.writeRequirements(t, urlRoleRequirements(roleURL, "held", ""))

	f.cfg.Check = true
	if err := collections.Lock(context.Background(), f.cfg, f.runtime); !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("lock --check after dropping the label: %v, want ErrLockfileDrift", err)
	}
	f.cfg.Check = false
	if got := f.lockfile(t).Roles[0]; got.SHA256 != sha || got.Version != sha[:12] {
		t.Fatalf("url role entry = %+v, want sha256 %s labeled %s", got, sha, sha[:12])
	}

	f.cfg.Offline = true
	f.runtime.HTTP = fetch.NewOffline(e2eTimeout)
	f.runtime.URLHTTP = fetch.NewURLDownload(e2eTimeout, true, nil)
	downloads := f.galaxy.Count(fakegalaxy.EndpointTarball)
	if err := f.install(t); err != nil {
		t.Fatalf("install --offline over the cache lock filled: %v", err)
	}
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != downloads {
		t.Fatalf("offline install reached the origin: downloads = %d, want %d", got, downloads)
	}
	if got := warnCount(f.printer, "Locked role"); got != 0 {
		t.Fatalf("a dropped label drew a locked-role warning: %q", f.printer.warns)
	}
}

// TestLockRecordsNoGalaxyPinTwoEntriesContest pins that two install names of
// one Galaxy role, asked at no version and locked at two releases, keep both,
// and neither writes their shared Galaxy pin key, so idle locks save nothing.
func TestLockRecordsNoGalaxyPinTwoEntriesContest(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - name: d1\n    src: geerlingguy.docker\n    version: 1.9.0\n"+
		"  - name: d2\n    src: geerlingguy.docker\n    version: 1.10.0\n")
	f.lockfile(t)
	f.writeRequirements(t, "roles:\n  - name: d1\n    src: geerlingguy.docker\n  - name: d2\n    src: geerlingguy.docker\n")
	before := lockRoleBytes(t, f)
	stamp := reloadLastSnapshot(t, f.cfg.CacheDir)

	for i := range 3 {
		f.lockfile(t)
		if got := reloadLastSnapshot(t, f.cfg.CacheDir); !got.Equal(stamp) {
			t.Fatalf("LastSnapshot after idle lock %d = %v, want unchanged %v", i, got, stamp)
		}
	}
	assertRoleLockBytes(t, f, before)
	lf := f.lockfile(t)
	if d1, d2 := findLockRole(t, lf, "d1"), findLockRole(t, lf, "d2"); d1.Version != "1.9.0" || d2.Version != dockerLocked {
		t.Fatalf("d1 locked at %s and d2 at %s, want 1.9.0 and %s", d1.Version, d2.Version, dockerLocked)
	}
	if pin, ok := loadStoreSnapshot(t, f.cfg, f.runtime).GetRolePin("galaxy\ngeerlingguy.docker\n"); ok {
		t.Fatalf("the contested Galaxy pin key was recorded: %+v", pin)
	}
}

// movedDockerURL is the repository the v1 API names for geerlingguy.docker
// once its locked repository is gone.
const movedDockerURL = "https://github.com/geerlingguy/ansible-role-docker-moved"

// moveDockerRepository deletes geerlingguy.docker's locked repository and,
// unless sameName, re-registers the role on movedDockerURL at docker-moved.
func moveDockerRepository(f *roleFixture, sameName bool) string {
	moved := fakeCommit("docker-moved")
	delete(f.git.repos, galaxyRoleURL)
	repoName := "ansible-role-docker"
	if !sameName {
		repoName = "ansible-role-docker-moved"
		repo := &fakeGitRepo{refs: map[string]string{"HEAD": moved, "refs/heads/master": moved, "refs/tags/" + dockerLocked: moved}}
		repo.addRole(moved, fakeGitRole{roleName: "docker", files: map[string]string{"COMMIT": moved + "\n"}})
		f.git.add(movedDockerURL, repo)
	}
	f.galaxy.AddRole("geerlingguy", "docker", "geerlingguy", repoName, "master",
		[]fakegalaxy.RoleVersion{{Name: dockerLocked, CommitSHA: moved}})
	return moved
}

// evictDockerArtifact removes geerlingguy.docker's cached role artifact at its
// locked commit, if any, while every recorded pin stays.
func evictDockerArtifact(t *testing.T, f *roleFixture) {
	t.Helper()
	locator := gitsource.Locator{URL: galaxyRoleURL, Commit: fakeCommit("docker-3")}.String()
	if err := os.Remove(roleArtifactPath(f, locator, "geerlingguy.docker", dockerLocked)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("evict the role artifact: %v", err)
	}
}

// lockOverAGoneRepository runs lock --check, which must report drift, then
// lock, each after prepare.
func lockOverAGoneRepository(t *testing.T, f *roleFixture, prepare func(t *testing.T, f *roleFixture)) {
	t.Helper()
	prepare(t, f)
	f.cfg.Check = true
	if err := lockRoles(f); !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("lock --check over a gone repository: %v, want ErrLockfileDrift", err)
	}
	prepare(t, f)
	f.cfg.Check = false
	if err := lockRoles(f); err != nil {
		t.Fatalf("lock over a gone repository: %v", err)
	}
}

// TestLockResolvesAGalaxyRoleWhoseLockedRepositoryIsGone pins that a Galaxy
// role whose locked repository is gone, and that the v1 API now names another,
// resolves anew with one warning per run, on an empty cache or pins alone.
func TestLockResolvesAGalaxyRoleWhoseLockedRepositoryIsGone(t *testing.T) {
	t.Parallel()
	for name, prepare := range map[string]func(t *testing.T, f *roleFixture){
		"empty cache":      goColdRoles,
		"evicted artifact": evictDockerArtifact,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGalaxyRoleFixture(t)
			f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
			f.lockfile(t)
			moved := moveDockerRepository(f, false)
			warning := "Locked role geerlingguy.docker: repository " + galaxyRoleURL +
				" can no longer be fetched; resolving the role anew"

			lockOverAGoneRepository(t, f, prepare)
			entry := findLockRole(t, f.lockfile(t), "geerlingguy.docker")
			if entry.Repository != movedDockerURL || entry.Commit != moved || entry.Version != dockerLocked {
				t.Fatalf("role entry = %+v, want %s at %s from %s", entry, dockerLocked, moved, movedDockerURL)
			}
			if got := warnCount(f.printer, warning); got != 2 {
				t.Fatalf("warnings %q = %d, want 2, one per run: %q", warning, got, f.printer.warns)
			}
		})
	}
}

// TestLockFailsOnAGalaxyRoleWhoseOnlyRepositoryIsGone is the control: when the
// v1 API still names the locked repository, nothing else can serve the role,
// so lock fails on the fetch, exit 4, with no warning.
func TestLockFailsOnAGalaxyRoleWhoseOnlyRepositoryIsGone(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	f.lockfile(t)
	moveDockerRepository(f, true)
	goColdRoles(t, f)

	err := lockRoles(f)
	if !errors.Is(err, helpers.ErrGitTransportFailed) {
		t.Fatalf("lock over the only repository gone: %v, want ErrGitTransportFailed", err)
	}
	if got := warnCount(f.printer, "Locked role"); got != 0 {
		t.Fatalf("a repository the v1 API still names drew a locked-role warning: %q", f.printer.warns)
	}
}

// TestLockRefetchesALockedRolePinRecordedUnderAnotherLabel pins the label in a
// locked git role's replay: a pin at the locked commit recorded as HEAD, as older
// releases did, is fetched once more under the locked label, rewriting nothing.
func TestLockRefetchesALockedRolePinRecordedUnderAnotherLabel(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	git := recordRoleCommits(f)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleAppURL+"\n    name: app\n")
	before := lockRoleBytes(t, f)
	locked := fakeCommit("role-app-1")
	locator := gitsource.Locator{URL: roleAppURL, Commit: locked}.String()
	built, err := os.ReadFile(roleArtifactPath(f, locator, "app", "main"))
	if err != nil {
		t.Fatalf("read the cached artifact: %v", err)
	}
	//nolint:gosec // path is built from this test's own temp dirs.
	if err := os.WriteFile(roleArtifactPath(f, locator, "app", "HEAD"), built, 0o600); err != nil {
		t.Fatalf("write the HEAD-labeled artifact: %v", err)
	}
	pinKey := gitsource.PinKey(roleAppURL, "HEAD", "")
	mutateStoreSnapshot(t, f.gitFixture, func(st *store.Store) {
		pin, ok := st.GetRolePin(pinKey)
		if !ok {
			t.Fatalf("no git role pin recorded under %q", pinKey)
		}
		pin.Version = "HEAD"
		st.SetRolePin(pinKey, pin)
	})
	git.take()

	for _, check := range []bool{true, false} {
		f.cfg.Check = check
		if err := lockRoles(f); err != nil {
			t.Fatalf("lock (check %t) over a pin recorded as HEAD: %v", check, err)
		}
		assertRoleLockBytes(t, f, before)
	}
	if got := git.take(); !slices.Equal(got, []string{locked}) {
		t.Fatalf("role acquisitions = %q, want one by the locked commit %s", got, locked)
	}
}

// TestLockKeepsTheServerAGalaxyRoleWasLockedFrom pins that a Galaxy role
// locked from the second server of the list keeps that server's source:
// --check passes and lock rewrites nothing, asking neither server's v1 API.
func TestLockKeepsTheServerAGalaxyRoleWasLockedFrom(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	hub := fakegalaxy.New(t)
	f.cfg.Servers = []config.Server{{ID: "hub", URL: hub.URL()}, {ID: "galaxy", URL: f.galaxy.URL()}}
	f.cfg.Server = hub.URL()
	f.runtime = multiServerRuntime(f.cfg)
	f.runtime.Git = f.git
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	before := lockRoleBytes(t, f)
	locked, err := lockfile.Load(roleLockPath(f))
	if err != nil {
		t.Fatalf("load lockfile: %v", err)
	}
	if got := findLockRole(t, locked, "geerlingguy.docker").Source; got != f.galaxy.URL() {
		t.Fatalf("geerlingguy.docker locked from %s, want the second server %s", got, f.galaxy.URL())
	}
	f.galaxy.ResetCounts()
	hub.ResetCounts()

	for _, check := range []bool{true, false} {
		f.cfg.Check = check
		if err := lockRoles(f); err != nil {
			t.Fatalf("lock (check %t) over a role locked from the second server: %v", check, err)
		}
		assertRoleLockBytes(t, f, before)
	}
	if got := f.galaxy.Total() + hub.Total(); got != 0 {
		t.Fatalf("relocks made %d Galaxy requests, want 0", got)
	}
}

// TestInstallAndWarmTakeMovedSourcesPastTheLockfile is the control for lock's
// git and role pins: install without --frozen and warm read no galaxy.lock, so
// on an empty cache both take main's new commit and a tag published later.
func TestInstallAndWarmTakeMovedSourcesPastTheLockfile(t *testing.T) {
	t.Parallel()
	for name, run := range map[string]func(context.Context, *config.Config, *infra.Infra) error{
		"install": collections.Start, "warm": collections.Warm,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGalaxyRoleFixture(t)
			f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\nroles:\n  - geerlingguy.docker\n")
			f.lockfile(t)
			f.git.repos[gitAppURL].refs[gitMainRef] = fakeCommit("app-2")
			publishDockerTag(f)
			goColdRoles(t, f)

			if err := run(context.Background(), f.cfg, f.runtime); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			st := loadStoreSnapshot(t, f.cfg, f.runtime)
			if pin, ok := st.GetGitPin(gitsource.PinKey(gitAppURL, "main", "")); !ok || pin.Commit != fakeCommit("app-2") {
				t.Fatalf("%s recorded git pin %+v (recorded: %t), want main's new commit %s", name, pin, ok, fakeCommit("app-2"))
			}
			if pin, ok := st.GetRolePin("galaxy\ngeerlingguy.docker\n"); !ok || pin.Version != "1.11.0" || pin.Commit != fakeCommit("docker-4") {
				t.Fatalf("%s recorded Galaxy pin %+v (recorded: %t), want the new tag 1.11.0", name, pin, ok)
			}
		})
	}
}
