package collections

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/metrics"
)

const (
	refetchRepoURL = "https://git.example/acme/app.git"
	refetchCommit  = "1111111111111111111111111111111111111111"
	refetchOther   = "2222222222222222222222222222222222222222"
	refetchRef     = "main"
	refetchVersion = "1.4.2"
)

// scriptedGitClient answers every acquisition with a fixed result or error
// and keeps the last request, so the request and the handling of the answer
// are observable.
type scriptedGitClient struct {
	acquireErr  error
	lastRequest *gitsource.Request
	lastRole    *gitsource.RoleRequest
	result      gitsource.Result
	roleResult  gitsource.RoleResult
	acquires    int
}

func (c *scriptedGitClient) Advertise(context.Context, gitsource.URL, gitsource.Ref, gitsource.Credential) (string, string, error) {
	return "", "", errStubNotImplemented
}

func (c *scriptedGitClient) Acquire(_ context.Context, req gitsource.Request) (gitsource.Result, error) {
	c.acquires++
	c.lastRequest = &req
	if c.acquireErr != nil {
		return gitsource.Result{}, c.acquireErr
	}
	return c.result, nil
}

func (c *scriptedGitClient) AcquireRole(_ context.Context, req gitsource.RoleRequest) (gitsource.RoleResult, error) {
	c.acquires++
	c.lastRole = &req
	if c.acquireErr != nil {
		return gitsource.RoleResult{}, c.acquireErr
	}
	return c.roleResult, nil
}

// newRefetchDeps builds installDeps over a fresh cache and install tree with
// client wired as the run's git client and a capturing printer.
func newRefetchDeps(t *testing.T, client gitsource.Client) (installDeps, *capturingPrinter) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{CacheDir: filepath.Join(root, "cache"), DownloadPath: filepath.Join(root, "install"), Workers: 1}
	if err := os.MkdirAll(cfg.CacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	deps := newTestInstallDeps(t, cfg)
	printer := &capturingPrinter{}
	deps.runtime.Output = printer
	deps.runtime.Git = client
	return deps, printer
}

// writeBuiltArtifact writes stand-in artifact bytes under dir and returns the
// path and their sha256; the refetch never reads them, only moves them.
func writeBuiltArtifact(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	data := []byte("stand-in artifact " + name)
	path := filepath.Join(dir, name)
	mustWriteFile(t, path, data)
	return path, sha256Hex(data)
}

// refetchCollection is acme.app pinned to refetchCommit on refetchRef.
func refetchCollection() collection {
	return collection{
		Namespace: "acme", Name: "app", Version: refetchVersion, Ref: refetchRef,
		Source: gitsource.Locator{URL: refetchRepoURL, Subdir: "collections/app", Commit: refetchCommit}.String(),
	}
}

// refetchOutcome is what a refetch refusal test observes: the result and
// error, how often the remote was asked, and how often a build was released.
type refetchOutcome struct {
	err      error
	got      downloadResult
	acquires int
	released int
}

// assertRefetchRefused fails the test unless the refetch failed with wantErr,
// returned the zero result, asked the remote wantAcquires times and released
// wantReleased builds.
func assertRefetchRefused(t *testing.T, out refetchOutcome, wantErr error, wantAcquires, wantReleased int) {
	t.Helper()
	if !errors.Is(out.err, wantErr) {
		t.Fatalf("refetch error = %v, want %v", out.err, wantErr)
	}
	if out.got.Path != "" || out.got.Cleanup != nil {
		t.Fatalf("refetch = %+v on a refusal, want the zero result", out.got)
	}
	if out.acquires != wantAcquires {
		t.Fatalf("acquisitions = %d, want %d", out.acquires, wantAcquires)
	}
	if out.released != wantReleased {
		t.Fatalf("released %d builds, want %d", out.released, wantReleased)
	}
}

// assertRefetchReported fails the test unless warning reached the warn tier
// prefixed by the repository, and the metrics show bytes and misses.
func assertRefetchReported(t *testing.T, printer *capturingPrinter, totals metrics.Totals, warning string, bytes, misses int64) {
	t.Helper()
	if !printer.hasWarnContaining(refetchRepoURL + ": " + warning) {
		t.Fatalf("warnings = %v, want %q prefixed by the repository", printer.warns, warning)
	}
	if totals.BytesDownloaded != bytes || totals.CacheMisses != misses {
		t.Fatalf("metrics = %+v, want %d bytes and %d misses", totals, bytes, misses)
	}
}

