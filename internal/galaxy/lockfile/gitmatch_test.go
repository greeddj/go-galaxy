package lockfile

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// The repository the entries below are locked from, another one, the ref whose
// entries sit at one commit, the commit every other ref's entries sit at, and
// the verdict text for gitMatchRepo.
const (
	gitMatchRepo        = "https://git.example/acme/mono.git"
	gitMatchOther       = "https://git.example/acme/other.git"
	gitMatchMainRef     = "main"
	gitMatchMainCommit  = "1111111111111111111111111111111111111111"
	gitMatchOtherCommit = "2222222222222222222222222222222222222222"
	gitMatchPrefix      = "lockfile does not match resolved requirements: git root " + gitMatchRepo
	gitMatchNoEntry     = gitMatchPrefix + " has no lockfile entry"
)

// gitMatchEntry is a git entry of gitMatchRepo named fqdn at subdir, locked
// from ref at gitMatchMainCommit when ref is main, else at gitMatchOtherCommit.
func gitMatchEntry(fqdn, subdir, ref string) Entry {
	commit := gitMatchOtherCommit
	if ref == gitMatchMainRef {
		commit = gitMatchMainCommit
	}
	return Entry{Name: fqdn, Type: TypeGit, Version: "1.0.0", Source: gitMatchRepo, Ref: ref, Commit: commit, Subdir: subdir}
}

// gitMatchReq is a requirement of gitMatchRepo at subdir from ref, naming fqdn
// when it is set.
func gitMatchReq(fqdn, subdir, ref string) GitRequirement {
	return GitRequirement{URL: gitMatchRepo, Subdir: subdir, Ref: ref, FQDN: fqdn}
}

// gitMatchRefMismatch is the verdict of a requirement asking for asked that is
// charged an entry locked from locked.
func gitMatchRefMismatch(locked, asked string) string {
	return gitMatchPrefix + ` locked from ref "` + locked + `", requirements ask for "` + asked + `"`
}

// gitMatchWant is one requirement's expected share, names joined by commas,
// and its verdict text, "" for none.
type gitMatchWant struct {
	owned, mismatched, err string
}

type gitMatchCase struct {
	reqs    []GitRequirement
	entries []Entry
	want    []gitMatchWant
}

