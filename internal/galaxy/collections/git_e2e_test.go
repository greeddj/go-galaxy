package collections_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

const (
	gitAppURL   = "https://git.example/acme/app.git"
	gitMonoURL  = "git@git.example:acme/mono.git"
	gitMainRef  = "refs/heads/main"
	gitV1Ref    = "refs/tags/v1"
	gitNoCommit = ""
)

// gitFixture is the suite's git scenario: a fakegalaxy serving acme.lib, a fake
// git client holding acme.app (depending on acme.lib) and a repository with
// acme.one and acme.two under collections/, and a config over them.
type gitFixture struct {
	galaxy       *fakegalaxy.Server
	git          *fakeGitClient
	cfg          *config.Config
	runtime      *infra.Infra
	printer      *warnCapturingPrinter
	reqPath      string
	cacheDir     string
	downloadPath string
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	root := t.TempDir()
	f := &gitFixture{
		galaxy:       fakegalaxy.New(t),
		git:          newFakeGitClient(),
		printer:      &warnCapturingPrinter{},
		reqPath:      filepath.Join(root, "requirements.yml"),
		cacheDir:     filepath.Join(root, "cache"),
		downloadPath: filepath.Join(root, "install"),
	}
	f.galaxy.AddVersion("acme", "lib", testVersion100, nil)
	f.galaxy.AddVersion("acme", "lib", "2.0.0", nil)

	appC1 := fakeCommit("app-1")
	appC2 := fakeCommit("app-2")
	f.git.add(gitAppURL, &fakeGitRepo{
		refs: map[string]string{"HEAD": appC1, gitMainRef: appC1, gitV1Ref: appC1, "refs/heads/dev": appC2},
		commits: map[string][]fakeGitCollection{
			appC1: {{namespace: "acme", name: "app", version: "1.2.3", deps: map[string]string{"acme.lib": ">=1.0.0"}}},
			appC2: {{namespace: "acme", name: "app", version: "1.3.0", deps: map[string]string{"acme.lib": ">=1.0.0"}}},
		},
	})
	monoC1 := fakeCommit("mono-1")
	f.git.add(gitMonoURL, &fakeGitRepo{
		refs: map[string]string{"HEAD": monoC1, gitMainRef: monoC1},
		commits: map[string][]fakeGitCollection{
			monoC1: {
				{namespace: "acme", name: "one", version: "0.1.0", subdir: "collections/one"},
				{namespace: "acme", name: "two", version: "0.2.0", subdir: "collections/two", deps: map[string]string{"acme.one": "*"}},
			},
		},
	})

	f.cfg = &config.Config{
		Server:           f.galaxy.URL(),
		CacheDir:         f.cacheDir,
		DownloadPath:     f.downloadPath,
		RequirementsFile: f.reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
	}
	f.runtime = infra.New(f.printer, f.galaxy.Client())
	f.runtime.Git = f.git
	return f
}

func (f *gitFixture) writeRequirements(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(f.reqPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write requirements: %v", err)
	}
}

func (f *gitFixture) install(t *testing.T) error {
	t.Helper()
	return collections.Start(context.Background(), f.cfg, f.runtime)
}

func (f *gitFixture) lockfile(t *testing.T) *lockfile.File {
	t.Helper()
	if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	lf, err := lockfile.Load(lockfile.ResolveDefaultPath(f.reqPath, ""))
	if err != nil {
		t.Fatalf("load lockfile: %v", err)
	}
	return lf
}

