package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/buildinfo"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
)

// runWithArgs calls run with os.Args set to argv and os.Stdout and os.Stderr
// swapped for pipes, restoring all three; it returns the code and both streams.
func runWithArgs(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	origArgs, origOut, origErr := os.Args, os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stdout): %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stderr): %v", err)
	}
	var outBuf, errBuf bytes.Buffer
	outDone, errDone := make(chan struct{}), make(chan struct{})
	go func() { _, _ = io.Copy(&outBuf, outR); close(outDone) }()
	go func() { _, _ = io.Copy(&errBuf, errR); close(errDone) }()

	os.Args, os.Stdout, os.Stderr = append([]string{"go-galaxy"}, argv...), outW, errW
	code := run()
	os.Args, os.Stdout, os.Stderr = origArgs, origOut, origErr

	_ = outW.Close()
	_ = errW.Close()
	<-outDone
	<-errDone
	_ = outR.Close()
	_ = errR.Close()
	return code, outBuf.String(), errBuf.String()
}

// TestRunPrintsTheFormattedVersion pins that --version prints the
// buildinfo.Version string, never the raw Version global a dev build leaves
// empty, and exits 0.
func TestRunPrintsTheFormattedVersion(t *testing.T) {
	code, stdout, stderr := runWithArgs(t, "--version")
	if code != exitcode.ExitOK {
		t.Fatalf("run() = %d, want %d; stderr = %q", code, exitcode.ExitOK, stderr)
	}
	want := buildinfo.Version(Version, Commit, Date, BuiltBy) + "\n"
	if stdout != want || strings.TrimSpace(stdout) == "" {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestRunReportsAFlagFailureOnce pins that a flag failure urfave reports itself
// exits ExitUsage and is not printed a second time by run.
func TestRunReportsAFlagFailureOnce(t *testing.T) {
	code, _, stderr := runWithArgs(t, "install", "--workers=abc")
	if code != exitcode.ExitUsage {
		t.Fatalf("run() = %d, want %d; stderr = %q", code, exitcode.ExitUsage, stderr)
	}
	if n := strings.Count(stderr, `invalid value "abc" for flag -workers`); n != 1 {
		t.Errorf("stderr reports the bad value %d times, want once:\n%s", n, stderr)
	}
}

// TestRunPrintsACapturedActionError pins that an error an action returns is
// classified by exitcode.FromError and printed by run, once.
func TestRunPrintsACapturedActionError(t *testing.T) {
	code, stdout, stderr := runWithArgs(t, "hash", "-r", "requirements.txt")
	if code != exitcode.ExitUsage {
		t.Fatalf("run() = %d, want %d; stderr = %q", code, exitcode.ExitUsage, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing from a refused hash", stdout)
	}
	if n := strings.Count(stderr, `"requirements.txt"`); n != 1 {
		t.Errorf("stderr names the refused file %d times, want once:\n%s", n, stderr)
	}
}
