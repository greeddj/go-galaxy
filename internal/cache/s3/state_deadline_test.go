package s3

// These tests pin cacheManager.WithStateDeadline against this package's
// backend: its relation to the lock timings, and that it bounds a real
// LoadStore against a dripping GET.

import (
	"context"
	"errors"
	"testing"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// stateDeadlineDripBudget is the cacheManager.WithStateDeadline budget the drip
// test gives itself. It is what that test waits: the budget has to FIRE against
// a body that never ends, so it is small on purpose.
const stateDeadlineDripBudget = 300 * time.Millisecond

// stateDeadlineCompletionMargin is the budget the positive control's whole
// LoadStore must fit inside: a lazy Open's bucket HEAD and conditional-PUT
// probe, then the GET and inflate, so it exceeds the other completion margins.
const stateDeadlineCompletionMargin = 30 * time.Second

// TestStateObjectDeadlineFitsInsideTheLockTimings pins StateObjectDeadline <
// heartbeatInterval < lockTTL and 3*StateObjectDeadline < lockWaitCeiling, 3
// being how many state operations a locked install or cleanup performs.
func TestStateObjectDeadlineFitsInsideTheLockTimings(t *testing.T) {
	t.Parallel()
	if helpers.StateObjectDeadline >= heartbeatInterval {
		t.Fatalf("helpers.StateObjectDeadline (%s) must be less than heartbeatInterval (%s)",
			helpers.StateObjectDeadline, heartbeatInterval)
	}
	if heartbeatInterval >= lockTTL {
		t.Fatalf("heartbeatInterval (%s) must be less than lockTTL (%s)", heartbeatInterval, lockTTL)
	}
	if 3*helpers.StateObjectDeadline >= lockWaitCeiling {
		t.Fatalf("3*helpers.StateObjectDeadline (%s) must be less than lockWaitCeiling (%s)",
			3*helpers.StateObjectDeadline, lockWaitCeiling)
	}
}

// TestStateDeadlineBoundsADrippingSnapshotRead pins that WithStateDeadline
// turns a real LoadStore hung on a dripping GET into ErrStateObjectDeadline,
// which matches neither context sentinel.
func TestStateDeadlineBoundsADrippingSnapshotRead(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	putStoreObject(ctx, t, b, helpers.StoreSnapshotSchemaVersion, nil)
	fake.dripGet(b.key(statePrefix, storeObject), 5*time.Millisecond)

	wrapped := cacheManager.WithStateDeadline(b, stateDeadlineDripBudget)
	_, err := wrapped.LoadStore(ctx)
	if !errors.Is(err, helpers.ErrStateObjectDeadline) {
		t.Fatalf("LoadStore error = %v, want errors.Is ErrStateObjectDeadline", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("must not match context.DeadlineExceeded: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("must not match context.Canceled: %v", err)
	}
}

// TestStateDeadlineBoundsADrippingSnapshotReadPositiveControl pins that the
// same fake with no drip armed loads a real store through the same wrapper.
func TestStateDeadlineBoundsADrippingSnapshotReadPositiveControl(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	putStoreObject(ctx, t, b, helpers.StoreSnapshotSchemaVersion, nil)

	wrapped := cacheManager.WithStateDeadline(b, stateDeadlineCompletionMargin)
	st, err := wrapped.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if st == nil {
		t.Fatal("LoadStore returned a nil store with a nil error")
	}
}
