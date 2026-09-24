package main

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// inputFileCase is one row of TestUnusableInputFilesExitUsage: a command line,
// the sentinel its failure must carry and, when "" is not enough, a fragment
// the printed error must carry.
type inputFileCase struct {
	want     error
	name     string
	wantText string
	args     []string
}

// inputFileFixture holds the paths inputFileCases builds its rows from.
type inputFileFixture struct {
	notYAML  string
	notTOML  string
	badTOML  string
	toolKey  string
	toolEnv  string
	dir      string
	cfg      string
	cfgDir   string
	longCfg  string
	lockPath string
	cache    string
}

// newInputFileFixture writes requirements that are not YAML, a .toml that is
// not TOML, three galaxy.toml files each breaking one rule, a directory in
// place of a file, two ansible.cfg files and an empty lockfile for tree.
func newInputFileFixture(t *testing.T) inputFileFixture {
	t.Helper()
	root := t.TempDir()
	f := inputFileFixture{
		notYAML:  filepath.Join(root, "broken.yml"),
		notTOML:  filepath.Join(root, "broken.toml"),
		badTOML:  filepath.Join(root, "unknown-key", "galaxy.toml"),
		toolKey:  filepath.Join(root, "unknown-tool-key", "galaxy.toml"),
		toolEnv:  filepath.Join(root, "unset-variable", "galaxy.toml"),
		dir:      filepath.Join(root, "dir"),
		cfg:      filepath.Join(root, "empty.cfg"),
		cfgDir:   filepath.Join(root, "cfgdir"),
		longCfg:  filepath.Join(root, "long.cfg"),
		lockPath: filepath.Join(root, lockfile.DefaultName),
		cache:    filepath.Join(root, "cache"),
	}
	for _, path := range []string{f.badTOML, f.toolKey, f.toolEnv} {
		if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
	}
	files := map[string]string{
		f.notYAML: "collections:\n  - name: [unclosed\n",
		f.notTOML: "[project]\ncollections = [\n",
		f.badTOML: "[project]\nlicense = \"MIT\"\ncollections = [\"acme.widgets\"]\n",
		f.toolKey: "[project]\ncollections = [\"acme.widgets\"]\n\n[tool.go-galaxy]\nbogus = 1\n",
		f.toolEnv: "[project]\ncollections = [\"acme.widgets\"]\n\n[tool.go-galaxy.s3]\nsecret_key = \"${INPUT_FILES_UNSET}\"\n",
		f.cfg:     "",
		f.longCfg: "[defaults]\nx = " + strings.Repeat("a", bufio.MaxScanTokenSize) + "\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), helpers.FileMod); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	for _, path := range []string{f.dir, f.cfgDir} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	if err := lockfile.Save(f.lockPath, &lockfile.File{SchemaVersion: lockfile.SchemaVersion}); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
	return f
}

// inputFileCases covers every command that reads the requirements file with
// one that cannot be used, and a command of each config path with an
// ansible.cfg that cannot be read.
func inputFileCases(f inputFileFixture) []inputFileCase {
	collection := func(cmd, req string) []string {
		return []string{cmd, "--quiet", "--offline", "--cache-dir", f.cache, "--ansible-config", f.cfg, "-r", req}
	}
	withCfg := func(cfg string) []string {
		return []string{"install", "--quiet", "--offline", "--cache-dir", f.cache, "--ansible-config", cfg, "-r", f.notYAML}
	}
	return []inputFileCase{
		{name: "install, requirements not YAML", args: collection("install", f.notYAML), want: helpers.ErrInvalidRequirementsYAML},
		{name: "install, requirements unreadable", args: collection("install", f.dir), want: helpers.ErrRequirementsUnreadable},
		{name: "warm, requirements not YAML", args: collection("warm", f.notYAML), want: helpers.ErrInvalidRequirementsYAML},
		{name: "lock, requirements unreadable", args: collection("lock", f.dir), want: helpers.ErrRequirementsUnreadable},
		{name: "install, .toml requirements not TOML", args: collection("install", f.notTOML), want: helpers.ErrInvalidRequirementsTOML},
		{name: "warm, .toml requirements not TOML", args: collection("warm", f.notTOML), want: helpers.ErrInvalidRequirementsTOML},
		{name: "lock, .toml requirements not TOML", args: collection("lock", f.notTOML), want: helpers.ErrInvalidRequirementsTOML},
		{
			name: "install, galaxy.toml with an unknown [project] key",
			args: collection("install", f.badTOML),
			want: helpers.ErrUnsupportedRequirementsFormat,
		},
		{
			name: "tree, requirements not YAML",
			args: []string{"tree", "-r", f.notYAML, "--lock-file", f.lockPath},
			want: helpers.ErrInvalidRequirementsYAML,
		},
		{
			name: "tree, .toml requirements not TOML",
			args: []string{"tree", "-r", f.notTOML, "--lock-file", f.lockPath},
			want: helpers.ErrInvalidRequirementsTOML,
		},
		{
			name: "tree, requirements unreadable",
			args: []string{"tree", "-r", f.dir, "--lock-file", f.lockPath},
			want: helpers.ErrRequirementsUnreadable,
		},
		{
			name: "hash, requirements unreadable with no lockfile",
			args: []string{"hash", "-r", f.dir, "--lock-file", f.lockPath + ".absent"},
			want: helpers.ErrRequirementsUnreadable,
		},
		{name: "install, named ansible.cfg is a directory", args: withCfg(f.cfgDir), want: helpers.ErrAnsibleConfigUnreadable},
		{name: "install, named ansible.cfg line too long", args: withCfg(f.longCfg), want: helpers.ErrAnsibleConfigUnreadable},
	}
}

// projectSettingsCases covers every command that reads a galaxy.toml with one
// whose [tool.go-galaxy] table breaks the schema or names an unset variable.
// hash, tree and explain carry no --lock-file, so the table names their lockfile.
func projectSettingsCases(f inputFileFixture) []inputFileCase {
	argsFor := func(cmd, req string) []string {
		switch cmd {
		case "tree", "hash":
			return []string{cmd, "-r", req}
		case "explain":
			return []string{cmd, "-r", req, "acme.widgets"}
		case "cleanup":
			return []string{cmd, "--quiet", "--cache-dir", f.cache, "-r", req}
		default:
			return []string{cmd, "--quiet", "--offline", "--cache-dir", f.cache, "--ansible-config", f.cfg, "-r", req}
		}
	}
	commands := []string{"install", "warm", "lock", "outdated", "tree", "hash", "explain", "cleanup"}
	cases := make([]inputFileCase, 0, 2*len(commands))
	for _, cmd := range commands {
		cases = append(cases,
			inputFileCase{
				name:     cmd + ", galaxy.toml with an unknown [tool.go-galaxy] key",
				args:     argsFor(cmd, f.toolKey),
				want:     helpers.ErrUnsupportedRequirementsFormat,
				wantText: `unknown key "bogus" in [tool.go-galaxy]`,
			},
			inputFileCase{
				name:     cmd + ", galaxy.toml naming an unset variable",
				args:     argsFor(cmd, f.toolEnv),
				want:     helpers.ErrProjectFileEnvUnset,
				wantText: "unset environment variables: INPUT_FILES_UNSET",
			})
	}
	return cases
}

// TestUnusableInputFilesExitUsage pins, through the root command main runs,
// that a requirements file or ansible.cfg that exists but cannot be read or
// parsed exits ExitUsage. Not parallel: t.Setenv.
func TestUnusableInputFilesExitUsage(t *testing.T) {
	// The unset-variable fixture needs its name unset; t.Setenv registers the
	// restore, and an exported-empty value would expand rather than refuse.
	t.Setenv("INPUT_FILES_UNSET", "")
	if err := os.Unsetenv("INPUT_FILES_UNSET"); err != nil {
		t.Fatalf("os.Unsetenv(%q) error = %v, want nil", "INPUT_FILES_UNSET", err)
	}
	f := newInputFileFixture(t)
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "absent.cfg"))
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	for _, tc := range append(inputFileCases(f), projectSettingsCases(f)...) {
		captured := runRootCommand(t, tc.args)
		assertExitsUsageWith(t, tc.name, captured, tc.want)
		if tc.wantText != "" && (captured == nil || !strings.Contains(captured.Error(), tc.wantText)) {
			t.Errorf("%s: captured error = %v, want it to carry %q", tc.name, captured, tc.wantText)
		}
	}

	// Discovery, the only path cleanup has, since it takes no --ansible-config.
	t.Setenv("ANSIBLE_CONFIG", f.cfgDir)
	for _, cmd := range []string{"cleanup", "outdated"} {
		captured := runRootCommand(t, []string{cmd, "--cache-dir", f.cache})
		assertExitsUsageWith(t, cmd+", discovered ansible.cfg is a directory", captured, helpers.ErrAnsibleConfigUnreadable)
	}
}

// assertExitsUsageWith checks that captured carries want and classifies as
// ExitUsage, naming the row on failure.
func assertExitsUsageWith(t *testing.T, name string, captured, want error) {
	t.Helper()
	if !errors.Is(captured, want) {
		t.Errorf("%s: captured error = %v, want errors.Is %v", name, captured, want)
		return
	}
	if code := exitcode.FromError(captured); code != exitcode.ExitUsage {
		t.Errorf("%s: exit code = %d, want %d (%v)", name, code, exitcode.ExitUsage, captured)
	}
}
