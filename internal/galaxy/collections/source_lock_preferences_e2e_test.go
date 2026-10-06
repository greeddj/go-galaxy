package collections_test

// This file pins, through the public Lock, that lock keeps the pin galaxy.lock
// holds for every git and url root a requirement still matches: a git root's
// commit, read from the entries it owns, and a url root's bytes.

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
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

const (
	// appLockedVersion and appMovedVersion are acme.app's versions at app-1,
	// where the git fixture locks it, and at app-2; kafkaNewVersion is what the
	// url fixture's URL serves once serveNewKafkaBytes changes its bytes.
	appLockedVersion = "1.2.3"
	appMovedVersion  = "1.3.0"
	kafkaNewVersion  = "0.25.0"
)

// gitCommitRecorder is the fake git client recording the commit each
// collection acquisition asked for, "" for one that resolved its ref instead.
type gitCommitRecorder struct {
	*fakeGitClient

	commits []string
	mu      sync.Mutex
}

// recordGitCommits puts a gitCommitRecorder in front of f's git client.
func recordGitCommits(f *gitFixture) *gitCommitRecorder {
	r := &gitCommitRecorder{fakeGitClient: f.git}
	f.runtime.Git = r
	return r
}

// Acquire records the commit req asks for, then serves it as the fake does.
func (r *gitCommitRecorder) Acquire(ctx context.Context, req gitsource.Request) (gitsource.Result, error) {
	r.mu.Lock()
	r.commits = append(r.commits, req.Commit)
	r.mu.Unlock()
	return r.fakeGitClient.Acquire(ctx, req)
}

// take returns the commits recorded since the last call, sorted, and forgets
// them; git roots expand concurrently.
func (r *gitCommitRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.commits
	r.commits = nil
	slices.Sort(out)
	return out
}

// lockGitRun runs Lock over f's configuration.
func lockGitRun(f *gitFixture) error {
	return collections.Lock(context.Background(), f.cfg, f.runtime)
}

// goColdGit points f at a new, empty cache and resets the request counts.
func goColdGit(t *testing.T, f *gitFixture) {
	t.Helper()
	f.cfg.CacheDir = filepath.Join(t.TempDir(), "cache")
	f.galaxy.ResetCounts()
	f.git.resetCounts()
}

// sourceLockBytes reads the lockfile lock writes beside reqPath.
func sourceLockBytes(t *testing.T, reqPath string) []byte {
	t.Helper()
	data, err := os.ReadFile(lockfile.ResolveDefaultPath(reqPath, ""))
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	return data
}

// assertSourceLockBytes fails unless the lockfile beside reqPath holds want.
func assertSourceLockBytes(t *testing.T, reqPath string, want []byte) {
	t.Helper()
	if got := sourceLockBytes(t, reqPath); string(got) != string(want) {
		t.Fatalf("lockfile changed:\n--- before\n%s--- after\n%s", want, got)
	}
}

// TestLockKeepsAGitRootWhoseBranchMovedOnAColdCache pins that a git root keeps
// its locked commit once its branch moved: on an empty cache --check and lock
// pass, rewriting nothing, after one advertisement and one fetch each.
func TestLockKeepsAGitRootWhoseBranchMovedOnAColdCache(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	git := recordGitCommits(f)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n")
	f.lockfile(t)
	before := sourceLockBytes(t, f.reqPath)
	locked := fakeCommit("app-1")
	f.git.repos[gitAppURL].refs[gitMainRef] = fakeCommit("app-2")

	for _, check := range []bool{true, false} {
		goColdGit(t, f)
		git.take()
		f.cfg.Check = check
		if err := lockGitRun(f); err != nil {
			t.Fatalf("lock (check %t) on a cold cache after main moved: %v", check, err)
		}
		if got := git.take(); !slices.Equal(got, []string{locked}) {
			t.Fatalf("acquisitions (check %t) = %q, want one by the locked commit %s", check, got, locked)
		}
		if adv, _ := f.git.counts(); adv != 1 {
			t.Fatalf("lock (check %t) advertised %d times, want 1, which finds main moved", check, adv)
		}
		assertSourceLockBytes(t, f.reqPath, before)
	}

	// Positive control: --refresh sets the pin aside and sees main's new commit.
	f.cfg.Check, f.cfg.Refresh = true, true
	if err := lockGitRun(f); !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("lock --check --refresh after main moved: %v, want ErrLockfileDrift", err)
	}
}

