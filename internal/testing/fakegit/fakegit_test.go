package fakegit

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	gogithttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Names shared by the fixtures below.
const (
	fixtureRepo   = "acme"
	fixtureBranch = "main"
	// abortAfter bounds a request a fault is expected to block: long enough
	// for a loopback exchange to reach the fault, short enough for the suite.
	abortAfter = 2 * time.Second
)

// fixture is one repository on a fresh server: two commits on main, HEAD
// symbolic to main, so a depth-1 fetch and a full fetch are told apart by
// how many commits arrive.
type fixture struct {
	srv    *Server
	repo   *Repo
	first  plumbing.Hash
	second plumbing.Hash
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	srv := New(t)
	repo := NewRepo(t)
	repo.SetHEAD(fixtureBranch)
	repo.AddCollection("", "acme", "tools", "1.0.0", map[string]string{"acme.base": ">=1.0.0"}, []string{"tests/*"})
	first := repo.Commit("first")
	repo.WriteFile("README.md", fixtureFileMode, []byte("# acme.tools\n\nmore\n"))
	second := repo.Commit("second")
	srv.Add(fixtureRepo, repo)
	return fixture{srv: srv, repo: repo, first: first, second: second}
}

// clone runs the stock go-git client against url into memory storage.
func clone(t *testing.T, url string, opts git.CloneOptions) (*git.Repository, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), abortAfter)
	defer cancel()
	opts.URL = url
	return git.CloneContext(ctx, memory.NewStorage(), nil, &opts)
}

// countCommits counts every commit object the cloned storage holds.
func countCommits(t *testing.T, r *git.Repository) int {
	t.Helper()
	iter, err := r.CommitObjects()
	if err != nil {
		t.Fatalf("CommitObjects: %v", err)
	}
	n := 0
	if err := iter.ForEach(func(*object.Commit) error { n++; return nil }); err != nil {
		t.Fatalf("iterate commits: %v", err)
	}
	return n
}

func branchOpts(depth int) git.CloneOptions {
	return git.CloneOptions{
		ReferenceName: plumbing.NewBranchReferenceName(fixtureBranch),
		SingleBranch:  true,
		Depth:         depth,
		Tags:          git.NoTags,
	}
}

func TestShallowCloneHoldsOneCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.SetCapabilities(fixtureRepo, Capabilities{Shallow: true})

	r, err := clone(t, f.srv.RepoURL(fixtureRepo), branchOpts(1))
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if got := countCommits(t, r); got != 1 {
		t.Fatalf("commits in depth-1 clone = %d, want 1", got)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Hash() != f.second {
		t.Fatalf("HEAD = %s, want %s", head.Hash(), f.second)
	}
	if f.srv.Count(EndpointInfoRefs) != 1 || f.srv.Count(EndpointUploadPack) != 1 || f.srv.Total() != 2 {
		t.Fatalf("counts = %d/%d/%d, want 1/1/2",
			f.srv.Count(EndpointInfoRefs), f.srv.Count(EndpointUploadPack), f.srv.Total())
	}
	f.srv.ResetCounts()
	if f.srv.Total() != 0 {
		t.Fatalf("Total after ResetCounts = %d, want 0", f.srv.Total())
	}
}

func TestFullCloneAgainstMinimalServer(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	r, err := clone(t, f.srv.RepoURL(fixtureRepo), branchOpts(0))
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if got := countCommits(t, r); got != 2 {
		t.Fatalf("commits in full clone = %d, want 2", got)
	}
	if _, err := r.CommitObject(f.first); err != nil {
		t.Fatalf("first commit missing from full clone: %v", err)
	}
}

func TestDepthAgainstServerWithoutShallowFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := clone(t, f.srv.RepoURL(fixtureRepo), branchOpts(1)); err == nil {
		t.Fatal("depth-1 clone against a server that did not advertise shallow succeeded")
	}
	if f.srv.Count(EndpointUploadPack) != 1 {
		t.Fatalf("upload-pack count = %d, want 1 (the deepen must reach the server)", f.srv.Count(EndpointUploadPack))
	}
}

func TestUnknownRepositoryIsNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := clone(t, f.srv.RepoURL("nothing"), branchOpts(0))
	if err == nil {
		t.Fatal("clone of an unregistered repository succeeded")
	}
}

// tagCloneCase is one clone by tag reference: which tag, at which depth,
// and whether the reference the client ends up with names a tag object.
type tagCloneCase struct {
	name      string
	tag       string
	depth     int
	annotated bool
}

