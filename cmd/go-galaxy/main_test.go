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

// TestHandleResult pins handleResult's precedence (signal, then captured
// error, then bare flag failure) and that the two usage rows, differing only in
// reported, give opposite printErr so a silent flag failure is still printed.
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

// handleResultCases builds TestHandleResult's table, split out to stay under
// the funlen budget; reported is spelled on both usage rows because they are
// one fixture differing in exactly that bit.
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
		// Unreachable from run, since urfave never calls ExitErrHandler for a
		// usage error it reports; the row pins that capturedErr is consulted
		// ahead of reported.
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

// TestRootCommandDisclosesDefaultCommandAndExitCodes pins that --help says a
// bare go-galaxy installs and lists every exit class; the rows use the exitcode
// constants, so renumbering a class without updating the help text fails.
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

// TestRootCommandRecordsUrfaveUsageReports pins errRecorder's observation: an
// argv flag failure urfave reports itself is recorded, an environment one it
// reports nowhere is not. Each subtest needs its own command, as Run mutates it.
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

// TestRootCommandRefusesPositionalArguments pins that an argument no command
// takes is refused as ErrUnexpectedArguments before any action, while accepted
// rows reach their actions and fail on missing files under t.TempDir only.
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

// runRootCommand runs a fresh root command over args, since urfave mutates
// command state across a Run, and returns the error ExitErrHandler captured.
func runRootCommand(t *testing.T, args []string) error {
	t.Helper()
	var captured error
	cmd, _ := newRootCommand(func(err error) { captured = err }, io.Discard)
	cmd.Writer = io.Discard
	_ = cmd.Run(context.Background(), append([]string{"go-galaxy"}, args...))
	return captured
}
