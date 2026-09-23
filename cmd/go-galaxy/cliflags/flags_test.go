package cliflags

import (
	"context"
	"slices"
	"testing"

	"github.com/urfave/cli/v3"
)

// envKeys extracts the environment variable names configured on a
// ValueSourceChain, in order, for assertions below.
func envKeys(t *testing.T, chain cli.ValueSourceChain) []string {
	t.Helper()
	keys := make([]string, 0, len(chain.Chain))
	for _, src := range chain.Chain {
		ev, ok := src.(cli.EnvValueSource)
		if !ok {
			t.Fatalf("source %v does not implement cli.EnvValueSource", src)
		}
		keys = append(keys, ev.Key())
	}
	return keys
}

// wantStringFlag is the expected shape of one LockInspectFlags entry.
type wantStringFlag struct {
	name    string
	usage   string
	value   string
	aliases []string
	envKeys []string
}

// assertStringFlag checks flag against want, reporting every mismatch.
func assertStringFlag(t *testing.T, flag cli.Flag, want wantStringFlag) {
	t.Helper()
	sf, ok := flag.(*cli.StringFlag)
	if !ok {
		t.Fatalf("flag = %T, want *cli.StringFlag", flag)
	}
	if sf.Name != want.name {
		t.Errorf("Name = %q, want %q", sf.Name, want.name)
	}
	if !slices.Equal(sf.Aliases, want.aliases) {
		t.Errorf("Aliases = %v, want %v", sf.Aliases, want.aliases)
	}
	if sf.Usage != want.usage {
		t.Errorf("Usage = %q, want %q", sf.Usage, want.usage)
	}
	if sf.Value != want.value {
		t.Errorf("Value = %q, want %q", sf.Value, want.value)
	}
	if got := envKeys(t, sf.Sources); !slices.Equal(got, want.envKeys) {
		t.Errorf("env sources = %v, want %v", got, want.envKeys)
	}
}

// TestLockInspectFlags checks that the shared lockfile-inspection flag set
// (used by hash, tree, explain) exposes exactly the requirements-file and
// lock-file flags with the expected names, alias, usage, and env sources.
func TestLockInspectFlags(t *testing.T) {
	t.Parallel()
	flags := LockInspectFlags()
	if len(flags) != 2 {
		t.Fatalf("LockInspectFlags() returned %d flags, want 2", len(flags))
	}

	tests := []struct {
		flag cli.Flag
		name string
		want wantStringFlag
	}{
		{
			name: "requirements-file",
			flag: flags[0],
			want: wantStringFlag{
				name: "requirements-file",
				// The default keeps hash, tree and explain reading the same file
				// as install when nobody says; the lock-file row's empty Value is
				// the control that this assertion tells the two apart.
				value:   "requirements.yml",
				aliases: []string{"r", "role-file"},
				usage:   "Path to requirements.yml",
				envKeys: []string{"GO_GALAXY_REQUIREMENTS_FILE", "ANSIBLE_GALAXY_REQUIREMENTS_FILE"},
			},
		},
		{
			name: "lock-file",
			flag: flags[1],
			want: wantStringFlag{
				name:    "lock-file",
				aliases: nil,
				usage:   "Path to lockfile (default: galaxy.lock beside requirements file)",
				envKeys: []string{"GO_GALAXY_LOCK_FILE"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertStringFlag(t, tt.flag, tt.want)
		})
	}
}

// TestDryRunEnv checks that the persistent --dry-run flag can be set via
// GO_GALAXY_DRY_RUN, since it is a global flag also consumed by cleanup
// (which has no --dry-run-specific wiring of its own).
func TestDryRunEnv(t *testing.T) {
	// Not parallel: t.Setenv panics in a test that has called t.Parallel.
	t.Setenv("GO_GALAXY_DRY_RUN", "true")

	var gotDryRun bool
	cmd := &cli.Command{
		Name:  "go-galaxy",
		Flags: CommonFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			gotDryRun = c.Bool("dry-run")
			return nil
		},
	}

	if err := cmd.Run(context.Background(), []string{"go-galaxy"}); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	if !gotDryRun {
		t.Error("c.Bool(\"dry-run\") = false, want true (from GO_GALAXY_DRY_RUN)")
	}
}
