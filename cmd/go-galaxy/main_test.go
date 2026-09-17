package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestHandleResult covers every way app.Run can finish: clean success, a flag
// failure in each of its two shapes, an action error captured via
// ExitErrHandler that gets classified through exitcode.FromError and still
// needs printing exactly once, and a caught signal that must take precedence
// over any captured error since the process is being told to stop.
//
// "usage error urfave reported itself" and "usage error nothing reported" are
// one fixture differing in exactly the reported bit, and they are required to
// produce opposite printErr values. The first is the positive control: it
// shows this fixture can reach the already-told answer at all. The second is
// the pin on the defect - a flag value urfave parsed in silence, which exited
// with nothing on either stream. Neither row proves anything alone: without
// the first, "it stayed quiet" is indistinguishable from "it stays quiet for
// every bare runErr"; without the second, nothing forbids that silence.
//
// KILLING MUTATION, run and reverted, in handleResult (main.go) - return
// exitcode.ExitUsage, nil from the silent arm, which is the behavior this
// test exists to forbid. Only "usage error nothing reported" fails:
//
//	main_test.go:69: printErr = <nil>, want errors.Is match with flag parse error
//
// KILLING MUTATION, run and reverted, in handleResult (main.go) - invert the
// arm's condition to "if !reported". Both rows fail, with opposite messages,
// which is what shows the control and the pin are reading the same bit from
// opposite sides:
//
//	main_test.go:64: printErr = flag parse error, want nil
//	main_test.go:69: printErr = <nil>, want errors.Is match with flag parse error
//
// KILLING MUTATION, run and reverted, in handleResult (main.go) - hoist the
// reported check above the capturedErr branch. Only "captured error outranks
// a usage report" fails:
//
//	main_test.go:60: code = 2, want 5
//	main_test.go:69: printErr = <nil>, want errors.Is match with installation failed
func TestHandleResult(t *testing.T) {
	t.Parallel()
	for _, tt := range handleResultCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, printErr := handleResult(tt.runErr, tt.capturedErr, tt.sig, tt.reported)
			if code != tt.wantCode {
				t.Errorf("code = %d, want %d", code, tt.wantCode)
			}
			if tt.wantPrintErr == nil {
				if printErr != nil {
					t.Errorf("printErr = %v, want nil", printErr)
				}
				return
			}
			if !errors.Is(printErr, tt.wantPrintErr) {
				t.Errorf("printErr = %v, want errors.Is match with %v", printErr, tt.wantPrintErr)
			}
		})
	}
}

// handleResultCase is one row of TestHandleResult: the three values app.Run
// can leave behind, the observation of whether urfave has already reported,
// and the answer handleResult must give for them.
type handleResultCase struct {
	runErr       error
	capturedErr  error
	sig          os.Signal
	wantPrintErr error
	name         string
	wantCode     int
	reported     bool
}

// handleResultCases builds TestHandleResult's table, split out from the test
// function itself purely to stay under the funlen budget - the same split
// internal/cache/s3's newS3RetryableCases makes for the identical reason.
//
// A zero-valued field is left off a row, except reported on the two usage
// rows: those two are one fixture differing in exactly that bit, so it is
// written on both rather than inferred from an absence.
func handleResultCases() []handleResultCase {
	return []handleResultCase{
		{name: "success", wantCode: exitcode.ExitOK},
		{
			name:     "usage error urfave reported itself",
			runErr:   errTestUsage,
			reported: true,
			wantCode: exitcode.ExitUsage,
		},
		{
			name:         "usage error nothing reported",
			runErr:       errTestUsage,
			reported:     false,
			wantCode:     exitcode.ExitUsage,
			wantPrintErr: errTestUsage,
		},
		{
			name:         "action error captured and classified via FromError",
			runErr:       helpers.ErrInstallationFailed,
			capturedErr:  helpers.ErrInstallationFailed,
			wantCode:     exitcode.ExitInstall,
			wantPrintErr: helpers.ErrInstallationFailed,
		},
		// This input combination is not reachable from run() today: urfave
		// reports an argv usage error and returns without ever calling
		// ExitErrHandler, so no run sets reported and capturedErr together.
		// The row pins branch order - capturedErr is consulted ahead of the
		// reported check - and is not a scenario an operator can produce.
		{
			name:         "captured error outranks a usage report",
			runErr:       errTestUsage,
			capturedErr:  helpers.ErrInstallationFailed,
			reported:     true,
			wantCode:     exitcode.ExitInstall,
			wantPrintErr: helpers.ErrInstallationFailed,
		},
		{
			name:        "caught signal takes precedence over a captured network error",
			runErr:      helpers.ErrDownloadFailed,
			capturedErr: helpers.ErrDownloadFailed,
			sig:         syscall.SIGINT,
			wantCode:    130,
		},
	}
}

