package commands

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/urfave/cli/v3"
)

// These tests drive migrate through a root carrying the global flags, from a
// fixture directory; t.Chdir, t.Setenv and the stdio swap are why nothing here
// is parallel.

// migrateYAML is a requirements.yml with one collection and one role.
const migrateYAML = "collections:\n  - name: acme.widgets\n    version: 1.10\nroles:\n  - geerlingguy.docker\n"

// migrateRoot is the command tree main.go builds, cut to migrate and hash.
func migrateRoot() *cli.Command {
	return &cli.Command{
		Name:     "go-galaxy",
		Flags:    cliflags.CommonFlags(),
		Commands: []*cli.Command{Migrate(), Hash()},
	}
}

// runMigrateCommand runs go-galaxy with args from dir and returns stdout,
// stderr and the command's error, with no variable that could steer the
// input or the dry run set.
func runMigrateCommand(t *testing.T, dir string, args ...string) (string, string, error) {
	t.Helper()
	clearRequirementsFileEnv(t)
	t.Setenv("GO_GALAXY_DRY_RUN", "")
	if err := os.Unsetenv("GO_GALAXY_DRY_RUN"); err != nil {
		t.Fatalf("unset GO_GALAXY_DRY_RUN: %v", err)
	}
	return runMigrateRoot(t, dir, args...)
}

// runMigrateRoot is runMigrateCommand with the environment left as the test
// set it.
func runMigrateRoot(t *testing.T, dir string, args ...string) (string, string, error) {
	t.Helper()
	t.Chdir(dir)
	var err error
	stdout, stderr := captureStdIO(t, func() {
		err = migrateRoot().Run(context.Background(), append([]string{"go-galaxy"}, args...))
	})
	return stdout, stderr, err
}

// writeMigrateFile writes content to dir/name, creating parent directories.
func writeMigrateFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readMigrateFile reads dir/name and fails the test on error.
func readMigrateFile(t *testing.T, dir, name string) string {
	t.Helper()
	//nolint:gosec // G304: the path is this test's own fixture.
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// assertNoGalaxyTOML fails when anything stands at dir/galaxy.toml.
func assertNoGalaxyTOML(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, helpers.RequirementsTOMLName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Lstat(galaxy.toml) = %v, want nothing written", err)
	}
}

// assertMigrateUsage fails unless err carries want and exits 2.
func assertMigrateUsage(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("migrate error = %v, want errors.Is %v", err, want)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitUsage {
		t.Fatalf("exit code = %d, want %d", got, exitcode.ExitUsage)
	}
}

// migrateNextStepLine is the step line migrate prints after a write.
func migrateNextStepLine(target, check, src string) string {
	return "· Check " + target + " with " + check + ", then delete " + src + " unless another tool still reads it\n"
}

// TestMigrateWritesGalaxyTOMLBesideTheInput pins the file's place, its
// project name and mode, and the two lines a write prints.
func TestMigrateWritesGalaxyTOMLBesideTheInput(t *testing.T) {
	dir := t.TempDir()
	writeMigrateFile(t, dir, "sub/requirements.yml", migrateYAML)
	stdout, stderr, err := runMigrateCommand(t, dir, "migrate", "-r", "sub/requirements.yml")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	want := "[project]\nname = \"sub\"\ncollections = [\n  \"acme.widgets 1.10\",\n]\nroles = [\n  \"geerlingguy.docker\",\n]\n"
	if got := readMigrateFile(t, dir, "sub/galaxy.toml"); got != want {
		t.Fatalf("galaxy.toml = %q, want %q", got, want)
	}
	info, err := os.Lstat(filepath.Join(dir, "sub", "galaxy.toml"))
	if err != nil || info.Mode().Perm() != helpers.FileMod {
		t.Fatalf("galaxy.toml mode = %v (%v), want %o", info, err, helpers.FileMod)
	}
	wantOut := "✔ Wrote sub/galaxy.toml (collections: 1, roles: 1)\n" +
		migrateNextStepLine("sub/galaxy.toml", "go-galaxy install --dry-run -r sub/galaxy.toml", "sub/requirements.yml")
	if stdout != wantOut {
		t.Fatalf("stdout = %q, want %q", stdout, wantOut)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want none", stderr)
	}
}