func (f *gitFixture) mustInstall(t *testing.T) {
	t.Helper()
	if err := f.install(t); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// TestGitInstallFromRepository asserts the whole pipeline for one git root:
// artifact under its locator key, record and sidecar with git provenance,
// Galaxy dependency installed, and a rerun that never touches the remote.
func TestGitInstallFromRepository(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\n")
	f.mustInstall(t)

	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
	if got := readManifestVersion(t, f.downloadPath, "app"); got != "1.2.3" {
		t.Fatalf("installed acme.app version = %q, want 1.2.3", got)
	}
	locator := gitsource.Locator{URL: gitAppURL, Commit: fakeCommit("app-1")}.String()
	entry := loadInstalledEntry(t, f.cfg, f.runtime, "acme.app@1.2.3")
	if entry.Source != locator {
		t.Fatalf("installed record Source = %q, want locator %q", entry.Source, locator)
	}
	assertArtifactFilePresent(t, f.cacheDir, locator, acmeArtifactFilename("app", "1.2.3"))
	assertInstalledProvenance(t, f.downloadPath, "app", "1.2.3", gitAppURL, "git_commit: "+fakeCommit("app-1"))
	assertTempFilesGone(t, f.cacheDir)
	if _, acquires := f.git.counts(); acquires != 1 {
		t.Fatalf("acquires = %d, want 1", acquires)
	}

	f.git.resetCounts()
	f.mustInstall(t)
	if adv, acq := f.git.counts(); adv != 0 || acq != 0 {
		t.Fatalf("rerun reached the remote: advertises=%d acquires=%d", adv, acq)
	}
}

// TestGitRefShapes pins that a branch, a tag, a qualified ref, the ",ref"
// suffix and a full commit all resolve to the commit the fake advertises.
func TestGitRefShapes(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		body    string
		version string
	}{
		"branch":        {body: "collections:\n  - name: " + gitAppURL + "\n    type: git\n    version: dev\n", version: "1.3.0"},
		"tag":           {body: "collections:\n  - name: " + gitAppURL + "\n    type: git\n    version: v1\n", version: "1.2.3"},
		"qualified":     {body: "collections:\n  - git+" + gitAppURL + ",refs/heads/dev\n", version: "1.3.0"},
		"comma wins":    {body: "collections:\n  - name: git+" + gitAppURL + ",dev\n    version: main\n", version: "1.3.0"},
		"commit":        {body: "collections:\n  - git+" + gitAppURL + "," + fakeCommit("app-2") + "\n", version: "1.3.0"},
		"explicit name": {body: "collections:\n  - name: acme.app\n    type: git\n    source: " + gitAppURL + "\n", version: "1.2.3"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGitFixture(t)
			f.writeRequirements(t, tc.body)
			f.mustInstall(t)
			if got := readManifestVersion(t, f.downloadPath, "app"); got != tc.version {
				t.Fatalf("installed version = %q, want %q", got, tc.version)
			}
		})
	}
}

// TestGitMultiCollectionRepository asserts a repository with collections in
// immediate children installs all of them with no Galaxy call for their
// mutual dependency, and that naming one installs that one alone.
func TestGitMultiCollectionRepository(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.writeRequirements(t, "collections:\n  - "+gitMonoURL+"#collections\n")
	f.mustInstall(t)
	assertManifestInstalled(t, f.downloadPath, "one")
	assertManifestInstalled(t, f.downloadPath, "two")
	if f.galaxy.Total() != 0 {
		t.Fatalf("a repository-internal dependency reached Galaxy: %d requests", f.galaxy.Total())
	}

	g := newGitFixture(t)
	g.writeRequirements(t, "collections:\n  - name: acme.one\n    type: git\n    source: "+gitMonoURL+"#collections\n")
	g.mustInstall(t)
	assertManifestInstalled(t, g.downloadPath, "one")
	assertPathAbsent(t, installPathFor(g.downloadPath, "two"))

	h := newGitFixture(t)
	h.writeRequirements(t, "collections:\n  - name: acme.three\n    type: git\n    source: "+gitMonoURL+"#collections\n")
	if err := h.install(t); !errors.Is(err, helpers.ErrGitNameMismatch) {
		t.Fatalf("naming an absent collection: %v, want ErrGitNameMismatch", err)
	}
}

// TestGitRootOwnsTheFQDN asserts the git pin is the single candidate for its
// fqdn: a Galaxy dependency constraint it satisfies is met from git, and one it
// cannot is a resolution failure, never a substitution from Galaxy.
func TestGitRootOwnsTheFQDN(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.galaxy.AddVersion("acme", "consumer", testVersion100, map[string]string{"acme.app": ">=1.0.0"})
	f.writeRequirements(t, "collections:\n  - acme.consumer\n  - git+"+gitAppURL+"\n")
	f.mustInstall(t)
	if got := readManifestVersion(t, f.downloadPath, "app"); got != "1.2.3" {
		t.Fatalf("acme.app came from somewhere else: %q", got)
	}

	g := newGitFixture(t)
	g.galaxy.AddVersion("acme", "consumer", testVersion100, map[string]string{"acme.app": ">=2.0.0"})
	g.writeRequirements(t, "collections:\n  - acme.consumer\n  - git+"+gitAppURL+"\n")
	err := g.install(t)
	if !errors.Is(err, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("conflicting constraint: %v, want a resolution failure", err)
	}
}