// errTestUsage is a static sentinel for the usage-only case, kept as a
// package-level var to satisfy the err113 linter (no inline errors.New in
// test bodies).
var errTestUsage = errors.New("flag parse error")

// TestRootCommandDisclosesDefaultCommandAndExitCodes pins the two facts the
// binary tells a CI author about itself in --help: a bare go-galaxy installs,
// which DefaultCommand makes true but which no flag or argument spells out;
// and the exit-code classes, which exist to be branched on and would
// otherwise be reachable only through docs/exit-codes.md's full table.
//
// The rows are built from the exitcode constants rather than from literals
// repeated here, so the pin is on the numbers themselves: renumbering a class
// without updating the help text fails this test, which is the failure mode
// worth catching - a help list naming a wrong number is worse than no list.
// The phrases stay literals, since they are prose the constants do not carry.
//
// KILLING MUTATION, run and reverted, on newRootCommand's Description in
// main.go - change the line for exit 8 to read "  9    Cache contention",
// which is what a careless renumbering looks like:
//
//	main_test.go:193: Description is missing the line for exit 8: "  8    Cache contention"
func TestRootCommandDisclosesDefaultCommandAndExitCodes(t *testing.T) {
	cmd, _ := newRootCommand(nil, io.Discard)

	if cmd.DefaultCommand != "install" {
		t.Fatalf("DefaultCommand = %q, want %q", cmd.DefaultCommand, "install")
	}
	if !strings.Contains(cmd.Usage, "install") {
		t.Errorf("Usage = %q, want it to name the default command", cmd.Usage)
	}

	rows := []struct {
		phrase string
		code   int
	}{
		{"Success", exitcode.ExitOK},
		{"Generic failure", exitcode.ExitError},
		{"Usage or configuration error", exitcode.ExitUsage},
		{"Dependency resolution failure", exitcode.ExitResolution},
		{"Network or Galaxy API failure", exitcode.ExitNetwork},
		{"Install-time failure", exitcode.ExitInstall},
		{"Lockfile error", exitcode.ExitLock},
		{"Artifact-integrity failure", exitcode.ExitIntegrity},
		{"Cache contention", exitcode.ExitCacheBusy},
		{"Persisted cache state is corrupt or oversized", exitcode.ExitCacheCorrupt},
		{"Signature verification failure", exitcode.ExitSignature},
		{"Interrupted", exitcode.ExitInterrupt},
	}
	for _, row := range rows {
		line := fmt.Sprintf("  %-5d%s", row.code, row.phrase)
		if !strings.Contains(cmd.Description, line+"\n") {
			t.Errorf("Description is missing the line for exit %d: %q", row.code, line)
		}
	}
}

// TestRootCommandRecordsUrfaveUsageReports pins the observation handleResult
// reads: whether urfave reported a flag failure itself. The two subtests are
// each other's positive control - one fixture per shape, differing only in
// where the unparseable value came from - so a recorder that saw nothing
// cannot be mistaken for a writer nothing ever reaches.
//
// Each subtest builds its own root command, since urfave mutates command and
// flag state across a Run.
//
// The err != nil check is a guard, not the pin: both inputs are structurally
// unparseable for a cli.IntFlag, which keeps the install action unreachable in
// a unit test, and the check makes that assumption fail loudly if it stops
// holding.
//
// KILLING MUTATION, run and reverted, in newRootCommand (main.go) - delete the
// ErrWriter: report line, so urfave reports to the default writer and the
// recorder never sees it. The argv subtest fails, and the line it was meant to
// capture leaks to the test run's own stderr:
//
//	main_test.go:229: rec.written = false, want true; errOut = ""
func TestRootCommandRecordsUrfaveUsageReports(t *testing.T) {
	t.Run("argv value urfave reports itself", func(t *testing.T) {
		var errOut bytes.Buffer
		cmd, rec := newRootCommand(nil, &errOut)
		cmd.Writer = io.Discard

		err := cmd.Run(context.Background(), []string{"go-galaxy", "install", "--workers=abc"})
		if err == nil {
			t.Fatalf("Run() = nil, want a flag-parse error; errOut = %q", errOut.String())
		}
		if !rec.written {
			t.Errorf("rec.written = false, want true; errOut = %q", errOut.String())
		}
	})

	t.Run("environment value urfave reports nowhere", func(t *testing.T) {
		t.Setenv("GO_GALAXY_WORKERS", "abc")

		var errOut bytes.Buffer
		cmd, rec := newRootCommand(nil, &errOut)
		cmd.Writer = io.Discard

		err := cmd.Run(context.Background(), []string{"go-galaxy", "install"})
		if err == nil {
			t.Fatalf("Run() = nil, want a flag-parse error; errOut = %q", errOut.String())
		}
		if rec.written {
			t.Errorf("rec.written = true, want false; errOut = %q", errOut.String())
		}
	})
}

