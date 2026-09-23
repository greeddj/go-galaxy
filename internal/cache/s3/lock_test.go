package s3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// foreignToken stands in for a lock token belonging to some other acquirer
// across the tests that seed a lock object out-of-band to simulate
// contention, reclaim, or takeover scenarios.
const foreignToken = "foreign-token"

// errRawInFlightPlaceholder stands in for an in-flight transport failure in
// TestWaitCeilingErrClassification; waitCeilingErr only checks it is non-nil.
var errRawInFlightPlaceholder = errors.New("raw in-flight transport failure")

// testLockTiming returns fast lock timing with the given ttl. waitCeiling is a
// liveness margin a test asserting it fires overrides; releaseTimeout stays
// short because abandonLockObject spends all of it on a silent endpoint.
func testLockTiming(ttl time.Duration) lockTiming {
	return lockTiming{
		ttl:                ttl,
		heartbeatInterval:  30 * time.Millisecond,
		heartbeatOpTimeout: 200 * time.Millisecond,
		releaseTimeout:     200 * time.Millisecond,
		waitCeiling:        10 * time.Second,
		backoffBase:        10 * time.Millisecond,
		backoffCap:         50 * time.Millisecond,
	}
}

// lockEventWaitCeiling is a liveness ceiling for waitForLockEvent: a slow
// machine makes those tests slower, never wrong.
const lockEventWaitCeiling = 10 * time.Second

// lockEventPollInterval is how often waitForLockEvent re-checks its
// condition while waiting.
const lockEventPollInterval = 2 * time.Millisecond

// waitForLockEvent polls cond until it holds, failing the test after
// lockEventWaitCeiling, so a test waits on an observable lock event instead
// of sleeping a number of heartbeat intervals.
func waitForLockEvent(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(lockEventWaitCeiling)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", lockEventWaitCeiling, what)
		}
		time.Sleep(lockEventPollInterval)
	}
}

// newLockFake starts a fake S3 server and returns its endpoint and client,
// so multiple Backends can be built against the same in-memory bucket to
// exercise cross-backend lock contention.
func newLockFake(t *testing.T) (string, *http.Client) {
	t.Helper()
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return srv.URL, srv.Client()
}

// newLockBackendAt builds a Backend pointed at an existing fake S3 endpoint
// (as returned by newLockFake), with timing set to timing.
func newLockBackendAt(t *testing.T, endpoint string, client *http.Client, timing lockTiming) *Backend {
	t.Helper()
	cfg := config.S3CacheConfig{
		Endpoint:  endpoint,
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	b, err := New(cfg, client, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b.lock = timing
	return b
}

// newLockBackendWithFake starts a fresh fake S3 server and returns a Backend
// on it plus the fake, for test-only knobs such as deleteDelay.
func newLockBackendWithFake(t *testing.T, timing lockTiming) (*Backend, *fakeS3) {
	t.Helper()
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return newLockBackendAt(t, srv.URL, srv.Client(), timing), fake
}

// testHolderCancel returns a holder-context cancel shaped like acquireLock's,
// for tests that call one acquisition step directly; the context itself is
// discarded and t.Cleanup cancels it.
func testHolderCancel(t *testing.T) context.CancelCauseFunc {
	t.Helper()
	_, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	return cancel
}

// seedLockObject writes the lock object under foreignToken with the given
// deadline, bypassing acquireLock, to stage another acquirer's lock.
func seedLockObject(ctx context.Context, t *testing.T, b *Backend, deadline time.Time) {
	t.Helper()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	key := b.key(locksPrefix, lockObject)
	if err := b.putLock(ctx, key, foreignToken, deadline, putCondition{}); err != nil {
		t.Fatalf("seed lock object: %v", err)
	}
}

// TestLockAcquiresOnEmptyBucket confirms a fresh bucket lets Lock succeed
// immediately and stamps the wire format's token and deadline metadata.
func TestLockAcquiresOnEmptyBucket(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(time.Minute)
	ctx := context.Background()

	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	key := b.key(locksPrefix, lockObject)
	headers, err := b.client.headObject(ctx, key)
	if err != nil {
		t.Fatalf("headObject: %v", err)
	}
	if token := headers.Get("X-Amz-Meta-Token"); token == "" {
		t.Fatalf("expected a non-empty lock token")
	}
	deadline, err := time.Parse(time.RFC3339, headers.Get("X-Amz-Meta-Deadline"))
	if err != nil {
		t.Fatalf("parse deadline: %v", err)
	}
	if !deadline.After(time.Now()) {
		t.Fatalf("expected a future deadline, got %v", deadline)
	}
}

// TestLockConcurrentFreshAcquireSingleWinner pins that two Backends racing for
// an absent lock yield one winner and one errS3LockWaitTimeout. The winner
// releases only after both return, or the loser could then acquire it.
func TestLockConcurrentFreshAcquireSingleWinner(t *testing.T) {
	t.Parallel()
	endpoint, client := newLockFake(t)
	// A copy of the fake's own client rather than a bare http.Client, so
	// whatever else httptest configured on it survives the wrapping.
	answering := *client
	answering.Transport = answeredDespiteCancelTransport{base: client.Transport}

	timing := testLockTiming(contendedLockHoldTTL)
	timing.waitCeiling = 300 * time.Millisecond
	b1 := newLockBackendAt(t, endpoint, &answering, timing)
	b2 := newLockBackendAt(t, endpoint, &answering, timing)

	type lockResult struct {
		release func() error
		err     error
	}
	results := make([]lockResult, 2)
	var wg sync.WaitGroup
	for i, b := range []*Backend{b1, b2} {
		wg.Go(func() {
			_, release, err := b.Lock(context.Background())
			results[i] = lockResult{release: release, err: err}
		})
	}
	wg.Wait()

	var successes, timeouts, otherErrs int
	for _, r := range results {
		switch {
		case r.err == nil:
			successes++
			_ = r.release()
		case errors.Is(r.err, errS3LockWaitTimeout):
			timeouts++
		default:
			otherErrs++
		}
	}

	if successes != 1 {
		t.Fatalf("expected exactly one winner, got %d successes (timeouts=%d, other=%d)", successes, timeouts, otherErrs)
	}
	if timeouts != 1 {
		t.Fatalf("expected exactly one timeout, got %d (successes=%d, other=%d)", timeouts, successes, otherErrs)
	}
}

// TestLockReclaimsExpiredLock pins that Lock takes over a foreign lock whose
// deadline has passed: the token becomes ours and the deadline moves forward.
// It compares with the hour-old seeded deadline, as RFC3339 keeps only seconds.
func TestLockReclaimsExpiredLock(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(20 * time.Millisecond)
	ctx := context.Background()

	pastDeadline := time.Now().UTC().Add(-time.Hour)
	seedLockObject(ctx, t, b, pastDeadline)

	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() { _ = release() })

	key := b.key(locksPrefix, lockObject)
	headers, err := b.client.headObject(ctx, key)
	if err != nil {
		t.Fatalf("headObject: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got == foreignToken {
		t.Fatalf("expected the reclaimer's token, still foreign-token")
	}
	deadline, err := time.Parse(time.RFC3339, headers.Get("X-Amz-Meta-Deadline"))
	if err != nil {
		t.Fatalf("parse deadline: %v", err)
	}
	if !deadline.After(pastDeadline) {
		t.Fatalf("expected the reclaim to move the deadline forward from %v, got %v", pastDeadline, deadline)
	}
}

// TestLockWaitsThenTimesOutOnLiveLock pins that a live foreign holder makes
// Lock back off at least once and then fail with errS3LockWaitTimeout, which
// carries helpers.ErrCacheBusy.
func TestLockWaitsThenTimesOutOnLiveLock(t *testing.T) {
	t.Parallel()
	b := newContendedLockBackend(t, 150*time.Millisecond)
	ctx := context.Background()

	start := time.Now()
	_, _, err := b.Lock(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, errS3LockWaitTimeout) {
		t.Fatalf("expected errS3LockWaitTimeout, got %v", err)
	}
	if elapsed < b.lock.backoffBase {
		t.Fatalf("expected at least one backoff sleep before timing out, elapsed %v", elapsed)
	}
	// A live foreign lock outlasting the ceiling is contention (exit 8);
	// TestLockAcquirePropagatesCallerCancellation is the negative control.
	if !errors.Is(err, helpers.ErrCacheBusy) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBusy), got %v", err)
	}
}

