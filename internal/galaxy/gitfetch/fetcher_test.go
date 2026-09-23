package gitfetch

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/manifest"
	"github.com/greeddj/go-galaxy/internal/testing/fakegit"
)

const testTimeout = 20 * time.Second

// appRepo is the single-collection fixture: acme.app 1.2.3 on main (HEAD),
// a second commit on dev, a lightweight tag v1 and an annotated tag v1.0.0
// on the first commit.
type appRepo struct {
	repo  *fakegit.Repo
	first plumbing.Hash
	dev   plumbing.Hash
}

func newAppRepo(t *testing.T) appRepo {
	t.Helper()
	r := fakegit.NewRepo(t)
	r.AddCollection("", "acme", "app", "1.2.3", map[string]string{"acme.lib": ">=1.0.0"}, nil)
	first := r.Commit("app 1.2.3")
	r.Branch("main", first)
	r.SetHEAD("main")
	r.Tag("v1", first)
	r.AnnotatedTag("v1.0.0", first)
	r.WriteFile("README.md", 0o644, []byte("# acme.app dev\n"))
	r.AddCollection("", "acme", "app", "1.3.0", nil, nil)
	dev := r.Commit("app 1.3.0")
	r.Branch("dev", dev)
	r.Branch("main", first)
	return appRepo{repo: r, first: first, dev: dev}
}

func newFetcher(t *testing.T) *Fetcher {
	t.Helper()
	return New(fetch.NewGit(testTimeout), t.TempDir)
}

func tempFileIn(dir string) gitsource.TempFileFunc {
	return func(context.Context) (*os.File, func(), error) {
		f, err := os.CreateTemp(dir, helpers.ArtifactDownloadTempPrefix+"*")
		if err != nil {
			return nil, nil, err
		}
		name := f.Name()
		return f, func() { _ = os.Remove(name) }, nil
	}
}

func mustURL(t *testing.T, raw string) gitsource.URL {
	t.Helper()
	u, err := gitsource.ParseURL(raw)
	if err != nil {
		t.Fatalf("ParseURL(%q): %v", raw, err)
	}
	return u
}

