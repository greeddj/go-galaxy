package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/commands"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/helpers"
	"github.com/urfave/cli/v2"
)

//nolint:gochecknoglobals
var (
	Version string
	Commit  string
	Date    string
	BuiltBy string
)

// main is the CLI entry point.
func main() {
	os.Exit(run())
}

// run configures and executes the CLI, returning the exit code.
func run() int {
	app := cli.NewApp()
	app.Name = "go-galaxy"
	app.Usage = "Galaxy Collection Manager for CI"
	app.Version = helpers.Version(Version, Commit, Date, BuiltBy)
	app.DefaultCommand = "install"
	app.HideHelpCommand = true
	app.UseShortOptionHandling = true
	app.Commands = []*cli.Command{
		commands.Install(),
		commands.Cleanup(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()

	if err := app.RunContext(ctx, os.Args); err != nil {
		return 1
	}
	return 0
}