// answeredDespiteCancelTransport strips cancellation from each request, so a
// round trip the wait ceiling interrupts still records its observation. Only
// for fixtures that answer every request: on a hanging one it hangs the test.
type answeredDespiteCancelTransport struct{ base http.RoundTripper }

// RoundTrip forwards req on a context stripped of cancellation and deadline.
// The type's own doc comment holds why, and where this must not be used.
func (t answeredDespiteCancelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req.Clone(context.WithoutCancel(req.Context())))
}

// contendedLockHoldTTL is the seeded foreign holder's TTL: an hour, so
// lockExpired can never read that holder as expired while a test runs,
// however slow the machine.
const contendedLockHoldTTL = time.Hour

// newContendedLockBackend builds a Backend whose lock a live foreign acquirer
// holds, with the given waitCeiling, over answeredDespiteCancelTransport so an
// acquisition always observes that holder before the ceiling fires.
func newContendedLockBackend(t *testing.T, waitCeiling time.Duration) *Backend {
	t.Helper()
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	// A copy of the server's own client rather than a bare http.Client, so
	// whatever else httptest configured on it survives the wrapping.
	client := *srv.Client()
	client.Transport = answeredDespiteCancelTransport{base: srv.Client().Transport}

	timing := testLockTiming(contendedLockHoldTTL)
	timing.waitCeiling = waitCeiling
	b := newLockBackendAt(t, srv.URL, &client, timing)
	seedLockObject(context.Background(), t, b, time.Now().UTC().Add(timing.ttl))
	return b
}

