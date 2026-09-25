package collections_test

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

const (
	roleAppURL  = "https://git.example/acme/ansible-role-app.git"
	roleBaseURL = "https://git.example/acme/ansible-role-base.git"
	roleMainRef = "refs/heads/main"
	roleV1Ref   = "refs/tags/v1.0.0"
)

// roleFixture is the git fixture plus two role repositories: app, depending
// on base by git pointer and on a local role ansible never looks up, and
// base, with no dependencies.
type roleFixture struct {
	*gitFixture

	rolesPath string
}

func newRoleFixture(t *testing.T) *roleFixture {
	t.Helper()
	f := &roleFixture{gitFixture: newGitFixture(t)}
	f.rolesPath = filepath.Join(filepath.Dir(f.downloadPath), "roles")
	f.cfg.RolesPath = f.rolesPath

	baseC1 := fakeCommit("role-base-1")
	base := &fakeGitRepo{refs: map[string]string{"HEAD": baseC1, roleMainRef: baseC1, roleV1Ref: baseC1}}
	base.addRole(baseC1, fakeGitRole{roleName: "base", files: map[string]string{"README.md": "# base\n"}})
	f.git.add(roleBaseURL, base)

	appC1 := fakeCommit("role-app-1")
	appC2 := fakeCommit("role-app-2")
	app := &fakeGitRepo{refs: map[string]string{"HEAD": appC1, roleMainRef: appC1, roleV1Ref: appC1, "refs/heads/dev": appC2}}
	app.addRole(appC1, fakeGitRole{roleName: "app", deps: []string{
		"src: git+" + roleBaseURL + "\n    name: base",
		"common",
	}, files: map[string]string{"handlers/main.yml": "- name: restart\n  debug: msg=restart\n"}})
	app.addRole(appC2, fakeGitRole{roleName: "app", files: map[string]string{"VERSION": "dev\n"}})
	f.git.add(roleAppURL, app)
	return f
}

func (f *roleFixture) rolePath(name string) string {
	return filepath.Join(f.rolesPath, name)
}

// loadInstalledRole reads the installed role record for name out of the
// persisted snapshot.
func loadInstalledRole(t *testing.T, f *roleFixture, name string) store.InstalledRoleEntry {
	t.Helper()
	st := loadStoreSnapshot(t, f.cfg, f.runtime)
	entry, ok := st.GetInstalledRole(name)
	if !ok {
		t.Fatalf("no installed role record for %s", name)
	}
	return entry
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // path is built from this test's own temp dirs.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s lacks %q:\n%s", path, want, data)
	}
}

// TestRoleInstallFromRepository pins the git role pipeline: one fetch, the
// role and its git dependency installed with ansible's install record, the
// local dependency skipped, and a rerun replaying the pin offline.
func TestRoleInstallFromRepository(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleAppURL+"\n    name: app\n")
	f.mustInstall(t)

	assertFileContains(t, filepath.Join(f.rolePath("app"), "tasks", "main.yml"), "debug")
	assertFileContains(t, filepath.Join(f.rolePath("app"), "handlers", "main.yml"), "restart")
	assertFileContains(t, filepath.Join(f.rolePath("app"), "meta", ".galaxy_install_info"), "version: main")
	assertFileContains(t, filepath.Join(f.rolePath("base"), "README.md"), "# base")
	assertPathAbsent(t, f.rolePath("common"))
	assertPathAbsent(t, filepath.Join(f.rolePath("app"), "MANIFEST.json"))

	locator := gitsource.Locator{URL: roleAppURL, Commit: fakeCommit("role-app-1")}.String()
	entry := loadInstalledRole(t, f, "app")
	if entry.Source != locator || entry.Version != "main" || entry.InstallPath != f.rolePath("app") {
		t.Fatalf("installed role record = %+v, want locator %s at %s", entry, locator, f.rolePath("app"))
	}
	if len(entry.Deps) != 1 || entry.Deps[0] != "base" {
		t.Fatalf("installed role deps = %v, want [base]", entry.Deps)
	}
	assertArtifactFilePresent(t, f.cacheDir, locator, helpers.RoleArtifactFilename("app", "main"))
	assertTempFilesGone(t, f.cacheDir)
	if got := f.git.roleAcquireCount(); got != 2 {
		t.Fatalf("role acquires = %d, want 2 (app and base)", got)
	}

	f.git.resetCounts()
	before := f.git.roleAcquireCount()
	f.mustInstall(t)
	if adv, _ := f.git.counts(); adv != 0 || f.git.roleAcquireCount() != before {
		t.Fatalf("rerun reached the remote: advertises=%d role acquires=%d", adv, f.git.roleAcquireCount()-before)
	}
}

