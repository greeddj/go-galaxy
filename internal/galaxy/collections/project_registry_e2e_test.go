package collections_test

// These e2e tests pin when install, warm and lock record their project in the
// cleanup registry: only once the requirements file loads, so a run failing on
// its file leaves the directory's last good record, and so cleanup, as it was.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/cleanup"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// registryCommand is one of the three commands that record a project.
type registryCommand struct {
	run  func(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error
	name string
}

// registryCommands lists install, warm and lock, each recording through the
// same helper once its requirements file loads.
func registryCommands() []registryCommand {
	return []registryCommand{
		{name: "install", run: collections.Start},
		{name: "warm", run: collections.Warm},
		{name: "lock", run: collections.Lock},
	}
}

// failingRequirementsFile is one requirements path that fails to load, and the
// sentinel the failure carries.
type failingRequirementsFile struct {
	want error
	name string
	path string
}

// plantFailingRequirements lays out in dir every shape of requirements file
// that fails to load: a typo'd name never written, a directory, bytes that are
// not YAML, and a galaxy.toml whose constraint semver cannot parse.
func plantFailingRequirements(t *testing.T, dir string) []failingRequirementsFile {
	t.Helper()
	if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	directory := filepath.Join(dir, "dir.yml")
	if err := os.Mkdir(directory, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", directory, err)
	}
	notYAML := filepath.Join(dir, "broken.yml")
	if err := os.WriteFile(notYAML, []byte("collections:\n  - name: [unclosed\n"), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", notYAML, err)
	}
	badTOML := filepath.Join(dir, helpers.RequirementsTOMLName)
	if err := os.WriteFile(badTOML, []byte("[project]\ncollections = [\"acme.app >= 0..20\"]\n"), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", badTOML, err)
	}
	return []failingRequirementsFile{
		{name: "a missing file", path: filepath.Join(dir, "requirement.yml"), want: os.ErrNotExist},
		{name: "a directory", path: directory, want: helpers.ErrRequirementsNotRegular},
		{name: "bytes that are not YAML", path: notYAML, want: helpers.ErrInvalidRequirementsYAML},
		{name: "a galaxy.toml constraint semver cannot parse", path: badTOML, want: helpers.ErrInvalidCollectionConstraint},
	}
}

// loadRegistry reads the local registry under cacheDir, failing the test on
// any error.
func loadRegistry(t *testing.T, cacheDir string) map[string]store.ProjectRecord {
	t.Helper()
	registry, err := store.LoadProjectRegistry(cacheDir)
	if err != nil {
		t.Fatalf("LoadProjectRegistry: %v", err)
	}
	return registry.Projects
}

// TestFailedLoadKeepsProjectRecord runs install, warm and lock over every
// failing requirements shape, beside the installed project and in a directory
// never recorded: each fails on its file, and the registry stays deep-equal.
func TestFailedLoadKeepsProjectRecord(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (good install): %v", err)
	}
	projectDir := filepath.Dir(f.cfg.RequirementsFile)
	want := loadRegistry(t, f.cfg.CacheDir)
	if record, ok := want[projectDir]; !ok || record.RequirementsFile != f.cfg.RequirementsFile {
		t.Fatalf("good install recorded %#v, want %q under %q", want, f.cfg.RequirementsFile, projectDir)
	}

	otherDir := filepath.Join(t.TempDir(), "other")
	shapes := append(plantFailingRequirements(t, filepath.Join(projectDir, "failing")),
		plantFailingRequirements(t, otherDir)...)
	// The typo'd file sits beside the recorded one too, the shape a mistyped -r takes.
	shapes = append(shapes, failingRequirementsFile{
		name: "a typo beside the recorded file", path: filepath.Join(projectDir, "requirement.yml"), want: os.ErrNotExist,
	})
	for _, cmd := range registryCommands() {
		for _, shape := range shapes {
			cfg := *f.cfg
			cfg.RequirementsFile = shape.path
			err := cmd.run(context.Background(), &cfg, f.runtime)
			if !errors.Is(err, shape.want) {
				t.Fatalf("%s over %s (%s): error = %v, want errors.Is %v", cmd.name, shape.name, shape.path, err, shape.want)
			}
			if got := loadRegistry(t, f.cfg.CacheDir); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s over %s (%s) changed the registry:\n got %#v\nwant %#v", cmd.name, shape.name, shape.path, got, want)
			}
		}
	}
}