// newSilentEndpointBackend builds a Backend on a listener that never answers.
// It skips Open, whose bucket and probe calls run before the wait ceiling
// exists and would hang the test.
func newSilentEndpointBackend(t *testing.T, waitCeiling time.Duration) *Backend {
	t.Helper()
	ln, _ := newAcceptingNeverRespondingListener(t)
	cfg := config.S3CacheConfig{
		Endpoint:  "http://" + ln.Addr().String(),
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	client, err := newClient(cfg, http.DefaultClient)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	timing := testLockTiming(time.Minute)
	timing.waitCeiling = waitCeiling
	return &Backend{client: client, lock: timing}
}

// TestLockWaitCeilingDistinguishesSilentBackendFromContention pins that under
// one wait ceiling a silent endpoint is helpers.ErrCacheBackendUnavailable,
// never ErrCacheBusy, while a live foreign lock is ErrCacheBusy.
func TestLockWaitCeilingDistinguishesSilentBackendFromContention(t *testing.T) {
	t.Parallel()

	const waitCeiling = 200 * time.Millisecond

	tests := []struct {
		build    func(t *testing.T) *Backend
		name     string
		wantBusy bool
	}{
		{
			name: "silent endpoint never answers a request",
			build: func(t *testing.T) *Backend {
				t.Helper()
				return newSilentEndpointBackend(t, waitCeiling)
			},
			wantBusy: false,
		},
		{
			name: "live foreign lock outlasts the ceiling",
			build: func(t *testing.T) *Backend {
				t.Helper()
				return newContendedLockBackend(t, waitCeiling)
			},
			wantBusy: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := tc.build(t)

			_, _, err := b.Lock(context.Background())
			if err == nil {
				t.Fatalf("expected Lock to fail, got nil")
			}

			if got := errors.Is(err, helpers.ErrCacheBusy); got != tc.wantBusy {
				t.Fatalf("errors.Is(err, helpers.ErrCacheBusy) = %v, want %v (err: %v)", got, tc.wantBusy, err)
			}
			if tc.wantBusy {
				return
			}
			if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
				t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
			}
		})
	}
}

// answeredWhileUngatedTransport strips cancellation only until gated, the
// fixture's own counter, turns non-zero; later requests keep the caller's
// context, the only thing that ends a request the fixture hangs.
type answeredWhileUngatedTransport struct {
	base  http.RoundTripper
	gated *atomic.Int32
}

// RoundTrip strips cancellation only while gated still reports zero. The type's
// own doc comment holds the predicate and why it is that way round.
func (t answeredWhileUngatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.gated.Load() > 0 {
		return t.base.RoundTrip(req)
	}
	return t.base.RoundTrip(req.Clone(context.WithoutCancel(req.Context())))
}

// TestLockAcquireTimesOutAfterObservationThenSilence pins that an observation
// counts for the whole wait: the first HEAD sees a live holder, every later
// one hangs, and Lock still fails with errS3LockWaitTimeout.
func TestLockAcquireTimesOutAfterObservationThenSilence(t *testing.T) {
	t.Parallel()

	fake := newFakeS3()
	key := path.Join(locksPrefix, lockObject)
	lockPath := "/" + fake.bucket + "/" + key

	var headCount atomic.Int32
	// hangGate parks every HEAD on the lock key after the first. Cleanup closes
	// it before the server, whose Close would otherwise block on the parked
	// handler.
	hangGate := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == lockPath {
			if headCount.Add(1) == 1 {
				fake.ServeHTTP(w, r)
				return
			}
			select {
			case <-r.Context().Done():
				http.Error(w, r.Context().Err().Error(), http.StatusGatewayTimeout)
			case <-hangGate:
			}
			return
		}
		fake.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		close(hangGate)
		srv.Close()
	})

	timing := testLockTiming(5 * time.Second) // stays live for the whole test window
	timing.waitCeiling = 300 * time.Millisecond
	// The transport reads the same headCount the handler writes, so which
	// request is "the one this fixture answers" has a single definition.
	client := *srv.Client()
	client.Transport = answeredWhileUngatedTransport{base: srv.Client().Transport, gated: &headCount}
	b := newLockBackendAt(t, srv.URL, &client, timing)

	seedLockObject(context.Background(), t, b, time.Now().UTC().Add(b.lock.ttl))

	_, _, err := b.Lock(context.Background())
	if !errors.Is(err, errS3LockWaitTimeout) {
		t.Fatalf("expected errS3LockWaitTimeout, got %v", err)
	}
	// lockPath assumes an empty backend prefix; were it to diverge, no HEAD would
	// be gated and the assertion above would pass for the wrong reason.
	if got := headCount.Load(); got < 2 {
		t.Fatalf("HEAD count on the lock key = %d, want at least 2: the gate never matched, so no attempt was ever silenced", got)
	}
}

// TestLockAcquireSurfacesAHardFailureOverTheCeiling pins that a HEAD failure
// while the wait ceiling is live ends Lock with errS3HeadFailed, even after a
// live holder was observed, never as a wait-ceiling outcome.
func TestLockAcquireSurfacesAHardFailureOverTheCeiling(t *testing.T) {
	t.Parallel()

	fake := newFakeS3()
	key := path.Join(locksPrefix, lockObject)
	lockPath := "/" + fake.bucket + "/" + key

	var headCount atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == lockPath && headCount.Add(1) > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fake.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	timing := testLockTiming(30 * time.Second) // stays live for the whole test window
	timing.waitCeiling = 5 * time.Second
	b := newLockBackendAt(t, srv.URL, srv.Client(), timing)

	seedLockObject(context.Background(), t, b, time.Now().UTC().Add(b.lock.ttl))

	_, _, err := b.Lock(context.Background())
	if !errors.Is(err, errS3HeadFailed) {
		t.Fatalf("expected errS3HeadFailed, got %v", err)
	}
	// Documentary: errS3HeadFailed and the wait-ceiling sentinels are disjoint,
	// so this names the confusion the test rules out rather than pinning it.
	if errors.Is(err, errS3LockWaitTimeout) || errors.Is(err, errS3LockWaitNoHolderObserved) {
		t.Fatalf("a hard failure must not be reclassified as a wait-ceiling outcome, got %v", err)
	}
	// Same reasoning as its sibling above: without this, a diverged lockPath
	// would leave every HEAD served normally, and the run would fail on
	// something other than the branch under test.
	if got := headCount.Load(); got < 2 {
		t.Fatalf("HEAD count on the lock key = %d, want at least 2: the gate never matched, so no attempt ever failed hard", got)
	}
}

