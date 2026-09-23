package cache

// This file pins LockLostError's decision table: which (parent, holder, err)
// shapes produce a lock-loss verdict, which pass through untouched, and what a
// verdict deliberately puts out of reach of errors.Is.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// errRunFailed stands in for the run's own error in the rows that do not care
// about its class: only its non-nilness and identity matter there.
var errRunFailed = errors.New("the run's own failure")

// lockLostVerdict names the three answers LockLostError can give, so each row
// below states which one it expects instead of carrying its own assertion
// closure.
type lockLostVerdict int

const (
	// verdictUnchanged means the row's own err must come back matchable, and
	// must not have been reclassified into a lock-loss verdict.
	verdictUnchanged lockLostVerdict = iota
	// verdictBareSentinel means the result must be exactly
	// helpers.ErrCacheLockLost with nothing wrapped around it: the run itself
	// produced no error to render.
	verdictBareSentinel
	// verdictWrapped means the result must match helpers.ErrCacheLockLost
	// through errors.Is and carry the run error's text, while every sentinel
	// in the row's mustNotMatch list stays unreachable through errors.Is.
	verdictWrapped
)

// lockLostCase is one row of TestLockLostError's table. fixture builds the
// two contexts, since a canceled one cannot be written as a literal.
type lockLostCase struct {
	err     error
	fixture func(t *testing.T) (parent, holder context.Context)
	name    string
	// mustNotMatch lists sentinels the verdict must have flattened out of
	// reach; only verdictWrapped rows populate it.
	mustNotMatch []error
	want         lockLostVerdict
}

// liveHolder is the ordinary shape: an uncanceled parent and a holder derived
// from it that is still live, i.e. this run still holds the lock.
func liveHolder(t *testing.T) (context.Context, context.Context) {
	t.Helper()
	holder, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	return context.Background(), holder
}

// nilHolder is what Backend.Lock returns when nothing was acquired. The row
// is required, not defensive: without LockLostError's nil guard,
// context.Cause(nil) would panic.
func nilHolder(t *testing.T) (context.Context, context.Context) {
	t.Helper()
	return context.Background(), nil
}

// holderCanceledWith builds a fixture whose holder is already canceled with
// cause, against a live parent - the shape a backend produces the instant it
// learns something about the lock this run holds.
func holderCanceledWith(cause error) func(t *testing.T) (context.Context, context.Context) {
	return func(t *testing.T) (context.Context, context.Context) {
		t.Helper()
		holder, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		return context.Background(), holder
	}
}

// canceledParentWithLostHolder cancels the holder with a loss cause, then the
// parent, so the row pins the check order: a caller's own cancellation
// outranks a loss, or Ctrl-C would exit 8, not 130.
func canceledParentWithLostHolder(t *testing.T) (context.Context, context.Context) {
	t.Helper()
	parent, cancelParent := context.WithCancel(context.Background())
	holder, cancelHolder := context.WithCancelCause(parent)
	cancelHolder(helpers.ErrCacheLockLost)
	cancelParent()
	return parent, holder
}

