package commands

import (
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// TestUsageNamesCollectionsAndRoles pins that the one-line help of each
// command that handles roles beside collections names both, since the --help
// command list shows nothing else about what a command covers.
func TestUsageNamesCollectionsAndRoles(t *testing.T) {
	t.Parallel()

	for _, cmd := range []*cli.Command{Install(), Warm(), Cleanup(), Outdated()} {
		if !strings.Contains(cmd.Usage, "collections and roles") {
			t.Errorf("%s Usage = %q, want it to name collections and roles", cmd.Name, cmd.Usage)
		}
	}
}

// TestOutdatedUsageNamesInstalledFallback pins that outdated's one-line help
// says it reads the installed collections when there is no lockfile, and does
// not confine the comparison to Galaxy, since git entries are asked by commit.
func TestOutdatedUsageNamesInstalledFallback(t *testing.T) {
	t.Parallel()

	usage := Outdated().Usage
	if !strings.Contains(usage, "installed collections") {
		t.Errorf("outdated Usage = %q, want it to name the installed collections fallback", usage)
	}
	if strings.Contains(usage, "Galaxy") {
		t.Errorf("outdated Usage = %q, want it not to confine the comparison to Galaxy", usage)
	}
}
