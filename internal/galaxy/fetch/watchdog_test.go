package fetch

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// testIdle is the inactivity window every test here uses; assertions wait on
// channels, never on elapsed wall-clock time, so a slow runner cannot flake.
const testIdle = 20 * time.Millisecond

// waitBound is the generous upper bound every test gives itself to observe
// a Read (or a goroutine) complete, so a genuine deadlock fails the test
// instead of hanging the suite.
const waitBound = 2 * time.Second

// blockingReadCloser's Read blocks until ctx is done and returns ctx.Err(); it
// closes started first, so a test can wait until the read is in flight.
type blockingReadCloser struct {
	ctx     context.Context //nolint:containedctx // test double: ctx is what the blocked Read waits on, not a stored request context.
	started chan struct{}
	once    sync.Once
	closed  atomic.Bool
}

func newBlockingReadCloser(ctx context.Context) *blockingReadCloser {
	return &blockingReadCloser{ctx: ctx, started: make(chan struct{})}
}

// Read signals started, then blocks until ctx is done and returns its error.
func (r *blockingReadCloser) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// Close records that it was called; it never errors.
func (r *blockingReadCloser) Close() error {
	r.closed.Store(true)
	return nil
}

// gatedReadCloser blocks every Read until release is closed, then returns
// context.Canceled; it watches no context, so a test can hold a read through
// both the watchdog firing and a parent cancel before letting it return.
type gatedReadCloser struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedReadCloser() *gatedReadCloser {
	return &gatedReadCloser{started: make(chan struct{}), release: make(chan struct{})}
}

// Read signals started, then blocks until release is closed and returns
// context.Canceled, mirroring what a real body read returns once its request
// context ends.
func (r *gatedReadCloser) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return 0, context.Canceled
}

// Close is a no-op; nothing in this file asserts on whether it was called
// for a gatedReadCloser.
func (r *gatedReadCloser) Close() error {
	return nil
}

// closeTrackingReadCloser adapts a plain io.Reader (e.g. strings.NewReader)
// into an io.ReadCloser that records whether Close was called.
type closeTrackingReadCloser struct {
	io.Reader

	closed atomic.Bool
}

// Close records that it was called; it never errors.
func (c *closeTrackingReadCloser) Close() error {
	c.closed.Store(true)
	return nil
}

// readResult carries a Read call's outcome across a goroutine boundary.
// err is ordered first so the struct's pointer-containing prefix is as
// short as possible for the garbage collector to scan.
type readResult struct {
	err error
	n   int
}

// TestWatchdogBody_StallReportsErrReadStalled pins that a read ended only by
// the watchdog firing reports helpers.ErrReadStalled and leaves the parent
// context untouched.
func TestWatchdogBody_StallReportsErrReadStalled(t *testing.T) {
	t.Parallel()

	parentCtx, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	// wctx mirrors what watchdogTransport.RoundTrip derives from the
	// request context: a child the watchdog timer can cancel on its own,
	// independent of the parent.
	wctx, wcancel := context.WithCancel(parentCtx)
	defer wcancel()

	body := newBlockingReadCloser(wctx)
	wb := newWatchdogBody(parentCtx, body, wcancel, testIdle)

	done := make(chan readResult, 1)
	go func() {
		n, err := wb.Read(make([]byte, 8))
		done <- readResult{err: err, n: n}
	}()

	select {
	case res := <-done:
		if !errors.Is(res.err, helpers.ErrReadStalled) {
			t.Fatalf("Read error = %v, want errors.Is(err, helpers.ErrReadStalled)", res.err)
		}
		if res.n != 0 {
			t.Fatalf("Read n = %d, want 0", res.n)
		}
	case <-time.After(waitBound):
		t.Fatal("Read did not return once the watchdog should have fired; the timer never unblocked it")
	}

	if parentCtx.Err() != nil {
		t.Fatalf("parent context Err() = %v, want nil: the watchdog must not touch the parent context", parentCtx.Err())
	}
}