type gitFetchRefusalCase struct {
	acquireErr   error
	wantErr      error
	name         string
	source       string
	ref          string
	built        []gitsource.Identity
	wantAcquires int
	noClient     bool
}

func gitFetchRefusalCases() []gitFetchRefusalCase {
	app := gitsource.Identity{Namespace: "acme", Name: "app", Version: refetchVersion}
	return []gitFetchRefusalCase{
		{name: "no git client wired", noClient: true, wantErr: helpers.ErrConfigIsNil},
		{name: "source is not a git locator", source: "https://hub.example/", wantErr: helpers.ErrInvalidGitLocator},
		{
			name:    "locator without a commit",
			source:  gitsource.Locator{URL: refetchRepoURL}.String(),
			wantErr: helpers.ErrInvalidGitLocator,
		},
		{name: "abbreviated commit as ref", ref: "abcdef1", wantErr: helpers.ErrGitAbbreviatedCommit},
		{name: "remote lost the commit", acquireErr: helpers.ErrGitCommitNotFound, wantErr: helpers.ErrGitCommitNotFound, wantAcquires: 1},
		{name: "nothing built", wantErr: helpers.ErrGitArtifactIdentityMismatch, wantAcquires: 1},
		{
			name:         "two collections built",
			built:        []gitsource.Identity{app, {Namespace: "acme", Name: "lib", Version: "1.0.0"}},
			wantErr:      helpers.ErrGitArtifactIdentityMismatch,
			wantAcquires: 1,
		},
		{
			name:         "another version built",
			built:        []gitsource.Identity{{Namespace: "acme", Name: "app", Version: "9.9.9"}},
			wantErr:      helpers.ErrGitArtifactIdentityMismatch,
			wantAcquires: 1,
		},
	}
}

// runGitFetchRefusal runs one refusal case against a scripted remote that
// builds tt.built, each build counting its own release.
func runGitFetchRefusal(t *testing.T, tt gitFetchRefusalCase) refetchOutcome {
	t.Helper()
	client := &scriptedGitClient{acquireErr: tt.acquireErr}
	deps, _ := newRefetchDeps(t, client)
	if tt.noClient {
		deps.runtime.Git = nil
	}
	out := refetchOutcome{}
	for _, id := range tt.built {
		client.result.Collections = append(client.result.Collections, gitsource.Collection{
			Namespace: id.Namespace, Name: id.Name, Version: id.Version, Cleanup: func() { out.released++ },
		})
	}
	col := refetchCollection()
	if tt.source != "" {
		col.Source = tt.source
	}
	if tt.ref != "" {
		col.Ref = tt.ref
	}
	got, err := gitFetchToCache(context.Background(), deps, col, true)
	out.got, out.err, out.acquires = got, err, client.acquires
	return out
}

// TestGitFetchToCacheRefusals pins the pinned-commit refetch's refusals:
// wiring, locator and ref before any fetch, and a build other than the pinned
// identity, with every build released.
func TestGitFetchToCacheRefusals(t *testing.T) {
	t.Parallel()
	for _, tt := range gitFetchRefusalCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertRefetchRefused(t, runGitFetchRefusal(t, tt), tt.wantErr, tt.wantAcquires, len(tt.built))
		})
	}
}

// assertRestrictedRefetchRequest fails the test unless req asks for the
// pinned commit, subdir and ref and is restricted to the pinned identity.
func assertRestrictedRefetchRequest(t *testing.T, req *gitsource.Request) {
	t.Helper()
	if req == nil || req.Commit != refetchCommit || req.Subdir != "collections/app" || req.Ref.Name != refetchRef {
		t.Fatalf("Acquire request = %+v, want commit %s, subdir collections/app and ref %s", req, refetchCommit, refetchRef)
	}
	if req.Only == nil || *req.Only != (gitsource.Identity{Namespace: "acme", Name: "app", Version: refetchVersion}) {
		t.Fatalf("Acquire request Only = %+v, want acme.app@%s", req.Only, refetchVersion)
	}
}