// TestLockKeepsALockedGitCommitWhoseBranchIsDeleted pins that a locked commit
// the repository still serves is kept once the branch it was locked from is
// gone: the commit is fetched by itself, with no ref to resolve.
func TestLockKeepsALockedGitCommitWhoseBranchIsDeleted(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	git := recordGitCommits(f)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",dev\n")
	f.lockfile(t)
	before := sourceLockBytes(t, f.reqPath)
	delete(f.git.repos[gitAppURL].refs, "refs/heads/dev")
	locked := fakeCommit("app-2")

	for _, check := range []bool{true, false} {
		goColdGit(t, f)
		git.take()
		f.cfg.Check = check
		if err := lockGitRun(f); err != nil {
			t.Fatalf("lock (check %t) on a cold cache after dev was deleted: %v", check, err)
		}
		if got := git.take(); !slices.Equal(got, []string{locked}) {
			t.Fatalf("acquisitions (check %t) = %q, want one by the locked commit %s", check, got, locked)
		}
		assertSourceLockBytes(t, f.reqPath, before)
	}
}

// TestLockResolvesAGitRootWhoseLockedCommitIsGone pins that a locked commit
// the repository no longer serves warns once per run, naming the root's one
// collection or else its source and ref, and the root is resolved anew.
func TestLockResolvesAGitRootWhoseLockedCommitIsGone(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		warning                               func(display string) string
		requirement, repo, entry, gone, moved string
		collections                           []fakeGitCollection
	}{
		"a root of one collection": {
			requirement: "  - git+" + gitAppURL + ",main\n", repo: gitAppURL, entry: "acme.app",
			gone: fakeCommit("app-1"), moved: fakeCommit("gone-app-2"),
			collections: []fakeGitCollection{{namespace: "acme", name: "app", version: appMovedVersion}},
			warning: func(display string) string {
				return "Locked acme.app: commit " + fakeCommit("app-1") + " is no longer served by " + display +
					"; resolving the collection anew"
			},
		},
		"a root of several collections": {
			requirement: gitFrozenRoot("collections", gitFrozenParentRef), repo: gitMonoURL, entry: "acme.one",
			gone: fakeCommit("mono-1"), moved: fakeCommit("gone-mono-2"),
			collections: []fakeGitCollection{
				{namespace: "acme", name: "one", version: "0.1.1", subdir: "collections/one"},
				{namespace: "acme", name: "two", version: "0.2.1", subdir: "collections/two"},
			},
			warning: func(display string) string {
				return "Locked git source " + display + "@main: commit " + fakeCommit("mono-1") +
					" is no longer served; resolving its collections anew"
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGitFixture(t)
			f.writeRequirements(t, "collections:\n"+tc.requirement)
			f.lockfile(t)
			warning := tc.warning(tc.repo)
			repo := f.git.repos[tc.repo]
			repo.commits[tc.moved] = tc.collections
			repo.refs["HEAD"], repo.refs[gitMainRef] = tc.moved, tc.moved
			delete(repo.commits, tc.gone)

			goColdGit(t, f)
			f.cfg.Check = true
			if err := lockGitRun(f); !errors.Is(err, helpers.ErrLockfileDrift) {
				t.Fatalf("lock --check over a commit no longer served: %v, want ErrLockfileDrift", err)
			}
			if got := warnCount(f.printer, warning); got != 1 {
				t.Fatalf("warnings %q after --check = %d, want 1: %q", warning, got, f.printer.warns)
			}

			goColdGit(t, f)
			f.cfg.Check = false
			if entry := findLockEntry(t, f.lockfile(t), tc.entry); entry.Commit != tc.moved || entry.Ref != gitFrozenParentRef {
				t.Fatalf("%s locked at %s from %q, want %s from main", tc.entry, entry.Commit, entry.Ref, tc.moved)
			}
			if got := warnCount(f.printer, warning); got != 2 {
				t.Fatalf("warnings %q after lock = %d, want 2, one per run: %q", warning, got, f.printer.warns)
			}
		})
	}
}

// TestLockOfflineRefusesAGitRootWhoseLockedCommitIsNotCached pins --offline
// over a cache whose pin install --refresh moved, or that holds none: the
// locked commit is not there to keep, and another pin is not taken instead.
func TestLockOfflineRefusesAGitRootWhoseLockedCommitIsNotCached(t *testing.T) {
	t.Parallel()
	for name, refreshed := range map[string]bool{"a pin at another commit": true, "no pin": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGitFixture(t)
			f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n")
			f.lockfile(t)
			before := sourceLockBytes(t, f.reqPath)
			f.git.repos[gitAppURL].refs[gitMainRef] = fakeCommit("app-2")
			if refreshed {
				f.cfg.Refresh = true
				f.mustInstall(t)
				f.cfg.Refresh = false
			} else {
				goColdGit(t, f)
			}

			f.goOffline()
			err := lockGitRun(f)
			if !errors.Is(err, helpers.ErrOfflineMode) || !strings.Contains(err.Error(), "locked commit "+fakeCommit("app-1")) {
				t.Fatalf("lock --offline without the locked commit: %v, want ErrOfflineMode naming it", err)
			}
			if adv, acq := f.git.counts(); adv != 0 || acq != 0 {
				t.Fatalf("offline lock reached the remote: advertises=%d acquires=%d", adv, acq)
			}
			assertSourceLockBytes(t, f.reqPath, before)
		})
	}
}