// TestWatchdogBody_StallErrorDoesNotMatchContextCanceled pins that a stall
// error keeps "context canceled" in its text but not in errors.Is, so a stall
// exits as a network failure rather than as a caught Ctrl-C.
func TestWatchdogBody_StallErrorDoesNotMatchContextCanceled(t *testing.T) {
	t.Parallel()

	parentCtx, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	wctx, wcancel := context.WithCancel(parentCtx)
	defer wcancel()

	body := newBlockingReadCloser(wctx)
	wb := newWatchdogBody(parentCtx, body, wcancel, testIdle)

	done := make(chan readResult, 1)
	go func() {
		n, err := wb.Read(make([]byte, 8))
		done <- readResult{err: err, n: n}
	}()

	select {
	case res := <-done:
		if !errors.Is(res.err, helpers.ErrReadStalled) {
			t.Fatalf("Read error = %v, want errors.Is(err, helpers.ErrReadStalled)", res.err)
		}
		if errors.Is(res.err, context.Canceled) {
			t.Fatalf("Read error = %v, must not match context.Canceled: the stall error must render "+
				"its cancellation cause with %%v, not wrap it with %%w, or it steals the exit-code "+
				"classification of a genuine Ctrl-C", res.err)
		}
		if !strings.Contains(res.err.Error(), "context canceled") {
			t.Errorf("Read error = %q, want it to still contain %q: rendering the cause with %%v "+
				"must not drop it from the message, only from errors.Is matchability", res.err.Error(), "context canceled")
		}
	case <-time.After(waitBound):
		t.Fatal("Read did not return once the watchdog should have fired; the timer never unblocked it")
	}
}

// TestWatchdogBody_ParentCancelPropagatesContextCanceled pins that a caller
// cancel before the watchdog could fire surfaces as context.Canceled, never as
// helpers.ErrReadStalled, since Ctrl-C exit-code handling branches on it.
func TestWatchdogBody_ParentCancelPropagatesContextCanceled(t *testing.T) {
	t.Parallel()

	parentCtx, parentCancel := context.WithCancel(t.Context())
	wctx, wcancel := context.WithCancel(parentCtx)
	defer wcancel()

	body := newBlockingReadCloser(wctx)
	// An idle window generous enough that this test's own runtime can
	// never make the watchdog itself fire; only parentCancel below can
	// unblock the read.
	wb := newWatchdogBody(parentCtx, body, wcancel, time.Hour)
	defer func() { _ = wb.Close() }()

	done := make(chan readResult, 1)
	go func() {
		n, err := wb.Read(make([]byte, 8))
		done <- readResult{err: err, n: n}
	}()

	// Wait until the read has actually entered its blocking phase before
	// canceling, so the cancellation is guaranteed to be what unblocks it
	// rather than racing a goroutine that has not started yet.
	select {
	case <-body.started:
	case <-time.After(waitBound):
		t.Fatal("Read never started")
	}
	parentCancel()

	select {
	case res := <-done:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("Read error = %v, want errors.Is(err, context.Canceled)", res.err)
		}
		if errors.Is(res.err, helpers.ErrReadStalled) {
			t.Fatalf("Read error = %v, must not be helpers.ErrReadStalled: the parent context, not the watchdog, ended this read", res.err)
		}
	case <-time.After(waitBound):
		t.Fatal("Read did not return once the parent context was canceled")
	}
}

// TestWatchdogBody_FiredWatchdogYieldsToParentCancel pins Read's parentCtx
// guard: after the watchdog fired, a parent canceled before the read returns
// still wins as context.Canceled; "parent still live" is the positive control.
func TestWatchdogBody_FiredWatchdogYieldsToParentCancel(t *testing.T) {
	t.Parallel()

	t.Run("parent still live", func(t *testing.T) {
		t.Parallel()
		runFiredWatchdogCase(t, false)
	})
	t.Run("parent canceled before the read returns", func(t *testing.T) {
		t.Parallel()
		runFiredWatchdogCase(t, true)
	})
}