// TestLockAcquirePropagatesCallerCancellation pins that canceling the
// caller's context during contention surfaces context.Canceled, not
// errS3LockWaitTimeout; ceiling and ttl are too long to fire first.
func TestLockAcquirePropagatesCallerCancellation(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	timing := testLockTiming(10 * time.Second)
	timing.waitCeiling = 10 * time.Second
	b.lock = timing

	seedLockObject(context.Background(), t, b, time.Now().UTC().Add(b.lock.ttl))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := b.Lock(ctx)
		errCh <- err
	}()

	time.Sleep(2 * b.lock.backoffBase)
	cancel()

	var err error
	select {
	case err = <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Lock did not return after caller cancellation")
	}

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected errors.Is(err, context.Canceled), got %v", err)
	}
	if errors.Is(err, errS3LockWaitTimeout) {
		t.Fatalf("expected the caller cancellation, not errS3LockWaitTimeout, got %v", err)
	}
	// Negative control for TestLockWaitsThenTimesOutOnLiveLock: a caller
	// cancellation must not read as contention (helpers.ErrCacheBusy).
	if errors.Is(err, helpers.ErrCacheBusy) {
		t.Fatalf("expected the caller cancellation, not helpers.ErrCacheBusy, got %v", err)
	}
}

// TestWaitCeilingErrClassification pins waitCeilingErr's order: a canceled
// parent wins, then observed decides busy versus unavailable whatever
// inFlight holds, and inFlight is only a diagnostic cause.
func TestWaitCeilingErrClassification(t *testing.T) {
	t.Parallel()

	canceledParent, cancel := context.WithCancel(context.Background())
	cancel()

	// noHolderSynthetic wraps context.Canceled, a shape no producer builds, to
	// pin that waitCeilingErr's %v rendering hides whatever inFlight wraps.
	noHolderSynthetic := fmt.Errorf("synthetic in-flight failure: %w", context.Canceled)

	tests := []waitCeilingErrCase{
		{
			name:         "a canceled parent wins even over an observed holder",
			parent:       canceledParent,
			observed:     true,
			wantCanceled: true,
		},
		{
			name:     "an observed holder is contention regardless of a non-nil inFlight",
			parent:   context.Background(),
			observed: true,
			inFlight: errRawInFlightPlaceholder,
			wantBusy: true,
		},
		{
			name:            "no observed holder with a non-nil cause is unavailable",
			parent:          context.Background(),
			observed:        false,
			inFlight:        errRawInFlightPlaceholder,
			wantUnavailable: true,
		},
		{
			name:            "a synthetic cause wrapping context.Canceled still classifies as unavailable, not canceled",
			parent:          context.Background(),
			observed:        false,
			inFlight:        noHolderSynthetic,
			wantUnavailable: true,
		},
		{
			name:            "no observed holder with a nil cause is still unavailable",
			parent:          context.Background(),
			observed:        false,
			inFlight:        nil,
			wantUnavailable: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertWaitCeilingErrClassification(t, tc)
		})
	}
}

// waitCeilingErrCase is one row of TestWaitCeilingErrClassification's table.
type waitCeilingErrCase struct {
	parent          context.Context //nolint:containedctx // a direct table-driven parameter to waitCeilingErr, not a stored request context.
	inFlight        error
	name            string
	observed        bool
	wantBusy        bool
	wantUnavailable bool
	wantCanceled    bool
}

// assertWaitCeilingErrClassification calls waitCeilingErr with one row's
// parameters and checks the result's busy, unavailable and canceled classes.
func assertWaitCeilingErrClassification(t *testing.T, tc waitCeilingErrCase) {
	t.Helper()
	err := waitCeilingErr(tc.parent, tc.observed, tc.inFlight)
	if err == nil {
		t.Fatalf("expected a non-nil error")
	}
	if got := errors.Is(err, helpers.ErrCacheBusy); got != tc.wantBusy {
		t.Errorf("errors.Is(err, helpers.ErrCacheBusy) = %v, want %v (err: %v)", got, tc.wantBusy, err)
	}
	if got := errors.Is(err, helpers.ErrCacheBackendUnavailable); got != tc.wantUnavailable {
		t.Errorf("errors.Is(err, helpers.ErrCacheBackendUnavailable) = %v, want %v (err: %v)", got, tc.wantUnavailable, err)
	}
	if got := errors.Is(err, context.Canceled); got != tc.wantCanceled {
		t.Errorf("errors.Is(err, context.Canceled) = %v, want %v (err: %v)", got, tc.wantCanceled, err)
	}
}

