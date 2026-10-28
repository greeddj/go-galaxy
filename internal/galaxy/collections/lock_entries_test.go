package collections

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// TestSourceLockfileEntryRefusesUnpinnedSources pins that a torn or
// unpinned git or url locator is a handled refusal naming the collection.
func TestSourceLockfileEntryRefusesUnpinnedSources(t *testing.T) {
	t.Parallel()
	refused := map[string]struct {
		wantErr error
		source  string
	}{
		"torn git locator":     {source: gitsource.LocatorPrefix + "x", wantErr: helpers.ErrInvalidGitLocator},
		"unpinned git locator": {source: snapGitUnpinned(), wantErr: helpers.ErrInvalidGitLocator},
		"unpinned url locator": {source: snapURLUnpinned(), wantErr: helpers.ErrInvalidURLLocator},
	}
	for name, tt := range refused {
		col := collection{Namespace: "acme", Name: "app", Version: "1.0.0", Source: tt.source}
		entry, handled, err := sourceLockfileEntry("acme.app", col, nil)
		if !handled || !errors.Is(err, tt.wantErr) || !strings.HasPrefix(err.Error(), "lockfile: acme.app: ") {
			t.Errorf("%s: sourceLockfileEntry() = (%+v, %v, %v), want a handled %v naming acme.app", name, entry, handled, err, tt.wantErr)
		}
	}
}

// TestGitLockfileEntryWithoutRefPinsTheCommit pins that a git collection that
// lost its ref is locked with its commit as the ref, and that only
// well-formed graph edges become dependencies.
func TestGitLockfileEntryWithoutRefPinsTheCommit(t *testing.T) {
	t.Parallel()
	col := collection{
		Namespace: "acme", Name: "app", Version: "1.0.0",
		Source: gitsource.Locator{URL: refetchRepoURL, Subdir: "collections/app", Commit: refetchCommit}.String(),
	}
	graph := map[string][]string{col.key(): {"acme.lib@1.0.0", "malformed"}}
	entry, handled, err := sourceLockfileEntry("acme.app", col, graph)
	want := lockfile.Entry{
		Name: "acme.app", Type: lockfile.TypeGit, Version: "1.0.0", Source: refetchRepoURL,
		Ref: refetchCommit, Commit: refetchCommit, Subdir: "collections/app", Deps: []string{"acme.lib"},
	}
	if err != nil || !handled || !reflect.DeepEqual(entry, want) {
		t.Fatalf("sourceLockfileEntry(git) = (%+v, %v, %v), want %+v", entry, handled, err, want)
	}
}

// TestCheckDownloadURLOriginNeedsAbsoluteURLs pins that a server or a locked
// download_url without scheme and host is off-origin by definition.
func TestCheckDownloadURLOriginNeedsAbsoluteURLs(t *testing.T) {
	t.Parallel()
	cases := map[string][2]string{
		"relative server":       {"hub.example/api/", "https://hub.example/download/acme-app-1.0.0.tar.gz"},
		"relative download_url": {"https://hub.example/", "/download/acme-app-1.0.0.tar.gz"},
	}
	for name, tc := range cases {
		err := checkDownloadURLOrigin(tc[0], tc[1])
		if !errors.Is(err, helpers.ErrDownloadURLOffServerOrigin) || !strings.Contains(err.Error(), "not an absolute URL") {
			t.Errorf("%s: checkDownloadURLOrigin() error = %v, want ErrDownloadURLOffServerOrigin for a non-absolute URL", name, err)
		}
	}
}

// TestIndexLockfile pins the lockfile index: a nil file and an entry that is
// not an fqdn are invalid, and only a Galaxy entry without a source takes the
// run's server.
func TestIndexLockfile(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: snapshotServer}
	if _, err := indexLockfile(nil, cfg); !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("indexLockfile(nil) error = %v, want ErrLockfileInvalid", err)
	}
	badName := &lockfile.File{Collections: []lockfile.Entry{{Name: "acme"}}}
	if _, err := indexLockfile(badName, cfg); !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("indexLockfile(bad name) error = %v, want ErrLockfileInvalid", err)
	}
	got, err := indexLockfile(&lockfile.File{Collections: []lockfile.Entry{
		{Name: "acme.app", Version: "1.0.0"},
		{Name: "acme.git", Version: "1.0.0", Type: lockfile.TypeGit},
	}}, cfg)
	if err != nil || got["acme.app"].Source != snapshotServer || got["acme.git"].Source != "" {
		t.Fatalf("indexLockfile() = (%+v, %v), want the server on the Galaxy entry only", got, err)
	}
}

