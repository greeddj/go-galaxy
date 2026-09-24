package config

import (
	"context"
	"os"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

const (
	bothPresentWarning = "galaxy.toml and requirements.yml are both present in the current directory; " +
		"using galaxy.toml and ignoring requirements.yml (name one with --requirements-file to choose)"
	notRegularWarning = "./galaxy.toml is not a regular file and is ignored; reading requirements.yml instead " +
		"(name a file with --requirements-file to read it)"
)

// runRequirementsPath runs RequirementsPath from the action of a subcommand
// mounting subFlags under a root carrying cliflags.CommonFlags, the tree
// main.go builds, and returns what it answered.
func runRequirementsPath(t *testing.T, subFlags []cli.Flag, args []string) (string, string) {
	t.Helper()

	var gotPath, gotWarning string
	app := &cli.Command{
		Name:  "go-galaxy",
		Flags: cliflags.CommonFlags(),
		Commands: []*cli.Command{
			{
				Name:  "tree",
				Flags: subFlags,
				Action: func(_ context.Context, c *cli.Command) error {
					gotPath, gotWarning = RequirementsPath(c)
					return nil
				},
			},
		},
	}

	fullArgs := append([]string{"go-galaxy", "tree"}, args...)
	if err := app.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("app.Run() error = %v, want nil", err)
	}
	return gotPath, gotWarning
}

// clearRequirementsEnv hides both requirements-file variables an ambient
// shell may export, restoring them when the test ends, so a row's sources
// are only the ones it sets itself.
func clearRequirementsEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"GO_GALAXY_REQUIREMENTS_FILE", "ANSIBLE_GALAXY_REQUIREMENTS_FILE"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
}

