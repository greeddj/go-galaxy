package cliflags

import (
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// findStringSliceFlag returns the *cli.StringSliceFlag flags declares as name,
// failing the test when there is none or it has another type.
func findStringSliceFlag(t *testing.T, flags []cli.Flag, name string) *cli.StringSliceFlag {
	t.Helper()
	for _, flag := range flags {
		if !slices.Contains(flag.Names(), name) {
			continue
		}
		sf, ok := flag.(*cli.StringSliceFlag)
		if !ok {
			t.Fatalf("--%s = %T, want *cli.StringSliceFlag", name, flag)
		}
		return sf
	}
	t.Fatalf("no flag %q declared", name)
	return nil
}

// TestLockFlagsMountTheRefreshSelection pins lock's --collection (-c) and
// --role as string slices with no variable, a selection being one run's
// intent; the set install, warm and outdated mount has neither.
func TestLockFlagsMountTheRefreshSelection(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name    string
		sibling string
		aliases []string
	}{
		{name: "collection", aliases: []string{"c"}, sibling: "git source"},
		{name: "role", sibling: "tarball URL"},
	}
	for _, row := range rows {
		flag := findStringSliceFlag(t, LockFlags(), row.name)
		if flag.Name != row.name || !slices.Equal(flag.Aliases, row.aliases) {
			t.Errorf("--%s Names() = %v, want %q with aliases %v", row.name, flag.Names(), row.name, row.aliases)
		}
		for _, fragment := range []string{"--refresh", "repeatable or comma-separated", row.sibling} {
			if !strings.Contains(flag.Usage, fragment) {
				t.Errorf("--%s Usage = %q, want it to mention %q", row.name, flag.Usage, fragment)
			}
		}
		if got := envKeys(t, flag.Sources); len(got) != 0 {
			t.Errorf("--%s env sources = %v, want none", row.name, got)
		}
		if flag.Value != nil {
			t.Errorf("--%s Value = %v, want nil: no default selection", row.name, flag.Value)
		}
		for _, name := range append([]string{row.name}, row.aliases...) {
			if hasFlag(CollectionFlags(), name) {
				t.Errorf("CollectionFlags() declares %q, want it on lock alone", name)
			}
		}
	}
}

// TestLockCommandFlagNamesAreUnique pins that -c, the one short alias the
// selection adds, collides with no other name lock mounts beside the globals.
func TestLockCommandFlagNamesAreUnique(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for _, set := range [][]cli.Flag{CommonFlags(), LockFlags(), S3Flags()} {
		for _, flag := range set {
			for _, name := range flag.Names() {
				if seen[name] {
					t.Errorf("flag name %q is declared twice in lock's flag set", name)
				}
				seen[name] = true
			}
		}
	}
}