// TestLockResolvesAGitCommitRefAsInstallDoes pins that a root asked at a
// commit is not looked up in galaxy.lock, its ref being its pin: the commit is
// acquired by the ref, and --offline on an empty cache fails as install does.
func TestLockResolvesAGitCommitRefAsInstallDoes(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	git := recordGitCommits(f)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+","+fakeCommit("app-2")+"\n")
	f.lockfile(t)
	before := sourceLockBytes(t, f.reqPath)

	goColdGit(t, f)
	git.take()
	f.cfg.Check = true
	if err := lockGitRun(f); err != nil {
		t.Fatalf("lock --check of a commit ref on a cold cache: %v", err)
	}
	if got := git.take(); !slices.Equal(got, []string{gitNoCommit}) {
		t.Fatalf("acquisitions = %q, want one by the commit ref itself, as install makes it", got)
	}
	assertSourceLockBytes(t, f.reqPath, before)

	goColdGit(t, f)
	f.goOffline()
	err := lockGitRun(f)
	if !errors.Is(err, helpers.ErrOfflineMode) || strings.Contains(err.Error(), "locked commit") {
		t.Fatalf("lock --offline of a commit ref on a cold cache: %v, want install's ErrOfflineMode", err)
	}
}

// TestLockResolvesAChangedGitRootAsBefore pins that a git root galaxy.lock no
// longer matches, asked at another ref or from another subdir, resolves as
// install does: its ref's tip on an empty cache, drift, and no warning.
func TestLockResolvesAChangedGitRootAsBefore(t *testing.T) {
	t.Parallel()
	const movedURL = "https://git.example/acme/moved.git"
	for name, tc := range map[string]struct{ locked, changed, entry, want string }{
		"another ref": {
			locked: "  - git+" + gitAppURL + ",main\n", changed: "  - git+" + gitAppURL + ",dev\n",
			entry: "acme.app", want: fakeCommit("app-2"),
		},
		"another subdir": {
			locked:  "  - name: " + movedURL + "#collections/one\n    type: git\n    version: main\n",
			changed: "  - name: " + movedURL + "#ansible/one\n    type: git\n    version: main\n",
			entry:   "acme.one", want: fakeCommit("moved-2"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGitFixture(t)
			first, second := fakeCommit("moved-1"), fakeCommit("moved-2")
			f.git.add(movedURL, &fakeGitRepo{
				refs: map[string]string{"HEAD": first, gitMainRef: first},
				commits: map[string][]fakeGitCollection{
					first:  {{namespace: "acme", name: "one", version: "0.1.0", subdir: "collections/one"}},
					second: {{namespace: "acme", name: "one", version: "0.1.1", subdir: "ansible/one"}},
				},
			})
			git := recordGitCommits(f)
			f.writeRequirements(t, "collections:\n"+tc.locked)
			f.lockfile(t)
			f.git.repos[movedURL].refs[gitMainRef] = second
			f.writeRequirements(t, "collections:\n"+tc.changed)

			goColdGit(t, f)
			git.take()
			f.cfg.Check = true
			if err := lockGitRun(f); !errors.Is(err, helpers.ErrLockfileDrift) {
				t.Fatalf("lock --check after the requirement changed: %v, want ErrLockfileDrift", err)
			}
			if got := git.take(); !slices.Equal(got, []string{gitNoCommit}) {
				t.Fatalf("acquisitions = %q, want one resolving the ref, as install makes it", got)
			}
			f.cfg.Check = false
			if got := findLockEntry(t, f.lockfile(t), tc.entry).Commit; got != tc.want {
				t.Fatalf("%s locked at %s, want %s", tc.entry, got, tc.want)
			}
			if got := warnCount(f.printer, "Locked "); got != 0 {
				t.Fatalf("a changed requirement drew a locked-pin warning: %q", f.printer.warns)
			}
		})
	}
}

