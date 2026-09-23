package collections_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// gitLeakPassword is distinctive for the same reason leakToken is: no
// artifact byte, path or protocol word the pipeline emits on its own can
// collide with it, so any match is the password itself.
const gitLeakPassword = "g1t-passw0rd-must-not-appear-anywhere"

// newGitLeakFixture binds a basic credential carrying gitLeakPassword to the
// https fixture host, turns on the loudest output configuration (--verbose
// plus a metrics file) and swaps in a printer that records every tier.
func newGitLeakFixture(t *testing.T) (*gitFixture, *lineCapturingPrinter) {
	t.Helper()
	f := newGitFixture(t)
	bound, err := gitsource.ParsePrefix("https://git.example")
	if err != nil {
		t.Fatal(err)
	}
	printer := &lineCapturingPrinter{}
	f.runtime = infra.New(printer, f.galaxy.Client())
	f.runtime.Git = f.git
	f.runtime.GitCredentials = []gitsource.Credential{{URL: bound, Username: "ci", Password: gitLeakPassword}}
	f.cfg.Verbose = true
	f.cfg.MetricsFile = filepath.Join(t.TempDir(), "metrics.json")
	return f, printer
}

// assertNoGitPasswordInLines fails on any recorded line, on any tier, that
// carries the password.
func assertNoGitPasswordInLines(t *testing.T, printer *lineCapturingPrinter) {
	t.Helper()
	for _, line := range printer.snapshot() {
		if strings.Contains(line, gitLeakPassword) {
			t.Errorf("git password leaked into a printed line: %q", line)
		}
	}
}

// TestGitPasswordNeverLeaksOnSuccess asserts a bound git password, proven to
// have been presented, reaches neither the cache tree, the lockfile, GALAXY.yml,
// the metrics file nor any printed line under --verbose.
func TestGitPasswordNeverLeaksOnSuccess(t *testing.T) {
	t.Parallel()
	f, printer := newGitLeakFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\n")
	f.mustInstall(t)
	lockPath := lockfile.ResolveDefaultPath(f.reqPath, "")
	f.lockfile(t)
	if got := f.git.seenAuth[gitAppURL]; got.Password != gitLeakPassword {
		t.Fatalf("the fake client never saw the bound credential: %+v", got)
	}

	assertNoTokenInTree(t, f.cacheDir, gitLeakPassword)
	assertNoTokenInTree(t, f.downloadPath, gitLeakPassword)
	assertNoTokenInFile(t, lockPath, gitLeakPassword)
	assertNoTokenInFile(t, filepath.Join(f.downloadPath, "ansible_collections", "acme.app-1.2.3.info", "GALAXY.yml"), gitLeakPassword)
	assertNoTokenInFile(t, f.cfg.MetricsFile, gitLeakPassword)
	assertNoGitPasswordInLines(t, printer)
}

// TestGitPasswordNeverLeaksOnFailure asserts an authentication refusal and a
// repository the host does not serve put the password into neither the
// returned error nor any printed line.
func TestGitPasswordNeverLeaksOnFailure(t *testing.T) {
	t.Parallel()
	f, printer := newGitLeakFixture(t)
	f.git.failWith = fmt.Errorf("%w: boom", helpers.ErrGitAuthFailed)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\n")
	err := f.install(t)
	if err == nil {
		t.Fatal("install succeeded against a client that refuses every request")
	}
	if strings.Contains(err.Error(), gitLeakPassword) {
		t.Fatalf("git password leaked into the error: %v", err)
	}
	assertNoGitPasswordInLines(t, printer)

	g, gPrinter := newGitLeakFixture(t)
	g.writeRequirements(t, "collections:\n  - git+https://git.example/acme/missing.git\n")
	err = g.install(t)
	if err == nil {
		t.Fatal("install succeeded for a repository the host does not serve")
	}
	if strings.Contains(err.Error(), gitLeakPassword) {
		t.Fatalf("git password leaked into the error: %v", err)
	}
	assertNoGitPasswordInLines(t, gPrinter)
	assertNoTokenInTree(t, g.cacheDir, gitLeakPassword)
}
