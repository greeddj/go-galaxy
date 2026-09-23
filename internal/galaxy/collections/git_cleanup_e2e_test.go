package collections_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/cleanup"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// mutateStoreSnapshot loads the fixture's snapshot under the backend's
// exclusive lock, hands it to mutate and saves it back, simulating state an
// earlier binary or --clear-cache left. Safe only between runs.
func mutateStoreSnapshot(t *testing.T, f *gitFixture, mutate func(st *store.Store)) {
	t.Helper()
	ctx := context.Background()
	backend, err := cacheBackend.New(f.cfg, f.runtime)
	if err != nil {
		t.Fatalf("cacheBackend.New: %v", err)
	}
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()
	lockCtx, release, err := backend.Lock(ctx)
	if err != nil {
		t.Fatalf("backend.Lock: %v", err)
	}
	defer func() { _ = release() }()
	st, err := backend.LoadStore(lockCtx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	mutate(st)
	if err := backend.SaveStore(lockCtx, st); err != nil {
		t.Fatalf("backend.SaveStore: %v", err)
	}
}

// gitArtifactPath is where the store keeps the tarball built for a git
// collection: keyed by its locator, never by a server.
func gitArtifactPath(f *gitFixture, locator, name, version string) string {
	return filepath.Join(f.cacheDir, helpers.ArtifactKey(locator, acmeArtifactFilename(name, version)))
}

func mustCleanup(t *testing.T, f *gitFixture) {
	t.Helper()
	if err := cleanup.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("cleanup.Start: %v", err)
	}
}

// TestGitCleanupKeepsPinnedCollection asserts a git-installed collection and
// its artifact survive cleanup through the recorded pin and, once the pin is
// gone, through the installed record's locator naming the same repository.
func TestGitCleanupKeepsPinnedCollection(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\n")
	f.mustInstall(t)
	locator := gitsource.Locator{URL: gitAppURL, Commit: fakeCommit("app-1")}.String()
	artifact := gitArtifactPath(f, locator, "app", "1.2.3")

	mustCleanup(t, f)
	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("pinned git artifact after cleanup: %v", err)
	}

	mutateStoreSnapshot(t, f, func(st *store.Store) { st.ClearCaches() })
	mustCleanup(t, f)
	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("git artifact after cleanup without a pin: %v", err)
	}
}

// TestGitCleanupRemovesForeignLocatorWithoutPin asserts that with no pin, a
// record whose locator names another repository is unreachable: cleanup removes
// its tree, artifact and Galaxy dependency. While the pin exists it is kept.
func TestGitCleanupRemovesForeignLocatorWithoutPin(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\n")
	f.mustInstall(t)
	foreign := gitsource.Locator{URL: "https://git.example/other/app.git", Commit: fakeCommit("app-2")}.String()
	stale := gitArtifactPath(f, foreign, "app", "1.2.3")
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	mutateStoreSnapshot(t, f, func(st *store.Store) {
		entry, ok := st.GetInstalled("acme.app@1.2.3")
		if !ok {
			t.Fatal("no installed record for acme.app@1.2.3")
		}
		entry.Source = foreign
		st.SetInstalled("acme.app@1.2.3", entry)
	})
	// With the pin still recorded, the pin is the authority and the record's
	// locator is not consulted: the tree stays. This is what separates the
	// two branches, so the removal below is attributable to the fallback.
	mustCleanup(t, f)
	assertManifestInstalled(t, f.downloadPath, "app")

	mutateStoreSnapshot(t, f, func(st *store.Store) { st.ClearCaches() })
	mustCleanup(t, f)
	assertPathAbsent(t, installPathFor(f.downloadPath, "app"))
	assertPathAbsent(t, installPathFor(f.downloadPath, "lib"))
	assertPathAbsent(t, stale)
}

// TestGitCleanupNarrowsToTheNamedCollection asserts a multi-collection
// repository installed whole, then re-declared as one named collection, loses
// its sibling's tree and artifact on cleanup while the named one stays.
func TestGitCleanupNarrowsToTheNamedCollection(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.writeRequirements(t, "collections:\n  - "+gitMonoURL+"#collections\n")
	f.mustInstall(t)
	assertManifestInstalled(t, f.downloadPath, "one")
	assertManifestInstalled(t, f.downloadPath, "two")
	twoArtifact := gitArtifactPath(f,
		gitsource.Locator{URL: gitMonoURL, Subdir: "collections/two", Commit: fakeCommit("mono-1")}.String(), "two", "0.2.0")
	oneArtifact := gitArtifactPath(f,
		gitsource.Locator{URL: gitMonoURL, Subdir: "collections/one", Commit: fakeCommit("mono-1")}.String(), "one", "0.1.0")
	if _, err := os.Stat(twoArtifact); err != nil {
		t.Fatalf("acme.two artifact after install: %v", err)
	}

	f.writeRequirements(t, "collections:\n  - name: acme.one\n    type: git\n    source: "+gitMonoURL+"#collections\n")
	mustCleanup(t, f)
	assertManifestInstalled(t, f.downloadPath, "one")
	assertPathAbsent(t, installPathFor(f.downloadPath, "two"))
	assertPathAbsent(t, twoArtifact)
	if _, err := os.Stat(oneArtifact); err != nil {
		t.Fatalf("acme.one artifact after cleanup: %v", err)
	}
}