// TestLockKeepsTheLockedCommitOfEachOverlappingGitRoot pins that two roots of
// one repository, a parent and a child directory at other refs, named or not,
// each keep their own locked commit once both refs moved.
func TestLockKeepsTheLockedCommitOfEachOverlappingGitRoot(t *testing.T) {
	t.Parallel()
	for name, requirements := range map[string]string{
		"unnamed parent, named child": gitFrozenRoot("collections", gitFrozenParentRef) +
			gitFrozenNamedRoot("acme.three", gitFrozenSiblingSubdir, gitFrozenSiblingRef),
		"named parent, unnamed child": gitFrozenNamedRoot("acme.one", "collections", gitFrozenParentRef) +
			gitFrozenRoot(gitFrozenSiblingSubdir, gitFrozenSiblingRef),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGitFixture(t)
			git := recordGitCommits(f)
			monoAtTwoRefs(f, []fakeGitCollection{
				{namespace: "acme", name: "one", version: "0.1.0", subdir: "collections/one"},
				{namespace: "acme", name: "two", version: "0.2.0", subdir: "collections/two"},
			}, []fakeGitCollection{{namespace: "acme", name: "three", version: "0.3.0", subdir: gitFrozenSiblingSubdir}})
			f.writeRequirements(t, "collections:\n"+requirements)
			f.lockfile(t)
			before := sourceLockBytes(t, f.reqPath)
			want := []string{fakeCommit("mono-shared-main"), fakeCommit("mono-shared-dev")}
			slices.Sort(want)

			repo := f.git.repos[gitMonoURL]
			movedMain, movedDev := fakeCommit("overlap-main-2"), fakeCommit("overlap-dev-2")
			repo.commits[movedMain] = []fakeGitCollection{
				{namespace: "acme", name: "one", version: "0.1.1", subdir: "collections/one"},
				{namespace: "acme", name: "two", version: "0.2.1", subdir: "collections/two"},
			}
			repo.commits[movedDev] = []fakeGitCollection{{namespace: "acme", name: "three", version: "0.3.1", subdir: gitFrozenSiblingSubdir}}
			repo.refs[gitMainRef], repo.refs[gitMonoDevRef] = movedMain, movedDev

			for _, check := range []bool{true, false} {
				goColdGit(t, f)
				git.take()
				f.cfg.Check = check
				if err := lockGitRun(f); err != nil {
					t.Fatalf("lock (check %t) on a cold cache after both refs moved: %v", check, err)
				}
				if got := git.take(); !slices.Equal(got, want) {
					t.Fatalf("acquisitions (check %t) = %q, want the two locked commits %q", check, got, want)
				}
				assertSourceLockBytes(t, f.reqPath, before)
			}
		})
	}
}

// TestLockLeavesTheGitPinInstallRefreshMoved pins that lock keeps galaxy.lock's
// commit over the git pin install --refresh moved without recording it: the
// pin still names main's tip, which the next plain install replays.
func TestLockLeavesTheGitPinInstallRefreshMoved(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	git := recordGitCommits(f)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n")
	f.lockfile(t)
	before := sourceLockBytes(t, f.reqPath)
	locked, moved := fakeCommit("app-1"), fakeCommit("app-2")
	f.git.repos[gitAppURL].refs[gitMainRef] = moved
	f.cfg.Refresh = true
	f.mustInstall(t)
	if got := readManifestVersion(t, f.downloadPath, "app"); got != appMovedVersion {
		t.Fatalf("install --refresh installed acme.app %s, want %s", got, appMovedVersion)
	}

	f.cfg.Refresh = false
	git.take()
	if err := lockGitRun(f); err != nil {
		t.Fatalf("lock over an install --refresh pin: %v", err)
	}
	assertSourceLockBytes(t, f.reqPath, before)
	if got := git.take(); !slices.Equal(got, []string{locked}) {
		t.Fatalf("acquisitions = %q, want one by the locked commit %s", got, locked)
	}
	pin, ok := loadStoreSnapshot(t, f.cfg, f.runtime).GetGitPin(gitsource.PinKey(gitAppURL, "main", ""))
	if !ok || pin.Commit != moved {
		t.Fatalf("git pin after lock = %+v (recorded: %t), want main's tip %s, as install --refresh recorded it", pin, ok, moved)
	}

	f.git.resetCounts()
	f.mustInstall(t)
	if got := readManifestVersion(t, f.downloadPath, "app"); got != appMovedVersion {
		t.Fatalf("install after lock installed acme.app %s, want %s from the recorded pin", got, appMovedVersion)
	}
	if adv, acq := f.git.counts(); adv != 0 || acq != 0 {
		t.Fatalf("install after lock reached the remote: advertises=%d acquires=%d", adv, acq)
	}
}

// goColdURL points f at a new, empty cache directory.
func goColdURL(t *testing.T, f *urlFixture) {
	t.Helper()
	f.cfg.CacheDir = filepath.Join(t.TempDir(), "cache")
}

// lockURLRun runs Lock over f's configuration.
func lockURLRun(f *urlFixture) error {
	return collections.Lock(context.Background(), f.cfg, f.runtime)
}

