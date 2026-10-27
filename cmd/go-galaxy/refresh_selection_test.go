package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// TestRefreshSelectionFlagsAreLockOnly pins that --collection, -c and --role
// are undefined flags on every command but lock: a usage error (exit 2)
// urfave reports before anything is read. Not parallel.
func TestRefreshSelectionFlagsAreLockOnly(t *testing.T) {
	// The fixture's HOME and working directory keep a command that wrongly took a flag off real files.
	newRefreshSelectionFixture(t)
	commands := [][]string{nil, {"install"}, {"warm"}, {"outdated"}, {"hash"}, {"tree"}, {"explain"}, {"cleanup"}, {"migrate"}}
	selections := [][]string{{"-c", "a.b"}, {"--collection", "a.b"}, {"--role", "foo"}}
	for _, command := range commands {
		for _, selection := range selections {
			args := append(append([]string{}, command...), selection...)
			var errOut bytes.Buffer
			var captured error
			cmd, rec := newRootCommand(func(err error) { captured = err }, &errOut)
			cmd.Writer = io.Discard

			name := strings.Join(args, " ")
			runErr := cmd.Run(context.Background(), append([]string{"go-galaxy"}, args...))
			if runErr == nil {
				t.Errorf("%s: Run() = nil, want a flag-parse error; errOut = %q", name, errOut.String())
				continue
			}
			// An action that ran reports through captured, which main classifies by FromError.
			if code, _ := handleResult(runErr, captured, nil, rec.written); code != exitcode.ExitUsage {
				t.Errorf("%s: exit code = %d, want %d", name, code, exitcode.ExitUsage)
			}
			// urfave names the flag with one dash; help text in errOut would match "-c" anyway.
			flag := " -" + strings.TrimLeft(selection[0], "-")
			if !strings.Contains(runErr.Error(), flag) {
				t.Errorf("%s: the refusal does not name %q: err = %v", name, flag, runErr)
			}
		}
	}
}

// refreshSelectionFixture is a project whose galaxy.lock pins nothing, so
// every name a selection gives is one it lacks, and a cache under t.TempDir.
type refreshSelectionFixture struct {
	reqPath  string
	lockPath string
	cache    string
}

// newRefreshSelectionFixture writes the fixture with no variable steering
// --refresh or the inputs, and ansible.cfg discovery hidden. Not parallel:
// t.Setenv, t.Chdir.
func newRefreshSelectionFixture(t *testing.T) refreshSelectionFixture {
	t.Helper()
	for _, key := range []string{"GO_GALAXY_REFRESH", "GO_GALAXY_REQUIREMENTS_FILE", "ANSIBLE_GALAXY_REQUIREMENTS_FILE",
		"GO_GALAXY_LOCK_FILE", "GO_GALAXY_ANSIBLE_CONFIG", "GO_GALAXY_CACHE_DIR", "GO_GALAXY_S3_BUCKET"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("os.Unsetenv(%q) error = %v, want nil", key, err)
		}
	}
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "absent.cfg"))
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	project := t.TempDir()
	f := refreshSelectionFixture{
		reqPath:  filepath.Join(project, "requirements.yml"),
		lockPath: filepath.Join(project, "galaxy.lock"),
		cache:    filepath.Join(t.TempDir(), "cache"),
	}
	for path, body := range map[string]string{
		f.reqPath:  "collections: []\n",
		f.lockPath: "schema_version: 1\ncollections: []\n",
	} {
		if err := os.WriteFile(path, []byte(body), helpers.FileMod); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return f
}