func tagCloneCases() []tagCloneCase {
	return []tagCloneCase{
		{name: "lightweight full", tag: "v1", depth: 0},
		{name: "lightweight depth 1", tag: "v1", depth: 1},
		{name: "annotated full", tag: "v2", depth: 0, annotated: true},
		{name: "annotated depth 1", tag: "v2", depth: 1, annotated: true},
	}
}

func TestCloneByTag(t *testing.T) {
	t.Parallel()
	for _, tc := range tagCloneCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.SetCapabilities(fixtureRepo, Capabilities{Shallow: true})
			f.repo.Tag("v1", f.first)
			tagObj := f.repo.AnnotatedTag("v2", f.first)

			r, err := clone(t, f.srv.RepoURL(fixtureRepo), git.CloneOptions{
				ReferenceName: plumbing.NewTagReferenceName(tc.tag),
				SingleBranch:  true,
				Depth:         tc.depth,
				Tags:          git.NoTags,
			})
			if err != nil {
				t.Fatalf("clone: %v", err)
			}
			want := f.first
			if tc.annotated {
				want = tagObj
				assertTagObject(t, r, tagObj, f.first)
			}
			assertTagRef(t, r, tc.tag, want)
			if _, err := r.CommitObject(f.first); err != nil {
				t.Fatalf("tagged commit missing: %v", err)
			}
			// v1 and v2 both sit on the first commit, which has no parent, so
			// full and depth-1 clones alike hold exactly one commit.
			if got := countCommits(t, r); got != 1 {
				t.Fatalf("commits = %d, want 1", got)
			}
		})
	}
}

// assertTagObject requires the clone to hold the tag object tagObj
// pointing at target.
func assertTagObject(t *testing.T, r *git.Repository, tagObj, target plumbing.Hash) {
	t.Helper()
	tag, err := r.TagObject(tagObj)
	if err != nil {
		t.Fatalf("tag object not in clone: %v", err)
	}
	if tag.Target != target {
		t.Fatalf("tag target = %s, want %s", tag.Target, target)
	}
}

// assertTagRef requires refs/tags/name in the clone to name want.
func assertTagRef(t *testing.T, r *git.Repository, name string, want plumbing.Hash) {
	t.Helper()
	ref, err := r.Reference(plumbing.NewTagReferenceName(name), true)
	if err != nil {
		t.Fatalf("tag reference: %v", err)
	}
	if ref.Hash() != want {
		t.Fatalf("tag = %s, want %s", ref.Hash(), want)
	}
}