// TestHeartbeatRefreshesDeadline pins that the heartbeat advances the lock's
// deadline on its own. It rolls the deadline an hour back under our token
// and waits for a refresh, which RFC3339's second resolution cannot hide.
func TestHeartbeatRefreshesDeadline(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(10 * time.Second)
	ctx := context.Background()

	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	readLock := func() (string, time.Time) {
		t.Helper()
		headers, err := b.client.headObject(ctx, key)
		if err != nil {
			t.Fatalf("headObject: %v", err)
		}
		deadline, err := time.Parse(time.RFC3339, headers.Get("X-Amz-Meta-Deadline"))
		if err != nil {
			t.Fatalf("parse deadline: %v", err)
		}
		return headers.Get("X-Amz-Meta-Token"), deadline
	}

	token, acquiredDeadline := readLock()
	// Roll the deadline an hour back with the heartbeat's own same-token write,
	// so the refresh is visible without crossing a second boundary.
	rolledBack := time.Now().UTC().Add(-time.Hour)
	if err := b.putLock(ctx, key, token, rolledBack, putCondition{}); err != nil {
		t.Fatalf("roll the deadline back: %v", err)
	}

	var refreshed time.Time
	waitForLockEvent(t, "the heartbeat to advance the rolled-back deadline", func() bool {
		holder, deadline := readLock()
		if holder != token {
			t.Fatalf("expected the holder token to stay %q, got %q", token, holder)
		}
		refreshed = deadline
		return deadline.After(rolledBack)
	})
	// The refresh writes a full ttl ahead of its own now, so it can never
	// land before the deadline the acquisition recorded.
	if refreshed.Before(acquiredDeadline) {
		t.Fatalf("expected the refresh to be at least the acquired deadline (%v), got %v", acquiredDeadline, refreshed)
	}

	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// TestHeartbeatDetectsLostOwnershipAndReleaseSkipsDelete pins that after a
// takeover release reports errS3LockLost and keeps the foreign object. It
// waits on the holder context, since the fake counts a HEAD before it is read.
func TestHeartbeatDetectsLostOwnershipAndReleaseSkipsDelete(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(10 * time.Second)
	ctx := context.Background()

	holderCtx, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	// Simulate a foreign acquirer reclaiming the lock out-of-band (e.g.
	// after this holder stalled past its TTL from S3's point of view).
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(b.lock.ttl), putCondition{}); err != nil {
		t.Fatalf("seed foreign takeover: %v", err)
	}

	select {
	case <-holderCtx.Done():
	case <-time.After(lockEventWaitCeiling):
		t.Fatalf("the heartbeat did not observe the takeover within %v; if holderCancel was removed, "+
			"TestLockHolderContextCanceledWhenOwnershipLost is the test that owns that fact", lockEventWaitCeiling)
	}

	if err := release(); !errors.Is(err, errS3LockLost) {
		t.Fatalf("expected errS3LockLost, got %v", err)
	}

	headers, err := b.client.headObject(ctx, key)
	if err != nil {
		t.Fatalf("headObject: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got != foreignToken {
		t.Fatalf("expected the foreign lock to remain in place, got token %q", got)
	}
}

// TestLockHolderContextCanceledWhenOwnershipLost pins that a takeover closes
// the holder context with a helpers.ErrCacheLockLost cause, so the run stops
// writing; TestLockHolderContextStaysLiveWhileOwned is its positive control.
func TestLockHolderContextCanceledWhenOwnershipLost(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(10 * time.Second)
	ctx := context.Background()

	holderCtx, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(b.lock.ttl), putCondition{}); err != nil {
		t.Fatalf("seed foreign takeover: %v", err)
	}

	// The holder context closing is the event under test, so wait on it
	// directly; the ceiling is only a liveness bound.
	select {
	case <-holderCtx.Done():
	case <-time.After(lockEventWaitCeiling):
		t.Fatalf("holder context still live %v after a takeover; the run would keep writing without the lock", lockEventWaitCeiling)
	}
	if cause := context.Cause(holderCtx); !errors.Is(cause, helpers.ErrCacheLockLost) {
		t.Fatalf("context.Cause(holderCtx) = %v, want errors.Is helpers.ErrCacheLockLost", cause)
	}

	assertReleaseReportsLossAndKeepsForeignLock(t, b, release, key)
}