// TestLockRefreshSelectionExitsUsage pins, through main's root command, that
// each selection refusal exits ExitUsage, every name the lockfile lacks in one
// error that leaves the file unchanged.
func TestLockRefreshSelectionExitsUsage(t *testing.T) {
	f := newRefreshSelectionFixture(t)
	base := []string{"lock", "--quiet", "--cache-dir", f.cache, "-r", f.reqPath}
	rows := []struct {
		want     error
		name     string
		wantText string
		args     []string
	}{
		{name: "-c without --refresh", args: []string{"-c", "a.b"}, want: helpers.ErrRefreshSelectionWithoutRefresh},
		{name: "a name with no dot", args: []string{"--refresh", "-c", "nodot"}, want: helpers.ErrInvalidRefreshSelection},
		{
			name:     "names the lockfile pins nothing for",
			args:     []string{"--refresh", "-c", "c.d,a.b", "--collection", "e.f", "--role", "foo"},
			want:     helpers.ErrRefreshSelectionNotLocked,
			wantText: "collection a.b, collection c.d, collection e.f, role foo",
		},
		{
			name:     "a name the lockfile lacks under --offline",
			args:     []string{"--refresh", "--offline", "--role", "foo"},
			want:     helpers.ErrRefreshSelectionNotLocked,
			wantText: f.lockPath + ": role foo",
		},
	}
	before, err := os.ReadFile(f.lockPath)
	if err != nil {
		t.Fatalf("read %s: %v", f.lockPath, err)
	}
	for _, row := range rows {
		captured := runRootCommand(t, append(append([]string{}, base...), row.args...))
		assertExitsUsageWith(t, row.name, captured, row.want)
		if captured != nil && !strings.Contains(captured.Error(), row.wantText) {
			t.Errorf("%s: captured error = %v, want it to carry %q", row.name, captured, row.wantText)
		}
	}
	if after, err := os.ReadFile(f.lockPath); err != nil || !bytes.Equal(after, before) {
		t.Errorf("galaxy.lock after the refusals = %q (%v), want it unchanged: %q", after, err, before)
	}
}

// unloadableLockfile is a galaxy.lock that exists but does not load: no
// release writes its schema_version.
const unloadableLockfile = "schema_version: 999\ncollections: []\n"

// writeUnloadableLockfile writes unloadableLockfile as name under a fresh
// directory and returns its path.
func writeUnloadableLockfile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(unloadableLockfile), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestLockRefreshSelectionRefusesAnUnloadableLockfile pins that a selection
// over a lockfile that does not load exits ExitUsage, not ExitLock, naming the
// file and leaving it as is; plain lock --refresh still rewrites it.
func TestLockRefreshSelectionRefusesAnUnloadableLockfile(t *testing.T) {
	f := newRefreshSelectionFixture(t)
	base := []string{"lock", "--quiet", "--cache-dir", f.cache, "-r", f.reqPath, "--refresh"}
	for _, extra := range [][]string{nil, {"--offline"}, {"--check"}} {
		lockPath := writeUnloadableLockfile(t, "galaxy.lock")
		name := strings.TrimSpace("a selection over an unloadable lockfile " + strings.Join(extra, " "))
		args := append(append(append([]string{}, base...), "--lock-file", lockPath, "-c", "a.b"), extra...)
		captured := runRootCommand(t, args)
		assertExitsUsageWith(t, name, captured, helpers.ErrRefreshSelectionLockfileUnreadable)
		if errors.Is(captured, helpers.ErrLockfileInvalid) {
			t.Errorf("%s: captured error = %v, want the load error rendered, not wrapped", name, captured)
		}
		if _, printed := handleResult(nil, captured, nil, false); printed == nil || !strings.Contains(printed.Error(), lockPath) {
			t.Errorf("%s: printed error = %v, want it to name %s", name, printed, lockPath)
		}
		//nolint:gosec // G304: the path is this test's own fixture.
		if after, err := os.ReadFile(lockPath); err != nil || string(after) != unloadableLockfile {
			t.Errorf("%s: lockfile after the refusal = %q (%v), want it unchanged", name, after, err)
		}
	}

	// Without a selection the same file only warns: lock resolves without its pins.
	lockPath := writeUnloadableLockfile(t, "galaxy.lock")
	if captured := runRootCommand(t, append(append([]string{}, base...), "--lock-file", lockPath)); captured != nil {
		t.Fatalf("plain lock --refresh over an unloadable lockfile: captured error = %v, want nil", captured)
	}
	if _, err := lockfile.Load(lockPath); err != nil {
		t.Errorf("plain lock --refresh left %s unloadable: %v, want it rewritten", lockPath, err)
	}
}
