package collections_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// fakeGitRepo is one repository the in-memory git client serves: refs
// mapping to commits, and per commit the collections it carries under each
// subdir. Commits are forty-hex strings the test chooses.
type fakeGitRepo struct {
	refs    map[string]string              // full ref name or "HEAD" -> commit
	commits map[string][]fakeGitCollection // commit -> collections
	roles   map[string]fakeGitRole         // commit -> the role at the repository root
}

type fakeGitCollection struct {
	deps      map[string]string
	namespace string
	name      string
	version   string
	subdir    string
}

// fakeGitClient implements gitsource.Client without go-git, answering from
// fakeGitRepo and building through fakegalaxy.BuildArtifact, and counts calls
// and captures the credential presented per URL for assertions.
type fakeGitClient struct {
	failWith     error
	repos        map[string]*fakeGitRepo
	seenAuth     map[string]gitsource.Credential
	advertises   int
	acquires     int
	roleAcquires int
	mu           sync.Mutex
}

func newFakeGitClient() *fakeGitClient {
	return &fakeGitClient{repos: make(map[string]*fakeGitRepo), seenAuth: make(map[string]gitsource.Credential)}
}

// errFakeGitNoTempFile is the fake client's own failure for a request that
// came without a temp file supplier, a wiring mistake in the test itself.
var errFakeGitNoTempFile = errors.New("fake git client: no temp file supplier")

// lookup maps ref to (commit, full ref name) inside one repository. A bare
// name tries heads before tags, as git's own short-ref resolution does.
func (r *fakeGitRepo) lookup(ref gitsource.Ref) (string, string, error) {
	switch ref.Kind {
	case gitsource.RefCommit:
		if !r.hasCommit(ref.Name) {
			return "", "", fmt.Errorf("%w: %s", helpers.ErrGitCommitNotFound, ref.Name)
		}
		return ref.Name, ref.Name, nil
	case gitsource.RefHEAD:
		commit, ok := r.refs["HEAD"]
		if !ok {
			return "", "", fmt.Errorf("%w: no HEAD", helpers.ErrGitRefNotFound)
		}
		return commit, "refs/heads/main", nil
	case gitsource.RefQualified:
		commit, ok := r.refs[ref.Name]
		if !ok {
			return "", "", fmt.Errorf("%w: %s", helpers.ErrGitRefNotFound, ref.Name)
		}
		return commit, ref.Name, nil
	case gitsource.RefName:
		return r.lookupShortName(ref.Name)
	default:
		return "", "", fmt.Errorf("%w: unknown ref kind %d", helpers.ErrGitRefNotFound, ref.Kind)
	}
}

// hasCommit reports whether the repository holds commit, as a collection
// commit or a role commit.
func (r *fakeGitRepo) hasCommit(commit string) bool {
	_, col := r.commits[commit]
	_, role := r.roles[commit]
	return col || role
}

// lookupShortName resolves a bare ref name, heads before tags.
func (r *fakeGitRepo) lookupShortName(name string) (string, string, error) {
	for _, prefix := range [...]string{"refs/heads/", "refs/tags/"} {
		if commit, ok := r.refs[prefix+name]; ok {
			return commit, prefix + name, nil
		}
	}
	return "", "", fmt.Errorf("%w: %s", helpers.ErrGitRefNotFound, name)
}

func (c *fakeGitClient) Advertise(
	_ context.Context, u gitsource.URL, ref gitsource.Ref, auth gitsource.Credential,
) (string, string, error) {
	c.mu.Lock()
	c.advertises++
	c.mu.Unlock()
	_, commit, refName, err := c.resolve(u, ref, auth)
	return commit, refName, err
}

func (c *fakeGitClient) Acquire(ctx context.Context, req gitsource.Request) (gitsource.Result, error) {
	c.mu.Lock()
	c.acquires++
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return gitsource.Result{}, err
	}
	repo, commit, refName, err := c.resolve(req.URL, req.Ref, req.Auth)
	if err != nil {
		return gitsource.Result{}, err
	}
	if req.Commit != "" {
		commit = req.Commit
		if _, ok := repo.commits[commit]; !ok {
			return gitsource.Result{}, fmt.Errorf("%w: %s", helpers.ErrGitCommitNotFound, commit)
		}
	}
	result := gitsource.Result{Commit: commit, RefName: refName}
	for _, fc := range repo.commits[commit] {
		built, matched, err := buildIfRequested(ctx, req, fc)
		if err != nil {
			return gitsource.Result{}, err
		}
		if !matched {
			continue
		}
		result.Collections = append(result.Collections, built)
		result.BytesFetched += 1024
	}
	if len(result.Collections) == 0 {
		return gitsource.Result{}, fmt.Errorf("%w: nothing under %q", helpers.ErrGitCollectionNotFound, req.Subdir)
	}
	return result, nil
}

