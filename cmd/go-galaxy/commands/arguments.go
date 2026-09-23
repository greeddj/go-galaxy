package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// NoArguments is the root's ArgValidator, so a command refuses positional
// arguments unless it declares its own (as Explain does); with install as the
// DefaultCommand, a first word naming no command must not be silently dropped.
func NoArguments(_ context.Context, c *cli.Command) error {
	if c.NArg() == 0 {
		return nil
	}
	return unexpectedArguments(c, c.Args().Slice(), "none")
}

// unexpectedArguments refuses args beyond what c takes, quoting each so its
// bounds show and control characters stay escaped, and says so when c runs as
// the root's DefaultCommand, since the operator never named it.
func unexpectedArguments(c *cli.Command, args []string, takes string) error {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = strconv.Quote(arg)
	}
	reason := c.Name + " takes " + takes
	if c.Name == c.Root().DefaultCommand {
		reason += ", and is what runs when the first word names no command"
	}
	return fmt.Errorf("%w %s: %s", helpers.ErrUnexpectedArguments, strings.Join(quoted, " "), reason)
}