// TestRootCommandRefusesPositionalArguments pins that a positional argument no
// command takes is refused before any action runs, as a usage error: the
// captured error is helpers.ErrUnexpectedArguments, it classifies
// exitcode.ExitUsage, and its message names every refused argument. The first
// row is the defect this exists for - a word that names no command reached
// install as an argument, and install ran on the requirements file.
//
// The accepted rows are the positive controls, and they are what make the
// refusals mean something: install with no argument - named, and reached as
// the default command both with its own flags and with root flags alone -
// explain with its one and hash with none all get past validation and fail
// in their actions instead, each on a file that does not exist, so a
// validator refusing everything, or refusing install alone, cannot pass.
// GO_GALAXY_ANSIBLE_CONFIG names a missing file, which stops every install
// action at config.BuildCollectionConfig before it discovers the developer's
// ansible.cfg or opens a cache, so no row, and no regression that lets one
// reach an action, reads or writes anything outside t.TempDir.
//
// KILLING MUTATION, run and reverted, in newRootCommand (main.go) - delete the
// ArgValidator: commands.NoArguments line. The four rows refused by the root's
// validator fail and the explain row does not, since explain declares its own:
//
//	main_test.go:309: a first word that names no command: captured error =
//	ansible config file not found: .../001/ansible.cfg, want errors.Is match
//	with unexpected arguments
//
// KILLING MUTATION, run and reverted, in explainArguments (explain.go) - return
// nil for more than one argument. Only the explain row fails:
//
//	main_test.go:309: explain beyond its one argument: captured error =
//	lockfile not found: .../001/galaxy.lock, want errors.Is match with
//	unexpected arguments
//
// KILLING MUTATION, run and reverted, in unexpectedArguments (arguments.go) -
// drop the clause naming the default command. The three rows routed to install
// fail and the hash row, which must not carry the clause, passes:
//
//	main_test.go:317: a first word that names no command: message =
//	"unexpected arguments \"collection\" \"install\" \"ns.name\": install takes
//	none", want it to contain "unexpected arguments \"collection\" \"install\"
//	\"ns.name\": install takes none, and is what runs when the first word names
//	no command"
//
// KILLING MUTATION, run and reverted, in NoArguments (arguments.go) - delete
// the zero-argument early return, so every command is refused. The three
// install rows and the hash row among the accepted fail, the explain one does
// not:
//
//	main_test.go:325: install as the default command with root flags only:
//	captured error = unexpected arguments : install takes none, and is what runs
//	when the first word names no command, want errors.Is match with ansible
//	config file not found
func TestRootCommandRefusesPositionalArguments(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GO_GALAXY_ANSIBLE_CONFIG", dir+"/ansible.cfg")
	refused, accepted := positionalArgumentCases(dir)
	for _, tt := range refused {
		captured := runRootCommand(t, tt.args)
		if !errors.Is(captured, helpers.ErrUnexpectedArguments) {
			t.Errorf("%s: captured error = %v, want errors.Is match with %v", tt.name, captured, helpers.ErrUnexpectedArguments)
			continue
		}
		if code := exitcode.FromError(captured); code != exitcode.ExitUsage {
			t.Errorf("%s: exit code = %d, want %d", tt.name, code, exitcode.ExitUsage)
		}
		msg := captured.Error()
		if !strings.Contains(msg, tt.wantMessage) {
			t.Errorf("%s: message = %q, want it to contain %q", tt.name, msg, tt.wantMessage)
		} else if tt.notWant != "" && strings.Contains(msg, tt.notWant) {
			t.Errorf("%s: message = %q, want it not to contain %q", tt.name, msg, tt.notWant)
		}
	}
	for _, tt := range accepted {
		captured := runRootCommand(t, tt.args)
		if !errors.Is(captured, tt.wantErr) {
			t.Errorf("%s: captured error = %v, want errors.Is match with %v", tt.name, captured, tt.wantErr)
		}
	}
}