// assertReleaseReportsLossAndKeepsForeignLock asserts that release reports
// errS3LockLost and leaves the foreign holder's lock object in place.
func assertReleaseReportsLossAndKeepsForeignLock(t *testing.T, b *Backend, release func() error, key string) {
	t.Helper()
	if err := release(); !errors.Is(err, errS3LockLost) {
		t.Fatalf("release = %v, want errS3LockLost", err)
	}
	headers, err := b.client.headObject(context.Background(), key)
	if err != nil {
		t.Fatalf("headObject: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got != foreignToken {
		t.Fatalf("expected the foreign lock to remain in place, got token %q", got)
	}
}

// TestLockHolderContextStaysLiveWhileOwned pins that the holder context stays
// live across heartbeat ticks while the lock is held, and that a clean
// release ends it with plain cancellation, never a lock-loss cause.
func TestLockHolderContextStaysLiveWhileOwned(t *testing.T) {
	t.Parallel()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	b.lock = testLockTiming(10 * time.Second)
	ctx := context.Background()

	holderCtx, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	putsAtAcquire := fake.requestCount(key, http.MethodPut)
	// Three refresh PUTs the fake served prove three whole heartbeat ticks ran
	// under our token, the window a takeover would have been noticed in.
	waitForLockEvent(t, "three heartbeat refresh PUTs under our own token", func() bool {
		return fake.requestCount(key, http.MethodPut) >= putsAtAcquire+3
	})
	if err := holderCtx.Err(); err != nil {
		t.Fatalf("holderCtx.Err() = %v, want nil while this run still holds the lock", err)
	}

	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := b.client.headObject(ctx, key); !errors.Is(err, errS3NotFound) {
		t.Fatalf("expected the lock object to be deleted, headObject error = %v", err)
	}
	assertHolderContextEndedCleanly(t, holderCtx.Err(), context.Cause(holderCtx))
}

// assertHolderContextEndedCleanly asserts a clean release ended the holder
// context, not with a lock-loss cause, and with context.Canceled. It takes
// values, not the context, to satisfy revive's context-as-argument rule.
func assertHolderContextEndedCleanly(t *testing.T, ctxErr, cause error) {
	t.Helper()
	if ctxErr == nil {
		t.Fatalf("holder context still live after a clean release; the parent leaks a child per run")
	}
	if errors.Is(cause, helpers.ErrCacheLockLost) {
		t.Fatalf("holder context cause after a clean release = %v, must not be a lock-loss cause", cause)
	}
	if !errors.Is(cause, context.Canceled) {
		t.Fatalf("holder context cause after a clean release = %v, want context.Canceled", cause)
	}
}

// TestHeartbeatSurvivesTransientHeadFailures pins that failing heartbeat
// HEADs never mark the lock lost. The fault spans s3RetryMaxAttempts, or
// headObject's own retry would absorb it before heartbeatTick saw an error.
func TestHeartbeatSurvivesTransientHeadFailures(t *testing.T) {
	t.Parallel()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	b.lock = testLockTiming(10 * time.Second)
	ctx := context.Background()

	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	headsAtArm := fake.requestCount(key, http.MethodHead)
	putsAtArm := fake.requestCount(key, http.MethodPut)
	fake.failNext(key, http.MethodHead, http.StatusInternalServerError, s3RetryMaxAttempts)

	// A refresh PUT follows only a successful HEAD, so one new PUT proves the
	// armed failures are spent and none is left to hit release's own HEAD.
	waitForLockEvent(t, "a heartbeat refresh PUT after the forced HEAD failures", func() bool {
		return fake.requestCount(key, http.MethodPut) > putsAtArm
	})

	// Every forced failure plus the HEAD that finally succeeded: proof the
	// fault was really injected and really exhausted, not silently skipped.
	if heads := fake.requestCount(key, http.MethodHead) - headsAtArm; heads < s3RetryMaxAttempts+1 {
		t.Fatalf("expected %d forced failures and a successful HEAD, got %d HEADs after arming", s3RetryMaxAttempts, heads)
	}

	if err := release(); err != nil {
		t.Fatalf("expected release to succeed after transient HEAD failures, got %v", err)
	}
	if _, err := b.client.headObject(ctx, key); !errors.Is(err, errS3NotFound) {
		t.Fatalf("expected the lock object to be deleted, headObject error = %v", err)
	}
}

// TestReclaimIfExpiredLosesRaceOnRecreate pins that a 412 on the If-Match
// swap of an expired lock, another writer having changed it after our HEAD,
// is reported as observed, with no release and no immediate retry.
func TestReclaimIfExpiredLosesRaceOnRecreate(t *testing.T) {
	t.Parallel()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	b.lock = testLockTiming(time.Minute)
	ctx := context.Background()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	pastDeadline := time.Now().UTC().Add(-time.Hour)
	if err := b.putLock(ctx, key, foreignToken, pastDeadline, putCondition{}); err != nil {
		t.Fatalf("seed expired lock: %v", err)
	}

	// The next PUT to this key, reclaimIfExpired's If-Match swap, answers 412
	// as if another writer had changed the object after our HEAD.
	fake.failNext(key, http.MethodPut, http.StatusPreconditionFailed, 1)

	attempt, err := b.reclaimIfExpired(ctx, key, "our-token", testHolderCancel(t))
	if err != nil {
		t.Fatalf("reclaimIfExpired: %v", err)
	}
	if attempt.release != nil {
		t.Fatalf("expected no release function for a lost recreate race")
	}
	if attempt.retryNow {
		t.Fatalf("expected retryNow=false for a lost recreate race (back off, don't retry immediately)")
	}
	if !attempt.observed {
		t.Fatalf("expected observed=true: another creator winning the race is an acquirer this call saw")
	}
}

// TestReclaimIfExpiredRetriesImmediatelyWhenObjectVanished pins that a HEAD
// answering not-found means retryNow and not observed: a vanished object
// proves nothing about another acquirer.
func TestReclaimIfExpiredRetriesImmediatelyWhenObjectVanished(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := b.key(locksPrefix, "never-written-lock")
	attempt, err := b.reclaimIfExpired(ctx, key, "token", testHolderCancel(t))
	if err != nil {
		t.Fatalf("reclaimIfExpired: %v", err)
	}
	if attempt.release != nil {
		t.Fatalf("expected no release function when the object has vanished")
	}
	if !attempt.retryNow {
		t.Fatalf("expected retryNow=true when the object has vanished")
	}
	if attempt.observed {
		t.Fatalf("expected observed=false: a vanished object is not evidence of another acquirer")
	}
}

// TestTryAcquireOnceReportsObservationPerBranch pins lockAttempt.observed for
// each tryAcquireOnce branch against the real fake, since that evidence is
// what waitCeilingErr later reports as contention.
func TestTryAcquireOnceReportsObservationPerBranch(t *testing.T) {
	t.Parallel()

	tests := []tryAcquireOnceObservationCase{
		{
			name: "a live unexpired foreign holder is observed",
			setup: func(t *testing.T, b *Backend, _ *fakeS3, _ string) {
				t.Helper()
				seedLockObject(context.Background(), t, b, time.Now().UTC().Add(b.lock.ttl))
			},
			wantObserved: true,
		},
		{
			// Our create lands, then raceTokenOnNextHead writes a foreign
			// token just before our ownership-verifying HEAD.
			name: "a foreign token racing in right after our own create is observed",
			setup: func(t *testing.T, b *Backend, fake *fakeS3, key string) {
				t.Helper()
				if err := b.Open(context.Background()); err != nil {
					t.Fatalf("Open: %v", err)
				}
				fake.raceTokenOnNextHead(key, foreignToken)
			},
			wantObserved: true,
		},
		{
			// Both PUTs, the create and the If-Match swap on the expired seeded
			// object, answer 412, so the swap loses to another writer.
			name: "another creator winning the race for the just-deleted object is observed",
			setup: func(t *testing.T, b *Backend, fake *fakeS3, key string) {
				t.Helper()
				seedLockObject(context.Background(), t, b, time.Now().UTC().Add(-time.Hour))
				fake.failNext(key, http.MethodPut, http.StatusPreconditionFailed, 2)
			},
			wantObserved: true,
		},
		{
			name: "the object vanishing between our failed create and our HEAD is not observed",
			setup: func(t *testing.T, b *Backend, fake *fakeS3, key string) {
				t.Helper()
				seedLockObject(context.Background(), t, b, time.Now().UTC().Add(b.lock.ttl))
				fake.failNext(key, http.MethodHead, http.StatusNotFound, 1)
			},
			wantRetryNow: true,
		},
		{
			name: "a successful claim on an empty bucket is not observed",
			setup: func(t *testing.T, b *Backend, _ *fakeS3, _ string) {
				t.Helper()
				if err := b.Open(context.Background()); err != nil {
					t.Fatalf("Open: %v", err)
				}
			},
			wantRelease: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertTryAcquireOnceObservation(t, tc)
		})
	}
}