// TestRoleRefShapes pins that a tag, a branch, a qualified ref, the ",ref"
// string suffix and a commit each resolve to the commit the fake advertises,
// and that the version recorded is the ref as ansible would record it.
func TestRoleRefShapes(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		body    string
		version string
		marker  string
	}{
		"tag": {body: "roles:\n  - src: git+" + roleAppURL + "\n    version: v1.0.0\n    name: app\n",
			version: "v1.0.0", marker: "handlers"},
		"branch":    {body: "roles:\n  - src: git+" + roleAppURL + "\n    version: dev\n    name: app\n", version: "dev", marker: "VERSION"},
		"qualified": {body: "roles:\n  - git+" + roleAppURL + ",refs/heads/dev,app\n", version: "dev", marker: "VERSION"},
		"commit": {body: "roles:\n  - src: " + roleAppURL + "\n    scm: git\n    version: " + fakeCommit("role-app-2") + "\n    name: app\n",
			version: fakeCommit("role-app-2"), marker: "VERSION"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRoleFixture(t)
			f.writeRequirements(t, tc.body)
			f.mustInstall(t)
			if _, err := os.Stat(filepath.Join(f.rolePath("app"), tc.marker)); err != nil {
				t.Fatalf("installed tree lacks %s: %v", tc.marker, err)
			}
			assertFileContains(t, filepath.Join(f.rolePath("app"), "meta", ".galaxy_install_info"), "version: "+tc.version)
			if got := loadInstalledRole(t, f, "app").Version; got != tc.version {
				t.Fatalf("recorded version = %q, want %q", got, tc.version)
			}
		})
	}
}

// TestRoleRefRespelledOntoSameCommit pins a ref respelled onto the commit
// already installed: the tree is kept, .galaxy_install_info names the new
// version, and the re-tallied marker lets the next run skip without drift.
func TestRoleRefRespelledOntoSameCommit(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleBaseURL+"\n    version: main\n    name: base\n")
	f.mustInstall(t)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleBaseURL+"\n    version: v1.0.0\n    name: base\n")
	f.mustInstall(t)
	assertFileContains(t, filepath.Join(f.rolePath("base"), "meta", ".galaxy_install_info"), "version: v1.0.0")
	if got := loadInstalledRole(t, f, "base").Version; got != "v1.0.0" {
		t.Fatalf("recorded version = %q, want v1.0.0", got)
	}
	f.mustInstall(t)
	if f.printer.hasWarnContaining("no longer matches its extract marker") {
		t.Fatalf("the rewritten install info read as drift: %q", f.printer.warns)
	}
}

// TestRoleNoDepsStopsTheWalk proves --no-deps installs the requirement and
// nothing its meta asks for.
func TestRoleNoDepsStopsTheWalk(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.cfg.NoDeps = true
	f.writeRequirements(t, "roles:\n  - src: git+"+roleAppURL+"\n    name: app\n")
	f.mustInstall(t)
	assertPathAbsent(t, f.rolePath("base"))
	if got := f.git.roleAcquireCount(); got != 1 {
		t.Fatalf("role acquires = %d, want 1", got)
	}
}

// TestRoleFirstWins proves a requirement that names a role already asked for
// differently is ignored with a warning, and that a dependency never
// outranks a requirement.
func TestRoleFirstWins(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleBaseURL+"\n    version: v1.0.0\n    name: base\n"+
		"  - src: git+"+roleAppURL+"\n    name: app\n")
	f.mustInstall(t)
	if got := loadInstalledRole(t, f, "base").Version; got != "v1.0.0" {
		t.Fatalf("base installed as %q, want the requirement's v1.0.0 over the dependency's HEAD", got)
	}
	if !f.printer.hasWarnContaining("Role base: already requested") {
		t.Fatalf("expected a first-wins warning, got %q", f.printer.warns)
	}
}

// TestRoleForeignDirectoryIsRefused proves a directory under roles_path that
// neither this tool nor ansible-galaxy installed is never replaced, while
// one ansible-galaxy installed is converged with a warning.
func TestRoleForeignDirectoryIsRefused(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleBaseURL+"\n    name: base\n")
	foreign := filepath.Join(f.rolePath("base"), "tasks")
	if err := os.MkdirAll(foreign, 0o755); err != nil { //nolint:gosec // test directory
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "main.yml"), []byte("- debug: msg=mine\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := f.install(t)
	if !errors.Is(err, helpers.ErrRoleDirectoryForeign) {
		t.Fatalf("install over a hand-written role: %v, want ErrRoleDirectoryForeign", err)
	}
	assertFileContains(t, filepath.Join(foreign, "main.yml"), "mine")

	// The same directory with ansible-galaxy's record is a Galaxy-installed
	// role, and is replaced.
	if err := os.MkdirAll(filepath.Join(f.rolePath("base"), "meta"), 0o755); err != nil { //nolint:gosec // test directory
		t.Fatalf("mkdir: %v", err)
	}
	info := filepath.Join(f.rolePath("base"), "meta", ".galaxy_install_info")
	if err := os.WriteFile(info, []byte("install_date: x\nversion: 0.1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.mustInstall(t)
	assertFileContains(t, filepath.Join(f.rolePath("base"), "README.md"), "# base")
	assertFileContains(t, info, "version: main")
	if !f.printer.hasWarnContaining("replacing a role ansible-galaxy installed") {
		t.Fatalf("expected a warning about replacing an ansible-installed role, got %q", f.printer.warns)
	}
}

// TestRoleDryRunCreatesNothing proves a dry run reports the roles and leaves
// no roles directory behind, and that a project without roles never grows
// one on a real run either.
func TestRoleDryRunCreatesNothing(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.cfg.DryRun = true
	f.writeRequirements(t, "roles:\n  - src: git+"+roleBaseURL+"\n    name: base\n")
	f.mustInstall(t)
	assertPathAbsent(t, f.rolesPath)

	g := newRoleFixture(t)
	g.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\n")
	g.mustInstall(t)
	assertPathAbsent(t, g.rolesPath)
}

// TestRoleOfflineMissIsRefused proves an unrecorded role under --offline
// fails closed, and a recorded one installs from the cache alone.
func TestRoleOfflineMissIsRefused(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleBaseURL+"\n    name: base\n")
	f.cfg.Offline = true
	if err := f.install(t); !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("offline cold install: %v, want ErrOfflineMode", err)
	}
	f.cfg.Offline = false
	f.mustInstall(t)

	// A fresh roles tree, the same cache: --offline installs from the pin and
	// the cached artifact without the remote.
	if err := os.RemoveAll(f.rolesPath); err != nil {
		t.Fatalf("remove roles: %v", err)
	}
	f.cfg.Offline = true
	f.git.resetCounts()
	before := f.git.roleAcquireCount()
	f.mustInstall(t)
	if f.git.roleAcquireCount() != before {
		t.Fatalf("offline install reached the remote")
	}
	assertFileContains(t, filepath.Join(f.rolePath("base"), "README.md"), "# base")
}