func mustRef(t *testing.T, raw string) gitsource.Ref {
	t.Helper()
	ref, err := gitsource.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

func acquire(t *testing.T, f *Fetcher, req gitsource.Request) (gitsource.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	if req.TempFile == nil {
		req.TempFile = tempFileIn(t.TempDir())
	}
	res, err := f.Acquire(ctx, req)
	t.Cleanup(func() {
		for _, c := range res.Collections {
			c.Cleanup()
		}
	})
	return res, err
}

// assertBuiltCollection checks one built artifact: identity (every fixture
// builds acme.app), a real file on disk whose sha matches, and a manifest
// chain the reader accepts.
func assertBuiltCollection(t *testing.T, c gitsource.Collection, version string) {
	t.Helper()
	const namespace, name = "acme", "app"
	if c.Namespace != namespace || c.Name != name || c.Version != version {
		t.Fatalf("built %s.%s@%s, want %s.%s@%s", c.Namespace, c.Name, c.Version, namespace, name, version)
	}
	if !helpers.IsSHA256Hex(c.ArtifactSHA) {
		t.Fatalf("ArtifactSHA = %q", c.ArtifactSHA)
	}
	ctx := t.Context()
	manifestJSON, err := manifest.ReadFromTarGz(ctx, c.ArtifactPath)
	if err != nil {
		t.Fatalf("ReadFromTarGz: %v", err)
	}
	if err := manifest.VerifyChain(ctx, c.ArtifactPath, manifestJSON); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if !strings.Contains(string(manifestJSON), `"`+name+`"`) {
		t.Fatalf("MANIFEST.json does not name %s", name)
	}
}

// TestHardenRemovesFileAndGitTransports pins harden by state (no file or git
// transport, a nil ~/.ssh/config reader); a planted config would prove nothing,
// since os/user reads $HOME only for a uid missing from the password database.
func TestHardenRemovesFileAndGitTransports(t *testing.T) {
	t.Parallel()
	newFetcher(t)
	for _, proto := range []string{protocolFile, protocolGit} {
		if _, ok := client.Protocols[proto]; ok {
			t.Fatalf("go-git still registers the %q transport", proto)
		}
	}
	if _, ok := client.Protocols[protocolHTTPS]; !ok {
		t.Fatalf("go-git's default https transport was removed; only file and git must be")
	}
	if gogitssh.DefaultSSHConfig != nil {
		t.Fatal("go-git's ~/.ssh/config reader is still installed; a Hostname or Port entry could redirect a run")
	}
}

type httpRefCase struct {
	name        string
	ref         string
	commit      string
	wantVersion string
	wantRefName string
	shallow     bool
	shaInWant   bool
}

func httpRefCases(app appRepo) []httpRefCase {
	return []httpRefCase{
		{name: "HEAD shallow", ref: "", wantVersion: "1.2.3", wantRefName: "refs/heads/main", shallow: true},
		{name: "HEAD full", ref: "HEAD", wantVersion: "1.2.3", wantRefName: "refs/heads/main"},
		{name: "branch", ref: "dev", wantVersion: "1.3.0", wantRefName: "refs/heads/dev", shallow: true},
		{name: "qualified branch", ref: "refs/heads/dev", wantVersion: "1.3.0", wantRefName: "refs/heads/dev"},
		{name: "lightweight tag", ref: "v1", wantVersion: "1.2.3", wantRefName: "refs/tags/v1", shallow: true},
		{name: "annotated tag peeled", ref: "v1.0.0", wantVersion: "1.2.3", wantRefName: "refs/tags/v1.0.0", shallow: true},
		{name: "commit that is a tip", ref: app.dev.String(), wantVersion: "1.3.0", wantRefName: app.dev.String(), shallow: true},
		{name: "pinned commit by request", ref: "main", commit: app.dev.String(), wantVersion: "1.3.0", wantRefName: "main"},
		{
			name: "pinned commit sha-in-want", ref: "", commit: app.first.String(),
			wantVersion: "1.2.3", wantRefName: "HEAD", shaInWant: true, shallow: true,
		},
	}
}

// TestAcquireOverHTTP drives every ref shape through the real smart-HTTP
// transport against fakegit, with and without the shallow capability, and
// checks the built artifact each time.
func TestAcquireOverHTTP(t *testing.T) {
	t.Parallel()
	app := newAppRepo(t)
	for _, tc := range httpRefCases(app) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := fakegit.New(t)
			srv.Add("app", app.repo)
			srv.SetCapabilities("app", fakegit.Capabilities{Shallow: tc.shallow, AllowReachableSHA1: tc.shaInWant})
			res, err := acquire(t, newFetcher(t), gitsource.Request{
				URL:    mustURL(t, srv.RepoURL("app")),
				Ref:    mustRef(t, tc.ref),
				Commit: tc.commit,
			})
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			if len(res.Collections) != 1 {
				t.Fatalf("built %d collections, want 1", len(res.Collections))
			}
			assertBuiltCollection(t, res.Collections[0], tc.wantVersion)
			if res.RefName != tc.wantRefName {
				t.Fatalf("RefName = %q, want %q", res.RefName, tc.wantRefName)
			}
			if res.BytesFetched <= 0 {
				t.Fatalf("BytesFetched = %d", res.BytesFetched)
			}
			if srv.Count(fakegit.EndpointInfoRefs) != 1 {
				t.Fatalf("info/refs requested %d times, want 1", srv.Count(fakegit.EndpointInfoRefs))
			}
		})
	}
}

// TestAcquireCommitNotATipFallsBackToTheHint proves a pinned commit the
// remote does not serve by hash is reached through the ref it came from, and
// through every tip when the hint fails.
func TestAcquireCommitNotATipFallsBackToTheHint(t *testing.T) {
	t.Parallel()
	r := fakegit.NewRepo(t)
	r.AddCollection("", "acme", "app", "1.0.0", nil, nil)
	old := r.Commit("first")
	r.AddCollection("", "acme", "app", "1.1.0", nil, nil)
	tip := r.Commit("second")
	r.Branch("main", tip)
	r.SetHEAD("main")

	srv := fakegit.New(t)
	srv.Add("app", r)
	f := newFetcher(t)
	res, err := acquire(t, f, gitsource.Request{URL: mustURL(t, srv.RepoURL("app")), Ref: mustRef(t, "main"), Commit: old.String()})
	if err != nil {
		t.Fatalf("Acquire via hint: %v", err)
	}
	assertBuiltCollection(t, res.Collections[0], "1.0.0")

	// A hint naming a ref the remote lacks falls through to the full fetch.
	res, err = acquire(t, f, gitsource.Request{URL: mustURL(t, srv.RepoURL("app")), Ref: mustRef(t, "gone"), Commit: old.String()})
	if err != nil {
		t.Fatalf("Acquire via full fetch: %v", err)
	}
	assertBuiltCollection(t, res.Collections[0], "1.0.0")

	unknown := strings.Repeat("a", 40)
	_, err = acquire(t, f, gitsource.Request{URL: mustURL(t, srv.RepoURL("app")), Ref: mustRef(t, "main"), Commit: unknown})
	if !errors.Is(err, helpers.ErrGitCommitNotFound) {
		t.Fatalf("unknown commit: %v, want ErrGitCommitNotFound", err)
	}
}