// tryAcquireOnceObservationCase is one row of
// TestTryAcquireOnceReportsObservationPerBranch: setup reaches one branch
// and the want fields give that branch's lockAttempt.
type tryAcquireOnceObservationCase struct {
	setup        func(t *testing.T, b *Backend, fake *fakeS3, key string)
	name         string
	wantObserved bool
	wantRetryNow bool
	wantRelease  bool
}

// assertTryAcquireOnceObservation runs one row: tc.setup on a fresh Backend
// and fake, one tryAcquireOnce, then every lockAttempt field against tc's
// want fields, releasing any lock it took.
func assertTryAcquireOnceObservation(t *testing.T, tc tryAcquireOnceObservationCase) {
	t.Helper()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	b.lock = testLockTiming(time.Minute)
	key := b.key(locksPrefix, lockObject)

	tc.setup(t, b, fake, key)

	attempt, err := b.tryAcquireOnce(context.Background(), key, "our-token", testHolderCancel(t))
	if err != nil {
		t.Fatalf("tryAcquireOnce: %v", err)
	}
	if attempt.observed != tc.wantObserved {
		t.Errorf("attempt.observed = %v, want %v", attempt.observed, tc.wantObserved)
	}
	if attempt.retryNow != tc.wantRetryNow {
		t.Errorf("attempt.retryNow = %v, want %v", attempt.retryNow, tc.wantRetryNow)
	}
	if gotRelease := attempt.release != nil; gotRelease != tc.wantRelease {
		t.Errorf("attempt.release != nil = %v, want %v", gotRelease, tc.wantRelease)
	}
	if attempt.release != nil {
		if err := attempt.release(); err != nil {
			t.Errorf("release: %v", err)
		}
	}
}

// TestLockAcquireBoundsImmediateRetrySpin pins maxImmediateLockRetries: a
// backend answering every create with 412 and every HEAD with 404 must not
// spin unbounded, and ends in errS3LockWaitNoHolderObserved, not contention.
func TestLockAcquireBoundsImmediateRetrySpin(t *testing.T) {
	t.Parallel()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	timing := testLockTiming(time.Minute)
	timing.waitCeiling = 300 * time.Millisecond
	b.lock = timing
	ctx := context.Background()

	// Arm both halves of the inconsistency for the whole test; a conforming
	// backend only ever shows it transiently.
	key := b.key(locksPrefix, lockObject)
	fake.failNext(key, http.MethodPut, http.StatusPreconditionFailed, -1)
	fake.failNext(key, http.MethodHead, http.StatusNotFound, -1)

	start := time.Now()
	_, _, err := b.Lock(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, errS3LockWaitNoHolderObserved) {
		t.Fatalf("expected errS3LockWaitNoHolderObserved, got %v", err)
	}
	// A generous upper margin over waitCeiling: it only needs to catch a
	// genuine hang regression, not pin down the exact backoff-jitter tail.
	if elapsed > timing.waitCeiling+2*time.Second {
		t.Fatalf("expected Lock to terminate close to the wait ceiling (%v), took %v", timing.waitCeiling, elapsed)
	}

	// An order of magnitude above what the bounded spin produces: the check is
	// that the spin is bounded at all, not an exact count.
	const maxRequestBudget = 300
	if puts := fake.requestCount(key, http.MethodPut); puts > maxRequestBudget {
		t.Fatalf("expected the PUT spin to stay bounded (budget %d), got %d requests", maxRequestBudget, puts)
	}
}

// TestReleaseLockOnMissingObjectReturnsNil directly unit-tests releaseLock's
// not-found branch: a key that was never written is treated as an already
// released lock, not an error.
func TestReleaseLockOnMissingObjectReturnsNil(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := b.key(locksPrefix, "never-written-lock")
	if err := b.releaseLock(ctx, key, "token"); err != nil {
		t.Fatalf("expected nil for a missing object, got %v", err)
	}
}

