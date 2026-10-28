package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/urfave/cli/v3"
)

const (
	treeLibURL  = "https://files.example.com/acme-lib-1.0.0.tar.gz"
	treeGoneURL = "https://files.example.com/acme-gone-1.0.0.tar.gz"
	treeRoleURL = "https://files.example.com/role-web.tar.gz"
	treeLibSHA  = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	treeRoleSHA = "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae"
)

// treeURLRequirements names a Galaxy root, a url root the lockfile holds, a
// url root it does not, and a url role.
const treeURLRequirements = "collections:\n" +
	"  - name: acme.app\n" +
	"  - name: " + treeLibURL + "\n    type: url\n" +
	"  - name: " + treeGoneURL + "\n    type: url\n" +
	"roles:\n" +
	"  - src: " + treeRoleURL + "\n    name: web\n"

// treeURLLockfile locks acme.app over acme.lib twice, once through acme.util,
// and the url role web over base twice, once through common.
func treeURLLockfile(t *testing.T) *lockfile.File {
	t.Helper()
	source := canonicalGitSource(t)
	return &lockfile.File{
		Collections: []lockfile.Entry{
			{Name: "acme.app", Version: "2.0.0", DownloadURL: testDownloadURL("acme.app", "2.0.0"), Deps: []string{"acme.util", "acme.lib"}},
			{Name: "acme.util", Version: "1.1.0", DownloadURL: testDownloadURL("acme.util", "1.1.0"), Deps: []string{"acme.lib"}},
			{Name: "acme.lib", Version: "1.0.0", Type: lockfile.TypeURL, Source: treeLibURL, SHA256: treeLibSHA},
		},
		Roles: []lockfile.RoleEntry{
			{Name: "web", Type: lockfile.RoleTypeURL, Version: "1.0", Source: treeRoleURL, SHA256: treeRoleSHA, Deps: []string{"common", "base"}},
			{
				Name: "common", Type: lockfile.RoleTypeGit, Version: "main", Source: source, Ref: "main", Commit: gitTestCommit,
				Deps: []string{"base"},
			},
			{Name: "base", Type: lockfile.RoleTypeGit, Version: "main", Source: source, Ref: "main", Commit: gitTestCommit},
		},
	}
}

// writeTreeFixture writes requirements and, when lf is not nil, galaxy.lock
// beside it in a fresh directory, and returns the directory.
func writeTreeFixture(t *testing.T, requirementsBody string, lf *lockfile.File) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.yml"), []byte(requirementsBody), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
	if lf != nil {
		if err := lockfile.Save(filepath.Join(dir, "galaxy.lock"), lf); err != nil {
			t.Fatalf("lockfile.Save: %v", err)
		}
	}
	return dir
}

// runTreeIn runs the tree command from dir and returns its stdout and error.
func runTreeIn(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	var err error
	stdout, _ := captureStdIO(t, func() {
		err = runCommandInDir(t, dir, Tree(), args...)
	})
	return stdout, err
}

// TestTreePrintsURLSourcesAndMarksRevisits pins tree over url sources: a url
// entry shows its tarball and sha256 prefix, an unlocked one its locator, and
// a revisited node (*).
func TestTreePrintsURLSourcesAndMarksRevisits(t *testing.T) {
	clearRequirementsFileEnv(t)
	t.Setenv("GO_GALAXY_LOCK_FILE", "")
	if err := os.Unsetenv("GO_GALAXY_LOCK_FILE"); err != nil {
		t.Fatalf("unset GO_GALAXY_LOCK_FILE: %v", err)
	}
	dir := writeTreeFixture(t, treeURLRequirements, treeURLLockfile(t))

	stdout, err := runTreeIn(t, dir)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	source := canonicalGitSource(t)
	libOrigin := " (url " + treeLibURL + " sha256:" + treeLibSHA[:helpers.ArtifactKeyFingerprintLen] + ")"
	gitOrigin := " (git " + source + " @" + gitTestCommit + ")"
	want := strings.Join([]string{
		"requirements.yml",
		"├── acme.app 2.0.0",
		"│   ├── acme.lib 1.0.0" + libOrigin,
		"│   └── acme.util 1.1.0",
		"│       └── acme.lib 1.0.0 (*)",
		"├── acme.lib 1.0.0" + libOrigin,
		"└── url+" + treeGoneURL + "# (missing in lockfile)",
		"roles:",
		"└── web 1.0 (url " + treeRoleURL + " sha256:" + treeRoleSHA[:helpers.ArtifactKeyFingerprintLen] + ")",
		"    ├── base main" + gitOrigin,
		"    └── common main" + gitOrigin,
		"        └── base main (*)",
		"",
	}, "\n")
	if stdout != want {
		t.Fatalf("tree printed\n%s\nwant\n%s", stdout, want)
	}
}

