package collections

import (
	"errors"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// The repository the entries below are locked from, another one, two commits,
// and how often each verdict is taken: Go starts every range over a map at a
// random point, so 200 runs leave a map-order miss practically no chance.
const (
	frozenRootRepoURL   = "https://git.example/acme/mono.git"
	frozenRootOtherURL  = "https://git.example/acme/other.git"
	frozenRootOneCommit = "1111111111111111111111111111111111111111"
	frozenRootTwoCommit = "2222222222222222222222222222222222222222"
	frozenRootRuns      = 200
)

// The verdicts the frozen check reads out for frozenRootRepoURL: a root asking
// for main judged by an entry from dev, the reverse, and a root with no entry.
const (
	frozenRootDevForMain = "lockfile does not match resolved requirements: git root " + frozenRootRepoURL +
		` locked from ref "dev", requirements ask for "main"`
	frozenRootMainForDev = "lockfile does not match resolved requirements: git root " + frozenRootRepoURL +
		` locked from ref "main", requirements ask for "dev"`
	frozenRootNoEntry = "lockfile does not match resolved requirements: git root " + frozenRootRepoURL +
		" has no lockfile entry"
)

// frozenRootEntry is a git entry of frozenRootRepoURL at subdir, locked from
// ref at commit.
func frozenRootEntry(fqdn, subdir, ref, commit string) lockfile.Entry {
	return lockfile.Entry{
		Name: fqdn, Type: lockfile.TypeGit, Version: "1.0.0",
		Source: frozenRootRepoURL, Ref: ref, Commit: commit, Subdir: subdir,
	}
}

// frozenGitRoot is a git root of frozenRootRepoURL at subdir as prepareRoots
// leaves it: named when fqdn is set, unnamed when it is empty.
func frozenGitRoot(t *testing.T, fqdn, subdir, ref string) collection {
	t.Helper()
	root := collection{
		Source:     gitsource.Locator{URL: frozenRootRepoURL, Subdir: subdir}.String(),
		Constraint: ref,
		Type:       typeGit,
		Ref:        ref,
	}
	if fqdn == "" {
		return root
	}
	namespace, name, ok := helpers.SplitFQDN(fqdn)
	if !ok {
		t.Fatalf("fixture fqdn %q does not split", fqdn)
	}
	root.Namespace, root.Name = namespace, name
	return root
}

// frozenOtherRepoRoot is frozenGitRoot moved to frozenRootOtherURL.
func frozenOtherRepoRoot(t *testing.T, fqdn, subdir, ref string) collection {
	t.Helper()
	root := frozenGitRoot(t, fqdn, subdir, ref)
	root.Source = gitsource.Locator{URL: frozenRootOtherURL, Subdir: subdir}.String()
	return root
}

// frozenRootIndex keys entries by name, as indexLockfile does.
func frozenRootIndex(entries ...lockfile.Entry) map[string]lockfile.Entry {
	byFQDN := make(map[string]lockfile.Entry, len(entries))
	for _, e := range entries {
		byFQDN[e.Name] = e
	}
	return byFQDN
}

// assertFrozenRootsVerdict takes the frozen root check frozenRootRuns times
// and requires one verdict every time: no error when want is empty, else an
// ErrLockfileMismatch reading exactly want.
func assertFrozenRootsVerdict(t *testing.T, roots []collection, byFQDN map[string]lockfile.Entry, want string) {
	t.Helper()
	for run := range frozenRootRuns {
		err := verifyRootsAgainstLockfile(roots, byFQDN)
		switch {
		case want == "" && err != nil:
			t.Fatalf("run %d: %v, want no error", run, err)
		case want == "":
		case !errors.Is(err, helpers.ErrLockfileMismatch) || err.Error() != want:
			t.Fatalf("run %d: %v, want ErrLockfileMismatch reading %q", run, err, want)
		}
	}
}

// TestFrozenNamedGitRootBesideASiblingRoot pins the shape prepareRoots allows
// and lock writes: acme.one named at collections from main, beside a root at
// collections/two from dev. Each root passes, whatever the map order.
func TestFrozenNamedGitRootBesideASiblingRoot(t *testing.T) {
	t.Parallel()
	byFQDN := frozenRootIndex(
		frozenRootEntry("acme.one", "collections/one", "main", frozenRootOneCommit),
		frozenRootEntry("acme.two", "collections/two", "dev", frozenRootTwoCommit),
	)
	roots := []collection{
		frozenGitRoot(t, "acme.one", "collections", "main"),
		frozenGitRoot(t, "", "collections/two", "dev"),
	}
	assertFrozenRootsVerdict(t, roots, byFQDN, "")
}

// TestFrozenNamedGitRootRefusals pins what a named root is held to: its own
// entry, locked from its repository under its subdir at its ref. No other
// entry stands in, and a root left with no entry is refused naming acme.one.
func TestFrozenNamedGitRootRefusals(t *testing.T) {
	t.Parallel()
	const noEntry = frozenRootNoEntry + " for acme.one"
	sibling := frozenRootEntry("acme.two", "collections/two", "main", frozenRootTwoCommit)
	elsewhere := frozenRootEntry("acme.one", "collections/one", "main", frozenRootOneCommit)
	elsewhere.Source = frozenRootOtherURL
	fromGalaxy := lockfile.Entry{Name: "acme.one", Version: "1.0.0", Source: "https://galaxy.example"}
	lib := lockfile.Entry{Name: "acme.lib", Version: "1.0.0", Source: "https://galaxy.example"}
	cases := map[string]struct {
		want    string
		entries []lockfile.Entry
	}{
		"own entry from another ref": {entries: []lockfile.Entry{
			frozenRootEntry("acme.one", "collections/one", "dev", frozenRootOneCommit), sibling,
		}, want: frozenRootDevForMain},
		"own entry missing":                 {entries: []lockfile.Entry{sibling}, want: noEntry},
		"own entry from another repository": {entries: []lockfile.Entry{elsewhere, sibling}, want: noEntry},
		"own entry outside the subdir": {entries: []lockfile.Entry{
			frozenRootEntry("acme.one", "other/one", "main", frozenRootOneCommit), sibling,
		}, want: noEntry},
		"own entry from Galaxy":       {entries: []lockfile.Entry{fromGalaxy, sibling}, want: noEntry},
		"nothing from its repository": {entries: []lockfile.Entry{elsewhere, lib}, want: noEntry},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			roots := []collection{frozenGitRoot(t, "acme.one", "collections", "main")}
			assertFrozenRootsVerdict(t, roots, frozenRootIndex(tc.entries...), tc.want)
		})
	}
}

