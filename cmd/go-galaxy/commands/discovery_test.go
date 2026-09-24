package commands

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/urfave/cli/v3"
)

// These tests drive hash, tree and explain with no --requirements-file
// through the real commands, pinning that they pick galaxy.toml over
// requirements.yml, warn when both exist, and fall back without a Stat.

// discoveryTOML is the galaxy.toml fixture; discoveryTOMLSHA256 is the
// sha256 of exactly these bytes, spelled out so hash's fallback key is pinned
// against a literal rather than re-derived from the constant.
const (
	discoveryTOML       = "[project]\nname = \"infra\"\ncollections = [\"acme.widgets\"]\n"
	discoveryTOMLSHA256 = "6bc1930e0ea48b90d8fad00afee84a8d98dca37b549686d67bb135769a0c57b0"
	discoveryYAML       = "collections:\n  - name: acme.other\n    version: \"*\"\n"
	bothPresentWarning  = "galaxy.toml and requirements.yml are both present in the current directory; " +
		"using galaxy.toml and ignoring requirements.yml (name one with --requirements-file to choose)"
)

// requirementsFileEnvKeys returns the variables that make the requirements
// file flag count as set, which every discovery row must start without.
func requirementsFileEnvKeys() []string {
	return []string{"GO_GALAXY_REQUIREMENTS_FILE", "ANSIBLE_GALAXY_REQUIREMENTS_FILE"}
}

// clearRequirementsFileEnv unsets both flag variables for the test, restoring
// them on cleanup through t.Setenv; an exported-empty value counts as set,
// so Setenv("") alone would not do.
func clearRequirementsFileEnv(t *testing.T) {
	t.Helper()
	for _, key := range requirementsFileEnvKeys() {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
}

// captureStdIO swaps the process-wide os.Stdout/os.Stderr for pipes while fn
// runs, draining both concurrently so fn cannot block, and returns what each
// received; hash, tree and explain write to those vars at call time.
func captureStdIO(t *testing.T, fn func()) (string, string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stdout): %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stderr): %v", err)
	}
	os.Stdout, os.Stderr = outW, errW
	defer func() {
		os.Stdout, os.Stderr = origOut, origErr
	}()

	var outBuf, errBuf bytes.Buffer
	outDone := make(chan struct{})
	errDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&outBuf, outR)
		close(outDone)
	}()
	go func() {
		_, _ = io.Copy(&errBuf, errR)
		close(errDone)
	}()

	fn()

	_ = outW.Close()
	_ = errW.Close()
	<-outDone
	<-errDone
	return outBuf.String(), errBuf.String()
}

// discoveryCase is one row of TestRequirementsDiscoveryThroughCommands.
type discoveryCase struct {
	files   map[string]string
	env     map[string]string
	command func() *cli.Command
	check   func(t *testing.T, stdout, stderr string, err error)
	name    string
	args    []string
	lock    bool
}

// discoveryLockfile writes a lockfile beside the fixtures naming both roots
// the two requirements files can ask for, so which file was read shows in
// which root the tree prints.
func discoveryLockfile(t *testing.T, dir string) {
	t.Helper()
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: "acme.widgets", Version: "1.0.0", Source: "galaxy", SHA256: strings.Repeat("ab", 32)},
			{Name: "acme.other", Version: "2.0.0", Source: "galaxy", SHA256: strings.Repeat("cd", 32)},
		},
	}
	if err := lockfile.Save(filepath.Join(dir, lockfile.DefaultName), lf); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
}

// discoveryFixture writes tc's files (and lockfile) into a fresh directory
// and returns it.
func discoveryFixture(t *testing.T, tc discoveryCase) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range tc.files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), helpers.FileMod); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if tc.lock {
		discoveryLockfile(t, dir)
	}
	return dir
}

// TestRequirementsDiscoveryThroughCommands runs each row's command from its
// fixture directory with the flag variables cleared; t.Chdir, t.Setenv and
// the stdio swap are why nothing here is parallel.
func TestRequirementsDiscoveryThroughCommands(t *testing.T) {
	for _, tc := range discoveryCases() {
		t.Run(tc.name, func(t *testing.T) {
			neutralizeAnsibleDiscovery(t)
			clearRequirementsFileEnv(t)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			dir := discoveryFixture(t, tc)
			var err error
			stdout, stderr := captureStdIO(t, func() {
				err = runCommandInDir(t, dir, tc.command(), tc.args...)
			})
			tc.check(t, stdout, stderr, err)
		})
	}
}

