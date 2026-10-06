package collections_test

// This file pins the advertisement lock makes before it fetches a commit
// galaxy.lock pins: it ends at the git deadline, and its debug line says why
// the commit is fetched without recording a pin, by cause.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestLockedPinAdvertisementEndsAtTheGitDeadline pins that the advertisement
// before a locked commit is fetched runs under the git deadline, as the fetch
// does, and ends the run as that deadline, not when the run's context ends.
func TestLockedPinAdvertisementEndsAtTheGitDeadline(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n")
	f.lockfile(t)
	goColdGit(t, f)
	f.runtime.Git = stallingAdvertiser{fakeGitClient: f.git}
	f.runtime.GitFetchDeadline = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := collections.Lock(ctx, f.cfg, f.runtime); !errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
		t.Fatalf("lock while the advertisement stalls: %v, want %v", err, helpers.ErrArtifactDownloadDeadline)
	}
}

// assertLockedPinDebugLine runs lock over f on an empty cache and fails
// unless it printed want among its lines, debug ones included.
func assertLockedPinDebugLine(t *testing.T, f *gitFixture, want string) {
	t.Helper()
	goColdGit(t, f)
	out := &lineCapturingPrinter{}
	f.runtime.Output = out
	if err := lockGitRun(f); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if lines := out.snapshot(); !slices.Contains(lines, want) {
		t.Fatalf("lock printed %q, want the line %q", lines, want)
	}
}

// TestLockedPinDebugLineNamesItsCause pins the debug line of a locked commit
// fetched without recording a pin: the ref gone, the ref at another commit,
// or the ref at that commit under another version than galaxy.lock's.
func TestLockedPinDebugLineNamesItsCause(t *testing.T) {
	t.Parallel()
	locked := fakeCommit("app-1")
	t.Run("ref gone", func(t *testing.T) {
		t.Parallel()
		f := newGitFixture(t)
		f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n")
		f.lockfile(t)
		delete(f.git.repos[gitAppURL].refs, gitMainRef)
		assertLockedPinDebugLine(t, f, fmt.Sprintf(
			"%s@main is not advertised; fetching commit %s, which galaxy.lock pins, without recording a pin", gitAppURL, locked))
	})
	t.Run("ref at another commit", func(t *testing.T) {
		t.Parallel()
		f := newGitFixture(t)
		f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n")
		f.lockfile(t)
		f.git.repos[gitAppURL].refs[gitMainRef] = fakeCommit("app-2")
		assertLockedPinDebugLine(t, f, fmt.Sprintf(
			"%s@main names commit %s; fetching commit %s, which galaxy.lock pins, without recording a pin",
			gitAppURL, fakeCommit("app-2"), locked))
	})
	t.Run("ref at that commit under another version", func(t *testing.T) {
		t.Parallel()
		f := newRoleFixture(t)
		addMovingRepo(f, "labeled-1")
		f.writeRequirements(t, "roles:\n  - src: git+"+movingRoleURL+"\n    version: main\n    name: moving\n")
		lf := f.lockfile(t)
		for i := range lf.Roles {
			lf.Roles[i].Version = "edited"
		}
		saveLockfile(t, f.reqPath, lf)
		assertLockedPinDebugLine(t, f.gitFixture, fmt.Sprintf(
			"%s@main names commit %s as version main; fetching it as version edited, which galaxy.lock pins, without recording a pin",
			movingRoleURL, fakeCommit("labeled-1")))
	})
}