// TestFrozenUnnamedGitRootJudgedByEveryEntry pins an unnamed root with no other
// root beside it: every entry locked from its repository under its subdir
// carries its ref, at least one exists, and the first refusal by name is reported.
func TestFrozenUnnamedGitRootJudgedByEveryEntry(t *testing.T) {
	t.Parallel()
	elsewhere := frozenRootEntry("acme.one", "collections/one", "main", frozenRootOneCommit)
	elsewhere.Source = frozenRootOtherURL
	cases := map[string]struct {
		want    string
		entries []lockfile.Entry
	}{
		"every entry from its ref": {entries: []lockfile.Entry{
			frozenRootEntry("acme.one", "collections/one", "main", frozenRootOneCommit),
			frozenRootEntry("acme.two", "collections/two", "main", frozenRootOneCommit),
		}},
		"two entries from other refs": {entries: []lockfile.Entry{
			frozenRootEntry("acme.one", "collections/one", "dev", frozenRootOneCommit),
			frozenRootEntry("acme.two", "collections/two", "feature", frozenRootTwoCommit),
		}, want: frozenRootDevForMain},
		"nothing from its repository": {entries: []lockfile.Entry{elsewhere}, want: frozenRootNoEntry},
		"nothing within its subdir": {entries: []lockfile.Entry{
			frozenRootEntry("acme.one", "collections/one/deeper", "main", frozenRootOneCommit),
		}, want: frozenRootNoEntry},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			roots := []collection{frozenGitRoot(t, "", "collections", "main")}
			assertFrozenRootsVerdict(t, roots, frozenRootIndex(tc.entries...), tc.want)
		})
	}
}