// TestRoleNoCacheHandsTheBuildToInstall proves a --no-cache run builds the
// role once at discovery and installs that build, leaving no artifact and no
// temp file behind.
func TestRoleNoCacheHandsTheBuildToInstall(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.cfg.NoCache = true
	f.writeRequirements(t, "roles:\n  - src: git+"+roleBaseURL+"\n    name: base\n")
	f.mustInstall(t)
	assertFileContains(t, filepath.Join(f.rolePath("base"), "README.md"), "# base")
	if got := f.git.roleAcquireCount(); got != 1 {
		t.Fatalf("role acquires = %d, want 1", got)
	}
	assertTempFilesGone(t, f.cacheDir)
}

// TestRoleAndCollectionShareAName proves a role and a collection directory
// with one name coexist: they live under different roots.
func TestRoleAndCollectionShareAName(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\nroles:\n  - src: git+"+roleAppURL+"\n    name: app\n")
	f.mustInstall(t)
	assertManifestInstalled(t, f.downloadPath, "app")
	assertFileContains(t, filepath.Join(f.rolePath("app"), "tasks", "main.yml"), "debug")
}

// TestRoleWithoutMetaIsRefused proves a repository that is not a role fails
// as a usage error naming the sentinel.
func TestRoleWithoutMetaIsRefused(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - src: git+"+gitAppURL+"\n    name: notarole\n")
	if err := f.install(t); !errors.Is(err, helpers.ErrRoleMetaNotFound) {
		t.Fatalf("install of a non-role repository: %v, want ErrRoleMetaNotFound", err)
	}
}

// TestRoleTokenNeverReachesGit pins that a Galaxy role's repository fetch
// gets the empty credential, not the server's Galaxy token, when no
// GO_GALAXY_GIT_* binding covers the host.
func TestRoleTokenNeverReachesGit(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.cfg.Servers = nil
	f.writeRequirements(t, "roles:\n  - src: git+"+roleBaseURL+"\n    name: base\n")
	f.mustInstall(t)
	u, err := gitsource.ParseURL(roleBaseURL)
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	f.git.mu.Lock()
	seen, ok := f.git.seenAuth[u.String()]
	f.git.mu.Unlock()
	if !ok || !seen.IsZero() {
		t.Fatalf("git client saw credential %+v for %s, want none", seen, roleBaseURL)
	}
}

// TestRoleWarm proves warm fills the artifact cache and the extracted store
// for a role and records it under a role-prefixed warmed key, touching no
// roles tree.
func TestRoleWarm(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleBaseURL+"\n    name: base\n")
	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	assertPathAbsent(t, f.rolesPath)
	locator := gitsource.Locator{URL: roleBaseURL, Commit: fakeCommit("role-base-1")}.String()
	assertArtifactFilePresent(t, f.cacheDir, locator, helpers.RoleArtifactFilename("base", "main"))
	warmed := loadStoreSnapshot(t, f.cfg, f.runtime).WarmedArtifactSHAByKey()
	if _, ok := warmed["role:base@main"]; !ok {
		t.Fatalf("warmed set lacks role:base@main: %v", warmed)
	}
	// The install that follows needs the remote only for nothing: the
	// artifact is cached and the pin recorded.
	f.git.resetCounts()
	before := f.git.roleAcquireCount()
	f.mustInstall(t)
	if f.git.roleAcquireCount() != before {
		t.Fatalf("install after warm reached the remote")
	}
}

// galaxyRoleURL is the repository the fake v1 API points geerlingguy.docker
// at; it is registered in the fake git client by its composed https form.
const galaxyRoleURL = "https://github.com/geerlingguy/ansible-role-docker"

