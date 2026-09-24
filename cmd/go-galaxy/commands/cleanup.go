package commands

import (
	"context"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/cleanup"
	"github.com/urfave/cli/v3"
)

// Cleanup returns the CLI command that removes the collections and roles no
// recorded project reaches, from every project and from the cache. It mounts
// the requirements flag for one reason: a galaxy.toml names the cache to clean.
func Cleanup() *cli.Command {
	return &cli.Command{
		Name:    "cleanup",
		Aliases: []string{"c"},
		Usage:   "Cleanup unused cached collections and roles across all projects",
		Flags:   append([]cli.Flag{cliflags.RequirementsFileFlag()}, cliflags.S3Flags()...),
		Action: func(ctx context.Context, c *cli.Command) error {
			return runCollectionCommand(ctx, c, cleanup.Start)
		},
	}
}
