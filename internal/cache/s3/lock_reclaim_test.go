package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// ourToken is the token the tests below hand to reclaimIfExpired when
	// they drive it directly instead of going through Lock, standing in for
	// the one acquireLockLoop generates per acquisition.
	ourToken = "our-reclaimer-token"
	// competingToken stands in for a second acquirer that writes the same
	// lock object while this one is reclaiming it - the shape the swap's own
	// If-Match condition exists to lose against.
	competingToken = "competing-reclaimer-token"
)

// TestReclaimSwapsOnlyTheVersionItRead pins that reclaimIfExpired's swap is
// conditioned on the ETag its own expiry HEAD read, and that it never deletes
// the lock key, whatever state lands between that HEAD and the swap.
func TestReclaimSwapsOnlyTheVersionItRead(t *testing.T) {
	t.Parallel()

	for _, tc := range reclaimSwapCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertReclaimSwap(t, tc)
		})
	}
}

// reclaimSwapCase is one row of TestReclaimSwapsOnlyTheVersionItRead: the
// state installed behind reclaimIfExpired's HEAD (a rewrite, a delete or a
// failed swap PUT) and the lockAttempt, error and object that must follow.
type reclaimSwapCase struct {
	name           string
	wantErr        error
	rewriteToken   string
	wantToken      string
	rewriteAhead   time.Duration
	failSwapStatus int
	dropObject     bool
	wantGone       bool
	wantObserved,
	wantRetryNow,
	wantRelease bool
}

// reclaimSwapCases returns the five states reachable between reclaimIfExpired's
// expiry decision and the swap it conditions on that same read.
func reclaimSwapCases() []reclaimSwapCase {
	return []reclaimSwapCase{
		{
			// Same token, same expired deadline, new ETag: only the ETag can
			// make this swap lose, so this row proves the ETag arbitrates.
			name:         "a rewrite that changes only the version loses the swap",
			rewriteToken: foreignToken,
			rewriteAhead: -time.Hour,
			wantToken:    foreignToken,
			wantObserved: true,
		},
		{
			name:         "a competing acquirer's write is left standing",
			rewriteToken: competingToken,
			rewriteAhead: time.Hour,
			wantToken:    competingToken,
			wantObserved: true,
		},
		{
			name:         "a lock deleted before the swap is retried immediately",
			dropObject:   true,
			wantGone:     true,
			wantRetryNow: true,
		},
		{
			// 403 because helpers.IsRetryableHTTPStatus does not retry it, so
			// the row skips the retry ladder a 5xx would drag through.
			name:           "a swap the backend refuses ends the attempt",
			wantToken:      foreignToken,
			wantErr:        helpers.ErrCacheBackendUnavailable,
			failSwapStatus: http.StatusForbidden,
		},
		{
			name:        "the same fixture with nothing racing in reclaims",
			wantToken:   ourToken,
			wantRelease: true,
		},
	}
}

// assertReclaimSwap runs one reclaimSwapCase end to end against a real fake S3
// server.
func assertReclaimSwap(t *testing.T, tc reclaimSwapCase) {
	t.Helper()
	ctx := context.Background()
	key := path.Join(locksPrefix, lockObject)
	b, fake := newReclaimSwapFixture(t, tc, key)

	// Open's probe also writes under the locks prefix; a probe that HEADed this
	// key would fire the interference behind the wrong request.
	if got := fake.requestCount(key, http.MethodHead); got != 0 {
		t.Fatalf("HEAD count on the lock key before the reclaim = %d, want 0", got)
	}

	attempt, err := b.reclaimIfExpired(ctx, key, ourToken, testHolderCancel(t))
	if attempt.release != nil {
		t.Cleanup(func() { _ = attempt.release() })
	}
	assertReclaimError(t, tc.wantErr, err)
	// A compare-and-swap reclaim has no delete step; even the 403 row's abandon
	// finds a foreign token on the object and declines to delete it.
	if got := fake.requestCount(key, http.MethodDelete); got != 0 {
		t.Fatalf("DELETE count on the lock key = %d, want 0: a swap reclaim never deletes", got)
	}
	assertReclaimOutcome(t, tc, attempt)
	assertReclaimSwapObject(t, tc, b, key)
}