// fetchAdvertisement decodes what GET info/refs serves for name, the way the
// client does, so a test can look at the capability list itself.
func fetchAdvertisement(t *testing.T, srv *Server, name string) *packp.AdvRefs {
	t.Helper()
	url := srv.RepoURL(name) + infoRefsPath + "?" + serviceQuery + "=" + uploadPackService
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET info/refs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != advertisementContentType {
		t.Fatalf("info/refs = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	ar := packp.NewAdvRefs()
	if err := ar.Decode(resp.Body); err != nil {
		t.Fatalf("decode advertisement: %v", err)
	}
	return ar
}

func TestAdvertisementShape(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tagObj := f.repo.AnnotatedTag("v2", f.first)
	f.srv.SetCapabilities(fixtureRepo, Capabilities{Shallow: true, AllowReachableSHA1: true})

	ar := fetchAdvertisement(t, f.srv, fixtureRepo)
	assertCapabilities(t, ar,
		[]capability.Capability{
			capability.Agent, capability.OFSDelta, capability.NoProgress, capability.Shallow, capability.AllowReachableSHA1InWant,
		},
		[]capability.Capability{capability.Sideband, capability.Sideband64k, capability.MultiACK, capability.ThinPack})
	if got := ar.Capabilities.Get(capability.SymRef); len(got) != 1 || got[0] != "HEAD:refs/heads/main" {
		t.Errorf("symref = %v", got)
	}
	if ar.Head == nil || *ar.Head != f.second {
		t.Errorf("Head = %v, want %s", ar.Head, f.second)
	}
	if ar.References["refs/tags/v2"] != tagObj || ar.Peeled["refs/tags/v2"] != f.first {
		t.Errorf("tag = %s peeled %s, want %s / %s", ar.References["refs/tags/v2"], ar.Peeled["refs/tags/v2"], tagObj, f.first)
	}
}

// assertCapabilities requires every capability in present to be advertised
// and none of absent.
func assertCapabilities(t *testing.T, ar *packp.AdvRefs, present, absent []capability.Capability) {
	t.Helper()
	for _, c := range present {
		if !ar.Capabilities.Supports(c) {
			t.Errorf("capability %s not advertised", c)
		}
	}
	for _, c := range absent {
		if ar.Capabilities.Supports(c) {
			t.Errorf("capability %s advertised", c)
		}
	}
}

func TestDetachedHeadAdvertisesNoSymref(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo.DetachHEAD(f.first)
	ar := fetchAdvertisement(t, f.srv, fixtureRepo)
	if ar.Capabilities.Supports(capability.SymRef) {
		t.Fatalf("symref advertised for a detached HEAD: %v", ar.Capabilities.Get(capability.SymRef))
	}
	if ar.Head == nil || *ar.Head != f.first {
		t.Fatalf("Head = %v, want %s", ar.Head, f.first)
	}
	// Pinned go-git behavior: with no symref, AdvRefs.AllReferences fails when
	// HEAD matches no branch tip, so a detached HEAD off every tip breaks the
	// stock client; production code must not resolve refs through it.
	if _, err := clone(t, f.srv.RepoURL(fixtureRepo), branchOpts(0)); err == nil {
		t.Fatal("clone against a detached HEAD off any tip succeeded")
	}
	// A detached HEAD that does sit on a tip is guessed onto that branch.
	f.repo.DetachHEAD(f.second)
	r, err := clone(t, f.srv.RepoURL(fixtureRepo), branchOpts(0))
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if got := countCommits(t, r); got != 2 {
		t.Fatalf("commits = %d, want 2", got)
	}
}

// fetchSHA fetches one exact commit into refs/heads/pinned.
func fetchSHA(t *testing.T, url string, sha plumbing.Hash) (*memory.Storage, error) {
	t.Helper()
	st := memory.NewStorage()
	remote := git.NewRemote(st, &config.RemoteConfig{Name: "origin", URLs: []string{url}})
	ctx, cancel := context.WithTimeout(t.Context(), abortAfter)
	defer cancel()
	spec := config.RefSpec(sha.String() + ":refs/heads/pinned")
	return st, remote.FetchContext(ctx, &git.FetchOptions{RefSpecs: []config.RefSpec{spec}, Tags: git.NoTags})
}

// exactSHACase is one fetch by commit hash: with or without the capability,
// of a reachable or an unknown commit.
type exactSHACase struct {
	pick      func(f fixture) plumbing.Hash
	name      string
	caps      Capabilities
	wantError bool
}

func exactSHACases() []exactSHACase {
	return []exactSHACase{
		{
			name: "reachable non-tip allowed",
			pick: func(f fixture) plumbing.Hash { return f.first },
			caps: Capabilities{AllowReachableSHA1: true},
		},
		{
			name: "tip allowed",
			pick: func(f fixture) plumbing.Hash { return f.second },
			caps: Capabilities{AllowReachableSHA1: true},
		},
		{
			// The client itself refuses an exact-sha refspec against a server
			// that advertised neither allow-reachable-sha1-in-want nor
			// allow-tip-sha1-in-want; nothing reaches upload-pack.
			name:      "refused without capability",
			pick:      func(f fixture) plumbing.Hash { return f.first },
			wantError: true,
		},
		{
			// Reachability is judged by the server: a hash it never saw is
			// answered with an ERR line.
			name:      "unknown commit refused",
			pick:      func(fixture) plumbing.Hash { return plumbing.NewHash("0123456789abcdef0123456789abcdef01234567") },
			caps:      Capabilities{AllowReachableSHA1: true},
			wantError: true,
		},
	}
}

func TestFetchExactSHA(t *testing.T) {
	t.Parallel()
	for _, tc := range exactSHACases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.SetCapabilities(fixtureRepo, tc.caps)
			sha := tc.pick(f)
			st, err := fetchSHA(t, f.srv.RepoURL(fixtureRepo), sha)
			if tc.wantError {
				if err == nil {
					t.Fatal("fetch succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			ref, err := st.Reference(plumbing.NewBranchReferenceName("pinned"))
			if err != nil || ref.Hash() != sha {
				t.Fatalf("pinned = %v, %v; want %s", ref, err, sha)
			}
			if _, err := object.GetCommit(st, sha); err != nil {
				t.Fatalf("fetched commit missing: %v", err)
			}
		})
	}
}

// basicAuthHeader is the exact header value RequireAuth expects for the
// user:pass credential the auth tests present.
func basicAuthHeader() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
}

func TestRequireAuthAcceptsMatchingHeader(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.RequireAuth(basicAuthHeader())

	opts := branchOpts(0)
	opts.Auth = &gogithttp.BasicAuth{Username: "user", Password: "pass"}
	if _, err := clone(t, f.srv.RepoURL(fixtureRepo), opts); err != nil {
		t.Fatalf("authenticated clone: %v", err)
	}
	for _, ep := range []Endpoint{EndpointInfoRefs, EndpointUploadPack} {
		got, present := f.srv.SeenAuth(ep)
		if !present || got != basicAuthHeader() {
			t.Fatalf("SeenAuth(%d) = %q, %v; want %q, true", ep, got, present, basicAuthHeader())
		}
	}
}

func TestRequireAuthRefusesMissingAndWrongHeader(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.RequireAuth(basicAuthHeader())

	if _, err := clone(t, f.srv.RepoURL(fixtureRepo), branchOpts(0)); err == nil {
		t.Fatal("anonymous clone against RequireAuth succeeded")
	}
	if f.srv.Count(EndpointInfoRefs) != 1 || f.srv.Count(EndpointUploadPack) != 0 {
		t.Fatalf("counts = %d/%d, want 1/0", f.srv.Count(EndpointInfoRefs), f.srv.Count(EndpointUploadPack))
	}
	if _, present := f.srv.SeenAuth(EndpointInfoRefs); present {
		t.Fatal("anonymous request recorded an Authorization header")
	}

	f.srv.AuthFailStatus(http.StatusForbidden)
	opts := branchOpts(0)
	opts.Auth = &gogithttp.BasicAuth{Username: "user", Password: "wrong"}
	if _, err := clone(t, f.srv.RepoURL(fixtureRepo), opts); err == nil {
		t.Fatal("clone with a wrong password succeeded")
	}
	if got, present := f.srv.SeenAuth(EndpointInfoRefs); !present || got == basicAuthHeader() {
		t.Fatalf("SeenAuth = %q, %v; want the wrong credential captured", got, present)
	}
}

// faultCase arms one Fault on one endpoint and states what the client must
// observe: an error, and whether the abort came from the client's own
// deadline (Hang and stall) rather than from the server.
type faultCase struct {
	name         string
	fault        Fault
	ep           Endpoint
	wantDeadline bool
}

func faultCases() []faultCase {
	return []faultCase{
		{name: "status on info/refs", ep: EndpointInfoRefs, fault: Fault{Status: http.StatusInternalServerError, Count: 1}},
		{name: "status on upload-pack", ep: EndpointUploadPack, fault: Fault{Status: http.StatusBadGateway, Count: 1}},
		{name: "hang on info/refs", ep: EndpointInfoRefs, fault: Fault{Hang: true, Count: 1}, wantDeadline: true},
		{name: "hang on upload-pack", ep: EndpointUploadPack, fault: Fault{Hang: true, Count: 1}, wantDeadline: true},
		{name: "stall mid-pack", ep: EndpointUploadPack, fault: Fault{StallAfterBytes: 64, Count: 1}, wantDeadline: true},
	}
}

func TestFaults(t *testing.T) {
	t.Parallel()
	for _, tc := range faultCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.Fail(tc.ep, fixtureRepo, tc.fault)
			ctx, cancel := context.WithTimeout(t.Context(), abortAfter)
			defer cancel()
			opts := branchOpts(0)
			opts.URL = f.srv.RepoURL(fixtureRepo)
			_, err := git.CloneContext(ctx, memory.NewStorage(), nil, &opts)
			if err == nil {
				t.Fatal("clone succeeded against an armed fault")
			}
			if tc.wantDeadline && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("clone failed before the deadline with %v; the fault did not block", err)
			}
			if f.srv.Count(tc.ep) != 1 {
				t.Fatalf("count = %d, want 1", f.srv.Count(tc.ep))
			}
			// Count 1 is spent: the next clone is served normally.
			if _, err := clone(t, f.srv.RepoURL(fixtureRepo), branchOpts(0)); err != nil {
				t.Fatalf("clone after the fault was consumed: %v", err)
			}
		})
	}
}