func TestAcquireRefusals(t *testing.T) {
	t.Parallel()
	app := newAppRepo(t)
	srv := fakegit.New(t)
	srv.Add("app", app.repo)
	empty := fakegit.NewRepo(t)
	srv.Add("empty", empty)
	f := newFetcher(t)
	cases := []struct {
		wantErr error
		name    string
		repo    string
		ref     string
	}{
		{name: "missing ref", repo: "app", ref: "nosuch", wantErr: helpers.ErrGitRefNotFound},
		{name: "missing repository", repo: "missing", ref: "", wantErr: helpers.ErrGitTransportFailed},
		{name: "empty repository", repo: "empty", ref: "", wantErr: helpers.ErrGitRefNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := acquire(t, f, gitsource.Request{URL: mustURL(t, srv.RepoURL(tc.repo)), Ref: mustRef(t, tc.ref)})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Acquire error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestAdvertiseResolvesWithoutAPack proves Advertise answers from the
// advertisement alone and never requests a pack.
func TestAdvertiseResolvesWithoutAPack(t *testing.T) {
	t.Parallel()
	app := newAppRepo(t)
	srv := fakegit.New(t)
	srv.Add("app", app.repo)
	f := newFetcher(t)
	commit, refName, err := f.Advertise(t.Context(), mustURL(t, srv.RepoURL("app")), mustRef(t, "dev"), gitsource.Credential{})
	if err != nil {
		t.Fatalf("Advertise: %v", err)
	}
	if commit != app.dev.String() || refName != "refs/heads/dev" {
		t.Fatalf("Advertise = (%s, %s)", commit, refName)
	}
	if srv.Count(fakegit.EndpointUploadPack) != 0 {
		t.Fatalf("Advertise requested a pack")
	}
	commit, _, err = f.Advertise(t.Context(), mustURL(t, srv.RepoURL("app")), mustRef(t, app.first.String()), gitsource.Credential{})
	if err != nil || commit != app.first.String() || srv.Count(fakegit.EndpointInfoRefs) != 1 {
		t.Fatalf("a commit ref cost a round trip: %v (%d info/refs)", err, srv.Count(fakegit.EndpointInfoRefs))
	}
}

// TestBasicAuthOverHTTP proves a bound credential reaches both endpoints as
// Basic auth and that a refused one classifies as an auth failure.
func TestBasicAuthOverHTTP(t *testing.T) {
	t.Parallel()
	app := newAppRepo(t)
	srv := fakegit.New(t)
	srv.Add("app", app.repo)
	srv.RequireAuth("Basic Y2k6dG9rZW4=") // ci:token
	f := newFetcher(t)
	u := mustURL(t, srv.RepoURL("app"))

	_, err := acquire(t, f, gitsource.Request{URL: u, Ref: mustRef(t, "")})
	if !errors.Is(err, helpers.ErrGitAuthFailed) {
		t.Fatalf("anonymous against a private repo: %v, want ErrGitAuthFailed", err)
	}
	if strings.Contains(err.Error(), "token") {
		t.Fatalf("error echoes a credential: %q", err.Error())
	}
	_, err = acquire(t, f, gitsource.Request{URL: u, Ref: mustRef(t, ""), Auth: gitsource.Credential{Username: "ci", Password: "wrong"}})
	if !errors.Is(err, helpers.ErrGitAuthFailed) {
		t.Fatalf("wrong credential: %v, want ErrGitAuthFailed", err)
	}
	res, err := acquire(t, f, gitsource.Request{URL: u, Ref: mustRef(t, ""), Auth: gitsource.Credential{Username: "ci", Password: "token"}})
	if err != nil {
		t.Fatalf("Acquire with credential: %v", err)
	}
	assertBuiltCollection(t, res.Collections[0], "1.2.3")
	for _, ep := range []fakegit.Endpoint{fakegit.EndpointInfoRefs, fakegit.EndpointUploadPack} {
		if got, ok := srv.SeenAuth(ep); !ok || got != "Basic Y2k6dG9rZW4=" {
			t.Fatalf("endpoint %d saw auth %q (present=%t)", ep, got, ok)
		}
	}
}

// TestServedCommitMismatchIsIntegrity proves a remote that advertises one
// commit and ships another fails with the integrity sentinel.
func TestServedCommitMismatchIsIntegrity(t *testing.T) {
	t.Parallel()
	app := newAppRepo(t)
	srv := fakegit.New(t)
	srv.Add("app", app.repo)
	// dev is wanted; the pack built from first (dev's parent) cannot carry it.
	srv.Fail(fakegit.EndpointUploadPack, "app", fakegit.Fault{ServeCommit: app.first, Count: 1})
	_, err := acquire(t, newFetcher(t), gitsource.Request{URL: mustURL(t, srv.RepoURL("app")), Ref: mustRef(t, "dev")})
	if !errors.Is(err, helpers.ErrGitCommitMismatch) {
		t.Fatalf("served another commit: %v, want ErrGitCommitMismatch", err)
	}
}

// TestPackCapRefusesALargeFetch proves the on-disk byte cap bites and leaves
// no storage directory behind.
func TestPackCapRefusesALargeFetch(t *testing.T) {
	t.Parallel()
	app := newAppRepo(t)
	srv := fakegit.New(t)
	srv.Add("app", app.repo)
	tempDir := t.TempDir()
	f := New(fetch.NewGit(testTimeout), func() string { return tempDir }, WithPackCap(256))
	_, err := acquire(t, f, gitsource.Request{URL: mustURL(t, srv.RepoURL("app")), Ref: mustRef(t, "")})
	if !errors.Is(err, helpers.ErrResponseTooLarge) {
		t.Fatalf("capped fetch: %v, want ErrResponseTooLarge", err)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "go-galaxy-git-") {
			t.Fatalf("storage directory left behind: %s", e.Name())
		}
	}
}

// TestStalledPackHonorsTheDeadline proves a remote that stops mid-pack is cut
// off by the caller's context and the temp storage is removed.
func TestStalledPackHonorsTheDeadline(t *testing.T) {
	t.Parallel()
	app := newAppRepo(t)
	srv := fakegit.New(t)
	srv.Add("app", app.repo)
	srv.Fail(fakegit.EndpointUploadPack, "app", fakegit.Fault{StallAfterBytes: 64, Count: 1})
	tempDir := t.TempDir()
	f := New(fetch.NewGit(testTimeout), func() string { return tempDir })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := f.Acquire(ctx, gitsource.Request{URL: mustURL(t, srv.RepoURL("app")), Ref: mustRef(t, ""), TempFile: tempFileIn(t.TempDir())})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled pack: %v, want context.DeadlineExceeded", err)
	}
	entries, _ := os.ReadDir(tempDir)
	if len(entries) != 0 {
		t.Fatalf("temp storage left behind: %v", entries)
	}
}

// TestHostileTreeEntriesAreRefused proves a tree entry go-git decodes without
// complaint is refused here before any path is built from it.
func TestHostileTreeEntriesAreRefused(t *testing.T) {
	t.Parallel()
	r := fakegit.NewRepo(t)
	blob := r.Blob([]byte("namespace: acme\nname: app\nversion: 1.0.0\nreadme: README.md\nauthors: [x]\n"))
	hostile := r.RawTreeCommit([]object.TreeEntry{
		{Name: "..", Mode: filemode.Regular, Hash: blob},
		{Name: "galaxy.yml", Mode: filemode.Regular, Hash: blob},
	})
	r.Branch("hostile", hostile)
	submodule := r.RawTreeCommit([]object.TreeEntry{
		{Name: "galaxy.yml", Mode: filemode.Regular, Hash: blob},
		{Name: "vendored", Mode: filemode.Submodule, Hash: hostile},
	})
	r.Branch("submodule", submodule)
	r.SetHEAD("submodule")

	srv := fakegit.New(t)
	srv.Add("app", r)
	f := newFetcher(t)
	_, err := acquire(t, f, gitsource.Request{URL: mustURL(t, srv.RepoURL("app")), Ref: mustRef(t, "hostile")})
	if !errors.Is(err, helpers.ErrGitTreeEntryInvalid) {
		t.Fatalf("hostile name: %v, want ErrGitTreeEntryInvalid", err)
	}
	res, err := acquire(t, f, gitsource.Request{URL: mustURL(t, srv.RepoURL("app")), Ref: mustRef(t, "submodule")})
	if err != nil {
		t.Fatalf("submodule tree: %v", err)
	}
	assertBuiltCollection(t, res.Collections[0], "1.0.0")
	joined := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joined, "submodule") {
		t.Fatalf("no submodule warning: %q", joined)
	}
}