// serveNewKafkaBytes makes f's tarball URL serve acme.kafka 0.25.0 and
// returns those bytes' sha256 and the warning a lock keeping f's pin prints.
func serveNewKafkaBytes(f *urlFixture) (string, string) {
	data, sha := fakegalaxy.BuildArtifact("acme", "kafka", kafkaNewVersion, map[string]string{"acme.lib": ">=1.0.0"})
	f.galaxy.AddTarball(urlKafkaPath, data)
	return sha, "Locked acme.kafka: sha256 " + f.tarballSHA + " is no longer served by " + f.tarballURL +
		", which now serves " + sha + "; keeping the new bytes"
}

// TestLockKeepsURLBytesThatHoldOnAColdCache pins a url root whose URL still
// serves the locked bytes: on an empty cache one download per run, no
// rewrite and no warning.
func TestLockKeepsURLBytesThatHoldOnAColdCache(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
	f.lockfile(t)
	before := sourceLockBytes(t, f.reqPath)

	for _, check := range []bool{true, false} {
		goColdURL(t, f)
		downloads := f.galaxy.Count(fakegalaxy.EndpointTarball)
		f.cfg.Check = check
		if err := lockURLRun(f); err != nil {
			t.Fatalf("lock (check %t) on a cold cache: %v", check, err)
		}
		if got := f.galaxy.Count(fakegalaxy.EndpointTarball) - downloads; got != 1 {
			t.Fatalf("lock (check %t) downloaded %d times, want 1", check, got)
		}
		assertSourceLockBytes(t, f.reqPath, before)
	}
	if got := warnCount(f.printer, "Locked "); got != 0 {
		t.Fatalf("unchanged bytes drew a locked-pin warning: %q", f.printer.warns)
	}
}

// TestLockTakesTheNewBytesOfAURLRoot pins a url root whose URL now serves
// other bytes: one warning per run, --check reports the drift, and lock
// writes the new bytes' sha256 and version.
func TestLockTakesTheNewBytesOfAURLRoot(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
	f.lockfile(t)
	newSHA, warning := serveNewKafkaBytes(f)

	goColdURL(t, f)
	f.cfg.Check = true
	if err := lockURLRun(f); !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("lock --check over other bytes: %v, want ErrLockfileDrift", err)
	}
	if got := warnCount(f.printer, warning); got != 1 {
		t.Fatalf("warnings %q after --check = %d, want 1: %q", warning, got, f.printer.warns)
	}

	goColdURL(t, f)
	f.cfg.Check = false
	if entry := findLockEntry(t, f.lockfile(t), "acme.kafka"); entry.SHA256 != newSHA || entry.Version != kafkaNewVersion {
		t.Fatalf("url entry = %+v, want sha256 %s at %s", entry, newSHA, kafkaNewVersion)
	}
	if got := warnCount(f.printer, warning); got != 2 {
		t.Fatalf("warnings %q after lock = %d, want 2, one per run: %q", warning, got, f.printer.warns)
	}
}

// TestLockDownloadsPastAURLPinOfOtherBytes pins that the url pin install
// --refresh recorded for new bytes is not replayed under the lock: the URL is
// downloaded again, the new bytes warn, and --check reports the drift.
func TestLockDownloadsPastAURLPinOfOtherBytes(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
	f.lockfile(t)
	_, warning := serveNewKafkaBytes(f)
	f.cfg.Refresh = true
	f.mustInstall(t)

	f.cfg.Refresh, f.cfg.Check = false, true
	downloads := f.galaxy.Count(fakegalaxy.EndpointTarball)
	if err := lockURLRun(f); !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("lock --check over an install --refresh pin of other bytes: %v, want ErrLockfileDrift", err)
	}
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball) - downloads; got != 1 {
		t.Fatalf("lock --check downloaded %d times, want 1: a pin of other bytes is not replayed", got)
	}
	if got := warnCount(f.printer, warning); got != 1 {
		t.Fatalf("warnings %q = %d, want 1: %q", warning, got, f.printer.warns)
	}
}

// TestLockOfflineRefusesAURLRootWhoseLockedBytesAreNotCached pins --offline
// over a cache install --refresh moved to other bytes: the locked sha256 is
// not recorded, and the recorded one is not taken instead.
func TestLockOfflineRefusesAURLRootWhoseLockedBytesAreNotCached(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
	f.lockfile(t)
	before := sourceLockBytes(t, f.reqPath)
	serveNewKafkaBytes(f)
	f.cfg.Refresh = true
	f.mustInstall(t)

	f.cfg.Refresh, f.cfg.Offline = false, true
	f.runtime.HTTP = fetch.NewOffline(e2eTimeout)
	f.runtime.URLHTTP = fetch.NewURLDownload(e2eTimeout, true, nil)
	downloads := f.galaxy.Count(fakegalaxy.EndpointTarball)
	err := lockURLRun(f)
	if !errors.Is(err, helpers.ErrOfflineMode) || !strings.Contains(err.Error(), "locked sha256 "+f.tarballSHA) {
		t.Fatalf("lock --offline without the locked bytes: %v, want ErrOfflineMode naming them", err)
	}
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != downloads {
		t.Fatalf("offline lock reached the origin: downloads = %d, want %d", got, downloads)
	}
	assertSourceLockBytes(t, f.reqPath, before)
}