func TestFaultCountNegativeFiresForever(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.Fail(EndpointInfoRefs, "", Fault{Status: http.StatusServiceUnavailable, Count: -1})
	for range 3 {
		if _, err := clone(t, f.srv.RepoURL(fixtureRepo), branchOpts(0)); err == nil {
			t.Fatal("clone succeeded against an indefinite fault")
		}
	}
	if f.srv.Count(EndpointInfoRefs) != 3 {
		t.Fatalf("count = %d, want 3", f.srv.Count(EndpointInfoRefs))
	}
}

func TestServeCommitFaultShipsAnotherCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	other := NewRepo(t)
	other.WriteFile("x", fixtureFileMode, []byte("x"))
	otherCommit := other.Commit("elsewhere")
	// The served commit must live in the same storage the pack is built from.
	stranger := f.repo.RawTreeCommit([]object.TreeEntry{{Name: "x", Mode: filemode.Regular, Hash: f.repo.Blob([]byte("x"))}})
	if stranger == otherCommit {
		t.Fatal("fixture: the raw commit must differ from a worktree commit")
	}
	f.srv.Fail(EndpointUploadPack, fixtureRepo, Fault{ServeCommit: stranger, Count: 1})

	st := memory.NewStorage()
	ctx, cancel := context.WithTimeout(t.Context(), abortAfter)
	defer cancel()
	opts := branchOpts(0)
	opts.URL = f.srv.RepoURL(fixtureRepo)
	_, cloneErr := git.CloneContext(ctx, st, nil, &opts)
	if _, err := object.GetCommit(st, f.second); err == nil {
		t.Fatalf("wanted commit arrived despite ServeCommit (clone error %v)", cloneErr)
	}
	if _, err := object.GetCommit(st, stranger); err != nil {
		t.Fatalf("served commit did not arrive: %v (clone error %v)", err, cloneErr)
	}
}