// positionalArgumentCase is one row of TestRootCommandRefusesPositionalArguments.
// wantMessage and notWant are read only for a refused row, wantErr only for an
// accepted one.
type positionalArgumentCase struct {
	wantErr     error
	name        string
	wantMessage string
	notWant     string
	args        []string
}

// defaultCommandClause is the clause a refusal by install carries and a refusal
// by any other command must not.
const defaultCommandClause = ", and is what runs when the first word names no command"

// positionalArgumentCases builds TestRootCommandRefusesPositionalArguments'
// refused and accepted rows over files under dir that do not exist, split out
// from the test function purely to stay under the funlen budget.
func positionalArgumentCases(dir string) ([]positionalArgumentCase, []positionalArgumentCase) {
	missingReq := dir + "/requirements.yml"
	installFlags := []string{"--cache-dir", dir + "/cache", "--dry-run", "-r", missingReq}
	inspectFlags := []string{"-r", missingReq, "--lock-file", dir + "/galaxy.lock"}

	refused := []positionalArgumentCase{
		{
			name:        "a first word that names no command",
			args:        append([]string{"collection", "install", "ns.name"}, installFlags...),
			wantMessage: `unexpected arguments "collection" "install" "ns.name": install takes none` + defaultCommandClause,
		},
		{
			name:        "an argument to install named on the command line",
			args:        append([]string{"install", "ns.name"}, installFlags...),
			wantMessage: `unexpected arguments "ns.name": install takes none` + defaultCommandClause,
		},
		{
			name:        "help is not a command",
			args:        append([]string{"help"}, installFlags...),
			wantMessage: `unexpected arguments "help": install takes none` + defaultCommandClause,
		},
		{
			name:        "a command other than the default",
			args:        append([]string{"hash", "extra"}, inspectFlags...),
			wantMessage: `unexpected arguments "extra": hash takes none`,
			notWant:     defaultCommandClause,
		},
		{
			name:        "explain beyond its one argument",
			args:        append([]string{"explain", "ns.a", "ns.b", "ns.c"}, inspectFlags...),
			wantMessage: `unexpected arguments "ns.b" "ns.c": explain takes one`,
		},
	}
	accepted := []positionalArgumentCase{
		{
			name:    "install named with none",
			args:    append([]string{"install"}, installFlags...),
			wantErr: helpers.ErrAnsibleConfigNotFound,
		},
		{
			name:    "install as the default command with its own flags",
			args:    installFlags,
			wantErr: helpers.ErrAnsibleConfigNotFound,
		},
		{
			name:    "install as the default command with root flags only",
			args:    []string{"--cache-dir", dir + "/cache", "--dry-run"},
			wantErr: helpers.ErrAnsibleConfigNotFound,
		},
		{
			name:    "explain with its one argument",
			args:    append([]string{"explain", "ns.a"}, inspectFlags...),
			wantErr: helpers.ErrLockfileMissing,
		},
		{
			name:    "hash with none",
			args:    append([]string{"hash"}, inspectFlags...),
			wantErr: os.ErrNotExist,
		},
	}
	return refused, accepted
}

// runRootCommand runs a fresh root command over args and returns the error
// ExitErrHandler captured, the value handleResult classifies. A fresh command
// per call is required, since urfave mutates command and flag state across a
// Run; Writer is discarded, as in TestRootCommandRecordsUrfaveUsageReports, so
// nothing urfave itself prints there reaches the test output.
func runRootCommand(t *testing.T, args []string) error {
	t.Helper()
	var captured error
	cmd, _ := newRootCommand(func(err error) { captured = err }, io.Discard)
	cmd.Writer = io.Discard
	_ = cmd.Run(context.Background(), append([]string{"go-galaxy"}, args...))
	return captured
}