// TestLockResolvesAChangedURLVersionAsBefore pins that a url root asserting
// another version than galaxy.lock's resolves as install does: the recorded
// pin is asserted against it, an empty cache takes the new bytes, no warning.
func TestLockResolvesAChangedURLVersionAsBefore(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	asking := func(version string) string {
		return "collections:\n  - name: " + f.tarballURL + "\n    type: url\n    version: " + version + "\n"
	}
	f.writeRequirements(t, asking(urlKafkaVersion))
	f.lockfile(t)
	newSHA, _ := serveNewKafkaBytes(f)
	f.writeRequirements(t, asking(kafkaNewVersion))

	if err := lockURLRun(f); !errors.Is(err, helpers.ErrURLCollectionVersionMismatch) {
		t.Fatalf("lock over the pin of 0.24.0 asked for 0.25.0: %v, want ErrURLCollectionVersionMismatch", err)
	}

	goColdURL(t, f)
	f.cfg.Check = true
	if err := lockURLRun(f); !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("lock --check after the version changed: %v, want ErrLockfileDrift", err)
	}
	goColdURL(t, f)
	f.cfg.Check = false
	if entry := findLockEntry(t, f.lockfile(t), "acme.kafka"); entry.SHA256 != newSHA || entry.Version != kafkaNewVersion {
		t.Fatalf("url entry = %+v, want sha256 %s at %s", entry, newSHA, kafkaNewVersion)
	}
	if got := warnCount(f.printer, "Locked "); got != 0 {
		t.Fatalf("a changed version drew a locked-pin warning: %q", f.printer.warns)
	}
}

// TestLockKeepsAParentGitRootBesideAChildRootAtItsRef pins that a child root
// added at a moved branch beside a locked parent leaves the parent at its
// commit, and that later locks keep both, as each owns its own entries.
func TestLockKeepsAParentGitRootBesideAChildRootAtItsRef(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	git := recordGitCommits(f)
	monoAtTwoRefs(f, []fakeGitCollection{{namespace: "acme", name: "one", version: "0.1.0", subdir: "collections/one"}}, nil)
	parent := gitFrozenRoot("collections", gitFrozenParentRef)
	f.writeRequirements(t, "collections:\n"+parent)
	f.lockfile(t)

	repo, locked, moved := f.git.repos[gitMonoURL], fakeCommit("mono-shared-main"), fakeCommit("child-main-2")
	repo.commits[moved] = []fakeGitCollection{
		{namespace: "acme", name: "one", version: "0.1.1", subdir: "collections/one"},
		{namespace: "acme", name: "three", version: "0.3.0", subdir: gitFrozenSiblingSubdir},
	}
	repo.refs[gitMainRef] = moved
	f.writeRequirements(t, "collections:\n"+parent+gitFrozenRoot(gitFrozenSiblingSubdir, gitFrozenParentRef))
	goColdGit(t, f)
	lf := f.lockfile(t)
	if one, three := findLockEntry(t, lf, "acme.one"), findLockEntry(t, lf, "acme.three"); one.Commit != locked || three.Commit != moved {
		t.Fatalf("acme.one locked at %s and acme.three at %s, want %s and %s", one.Commit, three.Commit, locked, moved)
	}
	before := sourceLockBytes(t, f.reqPath)

	goColdGit(t, f)
	git.take()
	f.cfg.Check = true
	if err := lockGitRun(f); err != nil {
		t.Fatalf("lock --check over the parent's and the child's pins: %v", err)
	}
	want := []string{locked, moved}
	slices.Sort(want)
	if got := git.take(); !slices.Equal(got, want) {
		t.Fatalf("acquisitions = %q, want each root's locked commit %q", got, want)
	}
	assertSourceLockBytes(t, f.reqPath, before)
	f.cfg.Check, f.cfg.Frozen = false, true
	if err := f.install(t); err != nil {
		t.Fatalf("install --frozen over what lock wrote: %v", err)
	}
}

// gitCacheStates are the caches a lock that must move a kept git commit runs
// on: an empty one, the one that locked it, and that one under --no-cache.
func gitCacheStates() map[string]func(t *testing.T, f *gitFixture) {
	return map[string]func(t *testing.T, f *gitFixture){
		"empty cache": goColdGit,
		"warm cache":  func(*testing.T, *gitFixture) {},
		"--no-cache":  func(_ *testing.T, f *gitFixture) { f.cfg.NoCache = true },
	}
}