// newGalaxyRoleFixture is the role fixture with geerlingguy.docker
// registered on the fake Galaxy's v1 API, pointing at a repository the fake
// git client serves with three tags and a default branch.
func newGalaxyRoleFixture(t *testing.T) *roleFixture {
	t.Helper()
	f := newRoleFixture(t)
	c1, c2, c3 := fakeCommit("docker-1"), fakeCommit("docker-2"), fakeCommit("docker-3")
	repo := &fakeGitRepo{refs: map[string]string{
		"HEAD": c3, "refs/heads/master": c3, "refs/tags/1.0.0": c1, "refs/tags/1.10.0": c3, "refs/tags/1.9.0": c2,
	}}
	for _, c := range []string{c1, c2, c3} {
		repo.addRole(c, fakeGitRole{roleName: "docker", files: map[string]string{"COMMIT": c + "\n"}})
	}
	f.git.add(galaxyRoleURL, repo)
	f.galaxy.AddRole("geerlingguy", "docker", "geerlingguy", "ansible-role-docker", "master", []fakegalaxy.RoleVersion{
		{Name: "1.0.0"}, {Name: "1.10.0", CommitSHA: c3}, {Name: "1.9.0"},
	})
	return f
}

// TestGalaxyRoleInstallsHighestTag pins that a Galaxy role maps through the
// v1 API to its highest tag, installs by git under the Galaxy name, and
// reruns with neither the v1 API nor the remote.
func TestGalaxyRoleInstallsHighestTag(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	f.mustInstall(t)

	assertFileContains(t, filepath.Join(f.rolePath("geerlingguy.docker"), "COMMIT"), fakeCommit("docker-3"))
	assertFileContains(t, filepath.Join(f.rolePath("geerlingguy.docker"), "meta", ".galaxy_install_info"), "version: 1.10.0")
	entry := loadInstalledRole(t, f, "geerlingguy.docker")
	if entry.GalaxyName != "geerlingguy.docker" || entry.Version != "1.10.0" {
		t.Fatalf("record = %+v", entry)
	}
	if want := (gitsource.Locator{URL: galaxyRoleURL, Commit: fakeCommit("docker-3")}).String(); entry.Source != want {
		t.Fatalf("record Source = %q, want %q", entry.Source, want)
	}
	if f.galaxy.Count(fakegalaxy.EndpointRoleLookup) != 1 || f.galaxy.Count(fakegalaxy.EndpointRoleVersions) != 1 {
		t.Fatalf("v1 requests: lookup=%d versions=%d, want one each",
			f.galaxy.Count(fakegalaxy.EndpointRoleLookup), f.galaxy.Count(fakegalaxy.EndpointRoleVersions))
	}

	f.galaxy.ResetCounts()
	f.git.resetCounts()
	before := f.git.roleAcquireCount()
	f.mustInstall(t)
	if f.galaxy.Total() != 0 || f.git.roleAcquireCount() != before {
		t.Fatalf("rerun reached the network: galaxy=%d role acquires=%d", f.galaxy.Total(), f.git.roleAcquireCount()-before)
	}
}

// TestGalaxyRoleVersionSelection pins the version rules: an explicit tag is
// fetched as named, one the server does not list is a resolution failure,
// and the default branch is accepted without being listed.
func TestGalaxyRoleVersionSelection(t *testing.T) {
	t.Parallel()
	t.Run("explicit tag", func(t *testing.T) {
		t.Parallel()
		f := newGalaxyRoleFixture(t)
		f.writeRequirements(t, "roles:\n  - src: geerlingguy.docker\n    version: 1.9.0\n")
		f.mustInstall(t)
		assertFileContains(t, filepath.Join(f.rolePath("geerlingguy.docker"), "COMMIT"), fakeCommit("docker-2"))
	})
	t.Run("unlisted tag", func(t *testing.T) {
		t.Parallel()
		f := newGalaxyRoleFixture(t)
		f.writeRequirements(t, "roles:\n  - src: geerlingguy.docker\n    version: 2.0.0\n")
		if err := f.install(t); !errors.Is(err, helpers.ErrRoleVersionNotFound) {
			t.Fatalf("unlisted version: %v, want ErrRoleVersionNotFound", err)
		}
	})
	t.Run("default branch", func(t *testing.T) {
		t.Parallel()
		f := newGalaxyRoleFixture(t)
		f.writeRequirements(t, "roles:\n  - src: geerlingguy.docker\n    version: master\n")
		f.mustInstall(t)
		assertFileContains(t, filepath.Join(f.rolePath("geerlingguy.docker"), "meta", ".galaxy_install_info"), "version: master")
	})
	t.Run("unknown role", func(t *testing.T) {
		t.Parallel()
		f := newGalaxyRoleFixture(t)
		f.writeRequirements(t, "roles:\n  - geerlingguy.nothing\n")
		if err := f.install(t); !errors.Is(err, helpers.ErrRoleNotFound) {
			t.Fatalf("unknown role: %v, want ErrRoleNotFound", err)
		}
	})
}

// TestGalaxyRoleWithoutV1API proves a server that serves no v1 role API -
// an Automation Hub, or the fake before any role is registered - fails the
// run as a configuration error naming the server.
func TestGalaxyRoleWithoutV1API(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	err := f.install(t)
	if !errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable) {
		t.Fatalf("no v1 API: %v, want ErrGalaxyRoleAPIUnavailable", err)
	}
}