// assertReclaimError requires a nil error when want is nil, else one matching
// want: the 403 row is about which cache-backend class a status the remote
// itself answered lands in, not merely that it fails.
func assertReclaimError(t *testing.T, want, err error) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Fatalf("reclaimIfExpired: %v", err)
		}
		return
	}
	if !errors.Is(err, want) {
		t.Fatalf("reclaimIfExpired error = %v, want one matching %v", err, want)
	}
}

// newReclaimSwapFixture seeds an expired foreign lock behind a handler that
// applies tc's interference after serving the first HEAD of the lock key but
// before returning, so the client receives that HEAD only after the change.
func newReclaimSwapFixture(t *testing.T, tc reclaimSwapCase, key string) (*Backend, *fakeS3) {
	t.Helper()
	ctx := context.Background()
	fake := newFakeS3()
	lockPath := "/" + fake.bucket + "/" + key

	var heads atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.ServeHTTP(w, r)
		if r.Method != http.MethodHead || r.URL.Path != lockPath || heads.Add(1) != 1 {
			return
		}
		switch {
		case tc.rewriteToken != "":
			fake.storeLockObject(key, tc.rewriteToken, time.Now().UTC().Add(tc.rewriteAhead))
		case tc.dropObject:
			fake.dropObject(key)
		}
		if tc.failSwapStatus != 0 {
			// One use only, so it is spent on the swap PUT and no later write
			// this row makes is affected.
			fake.failNext(key, http.MethodPut, tc.failSwapStatus, 1)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	b := newLockBackendAt(t, srv.URL, srv.Client(), testLockTiming(time.Minute))
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(-time.Hour), putCondition{}); err != nil {
		t.Fatalf("seed expired lock: %v", err)
	}
	return b, fake
}

// assertReclaimOutcome checks the lockAttempt one reclaimSwapCase expects.
// retryNow is checked before observed, so a row expecting retryNow reports the
// missing field rather than the spurious one it was swapped for.
func assertReclaimOutcome(t *testing.T, tc reclaimSwapCase, attempt lockAttempt) {
	t.Helper()
	if attempt.retryNow != tc.wantRetryNow {
		t.Fatalf("attempt.retryNow = %v, want %v", attempt.retryNow, tc.wantRetryNow)
	}
	if attempt.observed != tc.wantObserved {
		t.Fatalf("attempt.observed = %v, want %v", attempt.observed, tc.wantObserved)
	}
	if got := attempt.release != nil; got != tc.wantRelease {
		t.Fatalf("attempt holds the lock = %v, want %v", got, tc.wantRelease)
	}
}

