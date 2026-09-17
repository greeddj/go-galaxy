package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// NoArguments is the ArgValidator the root command declares, and so the one
// every command runs under unless it declares its own. urfave hands a command
// whatever positional arguments follow it and runs the action regardless, and
// every command but explain reads only flags, so an argument can only be a
// mistake. With install as the root's DefaultCommand, a first word that names
// no command is one too: "go-galaxy collection install ns.name" reaches
// install with all three words as its arguments, and accepting them would
// install the requirements file without a word about ns.name.
//
// Declaring it on the root rather than on each command is what makes refusal
// the default: a command added later refuses arguments until it declares a
// validator of its own, the way Explain does, instead of accepting them until
// someone remembers to refuse.
func NoArguments(_ context.Context, c *cli.Command) error {
	if c.NArg() == 0 {
		return nil
	}
	return unexpectedArguments(c, c.Args().Slice(), "none")
}

// unexpectedArguments renders the refusal of args, the arguments c was given
// beyond the count it takes, which takes names. Each argument is rendered with
// strconv.Quote, so an operator can see where one argument ends and the next
// begins, and one carrying a control character prints as its escape rather
// than reaching the terminal.
//
// When c is the root's default command the message says so, because an
// operator who typed "go-galaxy collection install" never named install and
// would otherwise not know why install is the command refusing.
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
