package commands

import (
	"context"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/urfave/cli/v3"
)

// Lock returns the CLI command that writes a lockfile from resolved deps.
func Lock() *cli.Command {
	flags := cliflags.LockFlags()
	flags = append(flags, cliflags.S3Flags()...)

	return &cli.Command{
		Name:    "lock",
		Aliases: []string{"l"},
		Usage:   "Resolve dependencies and write galaxy.lock",
		Flags:   flags,
		Action: func(ctx context.Context, c *cli.Command) error {
			return runCollectionCommand(ctx, c, collections.Lock)
		},
	}
}