// gitMatchCases are the shapes lock writes for requirements sharing a
// directory, each passing, then the edits that must not pass, then the
// entries no requirement could have locked.
func gitMatchCases() map[string]gitMatchCase {
	cases := map[string]gitMatchCase{
		"named root beside a sibling": {
			reqs: []GitRequirement{gitMatchReq("acme.one", "collections", "main"), gitMatchReq("", "collections/two", "dev")},
			entries: []Entry{
				gitMatchEntry("acme.one", "collections/one", "main"), gitMatchEntry("acme.two", "collections/two", "dev"),
			},
			want: []gitMatchWant{{owned: "acme.one"}, {owned: "acme.two"}},
		},
		"unnamed root beside a sibling": {
			reqs: []GitRequirement{gitMatchReq("", "collections", "main"), gitMatchReq("", "collections/three", "dev")},
			entries: []Entry{
				gitMatchEntry("acme.three", "collections/three", "dev"), gitMatchEntry("acme.two", "collections/two", "main"),
				gitMatchEntry("acme.one", "collections/one", "main"),
			},
			want: []gitMatchWant{{owned: "acme.one,acme.two"}, {owned: "acme.three"}},
		},
		"parent directory that is a collection": {
			reqs: []GitRequirement{gitMatchReq("", "collections", "main"), gitMatchReq("", "collections/three", "dev")},
			entries: []Entry{
				gitMatchEntry("acme.base", "collections", "main"), gitMatchEntry("acme.three", "collections/three", "dev"),
			},
			want: []gitMatchWant{{owned: "acme.base"}, {owned: "acme.three"}},
		},
		"child renamed between the refs": {
			reqs: []GitRequirement{gitMatchReq("", "collections", "main"), gitMatchReq("", "collections/three", "dev")},
			entries: []Entry{
				gitMatchEntry("acme.one", "collections/one", "main"), gitMatchEntry("acme.trois", "collections/three", "main"),
				gitMatchEntry("acme.three", "collections/three", "dev"),
			},
			want: []gitMatchWant{{owned: "acme.one,acme.trois"}, {owned: "acme.three"}},
		},
		"child split into children at the other ref": {
			reqs: []GitRequirement{gitMatchReq("", "collections", "main"), gitMatchReq("", "collections/three", "dev")},
			entries: []Entry{
				gitMatchEntry("acme.one", "collections/one", "main"), gitMatchEntry("acme.three", "collections/three", "main"),
				gitMatchEntry("acme.a", "collections/three/a", "dev"), gitMatchEntry("acme.b", "collections/three/b", "dev"),
			},
			want: []gitMatchWant{{owned: "acme.one,acme.three"}, {owned: "acme.a,acme.b"}},
		},
		"parent naming the child's collection": {
			reqs: []GitRequirement{gitMatchReq("acme.three", "collections", "main"), gitMatchReq("", "collections/three", "dev")},
			entries: []Entry{
				gitMatchEntry("acme.three", "collections/three", "main"),
				gitMatchEntry("acme.a", "collections/three/a", "dev"), gitMatchEntry("acme.b", "collections/three/b", "dev"),
			},
			want: []gitMatchWant{{owned: "acme.three"}, {owned: "acme.a,acme.b"}},
		},
		"repository root beside a child": {
			reqs: []GitRequirement{gitMatchReq("", "", "main"), gitMatchReq("", "tools", "dev")},
			entries: []Entry{
				gitMatchEntry("acme.root", "", "main"), gitMatchEntry("acme.tools", "tools", "dev"),
				gitMatchEntry("acme.deep", "tools/deep", "dev"),
			},
			want: []gitMatchWant{{owned: "acme.root"}, {owned: "acme.deep,acme.tools"}},
		},
	}
	maps.Copy(cases, gitMatchRefusalCases())
	maps.Copy(cases, gitMatchLimitCases())
	maps.Copy(cases, gitMatchUnjudgedCases())
	return cases
}

