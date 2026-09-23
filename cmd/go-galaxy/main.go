// Package main is the go-galaxy executable: it assembles the urfave/cli
// command tree, cancels every command's context on a caught signal, and turns
// the result into an exit code through exitcode, a decision handleResult owns.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/buildinfo"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/commands"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/urfave/cli/v3"
)

// Version, Commit, Date and BuiltBy are set at link time by -X ldflags (the
// Justfile and .goreleaser.yml) and are this binary's only build-info source.
//
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

// errRecorder is urfave's ErrWriter: it passes writes through to w and records
// whether urfave reported a usage error itself. go-galaxy never writes here, and
// written is set during app.Run's parse and read only after it returns.
type errRecorder struct {
	w       io.Writer
	written bool
}

// Write implements io.Writer, recording any non-empty write, since an empty
// write tells an operator nothing.
func (r *errRecorder) Write(p []byte) (int, error) {
	if len(p) > 0 {
		r.written = true
	}
	return r.w.Write(p)
}

// newRootCommand builds the root command; onErr receives ExitErrHandler's error
// and errOut becomes the root's ErrWriter, wrapped in the returned errRecorder.
// Each exit phrase in Description leads its row in docs/exit-codes.md.
func newRootCommand(onErr func(error), errOut io.Writer) (*cli.Command, *errRecorder) {
	report := &errRecorder{w: errOut}
	cmd := &cli.Command{
		Name:  "go-galaxy",
		Usage: "Galaxy Collection Manager for CI; with no command it runs install",
		Description: "With no command, go-galaxy runs install.\n" +
			"\n" +
			"Exit codes:\n" +
			"  0    Success\n" +
			"  1    Generic failure\n" +
			"  2    Usage or configuration error\n" +
			"  3    Dependency resolution failure\n" +
			"  4    Network or Galaxy API failure\n" +
			"  5    Install-time failure\n" +
			"  6    Lockfile error\n" +
			"  7    Artifact-integrity failure\n" +
			"  8    Cache contention\n" +
			"  9    Persisted cache state is corrupt or oversized\n" +
			"  10   Signature verification failure\n" +
			"  130  Interrupted\n",
		HideHelpCommand:        true,
		UseShortOptionHandling: true,
		DefaultCommand:         "install",
		ArgValidator:           commands.NoArguments,
		Version:                buildinfo.Version(Version, Commit, Date, BuiltBy),
		ErrWriter:              report,
		Flags:                  cliflags.CommonFlags(),
		Commands: []*cli.Command{
			commands.Install(),
			commands.Cleanup(),
			commands.Lock(),
			commands.Warm(),
			commands.Hash(),
			commands.Tree(),
			commands.Explain(),
			commands.Outdated(),
		},
		ExitErrHandler: func(_ context.Context, _ *cli.Command, err error) {
			if onErr != nil {
				onErr(err)
			}
		},
	}
	return cmd, report
}

// run configures and executes the CLI, returning the exit code.
func run() int {
	// Print the formatted buildinfo.Version string, never the raw Version
	// global, which is empty on a dev build.
	cli.VersionPrinter = func(c *cli.Command) {
		_, _ = fmt.Fprintln(c.Writer, c.Root().Version)
	}

	// cmdErr is what ExitErrHandler captured. A flag failure never reaches that
	// handler (urfave returns it from Run), so handleResult judges it through
	// report.written.
	var cmdErr error
	app, report := newRootCommand(func(err error) { cmdErr = err }, os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Deliberately omit syscall.SIGQUIT from this notify set: leaving it
	// unhandled restores Go's runtime default of dumping all goroutine stacks,
	// which is the standard way to diagnose a hung CI run.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	// caught records the signal (if any) that triggered cancellation, so run()
	// can report a signal-specific exit code even though ctx itself only carries
	// context.Canceled.
	var caught atomic.Pointer[os.Signal]
	done := make(chan struct{})
	go func() {
		select {
		case s := <-sigCh:
			caught.Store(&s)
			cancel()
		case <-done:
		}
	}()

	runErr := app.Run(ctx, os.Args)
	close(done)

	var sig os.Signal
	if p := caught.Load(); p != nil {
		sig = *p
	}

	// Not covered by a test, since run executes a real command: report.written
	// decides whether a flag failure is printed here or urfave already said it.
	code, printErr := handleResult(runErr, cmdErr, sig, report.written)
	if printErr != nil {
		progress.Errorf("%s", printErr.Error())
	}
	return code
}

// handleResult returns the exit code and the error still to print: a signal
// wins, then capturedErr via exitcode.FromError, then a bare flag failure exits
// ExitUsage and is printed only when urfave has not reported it already.
func handleResult(runErr, capturedErr error, sig os.Signal, reported bool) (int, error) {
	if sig != nil {
		return exitcode.FromSignal(sig), nil
	}
	if capturedErr != nil {
		return exitcode.FromError(capturedErr), capturedErr
	}
	if runErr != nil {
		if reported {
			return exitcode.ExitUsage, nil
		}
		return exitcode.ExitUsage, runErr
	}
	return exitcode.ExitOK, nil
}