// TestFrozenUnnamedGitRootBesideASiblingRoot pins an unnamed root at collections
// beside a root at collections/three: an entry the sibling asks for the ref of
// is the sibling's, and one neither asks for is charged to the sibling, nearer.
func TestFrozenUnnamedGitRootBesideASiblingRoot(t *testing.T) {
	t.Parallel()
	one := frozenRootEntry("acme.one", "collections/one", "main", frozenRootOneCommit)
	two := frozenRootEntry("acme.two", "collections/two", "main", frozenRootOneCommit)
	three := frozenRootEntry("acme.three", "collections/three", "dev", frozenRootTwoCommit)
	base := frozenRootEntry("acme.base", "collections", "main", frozenRootOneCommit)
	all := []lockfile.Entry{one, two, three}
	parent := frozenGitRoot(t, "", "collections", "main")
	sibling := frozenGitRoot(t, "", "collections/three", "dev")
	cases := map[string]struct {
		want    string
		roots   []collection
		entries []lockfile.Entry
	}{
		"unnamed sibling":      {roots: []collection{parent, sibling}, entries: all},
		"sibling listed first": {roots: []collection{sibling, parent}, entries: all},
		"sibling naming acme.three": {
			roots: []collection{parent, frozenGitRoot(t, "acme.three", "collections/three", "dev")}, entries: all,
		},
		"parent directory is itself a collection": {
			roots: []collection{parent, sibling}, entries: []lockfile.Entry{base, three},
		},
		"sibling asking for another ref": {
			roots: []collection{parent, frozenGitRoot(t, "", "collections/three", "feature")}, entries: all,
			want: "lockfile does not match resolved requirements: git root " + frozenRootRepoURL +
				` locked from ref "dev", requirements ask for "feature"`,
		},
		"sibling naming another collection": {
			roots: []collection{parent, frozenGitRoot(t, "acme.four", "collections/three", "dev")}, entries: all,
			want: frozenRootDevForMain,
		},
		"sibling in another repository": {
			roots: []collection{parent, frozenOtherRepoRoot(t, "", "collections/three", "dev")}, entries: all,
			want: frozenRootDevForMain,
		},
		"parent now asking for the sibling's ref": {
			roots: []collection{frozenGitRoot(t, "", "collections", "dev"), sibling}, entries: all,
			want: frozenRootMainForDev,
		},
		"only the sibling's entry": {
			roots: []collection{parent, sibling}, entries: []lockfile.Entry{three}, want: frozenRootNoEntry,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertFrozenRootsVerdict(t, tc.roots, frozenRootIndex(tc.entries...), tc.want)
		})
	}
}