// TestGalaxyRoleOfflineReplaysThePin proves that, once resolved, a Galaxy
// role installs under --offline from its pins and the cached artifact.
func TestGalaxyRoleOfflineReplaysThePin(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	f.cfg.Offline = true
	if err := f.install(t); !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("cold offline: %v, want ErrOfflineMode", err)
	}
	f.cfg.Offline = false
	f.mustInstall(t)
	if err := os.RemoveAll(f.rolesPath); err != nil {
		t.Fatalf("remove roles: %v", err)
	}
	f.cfg.Offline = true
	f.galaxy.ResetCounts()
	f.mustInstall(t)
	if f.galaxy.Total() != 0 {
		t.Fatalf("offline install reached Galaxy: %d requests", f.galaxy.Total())
	}
	assertFileContains(t, filepath.Join(f.rolePath("geerlingguy.docker"), "COMMIT"), fakeCommit("docker-3"))
}

// TestGalaxyRoleRefreshReAsksV1 proves --refresh goes back to the v1 API and
// the repository's advertisement, and keeps the pin when nothing moved.
func TestGalaxyRoleRefreshReAsksV1(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	f.mustInstall(t)
	f.galaxy.ResetCounts()
	f.git.resetCounts()
	before := f.git.roleAcquireCount()
	f.cfg.Refresh = true
	f.mustInstall(t)
	if f.galaxy.Count(fakegalaxy.EndpointRoleLookup) != 1 {
		t.Fatalf("--refresh did not re-ask the v1 API")
	}
	if adv, _ := f.git.counts(); adv != 1 || f.git.roleAcquireCount() != before {
		t.Fatalf("--refresh: advertises=%d role acquires=%d, want one advertisement and no acquisition", adv, f.git.roleAcquireCount()-before)
	}
}

// TestGalaxyRoleCommitMismatchWarns proves a v1 commit_sha that disagrees
// with the repository is a warning naming both, and the repository wins.
func TestGalaxyRoleCommitMismatchWarns(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.galaxy.AddRole("geerlingguy", "docker", "geerlingguy", "ansible-role-docker", "master", []fakegalaxy.RoleVersion{
		{Name: "1.10.0", CommitSHA: fakeCommit("docker-1")},
	})
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	f.mustInstall(t)
	if !f.printer.hasWarnContaining("Galaxy recorded commit " + fakeCommit("docker-1")) {
		t.Fatalf("expected a commit mismatch warning, got %q", f.printer.warns)
	}
	assertFileContains(t, filepath.Join(f.rolePath("geerlingguy.docker"), "COMMIT"), fakeCommit("docker-3"))
}

// TestGalaxyRoleTokenStaysOnTheServer proves the token configured for the
// Galaxy server reaches its v1 API and never the role's repository.
func TestGalaxyRoleTokenStaysOnTheServer(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.galaxy.RequireAuth("Token s3cret")
	f.cfg.Servers = []config.Server{{URL: f.galaxy.URL(), Token: config.NewSecret("s3cret")}}
	f.runtime = multiServerRuntime(f.cfg)
	f.runtime.Git = f.git
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	f.mustInstall(t)
	if got, _ := f.galaxy.SeenAuth(fakegalaxy.EndpointRoleLookup); got != "Token s3cret" {
		t.Fatalf("v1 lookup saw Authorization %q, want the configured token", got)
	}
	u, err := gitsource.ParseURL(galaxyRoleURL)
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	f.git.mu.Lock()
	seen := f.git.seenAuth[u.String()]
	f.git.mu.Unlock()
	if !seen.IsZero() {
		t.Fatalf("git client saw credential %+v for the role repository, want none", seen)
	}
}

// findLockRole returns the role entry named name, failing the test when the
// lockfile lacks it.
func findLockRole(t *testing.T, lf *lockfile.File, name string) lockfile.RoleEntry {
	t.Helper()
	for _, e := range lf.Roles {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("lockfile has no role %s: %+v", name, lf.Roles)
	return lockfile.RoleEntry{}
}

// lockedRoleFixture is a Galaxy role fixture whose requirements name a
// Galaxy role and a git role, locked once; tests build on the lockfile it
// wrote.
func lockedRoleFixture(t *testing.T) (*roleFixture, *lockfile.File) {
	t.Helper()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n  - src: git+"+roleAppURL+"\n    name: app\n")
	return f, f.lockfile(t)
}

// TestRoleLockWritesSchemaThree proves lock writes a schema-3 file pinning
// each role - Galaxy or git, requirement or dependency - to its commit.
func TestRoleLockWritesSchemaThree(t *testing.T) {
	t.Parallel()
	f, lf := lockedRoleFixture(t)
	if lf.SchemaVersion != lockfile.SchemaVersionRoles {
		t.Fatalf("schema = %d, want %d", lf.SchemaVersion, lockfile.SchemaVersionRoles)
	}
	wantDocker := lockfile.RoleEntry{
		Name: "geerlingguy.docker", Type: lockfile.RoleTypeGalaxy, Version: "1.10.0", Galaxy: "geerlingguy.docker",
		Source: f.galaxy.URL(), Repository: galaxyRoleURL, Ref: "refs/tags/1.10.0", Commit: fakeCommit("docker-3"),
	}
	wantApp := lockfile.RoleEntry{
		Name: "app", Type: lockfile.RoleTypeGit, Version: "main", Source: roleAppURL, Ref: "HEAD", Commit: fakeCommit("role-app-1"),
		Deps: []string{"base"},
	}
	wantBase := lockfile.RoleEntry{
		Name: "base", Type: lockfile.RoleTypeGit, Version: "main", Source: roleBaseURL, Ref: "HEAD", Commit: fakeCommit("role-base-1"),
	}
	for _, want := range []lockfile.RoleEntry{wantDocker, wantApp, wantBase} {
		got := findLockRole(t, lf, want.Name)
		if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
			t.Fatalf("role entry %s = %+v, want %+v", want.Name, got, want)
		}
	}
}