// gitMatchRefusalCases are requirements edited after lock wrote the entries,
// each refused.
func gitMatchRefusalCases() map[string]gitMatchCase {
	return map[string]gitMatchCase{
		"parent moved to another ref": {
			reqs: []GitRequirement{gitMatchReq("", "collections", "dev")},
			entries: []Entry{
				gitMatchEntry("acme.two", "collections/two", "main"), gitMatchEntry("acme.one", "collections/one", "main"),
			},
			want: []gitMatchWant{{mismatched: "acme.one,acme.two", err: gitMatchRefMismatch("main", "dev")}},
		},
		"parent moved, a new root at the old ref taking its entry": {
			reqs: []GitRequirement{
				gitMatchReq("", "collections", "dev"), gitMatchReq("", "collections/x", "main"), gitMatchReq("", "collections/z", "dev"),
			},
			entries: []Entry{gitMatchEntry("acme.x", "collections/x", "main"), gitMatchEntry("acme.z", "collections/z", "dev")},
			want:    []gitMatchWant{{err: gitMatchNoEntry}, {owned: "acme.x"}, {owned: "acme.z"}},
		},
		"child asking for a ref no entry carries": {
			reqs: []GitRequirement{gitMatchReq("", "collections", "main"), gitMatchReq("", "collections/three", "feature")},
			entries: []Entry{
				gitMatchEntry("acme.one", "collections/one", "main"), gitMatchEntry("acme.three", "collections/three", "dev"),
			},
			want: []gitMatchWant{{owned: "acme.one"}, {mismatched: "acme.three", err: gitMatchRefMismatch("dev", "feature")}},
		},
		"stale entry beside a sibling": {
			reqs: []GitRequirement{gitMatchReq("", "collections", "main"), gitMatchReq("", "collections/three", "dev")},
			entries: []Entry{
				gitMatchEntry("acme.one", "collections/one", "main"), gitMatchEntry("acme.three", "collections/three", "dev"),
				gitMatchEntry("acme.four", "collections/four", "dev"),
			},
			want: []gitMatchWant{
				{owned: "acme.one", mismatched: "acme.four", err: gitMatchRefMismatch("dev", "main")}, {owned: "acme.three"},
			},
		},
		"mismatches reported by name": {
			reqs: []GitRequirement{gitMatchReq("", "collections", "main")},
			entries: []Entry{
				gitMatchEntry("acme.two", "collections/two", "feature"), gitMatchEntry("acme.one", "collections/one", "dev"),
			},
			want: []gitMatchWant{{mismatched: "acme.one,acme.two", err: gitMatchRefMismatch("dev", "main")}},
		},
		"named root never charged another's entry": {
			reqs: []GitRequirement{gitMatchReq("acme.four", "collections/three", "dev"), gitMatchReq("", "collections", "main")},
			entries: []Entry{
				gitMatchEntry("acme.one", "collections/one", "main"), gitMatchEntry("acme.three", "collections/three", "dev"),
			},
			want: []gitMatchWant{
				{err: gitMatchNoEntry + " for acme.four"},
				{owned: "acme.one", mismatched: "acme.three", err: gitMatchRefMismatch("dev", "main")},
			},
		},
	}
}

// gitMatchLimitCases are what the rule decides without commits to go by: lock
// writes the first from two pins of one ref, and it fails closed; prepareRoots
// refuses the second, and the earlier requirement wins.
func gitMatchLimitCases() map[string]gitMatchCase {
	renamed := gitMatchEntry("acme.three", "collections/three", gitMatchMainRef)
	renamed.Commit = gitMatchOtherCommit
	return map[string]gitMatchCase{
		"one ref at two commits, the child renamed between them": {
			reqs:    []GitRequirement{gitMatchReq("", "collections", "main"), gitMatchReq("", "collections/three", "main")},
			entries: []Entry{gitMatchEntry("acme.trois", "collections/three", "main"), renamed},
			want:    []gitMatchWant{{err: gitMatchNoEntry}, {owned: "acme.three,acme.trois"}},
		},
		"two requirements at one subdir": {
			reqs:    []GitRequirement{gitMatchReq("", "collections", "main"), gitMatchReq("", "collections", "main")},
			entries: []Entry{gitMatchEntry("acme.one", "collections/one", "main")},
			want:    []gitMatchWant{{owned: "acme.one"}, {err: gitMatchNoEntry}},
		},
	}
}

