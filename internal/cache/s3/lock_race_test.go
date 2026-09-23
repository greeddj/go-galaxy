package s3

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// lockRaceIterations is a budget for hitting the release/tick window, not
	// a duration: each cycle is one heartbeat interval plus a few round trips.
	lockRaceIterations = 300
	// lockRaceSpreadSteps and lockRaceSpreadUnit sweep release across the
	// window between a heartbeat HEAD leaving the fake and the tick acting on
	// it; a machine outside that range loses detections, which fails loudly.
	lockRaceSpreadSteps = 48
	lockRaceSpreadUnit  = 2 * time.Microsecond
	// lockRaceEventCeiling is a LIVENESS bound on waiting for a heartbeat
	// HEAD, matching lockEventWaitCeiling's own contract: a slow machine only
	// makes this slower, never wrong.
	lockRaceEventCeiling = 2 * time.Second
)

// lockRaceTiming uses a 1ms heartbeat so the cycles stay fast, and a short
// ttl so a cycle that leaves a live lock object is reclaimed by the next one
// instead of stalling it until the wait ceiling.
func lockRaceTiming() lockTiming {
	return lockTiming{
		ttl:                100 * time.Millisecond,
		heartbeatInterval:  time.Millisecond,
		heartbeatOpTimeout: 200 * time.Millisecond,
		releaseTimeout:     200 * time.Millisecond,
		waitCeiling:        5 * time.Second,
		backoffBase:        time.Millisecond,
		backoffCap:         5 * time.Millisecond,
	}
}

// TestReleaseRacingTheTickKeepsTheLossCause pins that release joins the
// heartbeat before canceling the holder context: whenever release reports
// errS3LockLost the cause must be ErrCacheLockLost, and some cycle must detect.
func TestReleaseRacingTheTickKeepsTheLossCause(t *testing.T) {
	t.Parallel()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	b.lock = lockRaceTiming()
	key := b.key(locksPrefix, lockObject)

	detections := 0
	for cycle := range lockRaceIterations {
		if lockRaceCycle(t, b, fake, key, cycle) {
			detections++
		}
	}
	if detections == 0 {
		t.Fatalf("no cycle of %d reported a lost lock; the implication above held vacuously", lockRaceIterations)
	}
}

// lockRaceCycle runs one acquire, steal, release cycle and reports whether
// release detected the loss. It fails the test only on the implication this
// file exists for, or on a fixture step that did not do what it was asked.
func lockRaceCycle(t *testing.T, b *Backend, fake *fakeS3, key string, cycle int) bool {
	t.Helper()
	ctx := context.Background()
	holderCtx, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("cycle %d: Lock: %v", cycle, err)
	}
	// A foreign token with an elapsed deadline: the next heartbeat declares
	// the loss, and the next cycle can reclaim what this one leaves behind.
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(-b.lock.ttl), putCondition{}); err != nil {
		t.Fatalf("cycle %d: seed foreign takeover: %v", cycle, err)
	}
	waitForHeartbeatHead(t, fake, key, cycle, holderCtx.Err)
	spinFor(time.Duration(cycle%lockRaceSpreadSteps) * lockRaceSpreadUnit)

	if !errors.Is(release(), errS3LockLost) {
		return false
	}
	if cause := context.Cause(holderCtx); !errors.Is(cause, helpers.ErrCacheLockLost) {
		t.Fatalf("cycle %d: release reported the lock lost, context.Cause(holderCtx) = %v", cycle, cause)
	}
	return true
}

// waitForHeartbeatHead busy-polls until the fake serves a new heartbeat HEAD,
// or until the holder context ends, as the detecting tick's HEAD may predate
// the baseline; a sleep would step over the microsecond window.
func waitForHeartbeatHead(t *testing.T, fake *fakeS3, key string, cycle int, holderErr func() error) {
	t.Helper()
	before := fake.requestCount(key, http.MethodHead)
	deadline := time.Now().Add(lockRaceEventCeiling)
	for {
		if fake.requestCount(key, http.MethodHead) > before || holderErr() != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cycle %d: no heartbeat HEAD within %v", cycle, lockRaceEventCeiling)
		}
		runtime.Gosched()
	}
}

// spinFor busy-waits for d, yielding rather than sleeping: microsecond waits
// are below timer resolution, and yielding keeps the heartbeat runnable.
func spinFor(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		runtime.Gosched()
	}
}