// TestRoleFrozenInstallWarmCache proves a frozen install with every
// artifact cached touches no network at all.
func TestRoleFrozenInstallWarmCache(t *testing.T) {
	t.Parallel()
	f, _ := lockedRoleFixture(t)
	f.cfg.Frozen = true
	f.galaxy.ResetCounts()
	f.git.resetCounts()
	before := f.git.roleAcquireCount()
	f.mustInstall(t)
	if adv, _ := f.git.counts(); f.galaxy.Total() != 0 || adv != 0 || f.git.roleAcquireCount() != before {
		t.Fatalf("frozen install with a warm cache reached the network")
	}
	assertFileContains(t, filepath.Join(f.rolePath("geerlingguy.docker"), "COMMIT"), fakeCommit("docker-3"))
}

// writeRoleLockfile writes a lockfile holding only the given roles beside
// the fixture's requirements file.
func writeRoleLockfile(t *testing.T, f *roleFixture, roles ...lockfile.RoleEntry) {
	t.Helper()
	if err := lockfile.Save(lockfile.ResolveDefaultPath(f.reqPath, ""), &lockfile.File{Roles: roles}); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
}

// TestRoleFrozenInstallColdCache proves a frozen install on a cold artifact
// cache fetches by the pinned commit and never asks the v1 API, and that
// with --offline it fails closed instead.
func TestRoleFrozenInstallColdCache(t *testing.T) {
	t.Parallel()
	_, lf := lockedRoleFixture(t)
	docker := findLockRole(t, lf, "geerlingguy.docker")

	g := newGalaxyRoleFixture(t)
	g.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	writeRoleLockfile(t, g, docker)
	g.cfg.Frozen = true
	g.mustInstall(t)
	if g.galaxy.Total() != 0 || g.git.roleAcquireCount() != 1 {
		t.Fatalf("frozen cold install: galaxy=%d role acquires=%d, want 0 and 1", g.galaxy.Total(), g.git.roleAcquireCount())
	}
	assertFileContains(t, filepath.Join(g.rolePath("geerlingguy.docker"), "COMMIT"), fakeCommit("docker-3"))

	h := newGalaxyRoleFixture(t)
	h.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	writeRoleLockfile(t, h, docker)
	h.cfg.Frozen, h.cfg.Offline = true, true
	if err := h.install(t); !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("frozen offline cold install: %v, want ErrOfflineMode", err)
	}
}

// TestRoleFrozenMismatch proves a requirement that drifted from the lockfile
// - a different version, a different ref - is refused under --frozen.
func TestRoleFrozenMismatch(t *testing.T) {
	t.Parallel()
	f, _ := lockedRoleFixture(t)
	f.cfg.Frozen = true
	f.writeRequirements(t, "roles:\n  - src: geerlingguy.docker\n    version: 1.9.0\n  - src: git+"+roleAppURL+"\n    name: app\n")
	if err := f.install(t); !errors.Is(err, helpers.ErrLockfileMismatch) {
		t.Fatalf("frozen install with a drifted requirement: %v, want ErrLockfileMismatch", err)
	}
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n  - src: git+"+roleAppURL+"\n    version: dev\n    name: app\n")
	if err := f.install(t); !errors.Is(err, helpers.ErrLockfileMismatch) {
		t.Fatalf("frozen install with a drifted ref: %v, want ErrLockfileMismatch", err)
	}
}

// TestRoleLockCheckDrift proves lock --check reports a role that moved as
// drift, and that a lockfile whose roles match reads as up to date.
func TestRoleLockCheckDrift(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	f.lockfile(t)
	f.cfg.Check = true
	if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("lock --check on an up-to-date file: %v", err)
	}
	f.writeRequirements(t, "roles:\n  - src: geerlingguy.docker\n    version: 1.9.0\n")
	if err := collections.Lock(context.Background(), f.cfg, f.runtime); !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("lock --check with a changed role: %v, want ErrLockfileDrift", err)
	}
}

// TestRoleOutdated proves outdated reports a Galaxy role whose highest tag
// moved past the locked one, a git role whose branch moved as commit drift,
// and an up-to-date role as such, all from a live answer with no backend.
func TestRoleOutdated(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - src: geerlingguy.docker\n    version: 1.9.0\n"+
		"  - src: git+"+roleAppURL+"\n    version: main\n    name: app\n")
	f.lockfile(t)

	f.git.repos[roleAppURL].refs[roleMainRef] = fakeCommit("role-app-2")
	printer := &lineCapturingPrinter{}
	f.runtime = infra.New(printer, f.galaxy.Client())
	f.runtime.Git = f.git
	// Verbose, because base's up-to-date line is printed only then.
	f.cfg.Verbose = true
	if err := collections.Outdated(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	for _, want := range []string{
		"Outdated: role geerlingguy.docker 1.9.0 -> 1.10.0",
		"Outdated: role app " + fakeCommit("role-app-1") + " -> " + fakeCommit("role-app-2"),
		"Up to date: role base == " + fakeCommit("role-base-1"),
		": 1 up to date, 2 outdated, 0 failed",
	} {
		if !printer.hasLineContaining(want) {
			t.Fatalf("report lacks %q:\n%v", want, printer.snapshot())
		}
	}
}

