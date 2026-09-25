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
	name        string
	usage       string
	value       string
	defaultText string
	aliases     []string
	envKeys     []string
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
	if sf.DefaultText != want.defaultText {
		t.Errorf("DefaultText = %q, want %q", sf.DefaultText, want.defaultText)
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
			want: wantRequirementsFileFlag(),
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

// wantRequirementsFileFlag is the one shape every command's requirements-file
// flag must have: no Value, since discovery decides the default, and a
// DefaultText that states the discovery order; the lock-file row is the control.
func wantRequirementsFileFlag() wantStringFlag {
	return wantStringFlag{
		name:        "requirements-file",
		aliases:     []string{"r", "role-file"},
		defaultText: "galaxy.toml if present, else requirements.yml",
		usage: "Path to the requirements file: requirements.yml, or galaxy.toml by its .toml extension; " +
			"unset, ./galaxy.toml is read when present, else ./requirements.yml",
		envKeys: []string{"GO_GALAXY_REQUIREMENTS_FILE", "ANSIBLE_GALAXY_REQUIREMENTS_FILE"},
	}
}

// TestCollectionFlagsRequirementsFile pins that the install set mounts the
// same requirements-file declaration hash, tree and explain do, so the two
// cannot drift apart in help text, default or environment sources.
func TestCollectionFlagsRequirementsFile(t *testing.T) {
	t.Parallel()
	for _, flag := range CollectionFlags() {
		if slices.Contains(flag.Names(), "requirements-file") {
			assertStringFlag(t, flag, wantRequirementsFileFlag())
			return
		}
	}
	t.Fatal("CollectionFlags() mounts no requirements-file flag")
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

// TestLockFlagsTakeCheckInPlaceOfFrozen pins that lock's set reads --check
// from GO_GALAXY_CHECK and registers no --frozen, so GO_GALAXY_FROZEN reaches
// no lock run, while the install set keeps --frozen and has no --check.
func TestLockFlagsTakeCheckInPlaceOfFrozen(t *testing.T) {
	// Not parallel: t.Setenv panics in a test that has called t.Parallel.
	t.Setenv("GO_GALAXY_CHECK", "true")
	t.Setenv("GO_GALAXY_FROZEN", "true")

	var check, frozen bool
	cmd := &cli.Command{
		Name:  "lock",
		Flags: LockFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			check, frozen = c.Bool("check"), c.Bool("frozen")
			return nil
		},
	}
	if err := cmd.Run(context.Background(), []string{"lock"}); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	if !check {
		t.Error("c.Bool(\"check\") = false, want true (from GO_GALAXY_CHECK)")
	}
	if frozen {
		t.Error("c.Bool(\"frozen\") = true, want false: lock registers no --frozen")
	}
	if hasFlag(CollectionFlags(), "check") || !hasFlag(CollectionFlags(), "frozen") {
		t.Error("CollectionFlags() must keep --frozen and carry no --check")
	}
}

// hasFlag reports whether flags declares name.
func hasFlag(flags []cli.Flag, name string) bool {
	return slices.ContainsFunc(flags, func(f cli.Flag) bool { return slices.Contains(f.Names(), name) })
}