// runFiredWatchdogCase starts a read on a gated body, waits for the watchdog
// to fire, optionally cancels the parent while the read is still blocked, then
// releases the read and checks the error it returned.
func runFiredWatchdogCase(t *testing.T, cancelParent bool) {
	t.Helper()

	parentCtx, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	wctx, wcancel := context.WithCancel(parentCtx)
	defer wcancel()

	body := newGatedReadCloser()
	wb := newWatchdogBody(parentCtx, body, wcancel, testIdle)

	done := make(chan readResult, 1)
	go func() {
		n, err := wb.Read(make([]byte, 8))
		done <- readResult{err: err, n: n}
	}()

	select {
	case <-body.started:
	case <-time.After(waitBound):
		t.Fatal("Read never started")
	}

	// onStall stores fired before canceling wctx, so observing wctx.Done()
	// guarantees fired reads true here, which no sleep could.
	select {
	case <-wctx.Done():
	case <-time.After(waitBound):
		t.Fatal("watchdog never fired (wctx was never canceled)")
	}

	if cancelParent {
		parentCancel()
	}
	// Only now does the blocked read actually return, with both preconditions
	// (fired, and - in the cancelParent case - a dead parent) already true.
	close(body.release)

	select {
	case res := <-done:
		checkFiredWatchdogResult(t, cancelParent, res.err)
	case <-time.After(waitBound):
		t.Fatal("Read did not return after release was closed")
	}
}

// checkFiredWatchdogResult asserts runFiredWatchdogCase's expected outcome
// for one of its two subtests, split out from runFiredWatchdogCase purely to
// stay under the cyclomatic-complexity budget.
func checkFiredWatchdogResult(t *testing.T, cancelParent bool, err error) {
	t.Helper()

	if cancelParent {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Read error = %v, want errors.Is(err, context.Canceled)", err)
		}
		if errors.Is(err, helpers.ErrReadStalled) {
			t.Errorf("Read error = %v, must not match helpers.ErrReadStalled: the parent was already "+
				"canceled before the read returned, so the guard must yield to it even though the "+
				"watchdog had already fired", err)
		}
		return
	}
	if !errors.Is(err, helpers.ErrReadStalled) {
		t.Errorf("Read error = %v, want errors.Is(err, helpers.ErrReadStalled)", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("Read error = %v, must not match context.Canceled", err)
	}
}

// TestWatchdogBody_NormalReadAndClose covers the common, non-stalled path:
// data is available immediately, Read returns it with no error, and Close
// closes the underlying body cleanly.
func TestWatchdogBody_NormalReadAndClose(t *testing.T) {
	t.Parallel()

	const want = "hello, watchdog"
	body := &closeTrackingReadCloser{Reader: strings.NewReader(want)}
	wb := newWatchdogBody(t.Context(), body, func() {}, time.Second)

	got, err := io.ReadAll(wb)
	if err != nil {
		t.Fatalf("ReadAll error = %v, want nil", err)
	}
	if string(got) != want {
		t.Fatalf("ReadAll = %q, want %q", got, want)
	}

	if err := wb.Close(); err != nil {
		t.Fatalf("Close error = %v, want nil", err)
	}
	if !body.closed.Load() {
		t.Fatal("Close did not close the underlying body")
	}
}

// TestWatchdogBody_CloseWithoutRead ensures Close is safe even when no Read
// ever ran, so the timer was never armed (it is nil), exercising the guard
// in Close that skips Stop in that case.
func TestWatchdogBody_CloseWithoutRead(t *testing.T) {
	t.Parallel()

	body := &closeTrackingReadCloser{Reader: strings.NewReader("unread")}
	wb := newWatchdogBody(t.Context(), body, func() {}, time.Second)

	if err := wb.Close(); err != nil {
		t.Fatalf("Close error = %v, want nil", err)
	}
	if !body.closed.Load() {
		t.Fatal("Close did not close the underlying body")
	}
}
