package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/urfave/cli/v3"
)

// These tests pin that tree and explain classify a missing lockfile, through
// lockfile.LoadRequired, as helpers.ErrLockfileMissing in the lockfile exit
// class, and that hash still falls back to the requirements file.

// missingLockfileFixture writes a requirements file into a fresh directory
// and returns that directory, with no lockfile beside it.
func missingLockfileFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	reqPath := filepath.Join(dir, "requirements.yml")
	body := []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n")
	if err := os.WriteFile(reqPath, body, helpers.FileMod); err != nil {
		t.Fatalf("write requirements: %v", err)
	}
	return dir
}

// runCommandInDir runs cmd with args from within dir, so the default lockfile
// path resolves to a genuinely absent file; t.Chdir is why nothing in this
// file calls t.Parallel.
func runCommandInDir(t *testing.T, dir string, cmd *cli.Command, args ...string) error {
	t.Helper()

	t.Chdir(dir)
	return cmd.Run(context.Background(), append([]string{cmd.Name}, args...))
}

// TestMissingLockfileClassifiesAsLockfileError pins both the sentinel and the
// exit code for each command here that requires a lockfile, so a sentinel that
// no longer maps to ExitLock is caught.
func TestMissingLockfileClassifiesAsLockfileError(t *testing.T) {
	for _, tc := range missingLockfileCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := missingLockfileFixture(t)

			err := runCommandInDir(t, dir, tc.command(), tc.args...)
			if !errors.Is(err, helpers.ErrLockfileMissing) {
				t.Fatalf("%s with no lockfile: err = %v, want errors.Is helpers.ErrLockfileMissing", tc.name, err)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitLock {
				t.Errorf("exitcode.FromError(err) = %d, want ExitLock (%d)", got, exitcode.ExitLock)
			}
			// The path belongs in the message: "not found" without naming
			// what was looked for leaves an operator guessing which of the
			// default and the --lock-file override was in effect.
			if !strings.Contains(err.Error(), lockfile.DefaultName) {
				t.Errorf("error does not name the lockfile path it looked for: %v", err)
			}
		})
	}
}

// missingLockfileCase is one row of TestMissingLockfileClassifiesAsLockfileError.
type missingLockfileCase struct {
	command func() *cli.Command
	name    string
	args    []string
}

// missingLockfileCases returns one row per command in this package that
// requires a lockfile to exist.
func missingLockfileCases() []missingLockfileCase {
	return []missingLockfileCase{
		{name: "tree", command: Tree},
		{name: "explain", command: Explain, args: []string{"acme.widgets"}},
	}
}

// TestMissingLockfileLeavesHashFallingBack pins hash's documented exception:
// with no lockfile it hashes the requirements file, which also proves the
// fixture sound for the rows above.
func TestMissingLockfileLeavesHashFallingBack(t *testing.T) {
	dir := missingLockfileFixture(t)

	got, err := computeHash(filepath.Join(dir, "requirements.yml"), filepath.Join(dir, lockfile.DefaultName))
	if err != nil {
		t.Fatalf("computeHash with no lockfile: %v", err)
	}
	if !strings.HasPrefix(got, "sha256:") {
		t.Errorf("computeHash = %q, want a sha256: prefixed digest of the requirements file", got)
	}
}
