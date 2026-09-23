package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// watchdogTransport fails a response body read that makes no progress for
// idle. It replaces an http.Client.Timeout, which would cap total transfer time
// and so truncate a large but healthy artifact download.
type watchdogTransport struct {
	base http.RoundTripper
	idle time.Duration
}

// RoundTrip runs the request on a cancelable context derived from the
// caller's and wraps the body: net/http body reads honor the round trip's
// context, and canceling that one is how the watchdog unblocks a stalled Read.
func (t watchdogTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	wctx, cancel := context.WithCancel(req.Context())
	resp, err := t.base.RoundTrip(req.Clone(wctx))
	if err != nil {
		cancel()
		return resp, err
	}
	resp.Body = newWatchdogBody(req.Context(), resp.Body, cancel, t.idle)
	return resp, nil
}

// watchdogBody fails a Read that blocks for idle with no progress. Unlike a raw
// response body it must not be Closed during a Read, as the timer is
// unsynchronized: callers abort by canceling the context, never by Close.
type watchdogBody struct {
	body io.ReadCloser
	//nolint:containedctx // parentCtx is the caller's original request
	// context, kept only to distinguish "watchdog fired" from "caller
	// canceled" on a read error; it is never used to start work, dial, or
	// spawn goroutines, so the usual leak/propagation concerns do not apply.
	parentCtx context.Context
	cancel    context.CancelFunc
	timer     *time.Timer
	idle      time.Duration
	// fired is set by the timer goroutine and read by whichever goroutine
	// is inside Read; atomic because those are two different goroutines
	// touching it concurrently.
	fired atomic.Bool
}

// newWatchdogBody constructs a watchdogBody; the timer is armed lazily by the
// first Read, so a body that is never read costs no timer.
func newWatchdogBody(parentCtx context.Context, body io.ReadCloser, cancel context.CancelFunc, idle time.Duration) *watchdogBody {
	return &watchdogBody{body: body, parentCtx: parentCtx, cancel: cancel, idle: idle}
}

// Read arms the idle timer around one underlying read. A failure after the
// watchdog fired, with the caller's context live, becomes ErrReadStalled (cause
// rendered by %v); any other error, a caller's own cancel included, passes as is.
func (b *watchdogBody) Read(p []byte) (int, error) {
	if b.timer == nil {
		b.timer = time.AfterFunc(b.idle, b.onStall)
	} else {
		b.timer.Reset(b.idle)
	}
	n, err := b.body.Read(p)
	b.timer.Stop()
	if err != nil && b.fired.Load() && b.parentCtx.Err() == nil {
		//nolint:errorlint // deliberately %v, not %w: see helpers.ErrReadStalled's doc comment.
		return n, fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, b.idle, err)
	}
	return n, err
}

// Close stops the timer, cancels the derived context and closes the body; it is
// safe after a stall or cancel, but must not race an in-flight Read.
func (b *watchdogBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	b.cancel()
	return b.body.Close()
}

// onStall runs on the timer goroutine: it records that the watchdog, not the
// caller, fired, then cancels the derived context to unblock the stuck Read.
func (b *watchdogBody) onStall() {
	b.fired.Store(true)
	b.cancel()
}
