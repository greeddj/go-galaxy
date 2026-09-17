// Package main is the go-galaxy executable. It assembles the urfave/cli
// command tree from cmd/go-galaxy/commands, installs the signal handler whose
// cancellation every command runs under, and turns what comes back into a
// process exit code through cmd/go-galaxy/exitcode - the decision handleResult
// owns. The Version, Commit, Date and BuiltBy variables are injected at link
// time by the Justfile's LDFLAGS and are this binary's only build-info source.
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

// errRecorder is the writer urfave prints its own diagnostics to. It passes
// every write through to w and records that one happened, which is the only
// question handleResult has about a bare runErr: has the operator been told.
//
// The observation is exact rather than approximate, and that is a property of
// this writer's authorship rather than of urfave's internals. urfave writes
// here for one reason - a usage error it is reporting itself, to
// cmd.Root().ErrWriter - and go-galaxy writes here for none at all: its own
// terminal line goes to os.Stderr through progress.Errorf, and
// cli.VersionPrinter writes to cmd.Writer. So a recorded write means a
// diagnostic reached the operator, and a urfave version that reports a
// further usage error on this writer stays covered with no change here.
//
// written needs no synchronization: urfave writes it inside app.Run, on the
// parse path and before any command action starts, and run reads it only
// after app.Run has returned.
type errRecorder struct {
	w       io.Writer
	written bool
}

// Write implements io.Writer, recording any non-empty write. The length guard
// is defensive: nothing writes zero bytes here today, so the branch is
// deliberately left unpinned, but an io.Writer may be handed an empty slice
// and an empty write tells an operator nothing.
func (r *errRecorder) Write(p []byte) (int, error) {
	if len(p) > 0 {
		r.written = true
	}
	return r.w.Write(p)
}

// newRootCommand builds the root command. onErr, when non-nil, receives what
// urfave hands ExitErrHandler - an argument validator, before, flag action,
// action or after failure. A nil onErr captures nothing, which is what a
// caller inspecting only the command's shape wants; run passes a real one.
//
// ArgValidator is commands.NoArguments, declared here so every command inherits
// it: an argument no command takes, a first word that names no command
// included, is refused before any action runs rather than silently dropped.
//
// errOut is where urfave prints the usage errors it reports itself. It is
// wrapped in the returned *errRecorder rather than installed bare, and the
// recorder is returned rather than left to be dug back out of cmd.ErrWriter,
// so no caller can build this command without the means to ask whether urfave
// has already reported a failure - the question handleResult answers with it.
// The root is the right place for this and the wrong place for
// cli.Command.OnUsageError, which is why that field is not used instead:
// urfave resolves the writer through cmd.Root(), so one assignment covers
// every subcommand, while it reads OnUsageError off whichever command was
// parsing, which for a flag declared on a subcommand is never the root.
//
// Usage and Description carry two facts a CI author cannot learn anywhere
// else from the binary itself. The first is that a bare go-galaxy installs:
// DefaultCommand makes it so, and nothing else the binary prints would say
// so, which is a surprising amount of work for a command someone ran to see
// what it does. The second is the exit-code classes. They exist to be
// branched on, so a pipeline author is exactly who needs them. Description is
// the one field that reaches --help with them, since urfave renders a
// DESCRIPTION block whenever it is non-empty.
//
// Each phrase below is the leading phrase of the matching row in docs/
// exit-codes.md rather than a fresh wording, so the two cannot come to
// describe the same number differently. Those rows carry the full
// qualifications; this list is the index, not a replacement.
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
	// Customize the version printer to show only the formatted version
	// string (c.Root().Version, set below via buildinfo.Version). The raw
	// Version global can be empty on dev builds; the formatted string never is.
	cli.VersionPrinter = func(c *cli.Command) {
		_, _ = fmt.Fprintln(c.Writer, c.Root().Version)
	}

	// cmdErr captures argument-validator/before/flag-action/action/after errors
	// via ExitErrHandler.
	// A flag failure never reaches this handler at all - urfave returns it from
	// Run instead - so handleResult judges that shape through report.written.
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

	// Not covered by a test: run reads os.Args and executes a real command, so
	// nothing exercises this call. Passing a constant in place of
	// report.written would either restore the silence this fixes or, for the
	// argv shape, print a second line beside the one urfave has already
	// written.
	code, printErr := handleResult(runErr, cmdErr, sig, report.written)
	if printErr != nil {
		progress.Errorf("%s", printErr.Error())
	}
	return code
}

// handleResult decides the process exit code and the error, if any, that
// still needs printing. A caught signal takes precedence over everything
// else (the process is being asked to stop, not to report a business
// error); otherwise capturedErr (from ExitErrHandler) wins since it carries
// the actual error value, classified via exitcode.FromError.
//
// A bare runErr with no capturedErr is a flag failure urfave returned without
// routing it through ExitErrHandler, and it arrives in two shapes the error
// value itself does not tell apart: one urfave has already reported on the
// root command's ErrWriter, with a help block alongside it, and one it
// returns in silence. reported is which. It is an observation - whether
// anything reached that writer - rather than an inference about where the
// value came from, because what this function needs to know is whether the
// operator has been told, not which parsing phase failed. The silent shape
// has one producer today, a value urfave took from an environment variable
// and parsed in a phase that reports nothing, and nothing here depends on
// that staying the only one.
//
// Both shapes exit exitcode.ExitUsage, a fixed answer rather than a
// classified one: such an error carries no go-galaxy sentinel, so
// exitcode.FromError would fall back to exitcode.ExitError for it.
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
