package collections_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// assertForeignCommitPinRefused runs install and lock over a pin, under the
// commit ref asked, that names the foreign commit, and fails unless both
// refuse it as a usage error naming that commit and lock writes no file.
func assertForeignCommitPinRefused(t *testing.T, f *gitFixture, foreign string) {
	t.Helper()
	ctx := context.Background()
	for _, run := range []struct {
		do   func() error
		name string
	}{
		{name: "install", do: func() error { return collections.Start(ctx, f.cfg, f.runtime) }},
		{name: "lock", do: func() error { return collections.Lock(ctx, f.cfg, f.runtime) }},
	} {
		err := run.do()
		if !errors.Is(err, helpers.ErrInvalidGitLocator) || !strings.Contains(err.Error(), foreign) {
			t.Fatalf("%s from a pin naming %s: %v, want ErrInvalidGitLocator naming that commit", run.name, foreign, err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitUsage {
			t.Fatalf("%s from a pin naming %s: exit %d, want %d", run.name, foreign, got, exitcode.ExitUsage)
		}
	}
	assertPathAbsent(t, lockfile.ResolveDefaultPath(f.reqPath, ""))
}

// TestGitCommitRefRefusesAPinAtAnotherCommit pins that a git pin recorded
// under a commit ref replays only at that commit: one copied there from
// another commit is refused, and the pin at its own commit still replays.
func TestGitCommitRefRefusesAPinAtAnotherCommit(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	asked, foreign := fakeCommit("app-1"), fakeCommit("app-2")
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+","+foreign+"\n")
	f.mustInstall(t)
	mutateStoreSnapshot(t, f, func(st *store.Store) {
		pin, ok := st.GetGitPin(gitsource.PinKey(gitAppURL, foreign, ""))
		if !ok {
			t.Fatalf("no git pin recorded at %s", foreign)
		}
		st.SetGitPin(gitsource.PinKey(gitAppURL, asked, ""), pin)
	})

	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+","+asked+"\n")
	assertForeignCommitPinRefused(t, f, foreign)

	// Positive control: the pin at its own commit replays with no git call.
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+","+foreign+"\n")
	f.git.resetCounts()
	f.mustInstall(t)
	if adv, acq := f.git.counts(); adv != 0 || acq != 0 {
		t.Fatalf("replay at its own commit reached the remote: advertises=%d acquires=%d", adv, acq)
	}
	if got := readManifestVersion(t, f.downloadPath, "app"); got != "1.3.0" {
		t.Fatalf("installed acme.app version = %q, want 1.3.0", got)
	}
}

// TestRoleCommitRefRefusesAPinAtAnotherCommit is the git role half, with the
// other commit's artifact cached so nothing but the commit check stands
// between that pin and its replay.
func TestRoleCommitRefRefusesAPinAtAnotherCommit(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	asked, foreign := fakeCommit("role-app-1"), fakeCommit("role-app-2")
	f.writeRequirements(t, "roles:\n  - src: git+"+roleAppURL+"\n    version: "+foreign+"\n    name: app\n")
	f.mustInstall(t)
	mutateStoreSnapshot(t, f.gitFixture, func(st *store.Store) {
		pin, ok := st.GetRolePin(gitsource.PinKey(roleAppURL, foreign, ""))
		if !ok {
			t.Fatalf("no git role pin recorded at %s", foreign)
		}
		st.SetRolePin(gitsource.PinKey(roleAppURL, asked, ""), pin)
	})

	f.writeRequirements(t, "roles:\n  - src: git+"+roleAppURL+"\n    version: "+asked+"\n    name: app\n")
	assertForeignCommitPinRefused(t, f.gitFixture, foreign)

	// Positive control: the pin at its own commit replays with no git call.
	f.writeRequirements(t, "roles:\n  - src: git+"+roleAppURL+"\n    version: "+foreign+"\n    name: app\n")
	f.git.resetCounts()
	before := f.git.roleAcquireCount()
	f.mustInstall(t)
	if adv, _ := f.git.counts(); adv != 0 || f.git.roleAcquireCount() != before {
		t.Fatalf("replay at its own commit reached the remote: advertises=%d role acquires=%d", adv, f.git.roleAcquireCount()-before)
	}
	if got := loadInstalledRole(t, f, "app").Version; got != foreign {
		t.Fatalf("installed role version = %q, want %s", got, foreign)
	}
}
