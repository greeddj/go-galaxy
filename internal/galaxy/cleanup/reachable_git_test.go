package cleanup

import (
	"slices"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

const (
	gitKeysRepoURL   = "https://git.example/acme/mono.git"
	gitKeysOtherURL  = "https://git.example/acme/other.git"
	gitKeysCommit    = "0123456789abcdef0123456789abcdef01234567"
	gitKeysOldCommit = "fedcba9876543210fedcba9876543210fedcba98"
)

type gitSubdirWithinCase struct {
	name   string
	entry  string
	root   string
	within bool
}

// gitSubdirWithinCases admits the subdir itself and its immediate children,
// and refuses a grandchild, a sibling, and a root entry under a subdir root.
func gitSubdirWithinCases() []gitSubdirWithinCase {
	return []gitSubdirWithinCase{
		{name: "root to root", entry: "", root: "", within: true},
		{name: "child of root", entry: "collections", root: "", within: true},
		{name: "grandchild of root", entry: "collections/one", root: "", within: false},
		{name: "subdir itself", entry: "collections", root: "collections", within: true},
		{name: "child of subdir", entry: "collections/one", root: "collections", within: true},
		{name: "grandchild of subdir", entry: "collections/one/deep", root: "collections", within: false},
		{name: "sibling", entry: "roles/one", root: "collections", within: false},
		{name: "root entry under a subdir root", entry: "", root: "collections", within: false},
	}
}

func TestGitSubdirWithin(t *testing.T) {
	t.Parallel()
	for _, tc := range gitSubdirWithinCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := gitSubdirWithin(tc.entry, tc.root); got != tc.within {
				t.Fatalf("gitSubdirWithin(%q, %q) = %v, want %v", tc.entry, tc.root, got, tc.within)
			}
		})
	}
}

// gitKeysStore builds the index and store the gitRootKeys rows share: pinned
// installs, an older commit only the locator scan sees, a grandchild, a foreign
// repository, and a pinned acme.ghost that nothing installed.
func gitKeysStore(withPin bool) (*store.Store, map[string][]installedCollection) {
	st := store.New()
	record := func(key, url, subdir, commit string) {
		st.SetInstalled(key, store.InstalledEntry{Source: gitsource.Locator{URL: url, Subdir: subdir, Commit: commit}.String()})
	}
	record("acme.one@0.1.0", gitKeysRepoURL, "collections/one", gitKeysCommit)
	record("acme.two@0.2.0", gitKeysRepoURL, "collections/two", gitKeysCommit)
	record("acme.two@0.9.0", gitKeysRepoURL, "collections/two", gitKeysOldCommit)
	record("acme.deep@1.0.0", gitKeysRepoURL, "collections/two/deep", gitKeysCommit)
	record("acme.foreign@1.0.0", gitKeysOtherURL, "collections/one", gitKeysCommit)
	installedByKey := make(map[string][]installedCollection)
	for _, key := range []string{"acme.one@0.1.0", "acme.two@0.2.0", "acme.two@0.9.0", "acme.deep@1.0.0", "acme.foreign@1.0.0"} {
		installedByKey[key] = []installedCollection{{Key: key}}
	}
	if withPin {
		st.SetGitPin(gitsource.PinKey(gitKeysRepoURL, "main", "collections"), store.GitPinEntry{
			Commit: gitKeysCommit,
			Collections: []store.GitPinCollection{
				{Namespace: "acme", Name: "one", Version: "0.1.0", Subdir: "collections/one"},
				{Namespace: "acme", Name: "two", Version: "0.2.0", Subdir: "collections/two"},
				{Namespace: "acme", Name: "ghost", Version: "1.0.0", Subdir: "collections/ghost"},
			},
		})
	}
	return st, installedByKey
}

type gitRootKeysCase struct {
	name      string
	namespace string
	cname     string
	want      []string
	withPin   bool
}

// gitRootKeysCases pins the pin branch (its installed identities alone), the
// locator fallback (every commit under the subdir or its children, sorted),
// and the narrowing an explicit name applies to both.
func gitRootKeysCases() []gitRootKeysCase {
	return []gitRootKeysCase{
		{name: "pin present", withPin: true, want: []string{"acme.one@0.1.0", "acme.two@0.2.0"}},
		{name: "pin present explicit name", withPin: true, namespace: "acme", cname: "two", want: []string{"acme.two@0.2.0"}},
		{name: "pin present explicit name not pinned", withPin: true, namespace: "acme", cname: "deep", want: []string{}},
		{name: "pin absent", want: []string{"acme.one@0.1.0", "acme.two@0.2.0", "acme.two@0.9.0"}},
		{name: "pin absent explicit name", namespace: "acme", cname: "two", want: []string{"acme.two@0.2.0", "acme.two@0.9.0"}},
		{name: "pin absent foreign repository named", namespace: "acme", cname: "foreign", want: []string{}},
	}
}

func TestGitRootKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range gitRootKeysCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkGitRootKeys(t, tc)
		})
	}
}

func checkGitRootKeys(t *testing.T, tc gitRootKeysCase) {
	t.Helper()
	st, installedByKey := gitKeysStore(tc.withPin)
	root := requirements.CollectionRequirement{
		Type:      requirements.TypeGit,
		Source:    gitKeysRepoURL,
		Ref:       "main",
		Subdir:    "collections",
		Namespace: tc.namespace,
		Name:      tc.cname,
	}
	got := gitRootKeys(st, installedByKey, root)
	if !slices.Equal(got, tc.want) {
		t.Fatalf("gitRootKeys = %v, want %v", got, tc.want)
	}
}