// gitMatchUnjudgedCases hold entries no requirement could have locked, left
// out of every share.
func gitMatchUnjudgedCases() map[string]gitMatchCase {
	elsewhere := gitMatchEntry("acme.two", "collections/two", "dev")
	elsewhere.Source = gitMatchOther
	galaxy := Entry{Name: "acme.lib", Version: "1.0.0", Source: "https://galaxy.example"}
	tarball := Entry{
		Name: "acme.tar", Type: TypeURL, Version: "1.0.0", Source: gitMatchRepo,
		SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	return map[string]gitMatchCase{
		"no candidates": {
			reqs: []GitRequirement{gitMatchReq("", "collections", "main")},
			entries: []Entry{
				gitMatchEntry("acme.one", "collections/one", "main"), gitMatchEntry("acme.deep", "collections/one/deep", "dev"),
				gitMatchEntry("acme.out", "other/out", "dev"), gitMatchEntry("acme.top", "", "dev"), elsewhere, galaxy, tarball,
			},
			want: []gitMatchWant{{owned: "acme.one"}},
		},
		"nothing to judge by": {
			reqs:    []GitRequirement{gitMatchReq("acme.one", "collections", "main"), gitMatchReq("", "collections/two", "main")},
			entries: []Entry{elsewhere, galaxy},
			want:    []gitMatchWant{{err: gitMatchNoEntry + " for acme.one"}, {err: gitMatchNoEntry}},
		},
		"url and Galaxy entries naming the repository": {
			reqs: []GitRequirement{gitMatchReq("", "", "main")},
			entries: []Entry{
				gitMatchEntry("acme.root", "", "main"), tarball,
				{Name: "acme.lib", Version: "1.0.0", Source: gitMatchRepo},
			},
			want: []gitMatchWant{{owned: "acme.root"}},
		},
	}
}

// TestMatchGitRequirements pins each requirement's share and verdict for every
// case, whatever order the entries come in.
func TestMatchGitRequirements(t *testing.T) {
	t.Parallel()
	for name, tc := range gitMatchCases() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, entries := range gitMatchOrders(tc.entries) {
				matches := MatchGitRequirements(tc.reqs, entries)
				if len(matches) != len(tc.want) {
					t.Fatalf("%d matches, want %d", len(matches), len(tc.want))
				}
				for i, want := range tc.want {
					assertGitMatch(t, i, tc.reqs[i], matches[i], want)
				}
			}
		})
	}
}

// gitMatchOrders returns entries as given, reversed and rotated by one.
func gitMatchOrders(entries []Entry) [][]Entry {
	reversed := slices.Clone(entries)
	slices.Reverse(reversed)
	rotated := slices.Clone(entries)
	if len(rotated) > 1 {
		rotated = append(rotated[1:], rotated[0])
	}
	return [][]Entry{entries, reversed, rotated}
}

// assertGitMatch requires match to carry req and the names and verdict want
// names; a verdict is always a helpers.ErrLockfileMismatch.
func assertGitMatch(t *testing.T, i int, req GitRequirement, match GitMatch, want gitMatchWant) {
	t.Helper()
	if match.Requirement != req {
		t.Fatalf("requirement %d: match carries %+v, want %+v", i, match.Requirement, req)
	}
	if got := gitMatchNames(match.Owned); got != want.owned {
		t.Fatalf("requirement %d owns %q, want %q", i, got, want.owned)
	}
	if got := gitMatchNames(match.Mismatched); got != want.mismatched {
		t.Fatalf("requirement %d is charged %q, want %q", i, got, want.mismatched)
	}
	err := match.Err()
	switch {
	case want.err == "" && err != nil:
		t.Fatalf("requirement %d: Err() = %v, want nil", i, err)
	case want.err == "":
	case !errors.Is(err, helpers.ErrLockfileMismatch) || err.Error() != want.err:
		t.Fatalf("requirement %d: Err() = %v, want ErrLockfileMismatch reading %q", i, err, want.err)
	}
}

func gitMatchNames(entries []Entry) string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return strings.Join(names, ",")
}

// TestSubdirWithin pins the two places a candidate's subdir may sit: the
// entry's own subdir and its parent, the repository root included.
func TestSubdirWithin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		entry, req string
		want       bool
	}{
		{entry: "collections/one", req: "collections", want: true},
		{entry: "collections", req: "collections", want: true},
		{entry: "collections/a/b", req: "collections", want: false},
		{entry: "collections", req: "collections/one", want: false},
		{entry: "", req: "", want: true},
		{entry: "x", req: "", want: true},
		{entry: "", req: "x", want: false},
		{entry: "collectionsx/one", req: "collections", want: false},
	}
	for _, tc := range cases {
		if got := subdirWithin(tc.entry, tc.req); got != tc.want {
			t.Fatalf("subdirWithin(%q, %q) = %t, want %t", tc.entry, tc.req, got, tc.want)
		}
	}
}