// TestMigrateNamesLockCheckBesideALockfile pins that the next step names
// lock --check only beside a regular galaxy.lock.
func TestMigrateNamesLockCheckBesideALockfile(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, dir string) string{
		"regular lockfile": func(t *testing.T, dir string) string {
			t.Helper()
			writeMigrateFile(t, dir, "sub/galaxy.lock", "collections: []\n")
			return "go-galaxy lock --check -r sub/galaxy.toml"
		},
		"lockfile directory": func(t *testing.T, dir string) string {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(dir, "sub", "galaxy.lock"), helpers.DirMod); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			return "go-galaxy install --dry-run -r sub/galaxy.toml"
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeMigrateFile(t, dir, "sub/requirements.yml", migrateYAML)
			check := plant(t, dir)
			stdout, _, err := runMigrateCommand(t, dir, "migrate", "-r", "sub/requirements.yml")
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if want := migrateNextStepLine("sub/galaxy.toml", check, "sub/requirements.yml"); !strings.HasSuffix(stdout, want) {
				t.Fatalf("stdout = %q, want it to end with %q", stdout, want)
			}
		})
	}
}

// TestMigrateReadsRequirementsYMLByDefault pins the unset -r: the working
// directory's requirements.yml, and galaxy.toml beside it named after the
// directory.
func TestMigrateReadsRequirementsYMLByDefault(t *testing.T) {
	dir := t.TempDir()
	writeMigrateFile(t, dir, helpers.RequirementsYAMLName, migrateYAML)
	stdout, _, err := runMigrateCommand(t, dir, "migrate")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := readMigrateFile(t, dir, helpers.RequirementsTOMLName); !strings.Contains(got, "name = \""+filepath.Base(dir)+"\"\n") {
		t.Fatalf("galaxy.toml = %q, want the directory's name", got)
	}
	if !strings.HasPrefix(stdout, "✔ Wrote galaxy.toml (collections: 1, roles: 1)\n") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// TestMigrateNeverDiscoversGalaxyTOML pins that a galaxy.toml alone is not an
// input: the absent requirements.yml exits 2 and galaxy.toml is untouched.
func TestMigrateNeverDiscoversGalaxyTOML(t *testing.T) {
	dir := t.TempDir()
	const toml = "[project]\ncollections = [\"acme.widgets\"]\n"
	writeMigrateFile(t, dir, helpers.RequirementsTOMLName, toml)
	_, _, err := runMigrateCommand(t, dir, "migrate")
	assertMigrateUsage(t, err, fs.ErrNotExist)
	if got := readMigrateFile(t, dir, helpers.RequirementsTOMLName); got != toml {
		t.Fatalf("galaxy.toml = %q, want it untouched", got)
	}
}

// TestMigrateIgnoresRequirementsFileVariables pins that neither variable, set
// or exported empty, steers migrate's input.
func TestMigrateIgnoresRequirementsFileVariables(t *testing.T) {
	for name, value := range map[string]string{"other.yml": "other.yml", "exported empty": ""} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeMigrateFile(t, dir, helpers.RequirementsYAMLName, migrateYAML)
			for _, key := range requirementsFileEnvKeys() {
				t.Setenv(key, value)
			}
			if _, _, err := runMigrateRoot(t, dir, "migrate"); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if got := readMigrateFile(t, dir, helpers.RequirementsTOMLName); !strings.Contains(got, "acme.widgets") {
				t.Fatalf("galaxy.toml = %q, want requirements.yml's entries", got)
			}
		})
	}
}

// TestMigrateRefusesItsInputByName pins exit 2 for an -r not named *.yml or
// *.yaml, with nothing written.
func TestMigrateRefusesItsInputByName(t *testing.T) {
	for _, input := range []string{helpers.RequirementsTOMLName, "deps.txt", ""} {
		t.Run(input, func(t *testing.T) {
			dir := t.TempDir()
			_, _, err := runMigrateCommand(t, dir, "migrate", "-r", input)
			assertMigrateUsage(t, err, helpers.ErrMigrateSourceName)
			assertNoGalaxyTOML(t, dir)
		})
	}
}