func discoveryCases() []discoveryCase {
	return []discoveryCase{
		{
			name: "tree reads galaxy.toml with no flag", command: Tree, lock: true,
			files: map[string]string{helpers.RequirementsTOMLName: discoveryTOML},
			check: checkTreeReadsTOML,
		},
		{
			name: "explain credits galaxy.toml as the root", command: Explain, lock: true,
			args:  []string{"acme.widgets"},
			files: map[string]string{helpers.RequirementsTOMLName: discoveryTOML},
			check: checkExplainRootIsTOML,
		},
		{
			name: "hash keys galaxy.toml's bytes with no lockfile", command: Hash,
			files: map[string]string{helpers.RequirementsTOMLName: discoveryTOML},
			check: checkHashIsTOMLBytes,
		},
		{
			name: "both present: tree reads galaxy.toml and warns", command: Tree, lock: true,
			files: map[string]string{helpers.RequirementsTOMLName: discoveryTOML, helpers.RequirementsYAMLName: discoveryYAML},
			check: checkTreeReadsTOMLAndWarns,
		},
		{
			name: "-r requirements.yml beside galaxy.toml reads it silently", command: Tree, lock: true,
			args:  []string{"-r", helpers.RequirementsYAMLName},
			files: map[string]string{helpers.RequirementsTOMLName: discoveryTOML, helpers.RequirementsYAMLName: discoveryYAML},
			check: checkTreeReadsYAMLSilently,
		},
		{
			name: "neither file: hash fails on requirements.yml as before", command: Hash,
			check: checkHashMissingRequirementsYAML,
		},
		{
			name: "GO_GALAXY_REQUIREMENTS_FILE exported empty outranks galaxy.toml", command: Hash,
			env:   map[string]string{"GO_GALAXY_REQUIREMENTS_FILE": ""},
			files: map[string]string{helpers.RequirementsTOMLName: discoveryTOML},
			check: checkHashEmptyFlagExitsUsage,
		},
	}
}

// assertOutputHas fails unless stdout contains each want line.
func assertOutputHas(t *testing.T, stdout string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q; got:\n%s", want, stdout)
		}
	}
}

func checkTreeReadsTOML(t *testing.T, stdout, stderr string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if !strings.HasPrefix(stdout, helpers.RequirementsTOMLName+"\n") {
		t.Errorf("tree header is not galaxy.toml; got:\n%s", stdout)
	}
	assertOutputHas(t, stdout, "└── acme.widgets 1.0.0\n")
	if stderr != "" {
		t.Errorf("tree with only galaxy.toml wrote to stderr: %q", stderr)
	}
}

func checkExplainRootIsTOML(t *testing.T, stdout, stderr string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	assertOutputHas(t, stdout, "acme.widgets 1.0.0\n", "    - galaxy.toml (root)\n")
	if stderr != "" {
		t.Errorf("explain with only galaxy.toml wrote to stderr: %q", stderr)
	}
}

func checkHashIsTOMLBytes(t *testing.T, stdout, stderr string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if want := "sha256:" + discoveryTOMLSHA256 + "\n"; stdout != want {
		t.Errorf("hash stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("hash with only galaxy.toml wrote to stderr: %q", stderr)
	}
}

func checkTreeReadsTOMLAndWarns(t *testing.T, stdout, stderr string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	assertOutputHas(t, stdout, helpers.RequirementsTOMLName+"\n", "└── acme.widgets 1.0.0\n")
	if strings.Contains(stdout, "acme.other") {
		t.Errorf("tree read requirements.yml's root beside a galaxy.toml; got:\n%s", stdout)
	}
	if !strings.Contains(stderr, bothPresentWarning) {
		t.Errorf("stderr lacks the both-present warning; got: %q", stderr)
	}
}

func checkTreeReadsYAMLSilently(t *testing.T, stdout, stderr string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	assertOutputHas(t, stdout, helpers.RequirementsYAMLName+"\n", "└── acme.other 2.0.0\n")
	if strings.Contains(stdout, "acme.widgets") {
		t.Errorf("tree -r requirements.yml read galaxy.toml's root; got:\n%s", stdout)
	}
	if stderr != "" {
		t.Errorf("tree with -r wrote a warning: %q", stderr)
	}
}

// checkHashMissingRequirementsYAML pins that a directory holding neither file
// yields the pre-discovery message and class: the fallback names
// requirements.yml without a Stat, and hash reads it with no lockfile.
func checkHashMissingRequirementsYAML(t *testing.T, stdout, stderr string, err error) {
	t.Helper()
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("hash with neither file: err = %v, want errors.Is fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), "open requirements.yml") {
		t.Errorf("hash error = %q, want it to read \"open requirements.yml\"", err.Error())
	}
	if got := exitcode.FromError(err); got != exitcode.ExitUsage {
		t.Errorf("exitcode.FromError(err) = %d, want ExitUsage (%d)", got, exitcode.ExitUsage)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("hash with neither file printed stdout %q stderr %q", stdout, stderr)
	}
}

// checkHashEmptyFlagExitsUsage pins that an exported-empty variable is a set
// flag naming "", which discovery never overrides, so the read fails as usage
// even with a galaxy.toml in the working directory.
func checkHashEmptyFlagExitsUsage(t *testing.T, stdout, _ string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("hash with an empty GO_GALAXY_REQUIREMENTS_FILE succeeded with stdout %q", stdout)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitUsage {
		t.Errorf("exitcode.FromError(err) = %d, want ExitUsage (%d); err = %v", got, exitcode.ExitUsage, err)
	}
	if strings.Contains(stdout, discoveryTOMLSHA256) {
		t.Errorf("hash keyed galaxy.toml although the flag was set empty: %q", stdout)
	}
}
