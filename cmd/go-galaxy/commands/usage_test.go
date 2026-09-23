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

	for _, cmd := range []*cli.Command{Install(), Warm(), Cleanup()} {
		if !strings.Contains(cmd.Usage, "collections and roles") {
			t.Errorf("%s Usage = %q, want it to name collections and roles", cmd.Name, cmd.Usage)
		}
	}
}