// lockLostCases is TestLockLostError's table, built by a function rather than
// declared as a package-level var so it needs no gochecknoglobals exemption.
func lockLostCases() []lockLostCase {
	return []lockLostCase{
		{
			name:    "a nil holder is returned unchanged",
			fixture: nilHolder,
			err:     errRunFailed,
			want:    verdictUnchanged,
		},
		{
			name:    "a canceled parent outranks a genuinely lost holder",
			fixture: canceledParentWithLostHolder,
			err:     errRunFailed,
			want:    verdictUnchanged,
		},
		{
			name:    "a live holder is returned unchanged",
			fixture: liveHolder,
			err:     errRunFailed,
			want:    verdictUnchanged,
		},
		{
			// The local backend's holder is the caller's own context, so a
			// holder ended for any other reason must leave err as it was.
			name:    "a holder canceled for another reason is returned unchanged",
			fixture: holderCanceledWith(context.Canceled),
			err:     errRunFailed,
			want:    verdictUnchanged,
		},
		{
			// A run with no error of its own still fails: detection is late,
			// so its work was already done without exclusivity.
			name:    "a lost holder with no run error is the bare sentinel",
			fixture: holderCanceledWith(helpers.ErrCacheLockLost),
			err:     nil,
			want:    verdictBareSentinel,
		},
		{
			// The run failed because its holder was canceled, so err carries
			// context.Canceled; with %w, exitcode.FromError would classify a
			// stolen lock as an interrupt.
			name:         "a lost holder wraps the run error without leaving context.Canceled reachable",
			fixture:      holderCanceledWith(helpers.ErrCacheLockLost),
			err:          fmt.Errorf("save: %w", context.Canceled),
			mustNotMatch: []error{context.Canceled},
			want:         verdictWrapped,
		},
		{
			// Lock loss supersedes an integrity failure: the mismatch may be
			// the other holder rewriting the artifact underneath this run.
			name:         "a lost holder supersedes the run's own integrity failure",
			fixture:      holderCanceledWith(helpers.ErrCacheLockLost),
			err:          fmt.Errorf("install: %w", helpers.ErrSHA256Mismatch),
			mustNotMatch: []error{helpers.ErrSHA256Mismatch},
			want:         verdictWrapped,
		},
	}
}

func TestLockLostError(t *testing.T) {
	t.Parallel()
	for _, tt := range lockLostCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent, holder := tt.fixture(t)
			got := LockLostError(parent, holder, tt.err)
			switch tt.want {
			case verdictUnchanged:
				assertLockLostUnchanged(t, tt.err, got)
			case verdictBareSentinel:
				assertLockLostBareSentinel(t, got)
			case verdictWrapped:
				assertLockLostWrapped(t, tt, got)
			}
		})
	}
}

// assertLockLostUnchanged fails unless got is no lock-loss verdict and still
// matches want. The verdict check comes first: a spurious verdict also hides
// want, so only a check written first can name it.
func assertLockLostUnchanged(t *testing.T, want, got error) {
	t.Helper()
	if errors.Is(got, helpers.ErrCacheLockLost) {
		t.Fatalf("LockLostError = %v, want no lock-loss verdict", got)
	}
	if !errors.Is(got, want) {
		t.Fatalf("LockLostError = %v, want unchanged %v", got, want)
	}
}

// assertLockLostBareSentinel fails the test unless got is exactly the
// sentinel, with nothing wrapped around it: there was no run error to render,
// so anything else means a message was invented.
func assertLockLostBareSentinel(t *testing.T, got error) {
	t.Helper()
	if !errors.Is(got, helpers.ErrCacheLockLost) {
		t.Fatalf("LockLostError = %v, want errors.Is helpers.ErrCacheLockLost", got)
	}
	if got.Error() != helpers.ErrCacheLockLost.Error() {
		t.Fatalf("LockLostError = %q, want the bare sentinel %q", got.Error(), helpers.ErrCacheLockLost.Error())
	}
}

// assertLockLostWrapped fails the test unless got is a lock-loss verdict that
// carries the run error's text while leaving every sentinel the row listed
// unreachable through errors.Is.
func assertLockLostWrapped(t *testing.T, tt lockLostCase, got error) {
	t.Helper()
	if !errors.Is(got, helpers.ErrCacheLockLost) {
		t.Fatalf("LockLostError = %v, want errors.Is helpers.ErrCacheLockLost", got)
	}
	if !strings.Contains(got.Error(), tt.err.Error()) {
		t.Fatalf("LockLostError = %q, want it to carry the run error %q", got.Error(), tt.err.Error())
	}
	// This check alone pins %v over %w in LockLostError: under %w the
	// sentinel and message checks above still pass.
	for _, unreachable := range tt.mustNotMatch {
		if errors.Is(got, unreachable) {
			t.Fatalf("still matches %v: %v", unreachable, got)
		}
	}
}
