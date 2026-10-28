package collections

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

type discoveredIdentityCase struct {
	wantErr   error
	deps      map[string]string
	name      string
	namespace string
	collName  string
	version   string
}

// discoveredIdentityCases are the refusals git and url discovery share: a
// name part outside the alphabet, an inexact version, a dependency key that
// is not an fqdn.
func discoveredIdentityCases() []discoveredIdentityCase {
	return []discoveredIdentityCase{
		{
			name: "namespace outside the alphabet", namespace: "acme!", collName: "app", version: refetchVersion,
			wantErr: helpers.ErrInvalidCollectionName,
		},
		{
			name: "version that is not exact", namespace: "acme", collName: "app", version: ">=1.0.0",
			wantErr: helpers.ErrGitCollectionVersionNotExact,
		},
		{
			name: "dependency key that is not an fqdn", namespace: "acme", collName: "app", version: refetchVersion,
			deps: map[string]string{"lib": "*"}, wantErr: helpers.ErrInvalidDependencyKey,
		},
	}
}

// assertDiscoveredIdentityRefused fails the test unless err carries wantErr
// and names display, the source the identity was read from.
func assertDiscoveredIdentityRefused(t *testing.T, tt discoveredIdentityCase, err error, display string) {
	t.Helper()
	if !errors.Is(err, tt.wantErr) || !strings.Contains(err.Error(), display) {
		t.Errorf("%s: error = %v, want %v naming %s", tt.name, err, tt.wantErr, display)
	}
}

// TestGitCollectionRootJudgesTheIdentity pins the identity a git build
// declares, refused like a Galaxy answer, and the root and pin it yields.
func TestGitCollectionRootJudgesTheIdentity(t *testing.T) {
	t.Parallel()
	u, err := gitsource.ParseURL(refetchRepoURL)
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	req := gitRootRequest{url: u, ref: gitsource.Ref{Name: "refs/heads/main"}, display: refetchRepoURL}
	for _, tt := range discoveredIdentityCases() {
		_, _, err := gitCollectionRoot(req, refetchCommit, tt.namespace, tt.collName, tt.version, "", tt.deps)
		assertDiscoveredIdentityRefused(t, tt, err, refetchRepoURL)
	}

	deps := map[string]string{"acme.lib": " >=1.0.0 "}
	col, pin, err := gitCollectionRoot(req, refetchCommit, "acme", "app", refetchVersion, "collections/app", deps)
	if err != nil {
		t.Fatalf("gitCollectionRoot() error = %v", err)
	}
	want := gitsource.Locator{URL: refetchRepoURL, Subdir: "collections/app", Commit: refetchCommit}.String()
	wantCol := collection{
		Namespace: "acme", Name: "app", Version: refetchVersion, Source: want, Constraint: refetchVersion, Type: typeGit, Ref: "refs/heads/main",
	}
	if !reflect.DeepEqual(col, wantCol) {
		t.Fatalf("gitCollectionRoot() = %+v, want %+v", col, wantCol)
	}
	wantPin := gitPin{locator: want, ref: "refs/heads/main", version: refetchVersion, deps: map[string]string{"acme.lib": ">=1.0.0"}}
	if !reflect.DeepEqual(pin, wantPin) {
		t.Fatalf("gitCollectionRoot() pin = %+v, want %+v", pin, wantPin)
	}
}

// TestURLCollectionRootJudgesTheIdentity pins the url counterpart, whose
// relaxed alphabet takes a mixed-case namespace but still refuses punctuation.
func TestURLCollectionRootJudgesTheIdentity(t *testing.T) {
	t.Parallel()
	const tarball = "https://dl.example/acme-app.tar.gz"
	req := urlRootRequest{rawURL: tarball, display: tarball}
	sha := strings.Repeat("e", 64)
	for _, tt := range discoveredIdentityCases() {
		_, _, err := urlCollectionRoot(req, sha, tt.namespace, tt.collName, tt.version, tt.deps)
		assertDiscoveredIdentityRefused(t, tt, err, tarball)
	}

	col, pin, err := urlCollectionRoot(req, sha, "Acme", "app", refetchVersion, nil)
	want := urlsource.Locator{URL: tarball, SHA256: sha}.String()
	if err != nil || col.Source != want || col.SHA256 != sha || pin.locator != want {
		t.Fatalf("urlCollectionRoot() = (%+v, %+v, %v), want the pinned locator %s", col, pin, err, want)
	}
}

// TestExpandRootsRefuseTornLocators pins that a git or url root whose locator
// or ref does not parse is refused before any remote is asked, and that git
// roots need a git client.
func TestExpandRootsRefuseTornLocators(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := &scriptedGitClient{}
	runtime := infra.New(noopPrinter{}, nil)
	runtime.Git = client
	deps := newCollectionDeps(&config.Config{Server: snapshotServer}, runtime, store.New())

	tornGit := collection{Type: typeGit, Source: gitsource.LocatorPrefix + "x"}
	if _, err := expandGitRoot(ctx, deps, tornGit, lockedGitRoot{}); !errors.Is(err, helpers.ErrInvalidGitLocator) {
		t.Errorf("expandGitRoot(torn locator) error = %v, want ErrInvalidGitLocator", err)
	}
	abbreviated := collection{Type: typeGit, Source: snapGitUnpinned(), Ref: "abcdef1"}
	if _, err := expandGitRoot(ctx, deps, abbreviated, lockedGitRoot{}); !errors.Is(err, helpers.ErrGitAbbreviatedCommit) {
		t.Errorf("expandGitRoot(abbreviated ref) error = %v, want ErrGitAbbreviatedCommit", err)
	}
	tornURL := collection{Type: typeURL, Source: urlsource.LocatorPrefix + "x"}
	if _, err := expandURLRoot(ctx, deps, tornURL); !errors.Is(err, helpers.ErrInvalidURLLocator) {
		t.Errorf("expandURLRoot(torn locator) error = %v, want ErrInvalidURLLocator", err)
	}
	if client.acquires != 0 {
		t.Fatalf("git acquisitions = %d, want 0", client.acquires)
	}

	deps.runtime = infra.New(noopPrinter{}, nil)
	unpinned := []collection{{Type: typeGit, Source: snapGitUnpinned()}}
	if _, err := expandGitRoots(ctx, deps, unpinned); !errors.Is(err, helpers.ErrConfigIsNil) {
		t.Errorf("expandGitRoots(no git client) error = %v, want ErrConfigIsNil", err)
	}
}