// TestNetworkFailureAfterLoadStillRecords pins the other side: a file that
// loads is recorded even when the run then fails against its server, since
// what the run did install or cache is that file's to keep.
func TestNetworkFailureAfterLoadStillRecords(t *testing.T) {
	t.Parallel()
	for _, cmd := range registryCommands() {
		t.Run(cmd.name, func(t *testing.T) {
			t.Parallel()
			f := newE2EFixture(t)
			f.server.Fail(fakegalaxy.EndpointRootMetadata, "", "", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})
			f.server.Fail(fakegalaxy.EndpointVersionsList, "", "", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

			if err := cmd.run(context.Background(), f.cfg, f.runtime); err == nil {
				t.Fatalf("%s against a failing server succeeded, want an error", cmd.name)
			}
			record, ok := loadRegistry(t, f.cfg.CacheDir)[filepath.Dir(f.cfg.RequirementsFile)]
			if !ok || record.RequirementsFile != f.cfg.RequirementsFile {
				t.Fatalf("%s: registry record = %#v (present %v), want %q recorded", cmd.name, record, ok, f.cfg.RequirementsFile)
			}
		})
	}
}

// TestCleanupAfterFailedTypoRunRemovesNothing is the destructive case: a
// mistyped -r used to replace the record with a file that does not exist, and
// cleanup then deleted every install the project's real file reaches.
func TestCleanupAfterFailedTypoRunRemovesNothing(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (good install): %v", err)
	}
	typo := *f.cfg
	typo.RequirementsFile = filepath.Join(filepath.Dir(f.cfg.RequirementsFile), "requirement.yml")
	if err := collections.Start(context.Background(), &typo, f.runtime); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Start (typo): error = %v, want errors.Is os.ErrNotExist", err)
	}

	if err := cleanup.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("cleanup.Start: %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")

	// Control: once the recorded file itself names nothing, the same cleanup
	// removes both, so the survival above is the record's doing.
	if err := os.WriteFile(f.cfg.RequirementsFile, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("rewrite requirements: %v", err)
	}
	if err := cleanup.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("cleanup.Start (control): %v", err)
	}
	assertPathAbsent(t, manifestPathFor(f.downloadPath, "app"))
	assertPathAbsent(t, manifestPathFor(f.downloadPath, "lib"))
}

// TestCleanupKeepsWhatEveryLoadedFileReaches is the second destructive case:
// install, then install -r requirements-dev.yml in the same directory, used to
// leave a record naming only the second file, so cleanup removed acme.app.
func TestCleanupKeepsWhatEveryLoadedFileReaches(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.server.AddVersion("acme", "dev", testVersion100, nil)
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (requirements.yml): %v", err)
	}
	dev := *f.cfg
	dev.RequirementsFile = filepath.Join(filepath.Dir(f.cfg.RequirementsFile), "requirements-dev.yml")
	writeRequirements(t, dev.RequirementsFile, "acme.dev")
	if err := collections.Start(context.Background(), &dev, f.runtime); err != nil {
		t.Fatalf("Start (requirements-dev.yml): %v", err)
	}

	if err := cleanup.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("cleanup.Start: %v", err)
	}
	for _, name := range []string{"app", "lib", "dev"} {
		assertManifestInstalled(t, f.downloadPath, name)
	}

	// requirements-dev.yml goes; the next record forgets it, and cleanup then
	// removes what only it reached, the control for the survival above.
	if err := os.Remove(dev.RequirementsFile); err != nil {
		t.Fatalf("remove requirements-dev.yml: %v", err)
	}
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (requirements.yml again): %v", err)
	}
	record := loadRegistry(t, f.cfg.CacheDir)[filepath.Dir(f.cfg.RequirementsFile)]
	if want := []string{f.cfg.RequirementsFile}; !reflect.DeepEqual(record.RequirementsFiles, want) {
		t.Fatalf("RequirementsFiles = %q, want %q once requirements-dev.yml is gone", record.RequirementsFiles, want)
	}
	if err := cleanup.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("cleanup.Start (after removal): %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
	assertPathAbsent(t, manifestPathFor(f.downloadPath, "dev"))
}

// assertRecordedPaths fails the test unless the local registry under cacheDir
// records projectDir with exactly these install paths.
func assertRecordedPaths(t *testing.T, cacheDir, projectDir, collectionsPath, rolesPath string) {
	t.Helper()
	record, ok := loadRegistry(t, cacheDir)[projectDir]
	if !ok || record.CollectionsPath != collectionsPath || record.RolesPath != rolesPath {
		t.Fatalf("record under %q = %+v (present %v), want collections_path %q and roles_path %q",
			projectDir, record, ok, collectionsPath, rolesPath)
	}
}

