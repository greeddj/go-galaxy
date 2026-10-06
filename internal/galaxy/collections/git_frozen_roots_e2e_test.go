package collections_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// The ref each parent root below asks for, its sibling's ref and branch, the
// subdir of the sibling beside an unnamed parent, and how many frozen installs
// the named-root test takes: that check once took its verdict from map order.
const (
	gitFrozenParentRef     = "main"
	gitFrozenSiblingRef    = "dev"
	gitMonoDevRef          = "refs/heads/" + gitFrozenSiblingRef
	gitFrozenSiblingSubdir = "collections/three"
	gitFrozenSiblingRuns   = 100
)

// TestGitFrozenNamedRootBesideASiblingRoot drives lock and install --frozen
// over a root naming acme.one at collections from main beside a root at
// collections/two from dev: lock keeps both refs and every frozen run installs.
func TestGitFrozenNamedRootBesideASiblingRoot(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.git.repos[gitMonoURL].refs[gitMonoDevRef] = fakeCommit("mono-1")
	f.writeRequirements(t, "collections:\n"+
		gitFrozenNamedRoot("acme.one", "collections", gitFrozenParentRef)+gitFrozenRoot("collections/two", gitFrozenSiblingRef))
	lf := f.lockfile(t)
	one, two := findLockEntry(t, lf, "acme.one"), findLockEntry(t, lf, "acme.two")
	if one.Ref != gitFrozenParentRef || two.Ref != gitFrozenSiblingRef {
		t.Fatalf("locked refs: acme.one %q, acme.two %q, want %q and %q",
			one.Ref, two.Ref, gitFrozenParentRef, gitFrozenSiblingRef)
	}

	f.cfg.Frozen = true
	for run := range gitFrozenSiblingRuns {
		if err := f.install(t); err != nil {
			t.Fatalf("frozen run %d: %v", run, err)
		}
	}
	assertManifestInstalled(t, f.downloadPath, "one")
	assertManifestInstalled(t, f.downloadPath, "two")
	if adv, acq := f.git.counts(); adv != 0 || acq != 2 {
		t.Fatalf("lock then frozen runs: advertises=%d acquires=%d, want 0 and 2 (lock alone)", adv, acq)
	}
}

// TestGitFrozenUnnamedRootBesideASiblingRoot drives lock, install --frozen and
// warm --frozen over an unnamed root at collections from main beside a root at
// collections/three from dev, which main lacks: no frozen run refuses the file.
func TestGitFrozenUnnamedRootBesideASiblingRoot(t *testing.T) {
	t.Parallel()
	parent := gitFrozenRoot("collections", gitFrozenParentRef)
	cases := map[string]struct {
		sibling string
		own     []string
		baseDir bool
	}{
		"unnamed sibling": {sibling: gitFrozenRoot(gitFrozenSiblingSubdir, gitFrozenSiblingRef), own: []string{"one", "two"}},
		"sibling naming acme.three": {
			sibling: gitFrozenNamedRoot("acme.three", gitFrozenSiblingSubdir, gitFrozenSiblingRef), own: []string{"one", "two"},
		},
		"parent directory is itself a collection": {
			sibling: gitFrozenRoot(gitFrozenSiblingSubdir, gitFrozenSiblingRef), own: []string{"base"}, baseDir: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := lockBesideASibling(t, "collections:\n"+parent+tc.sibling, tc.own, tc.baseDir)
			assertFrozenRunsInstall(t, f, slices.Concat(tc.own, []string{"three"}))
		})
	}
}

// gitFrozenRoot renders a requirement of gitMonoURL at subdir from ref, and
// gitFrozenNamedRoot one that names fqdn there.
func gitFrozenRoot(subdir, ref string) string {
	return "  - name: " + gitMonoURL + "#" + subdir + "\n    type: git\n    version: " + ref + "\n"
}

func gitFrozenNamedRoot(fqdn, subdir, ref string) string {
	return "  - name: " + fqdn + "\n    type: git\n    source: " + gitMonoURL + "#" + subdir + "\n    version: " + ref + "\n"
}