func TestRedirectFaultSendsClientElsewhere(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.RequireAuth(basicAuthHeader())
	other := New(t)
	other.Add(fixtureRepo, f.repo)
	target := other.RepoURL(fixtureRepo) + infoRefsPath + "?" + serviceQuery + "=" + uploadPackService
	f.srv.Fail(EndpointInfoRefs, fixtureRepo, Fault{Redirect: target, Count: 1})

	opts := branchOpts(0)
	opts.Auth = &gogithttp.BasicAuth{Username: "user", Password: "pass"}
	_, err := clone(t, f.srv.RepoURL(fixtureRepo), opts)
	if err != nil {
		t.Fatalf("clone across the redirect: %v", err)
	}
	if f.srv.Count(EndpointInfoRefs) != 1 || f.srv.Count(EndpointUploadPack) != 0 {
		t.Fatalf("origin counts = %d/%d, want 1/0", f.srv.Count(EndpointInfoRefs), f.srv.Count(EndpointUploadPack))
	}
	if other.Count(EndpointInfoRefs) != 1 || other.Count(EndpointUploadPack) != 1 {
		t.Fatalf("target counts = %d/%d, want 1/1", other.Count(EndpointInfoRefs), other.Count(EndpointUploadPack))
	}
	// go-git drops the credential once the advertisement moved to another
	// host:port, so the target's upload-pack is anonymous.
	if _, present := other.SeenAuth(EndpointUploadPack); present {
		t.Fatal("credential followed the redirect onto the target's upload-pack")
	}
}

func TestRawTreeCommitRoundTrips(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	blob := f.repo.Blob([]byte("payload"))
	hostile := f.repo.RawTreeCommit([]object.TreeEntry{
		{Name: "..", Mode: filemode.Regular, Hash: blob},
		{Name: "a/b", Mode: filemode.Regular, Hash: blob},
		{Name: "vendored", Mode: filemode.Submodule, Hash: f.first},
		{Name: "dup", Mode: filemode.Regular, Hash: blob},
		{Name: "dup", Mode: filemode.Executable, Hash: blob},
	})
	f.repo.Branch("hostile", hostile)

	r, err := clone(t, f.srv.RepoURL(fixtureRepo), git.CloneOptions{
		ReferenceName: plumbing.NewBranchReferenceName("hostile"),
		SingleBranch:  true,
		Tags:          git.NoTags,
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	commit, err := r.CommitObject(hostile)
	if err != nil {
		t.Fatalf("hostile commit: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("hostile tree: %v", err)
	}
	got := make(map[string]int)
	for _, e := range tree.Entries {
		got[e.Name]++
	}
	if got[".."] != 1 || got["a/b"] != 1 || got["vendored"] != 1 || got["dup"] != 2 {
		t.Fatalf("entries = %v", got)
	}
	if _, err := r.BlobObject(blob); err != nil {
		t.Fatalf("blob behind the hostile names missing: %v", err)
	}
}

func TestFixedTimeIsShared(t *testing.T) {
	t.Parallel()
	a := NewRepo(t)
	a.WriteFile("f", fixtureFileMode, []byte("same"))
	b := NewRepo(t)
	b.WriteFile("f", fixtureFileMode, []byte("same"))
	if a.Commit("m") != b.Commit("m") {
		t.Fatal("two identical fixtures produced different commit hashes")
	}
	if !FixedTime().Equal(fixedTime) || FixedTime().Location() != time.UTC {
		t.Fatalf("FixedTime = %v", FixedTime())
	}
}