// existingTargets plants something at dir/galaxy.toml: a file, a directory
// or a dangling symlink.
func existingTargets() map[string]func(t *testing.T, dir string) {
	return map[string]func(t *testing.T, dir string){
		"file": func(t *testing.T, dir string) {
			t.Helper()
			writeMigrateFile(t, dir, helpers.RequirementsTOMLName, "[project]\ncollections = []\n")
		},
		"directory": func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(dir, helpers.RequirementsTOMLName), helpers.DirMod); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		},
		"dangling symlink": func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Symlink("nowhere", filepath.Join(dir, helpers.RequirementsTOMLName)); err != nil {
				t.Fatalf("symlink: %v", err)
			}
		},
		"file in a read-only directory": plantInReadOnlyDir,
	}
}

// plantInReadOnlyDir plants a file and takes write permission from dir, so a
// real run must refuse the target before it tries to create a temp there.
func plantInReadOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	writeMigrateFile(t, dir, helpers.RequirementsTOMLName, "[project]\ncollections = []\n")
	//nolint:gosec // G302: a directory mode with no write bit, so no temp can be created in it.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, helpers.DirMod); err != nil {
			t.Errorf("restore dir mode: %v", err)
		}
	})
}

// TestMigrateRefusesAnExistingGalaxyTOML pins that nothing standing at the
// target is replaced or followed, and that no success line is printed.
func TestMigrateRefusesAnExistingGalaxyTOML(t *testing.T) {
	for name, plant := range existingTargets() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeMigrateFile(t, dir, helpers.RequirementsYAMLName, migrateYAML)
			plant(t, dir)
			stdout, _, err := runMigrateCommand(t, dir, "migrate")
			assertMigrateUsage(t, err, helpers.ErrProjectFileExists)
			if strings.Contains(stdout, "✔") {
				t.Fatalf("stdout = %q, want no success line", stdout)
			}
			if _, err := os.Lstat(filepath.Join(dir, "nowhere")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("Lstat(nowhere) = %v, want the symlink target never created", err)
			}
		})
	}
}

// TestMigrateDryRunPrintsAndWritesNothing pins that --dry-run, as a flag or
// through GO_GALAXY_DRY_RUN, prints the bytes a real run writes and writes
// nothing.
func TestMigrateDryRunPrintsAndWritesNothing(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real", "infra")
	writeMigrateFile(t, realDir, helpers.RequirementsYAMLName, migrateYAML)
	if _, _, err := runMigrateCommand(t, realDir, "migrate"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	written := readMigrateFile(t, realDir, helpers.RequirementsTOMLName)

	dry := filepath.Join(root, "dry", "infra")
	writeMigrateFile(t, dry, helpers.RequirementsYAMLName, migrateYAML)
	stdout, _, err := runMigrateCommand(t, dry, "migrate", "--dry-run")
	if err != nil {
		t.Fatalf("migrate --dry-run: %v", err)
	}
	if stdout != written {
		t.Fatalf("--dry-run stdout = %q, want the written bytes %q", stdout, written)
	}
	assertNoGalaxyTOML(t, dry)

	t.Setenv("GO_GALAXY_DRY_RUN", "true")
	stdout, _, err = runMigrateRoot(t, dry, "migrate")
	if err != nil || stdout != written {
		t.Fatalf("GO_GALAXY_DRY_RUN: stdout = %q, error = %v, want the written bytes", stdout, err)
	}
	assertNoGalaxyTOML(t, dry)
}

// TestMigrateDryRunReportsAnExistingGalaxyTOML pins that --dry-run prints the
// file and still exits as a real run would over an existing target.
func TestMigrateDryRunReportsAnExistingGalaxyTOML(t *testing.T) {
	dir := t.TempDir()
	writeMigrateFile(t, dir, helpers.RequirementsYAMLName, migrateYAML)
	writeMigrateFile(t, dir, helpers.RequirementsTOMLName, "[project]\ncollections = []\n")
	stdout, _, err := runMigrateCommand(t, dir, "migrate", "--dry-run")
	assertMigrateUsage(t, err, helpers.ErrProjectFileExists)
	if !strings.HasPrefix(stdout, "[project]\n") || !strings.Contains(stdout, "acme.widgets 1.10") {
		t.Fatalf("stdout = %q, want the rendered file", stdout)
	}
}

// TestMigratePrintsNoticesOnStderr pins each notice as a warning line on
// stderr, prefixed with the input path, and the file written regardless.
func TestMigratePrintsNoticesOnStderr(t *testing.T) {
	dir := t.TempDir()
	writeMigrateFile(t, dir, helpers.RequirementsYAMLName, "# pinned\n"+migrateYAML+"extra: 1\n")
	stdout, stderr, err := runMigrateCommand(t, dir, "migrate")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	want := "! requirements.yml: top-level key \"extra\" is not carried into galaxy.toml\n" +
		"! requirements.yml: comments are not carried into galaxy.toml\n"
	if stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
	if strings.Contains(stdout, "not carried") {
		t.Fatalf("stdout = %q carries a notice", stdout)
	}
	if got := readMigrateFile(t, dir, helpers.RequirementsTOMLName); !strings.Contains(got, "acme.widgets") {
		t.Fatalf("galaxy.toml = %q", got)
	}
}

// TestMigrateWritesNothingWhenAnEntryIsRefused pins exit 2 and no file for an
// entry galaxy.toml cannot hold.
func TestMigrateWritesNothingWhenAnEntryIsRefused(t *testing.T) {
	for name, yaml := range map[string]string{
		"version with no value":     "collections:\n  - name: acme.widgets\n    version:\n",
		"constraint semver refuses": "collections:\n  - name: acme.widgets\n    version: latest\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeMigrateFile(t, dir, helpers.RequirementsYAMLName, yaml)
			_, _, err := runMigrateCommand(t, dir, "migrate")
			if got := exitcode.FromError(err); got != exitcode.ExitUsage {
				t.Fatalf("exit code = %d (%v), want %d", got, err, exitcode.ExitUsage)
			}
			assertNoGalaxyTOML(t, dir)
		})
	}
}

