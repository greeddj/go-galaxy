package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"sync/atomic"
	"testing"
	"time"
)

// freshCreateCutShortBudget is the failure row's budget: wide enough for the
// create PUT to land, and then the whole time the fixture holds the ownership
// check open.
const freshCreateCutShortBudget = 300 * time.Millisecond

// freshCreateCompletionMargin is the control row's deadline, separate from the
// budget above because a value that must fire and one a round trip must fit
// inside are opposite requirements.
const freshCreateCompletionMargin = 10 * time.Second

// orphanRowClient returns one orphan-cleanup row's client. The control row
// uses answeredDespiteCancelTransport so no clock can cut it short; the blocked
// row keeps the plain client, as its budget firing is the point.
func orphanRowClient(srv *httptest.Server, blockPostPutHead bool) *http.Client {
	// A copy of the server's own client rather than a bare http.Client, so
	// whatever else httptest configured on it survives the wrapping.
	client := *srv.Client()
	if !blockPostPutHead {
		client.Transport = answeredDespiteCancelTransport{base: srv.Client().Transport}
	}
	return &client
}

// TestFreshCreateAbandonsALockItNeverHeld pins that an acquisition failing
// after its create-if-absent PUT landed deletes that object rather than leaving
// an orphan; the control row shows the same fixture does grant the lock.
func TestFreshCreateAbandonsALockItNeverHeld(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		blockPostPutHead bool
		wantRelease      bool
	}{
		{name: "a budget that fits the create but not the ownership check", blockPostPutHead: true},
		{name: "the same fixture with nothing cutting the acquisition short", wantRelease: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertFreshCreateOrphanCleanup(t, tc.blockPostPutHead, tc.wantRelease)
		})
	}
}

// assertFreshCreateOrphanCleanup runs one row against a fake S3 server,
// calling tryAcquireOnce directly because Lock would retry past the failure.
func assertFreshCreateOrphanCleanup(t *testing.T, blockPostPutHead, wantRelease bool) {
	t.Helper()
	key := path.Join(locksPrefix, lockObject)
	fake := newFakeS3()
	lockPath := "/" + fake.bucket + "/" + key
	// The blocked row is the one whose budget must fire; the control row's has
	// to fit a whole acquisition inside it instead. See
	// freshCreateCompletionMargin.
	budget := freshCreateCompletionMargin
	if blockPostPutHead {
		budget = freshCreateCutShortBudget
	}
	opCtx, cancel := context.WithTimeout(context.Background(), budget)
	t.Cleanup(cancel)

	var putsServed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The ownership check that follows the create PUT is held until the
		// caller's budget is gone, so the failure row fails on that budget every
		// time instead of racing a loopback round trip it would win.
		if blockPostPutHead && r.Method == http.MethodHead && r.URL.Path == lockPath && putsServed.Load() > 0 {
			<-opCtx.Done()
		}
		fake.ServeHTTP(w, r)
		if r.Method == http.MethodPut && r.URL.Path == lockPath {
			putsServed.Add(1)
		}
	}))
	t.Cleanup(srv.Close)

	b := newLockBackendAt(t, srv.URL, orphanRowClient(srv, blockPostPutHead), testLockTiming(time.Minute))
	// Open writes only its probe key, so putsServed counts the create below
	// alone: it is the first PUT the lock key sees.
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	attempt, err := b.tryAcquireOnce(opCtx, key, ourToken, testHolderCancel(t))
	if attempt.release != nil {
		t.Cleanup(func() { _ = attempt.release() })
	}
	assertFreshCreateOutcome(t, wantRelease, attempt, err)
	assertFreshCreateObject(t, wantRelease, b, key)
}

// assertFreshCreateOutcome checks the attempt and the error one row expects.
func assertFreshCreateOutcome(t *testing.T, wantRelease bool, attempt lockAttempt, err error) {
	t.Helper()
	if got := attempt.release != nil; got != wantRelease {
		t.Fatalf("attempt holds the lock = %v, want %v (error: %v)", got, wantRelease, err)
	}
	// The first arm is documentary: lockAttempt forbids a release with an
	// error. The second catches a failure row that reported nothing wrong.
	switch {
	case wantRelease && err != nil:
		t.Fatalf("tryAcquireOnce: %v", err)
	case !wantRelease && err == nil:
		t.Fatalf("tryAcquireOnce reported no error for an acquisition cut short after its create PUT")
	}
}

// assertFreshCreateObject checks what the lock object holds once the attempt is
// over: nothing at all for a row whose acquisition was cut short, and this run's
// own token for the control.
func assertFreshCreateObject(t *testing.T, wantRelease bool, b *Backend, key string) {
	t.Helper()
	headers, err := b.client.headObject(context.Background(), key)
	if !wantRelease {
		if !errors.Is(err, errS3NotFound) {
			t.Fatalf("lock object after an abandoned acquisition: headObject err = %v, want errS3NotFound", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("headObject after the acquisition: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got != ourToken {
		t.Fatalf("lock object token after the acquisition = %q, want %q", got, ourToken)
	}
}

// TestConditionalConflictIsRetriedNotFatal pins that a 409 on either the
// create or the swap PUT is backed off and retried; the PUT count proves the
// conflict was served and retried, not merely that the lock was taken.
func TestConditionalConflictIsRetriedNotFatal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		onReclaim bool
		minPuts   int
	}{
		{name: "a conflict answering the create is retried", minPuts: 2},
		{name: "a conflict answering the swap is retried", onReclaim: true, minPuts: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertConditionalConflictRetried(t, tc.onReclaim, tc.minPuts)
		})
	}
}

// assertConditionalConflictRetried arms a single 409 against whichever
// conditional write the row names, then requires the acquisition to complete
// anyway and to have issued more PUTs than an unobstructed one would.
func assertConditionalConflictRetried(t *testing.T, onReclaim bool, minPuts int) {
	t.Helper()
	ctx := context.Background()
	fake := newFakeS3()
	key := path.Join(locksPrefix, lockObject)
	lockPath := "/" + fake.bucket + "/" + key

	var heads atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.ServeHTTP(w, r)
		// The swap is the first PUT after the reclaim's own HEAD, so arming the
		// conflict behind that HEAD lands it on the swap rather than on the
		// create-if-absent PUT that precedes both.
		if onReclaim && r.Method == http.MethodHead && r.URL.Path == lockPath && heads.Add(1) == 1 {
			fake.failNext(key, http.MethodPut, http.StatusConflict, 1)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	b := newLockBackendAt(t, srv.URL, srv.Client(), testLockTiming(time.Minute))
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if onReclaim {
		// An expired holder, so the acquisition reaches the swap at all.
		fake.storeLockObject(key, foreignToken, time.Now().UTC().Add(-time.Hour))
	} else {
		fake.failNext(key, http.MethodPut, http.StatusConflict, 1)
	}

	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() { _ = release() })
	if got := fake.requestCount(key, http.MethodPut); got < minPuts {
		t.Fatalf("PUT count on the lock key = %d, want at least %d: the conflict was never served or never retried", got, minPuts)
	}
}