// roleArtifactPath is where the store keeps a role's tarball: keyed by its
// locator and the role filename.
func roleArtifactPath(f *roleFixture, locator, name, version string) string {
	return filepath.Join(f.cacheDir, helpers.ArtifactKey(locator, helpers.RoleArtifactFilename(name, version)))
}

// TestRoleCleanup pins that cleanup keeps every reachable role and its
// extracted tree, removes an unreachable one with its artifact and record,
// and never touches a directory this tool did not install.
func TestRoleCleanup(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleAppURL+"\n    name: app\n"+
		"  - src: git+"+roleBaseURL+"\n    version: v1.0.0\n    name: pinned\n")
	f.mustInstall(t)
	foreign := filepath.Join(f.rolesPath, "handwritten", "tasks")
	if err := os.MkdirAll(foreign, 0o755); err != nil { //nolint:gosec // test directory
		t.Fatalf("mkdir: %v", err)
	}
	pinnedLocator := gitsource.Locator{URL: roleBaseURL, Commit: fakeCommit("role-base-1")}.String()
	pinnedArtifact := roleArtifactPath(f, pinnedLocator, "pinned", "v1.0.0")

	// Everything is reachable: nothing goes.
	mustCleanup(t, f.gitFixture)
	for _, name := range []string{"app", "base", "pinned", "handwritten"} {
		if _, err := os.Stat(f.rolePath(name)); err != nil {
			t.Fatalf("role %s after a no-op cleanup: %v", name, err)
		}
	}

	// Dropping pinned from the requirements removes it, its artifact and
	// its record; app and its dependency base stay, and so does the
	// hand-written directory.
	f.writeRequirements(t, "roles:\n  - src: git+"+roleAppURL+"\n    name: app\n")
	mustCleanup(t, f.gitFixture)
	assertPathAbsent(t, f.rolePath("pinned"))
	assertPathAbsent(t, pinnedArtifact)
	if _, ok := loadStoreSnapshot(t, f.cfg, f.runtime).GetInstalledRole("pinned"); ok {
		t.Fatalf("removed role still recorded")
	}
	for _, name := range []string{"app", "base", "handwritten"} {
		if _, err := os.Stat(f.rolePath(name)); err != nil {
			t.Fatalf("role %s after cleanup: %v", name, err)
		}
	}
	appLocator := gitsource.Locator{URL: roleAppURL, Commit: fakeCommit("role-app-1")}.String()
	if _, err := os.Stat(roleArtifactPath(f, appLocator, "app", "main")); err != nil {
		t.Fatalf("kept role's artifact after cleanup: %v", err)
	}
	f.mustInstall(t)
}

// TestRoleCleanupDryRunAndLegacyProject proves a dry run removes nothing,
// and that a project recorded without a roles path - by a binary that
// predates roles - has its roles directory left alone entirely.
func TestRoleCleanupDryRunAndLegacyProject(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleBaseURL+"\n    name: base\n")
	f.mustInstall(t)
	f.writeRequirements(t, "collections: []\n")

	f.cfg.DryRun = true
	mustCleanup(t, f.gitFixture)
	if _, err := os.Stat(f.rolePath("base")); err != nil {
		t.Fatalf("dry-run cleanup removed a role: %v", err)
	}
	f.cfg.DryRun = false

	// An older binary re-recording the project drops the roles path, which
	// is exactly what store.RecordProject does when handed none.
	if err := store.RecordProject(f.cacheDir, f.reqPath, f.downloadPath, ""); err != nil {
		t.Fatalf("RecordProject: %v", err)
	}
	mustCleanup(t, f.gitFixture)
	if _, err := os.Stat(f.rolePath("base")); err != nil {
		t.Fatalf("cleanup of a project without a recorded roles path touched its roles: %v", err)
	}

	// Once the path is recorded again, the unreachable role goes.
	f.mustInstall(t)
	mustCleanup(t, f.gitFixture)
	assertPathAbsent(t, f.rolePath("base"))
}

// TestRoleCleanupWithoutRecordsKeepsDependencies pins that with no
// installed-role records a dependency named by an installed role's own meta
// is still kept, and only a role nothing names is removed.
func TestRoleCleanupWithoutRecordsKeepsDependencies(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleAppURL+"\n    name: app\n"+
		"  - src: git+"+roleBaseURL+"\n    version: v1.0.0\n    name: pinned\n")
	f.mustInstall(t)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleAppURL+"\n    name: app\n")
	mutateStoreSnapshot(t, f.gitFixture, func(st *store.Store) {
		for name := range st.InstalledRolesSnapshot() {
			st.DeleteInstalledRole(name)
		}
	})
	mustCleanup(t, f.gitFixture)
	assertPathAbsent(t, f.rolePath("pinned"))
	for _, name := range []string{"app", "base"} {
		if _, err := os.Stat(f.rolePath(name)); err != nil {
			t.Fatalf("role %s after a cleanup without records: %v", name, err)
		}
	}
}

