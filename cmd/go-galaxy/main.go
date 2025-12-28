package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/commands"
	"github.com/urfave/cli/v2"
)

//nolint:gochecknoglobals
var (
	Version = "dev"
	Commit  = "0000000"
	Date    = "unknown"
	BuiltBy = "manual"
)

// main is the CLI entry point.
func main() {
	os.Exit(run())
}

// run configures and executes the CLI, returning the exit code.
func run() int {
	appName := "go-galaxy"

	app := cli.NewApp()
	app.Name = appName
	app.Usage = "Galaxy Collection Manager for CI"
	app.Version = fmt.Sprintf("%s (commit: %s, built: %s by %s) // %s", Version, Commit, Date, BuiltBy, runtime.Version())
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