// TestCleanupFindsTheInstallsOfAProjectRunFromItsParent is install -r
// sub/requirements.yml run from sub's parent with relative install paths: the
// record names the parent's trees, and cleanup prunes them. Not parallel: t.Chdir.
func TestCleanupFindsTheInstallsOfAProjectRunFromItsParent(t *testing.T) {
	f := newRoleFixture(t)
	parent := physicalTempDir(t)
	projectDir := filepath.Join(parent, "sub")
	if err := os.Mkdir(projectDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", projectDir, err)
	}
	f.reqPath = filepath.Join(projectDir, "requirements.yml")
	f.downloadPath = filepath.Join(parent, ".collections")
	f.rolesPath = filepath.Join(parent, ".roles")
	f.cfg.RequirementsFile = filepath.Join("sub", "requirements.yml")
	f.cfg.DownloadPath = ".collections"
	f.cfg.RolesPath = ".roles"
	f.galaxy.AddVersion("acme", "extra", testVersion100, nil)
	f.writeRequirements(t, "collections:\n  - name: acme.lib\n  - name: acme.extra\n"+
		"roles:\n  - src: git+"+roleAppURL+"\n    name: app\n"+
		"  - src: git+"+roleBaseURL+"\n    version: v1.0.0\n    name: pinned\n")
	t.Chdir(parent)
	f.mustInstall(t)
	assertManifestInstalled(t, f.downloadPath, "extra")
	assertPathAbsent(t, filepath.Join(projectDir, ".collections"))
	assertPathAbsent(t, filepath.Join(projectDir, ".roles"))
	assertRecordedPaths(t, f.cacheDir, projectDir, f.downloadPath, f.rolesPath)

	// A lock run elsewhere, where the same relative paths name other trees,
	// writes no install tree and so leaves the recorded paths as they are.
	t.Chdir(t.TempDir())
	lockCfg := *f.cfg
	lockCfg.RequirementsFile = f.reqPath
	if err := collections.Lock(context.Background(), &lockCfg, f.runtime); err != nil {
		t.Fatalf("Lock (from another directory): %v", err)
	}
	assertRecordedPaths(t, f.cacheDir, projectDir, f.downloadPath, f.rolesPath)

	pinnedLocator := gitsource.Locator{URL: roleBaseURL, Commit: fakeCommit("role-base-1")}.String()
	pinnedArtifact := roleArtifactPath(f, pinnedLocator, "pinned", "v1.0.0")
	if _, err := os.Stat(pinnedArtifact); err != nil {
		t.Fatalf("pinned role's artifact before cleanup: %v", err)
	}
	f.writeRequirements(t, "collections:\n  - name: acme.lib\nroles:\n  - src: git+"+roleAppURL+"\n    name: app\n")
	mustCleanup(t, f.gitFixture)
	assertPathAbsent(t, installPathFor(f.downloadPath, "extra"))
	assertPathAbsent(t, f.rolePath("pinned"))
	assertPathAbsent(t, pinnedArtifact)
	assertManifestInstalled(t, f.downloadPath, "lib")
	for _, name := range []string{"app", "base"} {
		if _, err := os.Stat(f.rolePath(name)); err != nil {
			t.Fatalf("role %s after cleanup: %v", name, err)
		}
	}
}

// physicalTempDir is a fresh t.TempDir with its symlinks resolved, the spelling
// the kernel gives the working directory; t.TempDir sits under a symlink on macOS.
func physicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return dir
}

// TestCleanupPrunesWhatInstallWroteFromASymlinkedDirectory installs from real/x
// entered by the link repo/deploy with ../ paths: the record and the role's
// install_path name real's trees, which cleanup prunes. Not parallel: t.Chdir.
func TestCleanupPrunesWhatInstallWroteFromASymlinkedDirectory(t *testing.T) {
	f := newRoleFixture(t)
	root := physicalTempDir(t)
	target := filepath.Join(root, "real", "x")
	link := filepath.Join(root, "repo", "deploy")
	for _, dir := range []string{target, filepath.Dir(link)} {
		if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s: %v", link, err)
	}
	f.reqPath = filepath.Join(target, "requirements.yml")
	f.downloadPath = filepath.Join(root, "real", "coll")
	f.rolesPath = filepath.Join(root, "real", "roles")
	f.cfg.RequirementsFile = "requirements.yml"
	f.cfg.DownloadPath = filepath.Join("..", "coll")
	f.cfg.RolesPath = filepath.Join("..", "roles")
	f.galaxy.AddVersion("acme", "extra", testVersion100, nil)
	f.writeRequirements(t, "collections:\n  - name: acme.lib\n  - name: acme.extra\n"+
		"roles:\n  - src: git+"+roleBaseURL+"\n    version: v1.0.0\n    name: pinned\n")
	t.Chdir(link)
	f.mustInstall(t)
	assertManifestInstalled(t, f.downloadPath, "extra")
	assertPathAbsent(t, filepath.Join(root, "repo", "coll"))
	assertRecordedPaths(t, f.cacheDir, target, f.downloadPath, f.rolesPath)
	if got := loadInstalledRole(t, f, "pinned").InstallPath; got != f.rolePath("pinned") {
		t.Fatalf("pinned role's recorded install_path = %q, want %q", got, f.rolePath("pinned"))
	}

	f.writeRequirements(t, "collections:\n  - name: acme.lib\n")
	mustCleanup(t, f.gitFixture)
	assertPathAbsent(t, installPathFor(f.downloadPath, "extra"))
	assertPathAbsent(t, f.rolePath("pinned"))
	assertManifestInstalled(t, f.downloadPath, "lib")
}