// lockBesideASibling locks requirements once dev holds acme.three at
// gitFrozenSiblingSubdir (and main a collection at collections, when baseDir),
// requiring the parent's own collections and acme.three locked from their refs.
func lockBesideASibling(t *testing.T, requirements string, own []string, baseDir bool) *gitFixture {
	t.Helper()
	f := newGitFixture(t)
	repo := f.git.repos[gitMonoURL]
	dev := fakeCommit("mono-dev")
	repo.commits[dev] = []fakeGitCollection{{namespace: "acme", name: "three", version: "0.3.0", subdir: gitFrozenSiblingSubdir}}
	repo.refs[gitMonoDevRef] = dev
	if baseDir {
		base := fakeCommit("mono-base")
		repo.commits[base] = []fakeGitCollection{{namespace: "acme", name: "base", version: "0.4.0", subdir: "collections"}}
		repo.refs[gitMainRef] = base
	}
	f.writeRequirements(t, requirements)
	lf := f.lockfile(t)
	for _, name := range own {
		if e := findLockEntry(t, lf, "acme."+name); e.Ref != gitFrozenParentRef {
			t.Fatalf("acme.%s locked from ref %q, want %q", name, e.Ref, gitFrozenParentRef)
		}
	}
	if e := findLockEntry(t, lf, "acme.three"); e.Ref != gitFrozenSiblingRef || e.Subdir != gitFrozenSiblingSubdir {
		t.Fatalf("acme.three locked from ref %q at %q, want %q at %q", e.Ref, e.Subdir, gitFrozenSiblingRef, gitFrozenSiblingSubdir)
	}
	return f
}

// assertFrozenRunsInstall runs install --frozen and warm --frozen over what f
// locked from two roots and requires each acme.<name> of installed in place,
// with no git call past the two acquisitions lock made.
func assertFrozenRunsInstall(t *testing.T, f *gitFixture, installed []string) {
	t.Helper()
	f.cfg.Frozen = true
	if err := f.install(t); err != nil {
		t.Fatalf("install --frozen: %v", err)
	}
	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("warm --frozen: %v", err)
	}
	for _, name := range installed {
		assertManifestInstalled(t, f.downloadPath, name)
	}
	if adv, acq := f.git.counts(); adv != 0 || acq != 2 {
		t.Fatalf("lock then frozen runs: advertises=%d acquires=%d, want 0 and 2 (lock alone)", adv, acq)
	}
}

// monoAtTwoRefs points main and dev of gitMonoURL at two new commits holding
// the given collections.
func monoAtTwoRefs(f *gitFixture, main, dev []fakeGitCollection) {
	repo := f.git.repos[gitMonoURL]
	mainCommit, devCommit := fakeCommit("mono-shared-main"), fakeCommit("mono-shared-dev")
	repo.commits[mainCommit], repo.commits[devCommit] = main, dev
	repo.refs[gitMainRef], repo.refs[gitMonoDevRef] = mainCommit, devCommit
}