// TestTreeRefusesAnUnusableRequirementsFile pins that tree refuses a
// requirements name it cannot read and a requirements file that does not
// load, both before printing anything.
func TestTreeRefusesAnUnusableRequirementsFile(t *testing.T) {
	clearRequirementsFileEnv(t)
	dir := writeTreeFixture(t, "collections:\n  - name:\n", treeURLLockfile(t))

	stdout, err := runTreeIn(t, dir, "-r", "requirements.txt")
	if !errors.Is(err, helpers.ErrRequirementsFileName) || stdout != "" {
		t.Errorf("tree -r requirements.txt = (%q, %v), want no output and %v", stdout, err, helpers.ErrRequirementsFileName)
	}

	stdout, err = runTreeIn(t, dir)
	if !errors.Is(err, helpers.ErrInvalidCollectionEntry) || stdout != "" {
		t.Errorf("tree over an entry with no name = (%q, %v), want no output and %v", stdout, err, helpers.ErrInvalidCollectionEntry)
	}
	if err != nil && !strings.HasPrefix(err.Error(), "load requirements requirements.yml: ") {
		t.Errorf("error = %q, want it to name the requirements file it loaded", err)
	}
}

// TestExplainReadsTheDiscoveredGalaxyToml pins explain under discovery: with
// both files present it warns, reads galaxy.toml, and credits a role root to
// it; a requirements name it cannot read is refused first.
func TestExplainReadsTheDiscoveredGalaxyToml(t *testing.T) {
	clearRequirementsFileEnv(t)
	t.Setenv("GO_GALAXY_LOCK_FILE", "")
	if err := os.Unsetenv("GO_GALAXY_LOCK_FILE"); err != nil {
		t.Fatalf("unset GO_GALAXY_LOCK_FILE: %v", err)
	}
	dir := writeTreeFixture(t, "collections: []\n", treeURLLockfile(t))
	toml := "[project]\ncollections = [\"acme.app\"]\nroles = [{ src = \"" + treeRoleURL + "\", name = \"web\" }]\n"
	if err := os.WriteFile(filepath.Join(dir, helpers.RequirementsTOMLName), []byte(toml), helpers.FileMod); err != nil {
		t.Fatalf("write galaxy.toml: %v", err)
	}

	var err error
	stdout, stderr := captureStdIO(t, func() { err = runCommandInDir(t, dir, Explain(), "web") })
	if err != nil {
		t.Fatalf("explain web: %v", err)
	}
	for _, want := range []string{
		"role web 1.0\n", "  required by:\n    - galaxy.toml (root)\n", "  depends on:\n    - role base\n    - role common\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("explain web output lacks %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stderr, "galaxy.toml and requirements.yml are both present") {
		t.Errorf("stderr = %q, want the discovery warning", stderr)
	}

	_, _ = captureStdIO(t, func() { err = runCommandInDir(t, dir, Explain(), "-r", "requirements.txt", "web") })
	if !errors.Is(err, helpers.ErrRequirementsFileName) {
		t.Errorf("explain -r requirements.txt = %v, want %v", err, helpers.ErrRequirementsFileName)
	}
}

// TestCollectionCommandsBuildTheirConfigFirst pins that each command sharing
// runCollectionCommand builds its Config first, so a requirements name it
// cannot read is refused with the usage sentinel and nothing is written.
func TestCollectionCommandsBuildTheirConfigFirst(t *testing.T) {
	clearRequirementsFileEnv(t)
	dir := t.TempDir()
	for _, cmd := range []*cli.Command{Install(), Lock(), Warm(), Outdated(), Cleanup()} {
		err := runCommandInDir(t, dir, cmd, "-r", "requirements.txt")
		if !errors.Is(err, helpers.ErrRequirementsFileName) {
			t.Errorf("%s -r requirements.txt = %v, want %v", cmd.Name, err, helpers.ErrRequirementsFileName)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("working directory holds %v (err %v), want nothing written by a refused command", entries, err)
	}
}
