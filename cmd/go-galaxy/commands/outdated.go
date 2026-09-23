package commands

import (
	"context"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/urfave/cli/v3"
)

// Outdated returns the CLI command that compares what the project runs - the
// lockfile, else the installed collections tree - with the latest upstream:
// a Galaxy version, a git ref's current commit, or a Galaxy role's highest tag.
func Outdated() *cli.Command {
	flags := cliflags.CollectionFlags()
	flags = append(flags, cliflags.S3Flags()...)

	return &cli.Command{
		Name:    "outdated",
		Aliases: []string{"o"},
		Usage:   "Compare locked collections and roles (else installed collections) with the latest upstream",
		Flags:   flags,
		Action: func(ctx context.Context, c *cli.Command) error {
			return runCollectionCommand(ctx, c, collections.Outdated)
		},
	}
}