// TestGitFrozenRootsSharingADirectory drives lock, install --frozen and warm
// --frozen over a root at collections from main beside one at collections/three
// from dev, where that directory changes between the refs: no run refuses.
func TestGitFrozenRootsSharingADirectory(t *testing.T) {
	t.Parallel()
	one := fakeGitCollection{namespace: "acme", name: "one", version: "0.1.0", subdir: "collections/one"}
	three := fakeGitCollection{namespace: "acme", name: "three", version: "0.3.0", subdir: gitFrozenSiblingSubdir}
	split := []fakeGitCollection{
		{namespace: "acme", name: "a", version: "1.0.0", subdir: gitFrozenSiblingSubdir + "/a"},
		{namespace: "acme", name: "b", version: "1.0.0", subdir: gitFrozenSiblingSubdir + "/b"},
	}
	child := gitFrozenRoot(gitFrozenSiblingSubdir, gitFrozenSiblingRef)
	cases := map[string]struct {
		parent    string
		main, dev []fakeGitCollection
		installed []string
	}{
		"child renamed between the refs": {
			parent: gitFrozenRoot("collections", gitFrozenParentRef),
			main: []fakeGitCollection{
				one, {namespace: "acme", name: "trois", version: "0.3.0", subdir: gitFrozenSiblingSubdir},
			},
			dev: []fakeGitCollection{three}, installed: []string{"one", "trois", "three"},
		},
		"child split into children at the other ref": {
			parent: gitFrozenRoot("collections", gitFrozenParentRef),
			main:   []fakeGitCollection{one, three}, dev: split, installed: []string{"one", "three", "a", "b"},
		},
		"parent naming the child's collection": {
			parent: gitFrozenNamedRoot("acme.three", "collections", gitFrozenParentRef),
			main:   []fakeGitCollection{one, three}, dev: split, installed: []string{"three", "a", "b"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGitFixture(t)
			monoAtTwoRefs(f, tc.main, tc.dev)
			f.writeRequirements(t, "collections:\n"+tc.parent+child)
			if lf := f.lockfile(t); len(lf.Collections) != len(tc.installed) {
				t.Fatalf("lock wrote %d entries, want %d", len(lf.Collections), len(tc.installed))
			}
			assertFrozenRunsInstall(t, f, tc.installed)
		})
	}
}

// TestGitFrozenRefusesAChangedRef locks two requirements of gitMonoURL, then
// changes one's ref, alone or with a new root taking the entry it locked at
// the old ref: install --frozen refuses either before any git call.
func TestGitFrozenRefusesAChangedRef(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		locked, changed     string
		wantMessageContains string
		main, dev           []fakeGitCollection
	}{
		"child moved to the parent's ref": {
			main: []fakeGitCollection{{namespace: "acme", name: "one", version: "0.1.0", subdir: "collections/one"}},
			dev:  []fakeGitCollection{{namespace: "acme", name: "three", version: "0.3.0", subdir: gitFrozenSiblingSubdir}},
			locked: gitFrozenRoot("collections", gitFrozenParentRef) +
				gitFrozenRoot(gitFrozenSiblingSubdir, gitFrozenSiblingRef),
			changed: gitFrozenRoot("collections", gitFrozenParentRef) +
				gitFrozenRoot(gitFrozenSiblingSubdir, gitFrozenParentRef),
			wantMessageContains: `locked from ref "dev", requirements ask for "main"`,
		},
		"parent moved, a new root at the old ref taking its entry": {
			main: []fakeGitCollection{{namespace: "acme", name: "x", version: "1.0.0", subdir: "collections/x"}},
			dev: []fakeGitCollection{
				{namespace: "acme", name: "s", version: "2.0.0", subdir: "collections"},
				{namespace: "acme", name: "z", version: "2.0.0", subdir: "collections/z"},
			},
			locked: gitFrozenRoot("collections", gitFrozenParentRef) + gitFrozenRoot("collections/z", gitFrozenSiblingRef),
			changed: gitFrozenRoot("collections", gitFrozenSiblingRef) + gitFrozenRoot("collections/x", gitFrozenParentRef) +
				gitFrozenRoot("collections/z", gitFrozenSiblingRef),
			wantMessageContains: "has no lockfile entry",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGitFixture(t)
			monoAtTwoRefs(f, tc.main, tc.dev)
			f.writeRequirements(t, "collections:\n"+tc.locked)
			f.lockfile(t)
			f.cfg.Frozen = true
			if err := f.install(t); err != nil {
				t.Fatalf("install --frozen over the requirements lock wrote it for: %v", err)
			}
			f.writeRequirements(t, "collections:\n"+tc.changed)
			err := f.install(t)
			if !errors.Is(err, helpers.ErrLockfileMismatch) || !strings.Contains(err.Error(), tc.wantMessageContains) {
				t.Fatalf("install --frozen after the ref change: %v, want ErrLockfileMismatch with %q", err, tc.wantMessageContains)
			}
			if adv, acq := f.git.counts(); adv != 0 || acq != 2 {
				t.Fatalf("lock then frozen runs: advertises=%d acquires=%d, want 0 and 2 (lock alone)", adv, acq)
			}
		})
	}
}