// writeCwdFile creates name in the current directory, which every row has
// already switched to a fresh temporary directory.
func writeCwdFile(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(name, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// requirementsPathRow is one working directory and flag shape: the files to
// plant, the sources to set, and the path and warning discovery must answer.
type requirementsPathRow struct {
	env         map[string]string
	flags       func() []cli.Flag
	name        string
	wantPath    string
	wantWarning string
	files       []string
	args        []string
	tomlDir     bool
	tomlSymlink bool
}

// requirementsPathRows builds the flags through a constructor rather than
// sharing one slice, since a flag object remembers the run it was parsed in.
func requirementsPathRows() []requirementsPathRow {
	inspect := cliflags.LockInspectFlags
	return []requirementsPathRow{
		{name: "unset with neither file", flags: inspect, wantPath: "requirements.yml"},
		{name: "only galaxy.toml", flags: inspect, files: []string{"galaxy.toml"}, wantPath: "galaxy.toml"},
		{name: "only requirements.yml", flags: inspect, files: []string{"requirements.yml"}, wantPath: "requirements.yml"},
		{
			name: "both files", flags: inspect, files: []string{"galaxy.toml", "requirements.yml"},
			wantPath: "galaxy.toml", wantWarning: bothPresentWarning,
		},
		{
			name: "galaxy.toml is a directory", flags: inspect, tomlDir: true, files: []string{"requirements.yml"},
			wantPath: "requirements.yml", wantWarning: notRegularWarning,
		},
		{name: "galaxy.toml is a symlink to a regular file", flags: inspect, tomlSymlink: true, wantPath: "galaxy.toml"},
		{
			name: "the flag outranks a present galaxy.toml", flags: inspect, files: []string{"galaxy.toml"},
			args: []string{"--requirements-file=requirements.yml"}, wantPath: "requirements.yml",
		},
		{name: "the -r alias is read", flags: inspect, args: []string{"-r", "deps.toml"}, wantPath: "deps.toml"},
		{name: "the --role-file alias is read", flags: inspect, args: []string{"--role-file=deps.yml"}, wantPath: "deps.yml"},
		{
			name: "GO_GALAXY_REQUIREMENTS_FILE outranks a present requirements.yml", flags: inspect,
			files: []string{"requirements.yml"}, env: map[string]string{"GO_GALAXY_REQUIREMENTS_FILE": "/x/galaxy.toml"},
			wantPath: "/x/galaxy.toml",
		},
		{
			name: "ANSIBLE_GALAXY_REQUIREMENTS_FILE alone is read", flags: inspect,
			env: map[string]string{"ANSIBLE_GALAXY_REQUIREMENTS_FILE": "/a/req.yml"}, wantPath: "/a/req.yml",
		},
		{
			name: "GO_GALAXY_REQUIREMENTS_FILE outranks the ansible variable", flags: inspect,
			env: map[string]string{
				"GO_GALAXY_REQUIREMENTS_FILE":      "/g/galaxy.toml",
				"ANSIBLE_GALAXY_REQUIREMENTS_FILE": "/a/req.yml",
			},
			wantPath: "/g/galaxy.toml",
		},
		{
			name: "an exported-empty variable counts as set", flags: inspect, files: []string{"galaxy.toml"},
			env: map[string]string{"GO_GALAXY_REQUIREMENTS_FILE": ""}, wantPath: "",
		},
		{name: "a command mounting no requirements flag discovers nothing", flags: cliflags.S3Flags, files: []string{"galaxy.toml"}},
	}
}

// plantRow lays the row's files out in the current directory: plain files,
// or galaxy.toml as a directory or as a symlink to a regular file beside it.
func plantRow(t *testing.T, row requirementsPathRow) {
	t.Helper()
	for _, name := range row.files {
		writeCwdFile(t, name)
	}
	if row.tomlDir {
		if err := os.Mkdir("galaxy.toml", helpers.DirMod); err != nil {
			t.Fatalf("mkdir galaxy.toml: %v", err)
		}
	}
	if row.tomlSymlink {
		writeCwdFile(t, "real.toml")
		if err := os.Symlink("real.toml", "galaxy.toml"); err != nil {
			t.Fatalf("symlink galaxy.toml: %v", err)
		}
	}
}

// TestRequirementsPath pins discovery row by row: a set source wins verbatim,
// else a regular ./galaxy.toml, else requirements.yml, with the two warning
// lines spelled exactly; the last row is the control for the unmounted flag.
func TestRequirementsPath(t *testing.T) {
	for _, row := range requirementsPathRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			clearRequirementsEnv(t)
			for key, value := range row.env {
				t.Setenv(key, value)
			}
			plantRow(t, row)

			gotPath, gotWarning := runRequirementsPath(t, row.flags(), row.args)
			if gotPath != row.wantPath {
				t.Errorf("path = %q, want %q", gotPath, row.wantPath)
			}
			if gotWarning != row.wantWarning {
				t.Errorf("warning = %q, want %q", gotWarning, row.wantWarning)
			}
		})
	}
}

// TestRequirementsPathUnmountedFlagIsThePositiveControl pins that the same
// cwd the unmounted row answered "" for yields galaxy.toml once the flag is
// mounted, so that row proves the flag scan and not an empty directory.
func TestRequirementsPathUnmountedFlagIsThePositiveControl(t *testing.T) {
	t.Chdir(t.TempDir())
	clearRequirementsEnv(t)
	writeCwdFile(t, "galaxy.toml")

	if got, _ := runRequirementsPath(t, cliflags.S3Flags(), nil); got != "" {
		t.Fatalf("unmounted flag: path = %q, want \"\"", got)
	}
	if got, _ := runRequirementsPath(t, cliflags.LockInspectFlags(), nil); got != "galaxy.toml" {
		t.Fatalf("mounted flag: path = %q, want \"galaxy.toml\"", got)
	}
}

// TestFlagMounted pins the scan behind the unmounted-flag rule: an alias of
// the flag counts, and a flag set carrying another name does not.
func TestFlagMounted(t *testing.T) {
	rows := []struct {
		name  string
		flags []cli.Flag
		want  bool
	}{
		{name: "the requirements flag itself", flags: cliflags.LockInspectFlags(), want: true},
		{name: "the collection flag set", flags: cliflags.CollectionFlags(), want: true},
		{name: "the s3 flag set", flags: cliflags.S3Flags(), want: false},
		{name: "no flags", flags: nil, want: false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := &cli.Command{Name: "x", Flags: row.flags}
			if got := flagMounted(c, "requirements-file"); got != row.want {
				t.Errorf("flagMounted() = %v, want %v", got, row.want)
			}
		})
	}
}