// TestGitLockAndFrozenInstall proves lock pins the git entry's commit with no
// sha256 (schema 5, set by its Galaxy dependency), and that a frozen install
// from that file installs from the cache alone: the fake client sees no call.
func TestGitLockAndFrozenInstall(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n")
	lf := f.lockfile(t)
	if lf.SchemaVersion != lockfile.SchemaVersionDownloadURL {
		t.Fatalf("schema = %d, want %d", lf.SchemaVersion, lockfile.SchemaVersionDownloadURL)
	}
	assertGitLockEntries(t, lf)

	f.git.resetCounts()
	f.cfg.Frozen = true
	f.mustInstall(t)
	assertManifestInstalled(t, f.downloadPath, "app")
	if adv, acq := f.git.counts(); adv != 0 || acq != 0 {
		t.Fatalf("frozen install reached the remote: advertises=%d acquires=%d", adv, acq)
	}

	assertFrozenMissRefetchesPin(t, f)

	// A changed ref against the same lockfile is a mismatch, not a silent
	// install of whatever was locked.
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",dev\n")
	if err := f.install(t); !errors.Is(err, helpers.ErrLockfileMismatch) {
		t.Fatalf("frozen with a changed ref: %v, want ErrLockfileMismatch", err)
	}
}

// assertGitLockEntries checks the shape of the two entries a git root plus
// its Galaxy dependency leave in the lockfile: the git one carries source,
// ref and commit and no sha256, the Galaxy one keeps its sha256.
func assertGitLockEntries(t *testing.T, lf *lockfile.File) {
	t.Helper()
	entry := findLockEntry(t, lf, "acme.app")
	if !entry.IsGit() || entry.Source != gitAppURL || entry.Ref != "main" ||
		entry.Commit != fakeCommit("app-1") || entry.SHA256 != "" {
		t.Fatalf("git lock entry = %+v", entry)
	}
	assertGalaxyLockEntryShape(t, findLockEntry(t, lf, "acme.lib"))
}

// assertFrozenMissRefetchesPin evicts the cached git artifact and the
// installed tree, then proves a frozen install rebuilds exactly the pinned
// commit with one acquisition and no advertisement.
func assertFrozenMissRefetchesPin(t *testing.T, f *gitFixture) {
	t.Helper()
	locator := gitsource.Locator{URL: gitAppURL, Commit: fakeCommit("app-1")}.String()
	key := helpers.ArtifactKey(locator, acmeArtifactFilename("app", "1.2.3"))
	if err := os.Remove(filepath.Join(f.cacheDir, key)); err != nil {
		t.Fatalf("evict cached artifact: %v", err)
	}
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatal(err)
	}
	f.mustInstall(t)
	assertManifestInstalled(t, f.downloadPath, "app")
	if adv, acq := f.git.counts(); adv != 0 || acq != 1 {
		t.Fatalf("frozen miss: advertises=%d acquires=%d, want 0 and 1", adv, acq)
	}
}

// TestGitOfflineAndRefresh asserts --offline replays a recorded pin and fails a
// cold one, and --refresh re-advertises a branch: unmoved, the pin stands with
// no fetch; moved, the new commit is acquired and installed.
func TestGitOfflineAndRefresh(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n")
	f.mustInstall(t)

	f.cfg.Offline = true
	f.git.resetCounts()
	f.mustInstall(t)
	if adv, acq := f.git.counts(); adv != 0 || acq != 0 {
		t.Fatalf("offline replay reached the remote: advertises=%d acquires=%d", adv, acq)
	}
	cold := newGitFixture(t)
	cold.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n")
	cold.cfg.Offline = true
	if err := cold.install(t); !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("cold offline: %v, want ErrOfflineMode", err)
	}

	f.cfg.Offline = false
	f.cfg.Refresh = true
	f.git.resetCounts()
	f.mustInstall(t)
	if adv, acq := f.git.counts(); adv != 1 || acq != 0 {
		t.Fatalf("refresh of an unmoved branch: advertises=%d acquires=%d, want 1 and 0", adv, acq)
	}

	f.git.repos[gitAppURL].refs[gitMainRef] = fakeCommit("app-2")
	f.git.resetCounts()
	f.mustInstall(t)
	if adv, acq := f.git.counts(); adv != 1 || acq != 1 {
		t.Fatalf("refresh of a moved branch: advertises=%d acquires=%d, want 1 and 1", adv, acq)
	}
	if got := readManifestVersion(t, f.downloadPath, "app"); got != "1.3.0" {
		t.Fatalf("moved branch installed version = %q, want 1.3.0", got)
	}
	entry := loadInstalledEntry(t, f.cfg, f.runtime, "acme.app@1.3.0")
	if !strings.HasSuffix(entry.Source, "@"+fakeCommit("app-2")) {
		t.Fatalf("installed record Source = %q, want the new commit", entry.Source)
	}
}