// TestMigratedFileKeepsTheLockfilePath pins that the new file finds the same
// galaxy.lock: hash prints the lockfile's key from either file.
func TestMigratedFileKeepsTheLockfilePath(t *testing.T) {
	dir := t.TempDir()
	writeMigrateFile(t, dir, "sub/requirements.yml", migrateYAML)
	discoveryLockfile(t, filepath.Join(dir, "sub"))
	lf, err := lockfile.Load(filepath.Join(dir, "sub", lockfile.DefaultName))
	if err != nil {
		t.Fatalf("load lockfile: %v", err)
	}
	sum, err := lf.Hash()
	if err != nil {
		t.Fatalf("lockfile hash: %v", err)
	}
	if _, _, err := runMigrateCommand(t, dir, "migrate", "-r", "sub/requirements.yml"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, input := range []string{"sub/requirements.yml", "sub/galaxy.toml"} {
		stdout, _, err := runMigrateCommand(t, dir, "hash", "-r", input)
		if err != nil || stdout != "sha256:"+sum+"\n" {
			t.Fatalf("hash -r %s = %q (%v), want the lockfile's key sha256:%s", input, stdout, err, sum)
		}
	}
}

// TestMigratedFileKeepsTheHashKey pins that with no lockfile the key hash
// derives from the new file equals the one from requirements.yml.
func TestMigratedFileKeepsTheHashKey(t *testing.T) {
	dir := t.TempDir()
	yaml := "collections:\n  - name: acme.widgets\n    version: 1.10\n    type: galaxy\n" +
		"  - git+https://git.example.com/acme/mono.git#collections/app,main\n" +
		"roles:\n  - {src: geerlingguy.postgresql, version: 3.5.0, name: postgres}\n"
	writeMigrateFile(t, dir, helpers.RequirementsYAMLName, yaml)
	if _, _, err := runMigrateCommand(t, dir, "migrate"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	lockPath := filepath.Join(dir, lockfile.DefaultName)
	fromYAML, err := computeHash(filepath.Join(dir, helpers.RequirementsYAMLName), lockPath)
	if err != nil {
		t.Fatalf("hash requirements.yml: %v", err)
	}
	fromTOML, err := computeHash(filepath.Join(dir, helpers.RequirementsTOMLName), lockPath)
	if err != nil {
		t.Fatalf("hash galaxy.toml: %v", err)
	}
	if fromYAML != fromTOML {
		t.Fatalf("hash keys differ: %s from requirements.yml, %s from galaxy.toml", fromYAML, fromTOML)
	}
}

// TestMigrateProjectNameAtTheRoot pins that a file at the file system root
// gets no project name rather than the separator.
func TestMigrateProjectNameAtTheRoot(t *testing.T) {
	name, err := migrateProjectName(string(filepath.Separator) + helpers.RequirementsYAMLName)
	if err != nil || name != "" {
		t.Fatalf("migrateProjectName = %q, %v; want \"\" and nil", name, err)
	}
}