// TestLockResolvesAGitRootWhoseLockedCommitHoldsACollectionAskedElsewhere pins
// that a git root kept at a commit holding a collection the requirements now
// take from Galaxy resolves anew, with one warning per run, on any cache.
func TestLockResolvesAGitRootWhoseLockedCommitHoldsACollectionAskedElsewhere(t *testing.T) {
	t.Parallel()
	for name, prepare := range gitCacheStates() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGitFixture(t)
			repo := f.git.repos[gitMonoURL]
			locked, moved := fakeCommit("moved-out-1"), fakeCommit("moved-out-2")
			repo.commits[locked] = []fakeGitCollection{
				{namespace: "acme", name: "one", version: "0.1.0", subdir: "collections/one"},
				{namespace: "acme", name: "lib", version: "0.5.0", subdir: "collections/lib"},
			}
			repo.refs[gitMainRef] = locked
			root := gitFrozenRoot("collections", gitFrozenParentRef)
			f.writeRequirements(t, "collections:\n"+root)
			f.lockfile(t)
			repo.commits[moved] = []fakeGitCollection{{namespace: "acme", name: "one", version: "0.1.1", subdir: "collections/one"}}
			repo.refs[gitMainRef] = moved
			f.writeRequirements(t, "collections:\n"+root+"  - acme.lib\n")
			prepare(t, f)
			warning := "Locked git source " + gitMonoURL + "@main: the requirements rule out commit " + locked +
				"; resolving its collections anew"

			f.cfg.Check = true
			if err := lockGitRun(f); !errors.Is(err, helpers.ErrLockfileDrift) {
				t.Fatalf("lock --check over a commit holding acme.lib: %v, want ErrLockfileDrift", err)
			}
			f.cfg.Check = false
			lf := f.lockfile(t)
			if one := findLockEntry(t, lf, "acme.one"); one.Commit != moved {
				t.Fatalf("acme.one locked at %s, want main's new commit %s", one.Commit, moved)
			}
			if lib := findLockEntry(t, lf, "acme.lib"); lib.IsGit() || lib.Version != e2eVersion200 {
				t.Fatalf("acme.lib locked as %+v, want the Galaxy release 2.0.0", lib)
			}
			if got := warnCount(f.printer, warning); got != 2 {
				t.Fatalf("warnings %q = %d, want 2, one per run: %q", warning, got, f.printer.warns)
			}
		})
	}
}

// conflictRepoURL is a repository whose acme.app needs acme.lib below 2.0.0
// at conflict-1 and at least 2.0.0 at conflict-2.
const conflictRepoURL = "https://git.example/acme/conflict.git"

// addConflictRepo registers conflictRepoURL with main at conflict-1.
func addConflictRepo(f *gitFixture) {
	first, second := fakeCommit("conflict-1"), fakeCommit("conflict-2")
	f.git.add(conflictRepoURL, &fakeGitRepo{
		refs: map[string]string{"HEAD": first, gitMainRef: first},
		commits: map[string][]fakeGitCollection{
			first:  {{namespace: "acme", name: "app", version: appLockedVersion, deps: map[string]string{"acme.lib": ">=1.0.0,<2.0.0"}}},
			second: {{namespace: "acme", name: "app", version: appMovedVersion, deps: map[string]string{"acme.lib": ">=2.0.0"}}},
		},
	})
}

// TestLockResolvesAGitRootWhoseLockedCommitConflicts pins that a git root kept
// at a commit whose dependencies the tightened requirements rule out resolves
// anew, with one warning per run, on any cache.
func TestLockResolvesAGitRootWhoseLockedCommitConflicts(t *testing.T) {
	t.Parallel()
	for name, prepare := range gitCacheStates() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGitFixture(t)
			addConflictRepo(f)
			f.writeRequirements(t, "collections:\n  - git+"+conflictRepoURL+",main\n")
			f.lockfile(t)
			locked, moved := fakeCommit("conflict-1"), fakeCommit("conflict-2")
			f.git.repos[conflictRepoURL].refs[gitMainRef] = moved
			f.writeRequirements(t, "collections:\n  - git+"+conflictRepoURL+",main\n  - name: acme.lib\n    version: \">=2.0.0\"\n")
			prepare(t, f)
			warning := "Locked acme.app: the requirements rule out commit " + locked + "; resolving the collection anew"

			f.cfg.Check = true
			if err := lockGitRun(f); !errors.Is(err, helpers.ErrLockfileDrift) {
				t.Fatalf("lock --check over a conflicting commit: %v, want ErrLockfileDrift", err)
			}
			f.cfg.Check = false
			lf := f.lockfile(t)
			if app := findLockEntry(t, lf, "acme.app"); app.Commit != moved || app.Version != appMovedVersion {
				t.Fatalf("acme.app locked as %+v, want %s at %s", app, appMovedVersion, moved)
			}
			if lib := findLockEntry(t, lf, "acme.lib"); lib.Version != e2eVersion200 {
				t.Fatalf("acme.lib locked at %s, want 2.0.0", lib.Version)
			}
			if got := warnCount(f.printer, warning); got != 2 {
				t.Fatalf("warnings %q = %d, want 2, one per run: %q", warning, got, f.printer.warns)
			}
		})
	}
}