// TestReleaseLockOnForeignTokenDoesNotDelete pins that releaseLock with a
// token the object does not record returns nil and deletes nothing.
func TestReleaseLockOnForeignTokenDoesNotDelete(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := b.key(locksPrefix, "foreign-lock")
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(time.Hour), putCondition{}); err != nil {
		t.Fatalf("seed foreign lock: %v", err)
	}

	if err := b.releaseLock(ctx, key, "our-token"); err != nil {
		t.Fatalf("expected nil for a foreign-token release, got %v", err)
	}

	headers, err := b.client.headObject(ctx, key)
	if err != nil {
		t.Fatalf("headObject: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got != foreignToken {
		t.Fatalf("expected the foreign lock to remain in place, got token %q", got)
	}
}

// TestReleaseLockPropagatesHeadError pins that a non-404 HEAD failure reaches
// releaseLock's caller. The 500 is armed indefinitely so headObject's retry
// cannot turn it into a 404, which reads as already released.
func TestReleaseLockPropagatesHeadError(t *testing.T) {
	t.Parallel()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	ctx := context.Background()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := b.key(locksPrefix, "lock-with-head-error")
	fake.failNext(key, http.MethodHead, http.StatusInternalServerError, -1)

	if err := b.releaseLock(ctx, key, "token"); !errors.Is(err, errS3HeadFailed) {
		t.Fatalf("expected errS3HeadFailed to propagate, got %v", err)
	}
}

// TestReleaseDeletesOwnedLock confirms the ordinary path: releasing a lock
// this Backend still owns deletes the object.
func TestReleaseDeletesOwnedLock(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(time.Second)
	ctx := context.Background()

	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	if _, err := b.client.headObject(ctx, key); !errors.Is(err, errS3NotFound) {
		t.Fatalf("expected the lock object to be deleted, headObject error = %v", err)
	}
}

// TestReleaseUsesFreshContextAfterInstallCancel pins that release deletes the
// lock object even after the caller's context was canceled, so an
// interrupted run does not leave the lock blocking others until its TTL.
func TestReleaseUsesFreshContextAfterInstallCancel(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	cancel() // simulate the install run this lock belongs to being canceled

	if err := release(); err != nil {
		t.Fatalf("expected release to succeed despite the canceled caller context, got %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	if _, err := b.client.headObject(context.Background(), key); !errors.Is(err, errS3NotFound) {
		t.Fatalf("expected the lock object to be deleted despite the canceled caller context, headObject error = %v", err)
	}
}

// TestReleaseTimeoutIsBounded pins that release's fresh context is bounded by
// releaseTimeout: a hanging DELETE yields a deadline error well before the
// hang would end.
func TestReleaseTimeoutIsBounded(t *testing.T) {
	t.Parallel()
	timing := testLockTiming(time.Minute)
	timing.releaseTimeout = 50 * time.Millisecond
	b, fake := newLockBackendWithFake(t, timing)

	_, release, err := b.Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	const hangDelay = 2 * time.Second
	fake.deleteDelay = hangDelay // far above releaseTimeout

	start := time.Now()
	releaseErr := release()
	elapsed := time.Since(start)

	if !errors.Is(releaseErr, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline-related error, got %v", releaseErr)
	}
	if elapsed >= hangDelay {
		t.Fatalf("expected release to return well before the %v DELETE hang, took %v", hangDelay, elapsed)
	}
}

// TestLockExpiredUsesDeadline confirms the writer-recorded X-Amz-Meta-Deadline
// header is authoritative: a past deadline is reclaimable regardless of when
// the object was actually written, and a future deadline is not.
func TestLockExpiredUsesDeadline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		deadline time.Time
		name     string
		want     bool
	}{
		{name: "past deadline is expired", deadline: time.Now().Add(-time.Hour), want: true},
		{name: "future deadline is not expired", deadline: time.Now().Add(time.Hour), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := http.Header{}
			headers.Set("X-Amz-Meta-Deadline", tt.deadline.UTC().Format(time.RFC3339))

			if got := lockExpired(headers, time.Minute); got != tt.want {
				t.Fatalf("lockExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLockExpiredMalformedDeadlineFallsBackToAge pins that an unparsable
// deadline falls back to Last-Modified age against ttl rather than failing.
func TestLockExpiredMalformedDeadlineFallsBackToAge(t *testing.T) {
	t.Parallel()

	const ttl = time.Minute

	tests := []struct {
		lastModified time.Time
		name         string
		want         bool
	}{
		{name: "recent Last-Modified is not expired", lastModified: time.Now(), want: false},
		{name: "old Last-Modified is expired", lastModified: time.Now().Add(-2 * ttl), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := http.Header{}
			headers.Set("X-Amz-Meta-Deadline", "not-a-valid-timestamp")
			headers.Set("Last-Modified", tt.lastModified.UTC().Format(http.TimeFormat))

			if got := lockExpired(headers, ttl); got != tt.want {
				t.Fatalf("lockExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLockExpiredNoTimingIsReclaimable pins that an object with neither a
// deadline nor Last-Modified is reclaimable, so it can never block forever.
func TestLockExpiredNoTimingIsReclaimable(t *testing.T) {
	t.Parallel()

	if got := lockExpired(http.Header{}, time.Minute); !got {
		t.Fatalf("lockExpired() = %v, want true (reclaimable) for headers with no timing information", got)
	}
}