// TestGalaxyRoleLockRecordsTheAnsweringServer proves that with a server list
// whose first entry serves no v1 role API, the role is found on the second
// and the lockfile names that server as the role's source.
func TestGalaxyRoleLockRecordsTheAnsweringServer(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	hub := fakegalaxy.New(t)
	f.cfg.Servers = []config.Server{{ID: "hub", URL: hub.URL()}, {ID: "galaxy", URL: f.galaxy.URL()}}
	f.cfg.Server = hub.URL()
	f.runtime = multiServerRuntime(f.cfg)
	f.runtime.Git = f.git
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	lf := f.lockfile(t)
	if got := findLockRole(t, lf, "geerlingguy.docker").Source; got != f.galaxy.URL() {
		t.Fatalf("lockfile source = %q, want the server that answered, %q", got, f.galaxy.URL())
	}
}

// TestRoleRepositoryShippingInstallInfo pins that a committed
// meta/.galaxy_install_info installs cleanly twice, replaced by this tool's
// record while the extracted store's tree stays untouched.
func TestRoleRepositoryShippingInstallInfo(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	c := fakeCommit("shipped-1")
	repo := &fakeGitRepo{refs: map[string]string{"HEAD": c, roleMainRef: c}}
	repo.addRole(c, fakeGitRole{files: map[string]string{"meta/.galaxy_install_info": "install_date: then\nversion: 0.0.1\n"}})
	f.git.add("https://git.example/acme/shipped.git", repo)
	f.writeRequirements(t, "roles:\n  - src: git+https://git.example/acme/shipped.git\n    name: shipped\n")
	f.mustInstall(t)
	info := filepath.Join(f.rolePath("shipped"), "meta", ".galaxy_install_info")
	assertFileContains(t, info, "version: main")
	if err := os.RemoveAll(f.rolesPath); err != nil {
		t.Fatalf("remove roles: %v", err)
	}
	f.mustInstall(t)
	assertFileContains(t, info, "version: main")
	entry := loadInstalledRole(t, f, "shipped")
	stored := filepath.Join(f.cacheDir, "extracted", entry.ArtifactSHA256, "meta", ".galaxy_install_info")
	assertPathAbsent(t, stored)
}

// TestRoleCleanupToleratesAnUnreadableRolesList pins that a refused roles
// list keeps every role of that project and still judges its collections,
// instead of aborting every cleanup against the cache.
func TestRoleCleanupToleratesAnUnreadableRolesList(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\nroles:\n  - src: git+"+roleBaseURL+"\n    name: base\n")
	f.mustInstall(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\nroles:\n  - include: other.yml\n")
	mustCleanup(t, f.gitFixture)
	assertManifestInstalled(t, f.downloadPath, "app")
	if _, err := os.Stat(f.rolePath("base")); err != nil {
		t.Fatalf("role of a project with an unreadable roles list was removed: %v", err)
	}
	if !f.printer.hasWarnContaining("roles list of") {
		t.Fatalf("expected a warning about the unreadable roles list, got %q", f.printer.warns)
	}
}

// TestGalaxyRoleOfflineOutranksRefresh proves --offline --refresh with a
// recorded Galaxy role installs from the pin, as --offline outranks
// --refresh everywhere else.
func TestGalaxyRoleOfflineOutranksRefresh(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	f.mustInstall(t)
	f.cfg.Offline, f.cfg.Refresh = true, true
	f.galaxy.ResetCounts()
	f.mustInstall(t)
	if f.galaxy.Total() != 0 {
		t.Fatalf("--offline --refresh reached Galaxy: %d requests", f.galaxy.Total())
	}
}

// TestGalaxyRolePoisonedPinIsRefused proves a Galaxy pin pointing anywhere
// but a GitHub repository is refused on replay rather than fetched.
func TestGalaxyRolePoisonedPinIsRefused(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")
	f.mustInstall(t)
	mutateStoreSnapshot(t, f.gitFixture, func(st *store.Store) {
		pin, ok := st.GetRolePin("galaxy\ngeerlingguy.docker\n")
		if !ok {
			t.Fatalf("no Galaxy pin recorded")
		}
		pin.Repository = roleBaseURL
		st.SetRolePin("galaxy\ngeerlingguy.docker\n", pin)
	})
	if err := os.RemoveAll(f.rolesPath); err != nil {
		t.Fatalf("remove roles: %v", err)
	}
	if err := f.install(t); !errors.Is(err, helpers.ErrGalaxyRoleInvalid) {
		t.Fatalf("install from a poisoned pin: %v, want ErrGalaxyRoleInvalid", err)
	}
}

// TestRoleInstallLineNamesTheVersion pins that the success line names the
// version each role settled on, whether pinned by tag, taken at HEAD, or
// reached only as a dependency.
func TestRoleInstallLineNamesTheVersion(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "roles:\n  - src: git+"+roleBaseURL+"\n    version: v1.0.0\n    name: pinned\n"+
		"  - src: git+"+roleAppURL+"\n    name: app\n")
	printer := &lineCapturingPrinter{}
	f.runtime = infra.New(printer, f.galaxy.Client())
	f.runtime.Git = f.git

	f.mustInstall(t)

	for _, want := range []string{
		"Installed: role pinned == v1.0.0",
		"Installed: role app == main",
		"Installed: role base == main",
	} {
		if !printer.hasLineContaining(want) {
			t.Fatalf("install report lacks %q:\n%v", want, printer.snapshot())
		}
	}
}