// TestGitFetchToCacheWithoutCacheHandsBackTheBuild pins the --no-cache arm:
// the request is restricted to the pinned identity and commit, warnings and
// fetched bytes are reported, and the build is returned uncommitted.
func TestGitFetchToCacheWithoutCacheHandsBackTheBuild(t *testing.T) {
	t.Parallel()
	client := &scriptedGitClient{}
	deps, printer := newRefetchDeps(t, client)
	path, sha := writeBuiltArtifact(t, t.TempDir(), "app.tar.gz")
	client.result = gitsource.Result{
		Commit:       refetchCommit,
		BytesFetched: 4096,
		Warnings:     []string{"symlink skipped"},
		Collections: []gitsource.Collection{
			{Namespace: "acme", Name: "app", Version: refetchVersion, ArtifactPath: path, ArtifactSHA: sha},
		},
	}
	col := refetchCollection()

	got, err := gitFetchToCache(context.Background(), deps, col, false)
	if err != nil || got.Path != path || got.SHA != sha {
		t.Fatalf("gitFetchToCache() = (%q, %q, %v), want the build (%q, %q)", got.Path, got.SHA, err, path, sha)
	}
	assertRestrictedRefetchRequest(t, client.lastRequest)
	assertRefetchReported(t, printer, deps.runtime.Metrics.Totals(), "symlink skipped", 4096, 0)
	if _, err := os.Stat(filepath.Join(deps.cfg.CacheDir, artifactKey(col))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cache slot stat error = %v, want nothing committed", err)
	}
}

// TestGitFetchToCacheCommitsTheBuild pins the cached arm: the build moves
// into col's artifact key and counts one miss.
func TestGitFetchToCacheCommitsTheBuild(t *testing.T) {
	t.Parallel()
	col := refetchCollection()
	client := &scriptedGitClient{}
	deps, _ := newRefetchDeps(t, client)
	path, sha := writeBuiltArtifact(t, t.TempDir(), "app.tar.gz")
	client.result.Collections = []gitsource.Collection{
		{Namespace: "acme", Name: "app", Version: refetchVersion, ArtifactPath: path, ArtifactSHA: sha},
	}

	got, err := gitFetchToCache(context.Background(), deps, col, true)
	if err != nil {
		t.Fatalf("gitFetchToCache() error = %v", err)
	}
	if want := filepath.Join(deps.cfg.CacheDir, artifactKey(col)); got.Path != want || got.SHA != sha {
		t.Fatalf("gitFetchToCache() = (%q, %q), want (%q, %q)", got.Path, got.SHA, want, sha)
	}
	assertFileSHA256(t, got.Path, sha)
	if misses := deps.runtime.Metrics.Totals().CacheMisses; misses != 1 {
		t.Fatalf("cache misses = %d, want 1", misses)
	}
}

// TestGitFetchToCacheReleasesAFailedCommit pins that a commit the store
// refuses releases the build, counts no miss and reports the store's error.
func TestGitFetchToCacheReleasesAFailedCommit(t *testing.T) {
	t.Parallel()
	client := &scriptedGitClient{}
	deps, _ := newRefetchDeps(t, client)
	deps.artifacts = local.NewArtifacts(filepath.Join(t.TempDir(), "absent"))
	path, sha := writeBuiltArtifact(t, t.TempDir(), "app.tar.gz")
	released := 0
	client.result.Collections = []gitsource.Collection{{
		Namespace: "acme", Name: "app", Version: refetchVersion, ArtifactPath: path, ArtifactSHA: sha,
		Cleanup: func() { released++ },
	}}

	_, err := gitFetchToCache(context.Background(), deps, refetchCollection(), true)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("gitFetchToCache() error = %v, want the store's fs.ErrNotExist", err)
	}
	if released != 1 || deps.runtime.Metrics.Totals().CacheMisses != 0 {
		t.Fatalf("released %d times with %+v, want one release and no miss", released, deps.runtime.Metrics.Totals())
	}
}

// TestSingleBuiltIdentityNamesTheMismatch pins that the identity refusal names
// what was built and what was pinned, so the operator sees both sides.
func TestSingleBuiltIdentityNamesTheMismatch(t *testing.T) {
	t.Parallel()
	col := refetchCollection()
	_, err := singleBuiltIdentity([]gitsource.Collection{{Namespace: "acme", Name: "app", Version: "2.5.0"}}, col, "repo")
	if !errors.Is(err, helpers.ErrGitArtifactIdentityMismatch) {
		t.Fatalf("singleBuiltIdentity() error = %v, want ErrGitArtifactIdentityMismatch", err)
	}
	for _, want := range []string{"repo built acme.app@2.5.0", "for acme.app@" + refetchVersion} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("singleBuiltIdentity() error = %v, want it to contain %q", err, want)
		}
	}
}