type frozenRootCase struct {
	wantErr error
	name    string
	root    collection
}

// frozenRootCases are Galaxy and url roots against frozenRootsIndex: the
// ones a frozen install accepts, then each mismatch.
func frozenRootCases() []frozenRootCase {
	urlRoot := func(tarball, constraint string) collection {
		return collection{Type: typeURL, Constraint: constraint, Source: urlsource.Locator{URL: tarball}.String()}
	}
	return []frozenRootCase{
		{name: "galaxy root at its exact version", root: collection{Namespace: "acme", Name: "app", Version: "1.0.0"}},
		{name: "url root without a version", root: urlRoot(snapshotTarball, "")},
		{name: "url root asserting the locked version", root: urlRoot(snapshotTarball, "0.24.0")},
		{name: "galaxy root not locked", root: collection{Namespace: "acme", Name: "new"}, wantErr: helpers.ErrLockfileMismatch},
		{
			name:    "galaxy root outside its constraint",
			root:    collection{Namespace: "acme", Name: "app", Constraint: ">=" + testVersion200},
			wantErr: helpers.ErrLockfileMismatch,
		},
		{name: "url root asserting another version", root: urlRoot(snapshotTarball, "0.25.0"), wantErr: helpers.ErrLockfileMismatch},
		{name: "url root not locked", root: urlRoot("https://dl.example/other.tar.gz", ""), wantErr: helpers.ErrLockfileMismatch},
		{name: "torn url root", root: collection{Type: typeURL, Source: urlsource.LocatorPrefix + "x"}, wantErr: helpers.ErrInvalidURLLocator},
	}
}

// TestVerifyRootsAgainstLockfileMismatches pins --frozen's root checks for
// Galaxy and url roots: a missing entry or one outside the root's constraint
// or asserted version is a mismatch, and a matching entry passes.
func TestVerifyRootsAgainstLockfileMismatches(t *testing.T) {
	t.Parallel()
	byFQDN := map[string]lockfile.Entry{
		"acme.app": {Name: "acme.app", Version: "1.0.0"},
		"acme.kafka": {
			Name: "acme.kafka", Version: "0.24.0", Type: lockfile.TypeURL, Source: snapshotTarball, SHA256: strings.Repeat("d", 64),
		},
	}
	for _, tt := range frozenRootCases() {
		err := verifyRootsAgainstLockfile([]collection{tt.root}, byFQDN)
		if !errors.Is(err, tt.wantErr) {
			t.Errorf("%s: verifyRootsAgainstLockfile() error = %v, want %v", tt.name, err, tt.wantErr)
		}
	}
}

// TestMaterializeLockfileEdges pins that a materialized graph keeps only
// edges to locked entries and that a key that is not an fqdn is invalid.
func TestMaterializeLockfileEdges(t *testing.T) {
	t.Parallel()
	resolved, graph, err := materializeLockfile(map[string]lockfile.Entry{
		"acme.app": {Name: "acme.app", Version: "1.0.0", Deps: []string{"acme.lib", "acme.gone"}},
		"acme.lib": {Name: "acme.lib", Version: testVersion200},
	}, false)
	if err != nil || len(resolved) != 2 {
		t.Fatalf("materializeLockfile() = (%v, %v), want both entries", resolved, err)
	}
	if got := graph["acme.app@1.0.0"]; !reflect.DeepEqual(got, []string{"acme.lib@" + testVersion200}) {
		t.Fatalf("materialized edges of acme.app = %v, want only acme.lib@%s", got, testVersion200)
	}
	badKey := map[string]lockfile.Entry{"acme": {Version: "1.0.0"}}
	if _, _, err := materializeLockfile(badKey, false); !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("materializeLockfile(bad key) error = %v, want ErrLockfileInvalid", err)
	}
}