// TestFrozenGitRootsSharingADirectory pins a root at collections from main
// beside one at collections/three from dev: what lock writes when the child
// changes between the refs passes, and a ref change is refused, masked or not.
func TestFrozenGitRootsSharingADirectory(t *testing.T) {
	t.Parallel()
	parent := frozenGitRoot(t, "", "collections", "main")
	child := frozenGitRoot(t, "", "collections/three", "dev")
	one := frozenRootEntry("acme.one", "collections/one", "main", frozenRootOneCommit)
	threeAtMain := frozenRootEntry("acme.three", "collections/three", "main", frozenRootOneCommit)
	childA := frozenRootEntry("acme.a", "collections/three/a", "dev", frozenRootTwoCommit)
	childB := frozenRootEntry("acme.b", "collections/three/b", "dev", frozenRootTwoCommit)
	cases := map[string]struct {
		want    string
		roots   []collection
		entries []lockfile.Entry
	}{
		"child renamed between the refs": {roots: []collection{parent, child}, entries: []lockfile.Entry{
			one, frozenRootEntry("acme.trois", "collections/three", "main", frozenRootOneCommit),
			frozenRootEntry("acme.three", "collections/three", "dev", frozenRootTwoCommit),
		}},
		"child split into children at the other ref": {
			roots: []collection{parent, child}, entries: []lockfile.Entry{one, threeAtMain, childA, childB},
		},
		"parent naming the child's collection": {
			roots:   []collection{frozenGitRoot(t, "acme.three", "collections", "main"), child},
			entries: []lockfile.Entry{threeAtMain, childA, childB},
		},
		"parent moved to the child's ref, a new root keeping its entry": {
			roots: []collection{
				frozenGitRoot(t, "", "collections", "dev"),
				frozenGitRoot(t, "", "collections/x", "main"),
				frozenGitRoot(t, "", "collections/z", "dev"),
			},
			entries: []lockfile.Entry{
				frozenRootEntry("acme.x", "collections/x", "main", frozenRootOneCommit),
				frozenRootEntry("acme.z", "collections/z", "dev", frozenRootTwoCommit),
			},
			want: frozenRootNoEntry,
		},
		"parent moved to another ref": {
			roots: []collection{frozenGitRoot(t, "", "collections", "dev")}, entries: []lockfile.Entry{
				one, frozenRootEntry("acme.two", "collections/two", "main", frozenRootOneCommit),
			},
			want: frozenRootMainForDev,
		},
		"stale entry beside the child": {roots: []collection{parent, child}, entries: []lockfile.Entry{
			one, frozenRootEntry("acme.three", "collections/three", "dev", frozenRootTwoCommit),
			frozenRootEntry("acme.four", "collections/four", "dev", frozenRootTwoCommit),
		}, want: frozenRootDevForMain},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertFrozenRootsVerdict(t, tc.roots, frozenRootIndex(tc.entries...), tc.want)
		})
	}
}

// TestFrozenGitRootVerdictsFollowRequirementsOrder pins that of two refused git
// roots the one listed first is reported: the parent is charged acme.one and
// acme.two from main, the child acme.three from dev.
func TestFrozenGitRootVerdictsFollowRequirementsOrder(t *testing.T) {
	t.Parallel()
	const prefix = "lockfile does not match resolved requirements: git root " + frozenRootRepoURL
	byFQDN := frozenRootIndex(
		frozenRootEntry("acme.one", "collections/one", "main", frozenRootOneCommit),
		frozenRootEntry("acme.two", "collections/two", "main", frozenRootOneCommit),
		frozenRootEntry("acme.three", "collections/three", "dev", frozenRootTwoCommit),
	)
	parent := frozenGitRoot(t, "", "collections", "release")
	child := frozenGitRoot(t, "", "collections/three", "feature")
	assertFrozenRootsVerdict(t, []collection{parent, child}, byFQDN,
		prefix+` locked from ref "main", requirements ask for "release"`)
	assertFrozenRootsVerdict(t, []collection{child, parent}, byFQDN,
		prefix+` locked from ref "dev", requirements ask for "feature"`)
}

// TestFrozenGitRootAfterAGalaxyRoot pins that a git root listed after a Galaxy
// root is judged by its own entries: both pass, and a changed ref is refused.
func TestFrozenGitRootAfterAGalaxyRoot(t *testing.T) {
	t.Parallel()
	lib := collection{Namespace: "acme", Name: "lib", Constraint: ">=1.0.0", Type: typeGalaxy}
	byFQDN := frozenRootIndex(
		lockfile.Entry{Name: "acme.lib", Version: "1.0.0", Source: "https://galaxy.example"},
		frozenRootEntry("acme.one", "collections/one", "main", frozenRootOneCommit),
	)
	assertFrozenRootsVerdict(t, []collection{lib, frozenGitRoot(t, "", "collections", "main")}, byFQDN, "")
	assertFrozenRootsVerdict(t, []collection{lib, frozenGitRoot(t, "", "collections", "dev")}, byFQDN, frozenRootMainForDev)
}