// assertReclaimSwapObject checks which write is standing on the lock object
// once the attempt is over - the question every row is ultimately about.
func assertReclaimSwapObject(t *testing.T, tc reclaimSwapCase, b *Backend, key string) {
	t.Helper()
	headers, err := b.client.headObject(context.Background(), key)
	if tc.wantGone {
		if !errors.Is(err, errS3NotFound) {
			t.Fatalf("lock object after a reclaim of a deleted lock: headObject err = %v, want errS3NotFound", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("headObject after the reclaim: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got != tc.wantToken {
		t.Fatalf("lock object token after the reclaim = %q, want %q", got, tc.wantToken)
	}
}

// TestReclaimRefusesAnUnversionedHead pins that a HEAD answered without an ETag
// after Open passed is refused as ErrCacheBackendUnusable with no PUT issued,
// since an empty ETag would turn the swap into an unconditional overwrite.
func TestReclaimRefusesAnUnversionedHead(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	key := path.Join(locksPrefix, lockObject)
	b, fake := newTestBackendAndFake(t)
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(-time.Hour), putCondition{}); err != nil {
		t.Fatalf("seed expired lock: %v", err)
	}
	puts := fake.requestCount(key, http.MethodPut)
	fake.setSuppressETag(true)

	attempt, err := b.reclaimIfExpired(ctx, key, ourToken, testHolderCancel(t))
	if attempt.release != nil {
		t.Cleanup(func() { _ = attempt.release() })
	}
	if !errors.Is(err, helpers.ErrCacheBackendUnusable) {
		t.Fatalf("reclaimIfExpired error = %v, want one matching %v", err, helpers.ErrCacheBackendUnusable)
	}
	if attempt.release != nil {
		t.Fatalf("the reclaim held the lock against a backend that names no version")
	}
	if got := fake.requestCount(key, http.MethodPut) - puts; got != 0 {
		t.Fatalf("PUT count on the lock key = %d, want 0: an unversioned HEAD must be refused, not overwritten", got)
	}
}

// reclaimCutShortBudget bounds the row that must fail after its swap PUT: it
// must clear the two loopback round trips before the swap, and it is also how
// long that row then waits in the held ownership check.
const reclaimCutShortBudget = 300 * time.Millisecond

// reclaimCompletionMargin is the deadline the control row hands
// reclaimIfExpired; its client ignores cancellation, so the row does not bet on
// it. freshCreateCompletionMargin is its twin on the create branch.
const reclaimCompletionMargin = 10 * time.Second

// TestReclaimAbandonsALockItNeverHeld pins that a reclaim cut short after its
// swap PUT landed deletes the object it wrote, which would otherwise block
// every acquirer for a full lock TTL; the second row is the positive control.
func TestReclaimAbandonsALockItNeverHeld(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		blockPostPutHead bool
		wantRelease      bool
	}{
		{name: "a budget that fits the swap but not the ownership check", blockPostPutHead: true},
		{name: "the same fixture with nothing cutting the acquisition short", wantRelease: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertReclaimOrphanCleanup(t, tc.blockPostPutHead, tc.wantRelease)
		})
	}
}

// assertReclaimOrphanCleanup runs one orphan-cleanup row end to end against a
// real fake S3 server, driving reclaimIfExpired directly: the create-PUT Lock
// issues first would poison the putsServed anchor and spend the row's budget.
func assertReclaimOrphanCleanup(t *testing.T, blockPostPutHead, wantRelease bool) {
	t.Helper()
	key := path.Join(locksPrefix, lockObject)
	fake := newFakeS3()
	lockPath := "/" + fake.bucket + "/" + key
	// The blocked row is the one whose budget must fire; the control row's has
	// to fit a whole reclaim inside it instead. See freshCreateCompletionMargin.
	budget := reclaimCompletionMargin
	if blockPostPutHead {
		budget = reclaimCutShortBudget
	}
	opCtx, cancel := context.WithTimeout(context.Background(), budget)
	t.Cleanup(cancel)

	var putsServed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The ownership check that follows the swap PUT is held until the
		// caller's budget is gone, so the failure row fails on that budget
		// every time instead of racing a loopback round trip it would win.
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
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Seeded straight into the fake rather than over HTTP, so the reclaim's own
	// swap PUT is the first one this key ever sees and the anchor above counts
	// only it.
	fake.storeLockObject(key, foreignToken, time.Now().UTC().Add(-time.Hour))

	attempt, err := b.reclaimIfExpired(opCtx, key, ourToken, testHolderCancel(t))
	if attempt.release != nil {
		t.Cleanup(func() { _ = attempt.release() })
	}
	assertReclaimOrphanOutcome(t, wantRelease, attempt, err)
	assertReclaimOrphanObject(t, wantRelease, b, key)
}

// assertReclaimOrphanOutcome checks the attempt and the error one
// orphan-cleanup row expects.
func assertReclaimOrphanOutcome(t *testing.T, wantRelease bool, attempt lockAttempt, err error) {
	t.Helper()
	if got := attempt.release != nil; got != wantRelease {
		t.Fatalf("attempt holds the lock = %v, want %v (error: %v)", got, wantRelease, err)
	}
	// The first arm needs a release and an error, which lockAttempt's contract
	// forbids; the second fails a cut-short row that reports success anyway.
	switch {
	case wantRelease && err != nil:
		t.Fatalf("reclaimIfExpired: %v", err)
	case !wantRelease && err == nil:
		t.Fatalf("reclaimIfExpired reported no error for an acquisition cut short after its swap PUT")
	}
}

