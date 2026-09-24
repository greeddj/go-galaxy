package collections_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// tomlAppProject is the galaxy.toml twin of what writeRequirements writes for
// acme.app: a bare name, which the TOML path reads as version "*".
const tomlAppProject = "[project]\ncollections = [\"acme.app\"]\n"

// useGalaxyTOML writes tomlAppProject as galaxy.toml beside the fixture's
// requirements.yml and points the config at it, so every derived path (the
// lockfile, the registry key) stays in the same project directory.
func useGalaxyTOML(t *testing.T, f *e2eFixture) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(f.cfg.RequirementsFile), helpers.RequirementsTOMLName)
	if err := os.WriteFile(path, []byte(tomlAppProject), helpers.FileMod); err != nil {
		t.Fatalf("write galaxy.toml: %v", err)
	}
	f.cfg.RequirementsFile = path
	return path
}

// mustLock runs Lock on the fixture and returns the lockfile it wrote.
func mustLock(t *testing.T, f *e2eFixture) *lockfile.File {
	t.Helper()
	if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	lf, err := lockfile.Load(lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, ""))
	if err != nil {
		t.Fatalf("load lockfile: %v", err)
	}
	return lf
}

// TestInstallFromGalaxyTOMLMatchesYAML pins that galaxy.toml and
// requirements.yml naming the same root install the same tree and lock to
// byte-identical lockfiles, so switching files changes no hash.
func TestInstallFromGalaxyTOMLMatchesYAML(t *testing.T) {
	t.Parallel()
	yamlF := newE2EFixture(t)
	tomlF := newE2EFixture(t)
	tomlPath := useGalaxyTOML(t, tomlF)
	// One server for both projects, so the lockfiles can agree on Server and
	// Source; the caches and install trees stay separate.
	tomlF.server, tomlF.cfg.Server, tomlF.runtime = yamlF.server, yamlF.cfg.Server, yamlF.runtime

	for _, f := range []*e2eFixture{yamlF, tomlF} {
		if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("Start from %s: %v", filepath.Base(f.cfg.RequirementsFile), err)
		}
		assertManifestInstalled(t, f.downloadPath, "app")
		assertManifestInstalled(t, f.downloadPath, "lib")
	}

	yamlLock := mustLock(t, yamlF)
	tomlLock := mustLock(t, tomlF)
	yamlHash, err := yamlLock.Hash()
	if err != nil {
		t.Fatalf("Hash (yaml): %v", err)
	}
	tomlHash, err := tomlLock.Hash()
	if err != nil {
		t.Fatalf("Hash (toml): %v", err)
	}
	if yamlHash != tomlHash {
		t.Fatalf("lockfile hashes differ: yaml %s, toml %s", yamlHash, tomlHash)
	}
	yamlBytes, err := os.ReadFile(lockfile.ResolveDefaultPath(yamlF.cfg.RequirementsFile, ""))
	if err != nil {
		t.Fatalf("read yaml lockfile: %v", err)
	}
	tomlBytes, err := os.ReadFile(lockfile.ResolveDefaultPath(tomlPath, ""))
	if err != nil {
		t.Fatalf("read toml lockfile: %v", err)
	}
	if string(yamlBytes) != string(tomlBytes) {
		t.Fatalf("lockfile bytes differ:\n--- yaml\n%s\n--- toml\n%s", yamlBytes, tomlBytes)
	}
	if len(tomlLock.Collections) != 2 {
		t.Fatalf("lockfile pins %d collections, want acme.app and acme.lib", len(tomlLock.Collections))
	}
}

// TestGalaxyTOMLReplaysTheYAMLResolution pins that snapshot replay is keyed
// by the parsed roots, not by the file: a galaxy.toml spelling the YAML
// roots identically reruns with no request and skips every install.
func TestGalaxyTOMLReplaysTheYAMLResolution(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start from requirements.yml: %v", err)
	}
	before := f.server.Total()
	if before == 0 {
		t.Fatalf("the first install reached the fake server %d times, want at least one (control)", before)
	}

	useGalaxyTOML(t, f)
	printer := &lineCapturingPrinter{}
	f.runtime = infra.New(printer, f.server.Client())
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start from galaxy.toml: %v", err)
	}
	if got := f.server.Total(); got != before {
		t.Fatalf("the galaxy.toml rerun made %d requests, want none (replayed from the snapshot)", got-before)
	}
	for _, name := range []string{"acme/app/", "acme/lib/"} {
		if want := "Skipping install, already installed: " + name; !printer.hasLineContaining(want) {
			t.Fatalf("rerun did not skip %s:\n%v", name, printer.snapshot())
		}
	}
}

// TestFrozenInstallFromGalaxyTOMLUsesTheYAMLLockfile pins that a lockfile
// written from requirements.yml drives a --frozen install from the equivalent
// galaxy.toml: no resolution on the first run, no request at all on a rerun.
func TestFrozenInstallFromGalaxyTOMLUsesTheYAMLLockfile(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	mustLock(t, f)
	f.server.ResetCounts()

	useGalaxyTOML(t, f)
	f.cfg.Frozen = true
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("frozen Start from galaxy.toml: %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
	if got := f.server.Count(fakegalaxy.EndpointVersionsList); got != 0 {
		t.Fatalf("EndpointVersionsList count = %d, want 0 (the lockfile pins, nothing is resolved)", got)
	}

	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove install tree: %v", err)
	}
	f.server.ResetCounts()
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("frozen rerun from galaxy.toml: %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
	if got := f.server.Total(); got != 0 {
		t.Fatalf("frozen rerun made %d requests, want 0 (pins and artifacts come from the cache)", got)
	}
}

// TestGalaxyTOMLProjectIsRecordedByItsOwnPath pins that the registry keys the
// project by its directory and records galaxy.toml's own absolute path in
// requirements_file, the field cleanup reloads through the extension dispatch.
func TestGalaxyTOMLProjectIsRecordedByItsOwnPath(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	tomlPath := useGalaxyTOML(t, f)
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start from galaxy.toml: %v", err)
	}

	registry, err := store.LoadProjectRegistry(f.cfg.CacheDir)
	if err != nil {
		t.Fatalf("LoadProjectRegistry: %v", err)
	}
	record, ok := registry.Projects[filepath.Dir(tomlPath)]
	if !ok {
		t.Fatalf("no record keyed by %q, got %#v", filepath.Dir(tomlPath), registry.Projects)
	}
	if record.RequirementsFile != tomlPath {
		t.Fatalf("RequirementsFile = %q, want %q", record.RequirementsFile, tomlPath)
	}
}

// useRoleGalaxyTOML is useGalaxyTOML for the role fixture: body lands beside
// the fixture's requirements.yml and the config reads it instead.
func useRoleGalaxyTOML(t *testing.T, f *roleFixture, body string) {
	t.Helper()
	path := filepath.Join(filepath.Dir(f.reqPath), helpers.RequirementsTOMLName)
	if err := os.WriteFile(path, []byte(body), helpers.FileMod); err != nil {
		t.Fatalf("write galaxy.toml: %v", err)
	}
	f.cfg.RequirementsFile = path
}

// TestGalaxyRoleInstallsFromGalaxyTOML pins the role half of the TOML path:
// a roles string in galaxy.toml maps through the v1 API and installs by git
// under the Galaxy name, exactly as the YAML list entry does.
func TestGalaxyRoleInstallsFromGalaxyTOML(t *testing.T) {
	t.Parallel()
	f := newGalaxyRoleFixture(t)
	useRoleGalaxyTOML(t, f, "[project]\nroles = [\"geerlingguy.docker\"]\n")
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
}