// TestLockKeepsAGitRootOutsideAConflict is the control: a conflict the kept
// commit takes no part in fails as it would without galaxy.lock, exit 3, and
// releases nothing.
func TestLockKeepsAGitRootOutsideAConflict(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	git := recordGitCommits(f)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n")
	f.lockfile(t)
	f.git.repos[gitAppURL].refs[gitMainRef] = fakeCommit("app-2")
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n  - acme.absent\n")
	goColdGit(t, f)
	git.take()

	err := lockGitRun(f)
	if !errors.Is(err, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("lock with a requirement no server has: %v, want ErrNoVersionSatisfiesConstraints", err)
	}
	if got := git.take(); !slices.Equal(got, []string{fakeCommit("app-1")}) {
		t.Fatalf("acquisitions = %q, want the locked commit alone, never released", got)
	}
	if got := warnCount(f.printer, "Locked "); got != 0 {
		t.Fatalf("a conflict without the kept commit drew a locked-pin warning: %q", f.printer.warns)
	}
}

// newAllSourcesFixture is the role fixture serving, beside its git sources, a
// url collection's tarball and a url role's, and requiring a git and a url
// collection, a url and a git role.
func newAllSourcesFixture(t *testing.T) *roleFixture {
	t.Helper()
	f := newRoleFixture(t)
	f.runtime.URLHTTP = fetch.NewURLDownload(e2eTimeout, false, nil)
	data, _ := fakegalaxy.BuildArtifact("acme", "kafka", urlKafkaVersion, map[string]string{"acme.lib": ">=1.0.0"})
	kafkaURL := f.galaxy.AddTarball(urlKafkaPath, data)
	roleData, _ := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=held\n"})
	roleURL := f.galaxy.AddTarball("dl/held.tar.gz", roleData)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n  - "+kafkaURL+"\n"+
		"roles:\n  - src: "+roleURL+"\n    name: held\n  - src: git+"+roleAppURL+"\n    name: app\n")
	return f
}

// TestLockOverAnAgreeingCacheReplaysEverySource pins that a lock over the
// cache that recorded galaxy.lock's pins replays every one of them: no git
// fetch, no tarball download, and a store left unsaved.
func TestLockOverAnAgreeingCacheReplaysEverySource(t *testing.T) {
	t.Parallel()
	f := newAllSourcesFixture(t)
	before := lockRoleBytes(t, f)
	stamp := reloadLastSnapshot(t, f.cfg.CacheDir)
	f.git.resetCounts()
	roleAcquires, downloads := f.git.roleAcquireCount(), f.galaxy.Count(fakegalaxy.EndpointTarball)

	f.lockfile(t)
	assertRoleLockBytes(t, f, before)
	if adv, acq := f.git.counts(); adv != 0 || acq != 0 || f.git.roleAcquireCount() != roleAcquires {
		t.Fatalf("a warm lock reached a remote: advertises=%d acquires=%d role acquires=%d",
			adv, acq, f.git.roleAcquireCount()-roleAcquires)
	}
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != downloads {
		t.Fatalf("a warm lock downloaded %d tarballs, want none", got-downloads)
	}
	if got := reloadLastSnapshot(t, f.cfg.CacheDir); !got.Equal(stamp) {
		t.Fatalf("LastSnapshot after an idle lock = %v, want unchanged %v", got, stamp)
	}
}

// TestLockNoCacheDownloadsLockedURLSourcesAgain pins that --no-cache reads no
// recorded url pin while still keeping galaxy.lock's: the url collection and
// the url role are each downloaded once more, and the file is unchanged.
func TestLockNoCacheDownloadsLockedURLSourcesAgain(t *testing.T) {
	t.Parallel()
	f := newAllSourcesFixture(t)
	before := lockRoleBytes(t, f)
	downloads := f.galaxy.Count(fakegalaxy.EndpointTarball)
	f.cfg.NoCache = true

	f.lockfile(t)
	assertRoleLockBytes(t, f, before)
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball) - downloads; got != 2 {
		t.Fatalf("lock --no-cache downloaded %d tarballs, want 2, one per url source", got)
	}
}