// buildIfRequested builds fc when the request's subdir and Only filter select
// it; matched is false otherwise. A collection Only selects at another version
// is the identity mismatch the pipeline must refuse.
func buildIfRequested(ctx context.Context, req gitsource.Request, fc fakeGitCollection) (gitsource.Collection, bool, error) {
	if !subdirMatches(fc.subdir, req.Subdir) {
		return gitsource.Collection{}, false, nil
	}
	if req.Only != nil {
		if fc.namespace != req.Only.Namespace || fc.name != req.Only.Name {
			return gitsource.Collection{}, false, nil
		}
		if fc.version != req.Only.Version {
			return gitsource.Collection{}, false, fmt.Errorf("%w: built %s", helpers.ErrGitArtifactIdentityMismatch, fc.version)
		}
	}
	built, err := writeFakeArtifact(ctx, req.TempFile, fc)
	if err != nil {
		return gitsource.Collection{}, false, err
	}
	return built, true, nil
}

// resolve records the credential offered for u and maps ref to a commit and
// a full ref name the way an advertisement would.
func (c *fakeGitClient) resolve(u gitsource.URL, ref gitsource.Ref, auth gitsource.Credential) (*fakeGitRepo, string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seenAuth[u.String()] = auth
	if c.failWith != nil {
		return nil, "", "", c.failWith
	}
	repo, ok := c.repos[u.String()]
	if !ok {
		return nil, "", "", fmt.Errorf("%w: %s: repository not found", helpers.ErrGitTransportFailed, u.String())
	}
	commit, refName, err := repo.lookup(ref)
	if err != nil {
		return nil, "", "", err
	}
	return repo, commit, refName, nil
}

func (c *fakeGitClient) add(url string, repo *fakeGitRepo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repos[url] = repo
}

func (c *fakeGitClient) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.advertises, c.acquires
}

func (c *fakeGitClient) resetCounts() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advertises, c.acquires = 0, 0
}

// subdirMatches mirrors discovery: a collection sits at the requested subdir
// itself or is an immediate child of it.
func subdirMatches(collectionSubdir, requested string) bool {
	if collectionSubdir == requested {
		return true
	}
	parent := ""
	if i := strings.LastIndex(collectionSubdir, "/"); i >= 0 {
		parent = collectionSubdir[:i]
	}
	return parent == requested
}

func writeFakeArtifact(ctx context.Context, tempFile gitsource.TempFileFunc, fc fakeGitCollection) (gitsource.Collection, error) {
	if tempFile == nil {
		return gitsource.Collection{}, errFakeGitNoTempFile
	}
	data, sha := fakegalaxy.BuildArtifact(fc.namespace, fc.name, fc.version, fc.deps)
	f, cleanup, err := tempFile(ctx)
	if err != nil {
		return gitsource.Collection{}, err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return gitsource.Collection{}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return gitsource.Collection{}, err
	}
	return gitsource.Collection{
		Cleanup:      cleanup,
		Dependencies: fc.deps,
		Namespace:    fc.namespace,
		Name:         fc.name,
		Version:      fc.version,
		Subdir:       fc.subdir,
		ArtifactPath: f.Name(),
		ArtifactSHA:  sha,
	}, nil
}

// fakeCommit returns a deterministic forty-hex commit for a label.
func fakeCommit(label string) string {
	h := 0
	for _, r := range label {
		h = h*31 + int(r)
	}
	return fmt.Sprintf("%040x", h&0xffffff)
}

// assertTempFilesGone fails when a download temp this run wrote still sits
// in dir: every built artifact is either committed (renamed away) or removed.
func assertTempFilesGone(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), helpers.ArtifactDownloadTempPrefix) {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}