// TestMultiCollectionRepository proves discovery over a real fetched tree:
// two collections under a subdir, and Only restricting the build to one.
func TestMultiCollectionRepository(t *testing.T) {
	t.Parallel()
	r := fakegit.NewRepo(t)
	r.AddCollection("collections/one", "acme", "one", "0.1.0", nil, nil)
	r.AddCollection("collections/two", "acme", "two", "0.2.0", map[string]string{"acme.one": "*"}, nil)
	head := r.Commit("mono")
	r.Branch("main", head)
	r.SetHEAD("main")
	srv := fakegit.New(t)
	srv.Add("mono", r)
	f := newFetcher(t)

	res, err := acquire(t, f, gitsource.Request{URL: mustURL(t, srv.RepoURL("mono")), Ref: mustRef(t, ""), Subdir: "collections"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if len(res.Collections) != 2 {
		t.Fatalf("built %d collections, want 2", len(res.Collections))
	}
	for _, c := range res.Collections {
		if c.Subdir != "collections/"+c.Name {
			t.Fatalf("Subdir = %q for %s", c.Subdir, c.Name)
		}
	}
	only, err := acquire(t, f, gitsource.Request{
		URL: mustURL(t, srv.RepoURL("mono")), Ref: mustRef(t, ""), Subdir: "collections/two",
		Only: &gitsource.Identity{Namespace: "acme", Name: "two", Version: "0.2.0"},
	})
	if err != nil || len(only.Collections) != 1 {
		t.Fatalf("Only: %v (%d collections)", err, len(only.Collections))
	}
	_, err = acquire(t, f, gitsource.Request{
		URL: mustURL(t, srv.RepoURL("mono")), Ref: mustRef(t, ""), Subdir: "collections/two",
		Only: &gitsource.Identity{Namespace: "acme", Name: "two", Version: "9.9.9"},
	})
	if !errors.Is(err, helpers.ErrGitArtifactIdentityMismatch) {
		t.Fatalf("Only with another version: %v, want ErrGitArtifactIdentityMismatch", err)
	}
	_, err = acquire(t, f, gitsource.Request{URL: mustURL(t, srv.RepoURL("mono")), Ref: mustRef(t, ""), Subdir: "nowhere"})
	if !errors.Is(err, helpers.ErrGitCollectionNotFound) {
		t.Fatalf("absent subdir: %v, want ErrGitCollectionNotFound", err)
	}
}

// TestCrossOriginRedirectIsRefused proves the git HTTP client refuses to
// follow an advertisement redirect off the origin, so the second server never
// sees the credential.
func TestCrossOriginRedirectIsRefused(t *testing.T) {
	t.Parallel()
	app := newAppRepo(t)
	target := fakegit.New(t)
	target.Add("app", app.repo)
	src := fakegit.New(t)
	src.Add("app", app.repo)
	src.Fail(fakegit.EndpointInfoRefs, "app", fakegit.Fault{Redirect: target.RepoURL("app") + "/info/refs?service=git-upload-pack", Count: 1})
	_, err := acquire(t, newFetcher(t), gitsource.Request{
		URL: mustURL(t, src.RepoURL("app")), Ref: mustRef(t, ""),
		Auth: gitsource.Credential{Username: "ci", Password: "token"},
	})
	if !errors.Is(err, helpers.ErrGitTransportFailed) {
		t.Fatalf("cross-origin redirect: %v, want ErrGitTransportFailed", err)
	}
	if target.Total() != 0 {
		t.Fatalf("the redirect target received %d requests", target.Total())
	}
}

// TestTLSServerIsTrustedThroughTheInjectedClient proves the Fetcher runs on
// the client it was built with, which is how a run's TLS trust reaches go-git.
func TestTLSServerIsTrustedThroughTheInjectedClient(t *testing.T) {
	t.Parallel()
	app := newAppRepo(t)
	srv := fakegit.New(t)
	srv.Add("app", app.repo)
	// A client refusing every request must make the acquisition fail, which
	// pins that the Fetcher's transport is the injected client.
	refusing := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errRefusedByTest
	})}
	f := New(refusing, t.TempDir)
	_, err := acquire(t, f, gitsource.Request{URL: mustURL(t, srv.RepoURL("app")), Ref: mustRef(t, "")})
	if !errors.Is(err, helpers.ErrGitTransportFailed) || !strings.Contains(err.Error(), errRefusedByTest.Error()) {
		t.Fatalf("injected client not used: %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var errRefusedByTest = errors.New("refused by the test transport")

func TestStorageDirIsRemovedAfterAcquire(t *testing.T) {
	t.Parallel()
	app := newAppRepo(t)
	srv := fakegit.New(t)
	srv.Add("app", app.repo)
	tempDir := t.TempDir()
	f := New(fetch.NewGit(testTimeout), func() string { return tempDir })
	res, err := acquire(t, f, gitsource.Request{URL: mustURL(t, srv.RepoURL("app")), Ref: mustRef(t, "")})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	entries, _ := os.ReadDir(tempDir)
	if len(entries) != 0 {
		t.Fatalf("storage left under the temp dir: %v", entries)
	}
	if _, err := os.Stat(filepath.Clean(res.Collections[0].ArtifactPath)); err != nil {
		t.Fatalf("artifact missing: %v", err)
	}
}

// newAmbiguousRepo is the app fixture with "release" spelled as both a branch
// (on the first commit) and a tag (on the dev commit).
func newAmbiguousRepo(t *testing.T) (appRepo, *fakegit.Server) {
	t.Helper()
	app := newAppRepo(t)
	app.repo.Branch("release", app.first)
	app.repo.Tag("release", app.dev)
	srv := fakegit.New(t)
	srv.Add("app", app.repo)
	return app, srv
}

// assertResolved checks an acquisition landed on one commit through one ref
// and built the expected version.
func assertResolved(t *testing.T, res gitsource.Result, refName string, commit plumbing.Hash, version string) {
	t.Helper()
	assertBuiltCollection(t, res.Collections[0], version)
	if res.RefName != refName || res.Commit != commit.String() {
		t.Fatalf("resolved to (%s, %s), want (%s, %s)", res.RefName, res.Commit, refName, commit)
	}
}

// TestAmbiguousNamePrefersTheBranch proves a short name that is both a branch
// and a tag resolves to the branch with a warning, while the qualified tag
// spelling reaches the tag silently, and Advertise picks the branch too.
func TestAmbiguousNamePrefersTheBranch(t *testing.T) {
	t.Parallel()
	app, srv := newAmbiguousRepo(t)
	f := newFetcher(t)
	u := mustURL(t, srv.RepoURL("app"))

	res, err := acquire(t, f, gitsource.Request{URL: u, Ref: mustRef(t, "release")})
	if err != nil {
		t.Fatalf("Acquire(release): %v", err)
	}
	assertResolved(t, res, "refs/heads/release", app.first, "1.2.3")
	if joined := strings.Join(res.Warnings, "\n"); !strings.Contains(joined, "names both a branch and a tag") {
		t.Fatalf("no ambiguity warning: %q", joined)
	}

	res, err = acquire(t, f, gitsource.Request{URL: u, Ref: mustRef(t, "refs/tags/release")})
	if err != nil {
		t.Fatalf("Acquire(refs/tags/release): %v", err)
	}
	assertResolved(t, res, "refs/tags/release", app.dev, "1.3.0")
	if len(res.Warnings) != 0 {
		t.Fatalf("qualified tag raised warnings: %q", res.Warnings)
	}

	commit, refName, err := f.Advertise(t.Context(), u, mustRef(t, "release"), gitsource.Credential{})
	if err != nil || commit != app.first.String() || refName != "refs/heads/release" {
		t.Fatalf("Advertise = (%s, %s, %v), want the branch at %s", commit, refName, err, app.first)
	}
}

// TestAnnotatedTagIsWantedAsTheTagObject pins that an annotated tag is wanted
// as the tag object, not its peeled commit, against a remote that, like git
// without allow-reachable-sha1-in-want, refuses a want that is not a tip.
func TestAnnotatedTagIsWantedAsTheTagObject(t *testing.T) {
	t.Parallel()
	app := newAppRepo(t)
	srv := fakegit.New(t)
	srv.Add("app", app.repo)
	srv.SetCapabilities("app", fakegit.Capabilities{Shallow: true})
	res, err := acquire(t, newFetcher(t), gitsource.Request{URL: mustURL(t, srv.RepoURL("app")), Ref: mustRef(t, "v1.0.0")})
	if err != nil {
		t.Fatalf("Acquire(v1.0.0): %v", err)
	}
	assertResolved(t, res, "refs/tags/v1.0.0", app.first, "1.2.3")
}