// assertReclaimOrphanObject checks what the lock object holds once the attempt
// is over: nothing at all for a row whose reclaim was cut short, and this run's
// own token for the control.
func assertReclaimOrphanObject(t *testing.T, wantRelease bool, b *Backend, key string) {
	t.Helper()
	headers, err := b.client.headObject(context.Background(), key)
	if !wantRelease {
		if !errors.Is(err, errS3NotFound) {
			t.Fatalf("lock object after an abandoned reclaim: headObject err = %v, want errS3NotFound", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("headObject after the reclaim: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got != ourToken {
		t.Fatalf("lock object token after the reclaim = %q, want %q", got, ourToken)
	}
}

// reclaimSwapHold is how long the first swap PUT is held before the fake sees
// it: it only has to exceed the other acquirer's swap round trip (87-169us on
// this fixture), and every millisecond beyond that is simply waited.
const reclaimSwapHold = 50 * time.Millisecond

// TestTwoReclaimersEndWithOneHolder pins that two acquirers reclaiming the same
// expired lock end with exactly one holder, a loser failing only as
// ErrCacheBusy; either may win, and the held first swap forces the ordering.
func TestTwoReclaimersEndWithOneHolder(t *testing.T) {
	t.Parallel()

	key := path.Join(locksPrefix, lockObject)
	timing := testLockTiming(time.Minute)
	timing.waitCeiling = 600 * time.Millisecond

	srv := newHeldFirstSwapServer(t, key, reclaimSwapHold)
	// The wrapper keeps the 600ms ceiling from cutting any request short, and is
	// admissible only because newHeldFirstSwapServer answers every request; the
	// copy keeps whatever else httptest configured on its client.
	answering := *srv.Client()
	answering.Transport = answeredDespiteCancelTransport{base: srv.Client().Transport}
	b1 := newLockBackendAt(t, srv.URL, &answering, timing)
	b2 := newLockBackendAt(t, srv.URL, &answering, timing)
	seedLockObject(context.Background(), t, b1, time.Now().UTC().Add(-time.Hour))

	releases := make([]func() error, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, b := range []*Backend{b1, b2} {
		wg.Go(func() {
			_, release, err := b.Lock(context.Background())
			releases[i], errs[i] = release, err
		})
	}
	wg.Wait()

	if holders := releaseHolders(t, releases); holders != 1 {
		t.Fatalf("%d of 2 reclaimers ended up holding the lock, want exactly 1 (errors: %v)", holders, errs)
	}
	for i, err := range errs {
		if err != nil && !errors.Is(err, helpers.ErrCacheBusy) {
			t.Fatalf("acquirer %d lost the reclaim with %v, want an error matching helpers.ErrCacheBusy", i, err)
		}
	}
}

// newHeldFirstSwapServer holds only the first take-over PUT for key for hold.
// It finds that PUT by position (the first PUT after the first HEAD), never by
// If-Match, so the ordering is still forced if a regression drops that header.
func newHeldFirstSwapServer(t *testing.T, key string, hold time.Duration) *httptest.Server {
	t.Helper()
	fake := newFakeS3()
	lockPath := "/" + fake.bucket + "/" + key

	var heads, swaps atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		onLock := r.URL.Path == lockPath
		if onLock && r.Method == http.MethodHead {
			heads.Add(1)
		}
		if onLock && r.Method == http.MethodPut && heads.Load() > 0 && swaps.Add(1) == 1 {
			time.Sleep(hold)
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// releaseHolders releases every acquirer holding the lock and returns how many
// there were; a failed release is a fixture failure, reported with Errorf.
func releaseHolders(t *testing.T, releases []func() error) int {
	t.Helper()
	holders := 0
	for i, release := range releases {
		if release == nil {
			continue
		}
		holders++
		if err := release(); err != nil {
			t.Errorf("acquirer %d: release: %v", i, err)
		}
	}
	return holders
}