// TestGitDryRunAndNoCache proves a dry run fetches (it has to, to learn the
// identities) but commits no artifact and installs nothing, and that a
// --no-cache run fetches once and hands the build to the install phase.
func TestGitDryRunAndNoCache(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\n")
	f.cfg.DryRun = true
	f.mustInstall(t)
	assertPathAbsent(t, installPathFor(f.downloadPath, "app"))
	locator := gitsource.Locator{URL: gitAppURL, Commit: fakeCommit("app-1")}.String()
	assertPathAbsent(t, filepath.Join(f.cacheDir, helpers.ArtifactKey(locator, acmeArtifactFilename("app", "1.2.3"))))
	assertTempFilesGone(t, f.cacheDir)
	if _, acq := f.git.counts(); acq != 1 {
		t.Fatalf("dry run acquires = %d, want 1", acq)
	}

	g := newGitFixture(t)
	g.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\n")
	g.cfg.NoCache = true
	g.mustInstall(t)
	assertManifestInstalled(t, g.downloadPath, "app")
	if _, acq := g.git.counts(); acq != 1 {
		t.Fatalf("no-cache acquires = %d, want 1 (the build is handed to the install phase)", acq)
	}
	assertPathAbsent(t, filepath.Join(g.cacheDir, helpers.ArtifactKey(locator, acmeArtifactFilename("app", "1.2.3"))))
	assertTempFilesGone(t, g.cacheDir)
}

// TestGitFailuresKeepTheirClass pins the classification of the remote-side
// refusals through the real pipeline: a missing ref, a missing repository,
// and a credential presented only to the host it is bound to.
func TestGitFailuresKeepTheirClass(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",nosuch\n")
	if err := f.install(t); !errors.Is(err, helpers.ErrGitRefNotFound) {
		t.Fatalf("missing ref: %v, want ErrGitRefNotFound", err)
	}
	f.writeRequirements(t, "collections:\n  - git+https://git.example/acme/missing.git\n")
	if err := f.install(t); !errors.Is(err, helpers.ErrGitTransportFailed) {
		t.Fatalf("missing repository: %v, want ErrGitTransportFailed", err)
	}

	bound, err := gitsource.ParsePrefix("https://git.example")
	if err != nil {
		t.Fatal(err)
	}
	f.runtime.GitCredentials = []gitsource.Credential{{URL: bound, Username: "ci", Password: "token"}}
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\n  - "+gitMonoURL+"#collections\n")
	f.mustInstall(t)
	if got := f.git.seenAuth[gitAppURL]; got.Username != "ci" || got.Password != "token" {
		t.Fatalf("https repository did not receive the bound credential: %+v", got)
	}
	if got := f.git.seenAuth[gitMonoURL]; !got.IsZero() {
		t.Fatalf("ssh repository on another origin received a credential: %+v", got)
	}
}

// TestGitExplicitNameDoesNotPinSiblings asserts that naming one collection of a
// repository keeps its siblings out of the solver: a Galaxy root for a
// sibling's fqdn resolves from Galaxy rather than from the repository.
func TestGitExplicitNameDoesNotPinSiblings(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.galaxy.AddVersion("acme", "two", "9.9.9", nil)
	f.writeRequirements(t, "collections:\n  - name: acme.one\n    type: git\n    source: "+gitMonoURL+"#collections\n"+
		"  - name: acme.two\n    version: '>=1.0.0'\n")
	f.mustInstall(t)
	if got := readManifestVersion(t, f.downloadPath, "two"); got != "9.9.9" {
		t.Fatalf("acme.two installed as %q, want 9.9.9 from Galaxy", got)
	}
	if f.galaxy.Total() == 0 {
		t.Fatalf("acme.two was resolved without asking Galaxy")
	}
	entry := loadInstalledEntry(t, f.cfg, f.runtime, "acme.two@9.9.9")
	if gitsource.IsLocator(entry.Source) {
		t.Fatalf("acme.two recorded with a git source: %q", entry.Source)
	}
}
