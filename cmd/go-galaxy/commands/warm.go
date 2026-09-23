package commands

import (
	"context"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/urfave/cli/v3"
)

// Warm returns the CLI command that downloads and extracts collections and
// roles into the cache without populating the install path or the roles path.
// Intended for CI image bake: subsequent install runs hardlink instantly.
func Warm() *cli.Command {
	flags := cliflags.CollectionFlags()
	flags = append(flags, cliflags.SignatureFlags()...)
	flags = append(flags, cliflags.S3Flags()...)

	return &cli.Command{
		Name:    "warm",
		Aliases: []string{"w"},
		Usage:   "Download and extract collections and roles into cache without installing",
		Flags:   flags,
		Action: func(ctx context.Context, c *cli.Command) error {
			return runCollectionCommand(ctx, c, collections.Warm)
		},
	}
}
