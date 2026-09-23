package s3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// s3RetryableCase is one row of TestS3Retryable's table. canceled selects a
// canceled context in the loop, since containedctx refuses a context.Context
// struct field.
type s3RetryableCase struct {
	err      error
	name     string
	canceled bool
	want     bool
}

// newS3RetryableCases builds TestS3Retryable's table, split out to stay under
// the funlen budget.
func newS3RetryableCases() []s3RetryableCase {
	// stalledProduction mirrors watchdogBody.Read: the cause is rendered with
	// %v, not wrapped, so it does not carry context.Canceled through errors.Is.
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape watchdogBody.Read builds.
	stalledProduction := fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, time.Second, context.Canceled)
	// stalledSynthetic also wraps context.Canceled, a shape no producer builds,
	// to pin that ErrReadStalled is classified before the transport arm
	// whatever the producer's rendering.
	stalledSynthetic := fmt.Errorf("%w: no data for %s: %w", helpers.ErrReadStalled, time.Second, context.Canceled)
	// transportFailure mirrors Client.do's shape: a timeout wrapped in
	// errS3TransportFailed that still matches context.DeadlineExceeded,
	// retried only while the caller's context is live.
	transportFailure := fmt.Errorf("%w: %w", errS3TransportFailed,
		&url.Error{Op: "Get", URL: "https://example.invalid", Err: context.DeadlineExceeded})

	return []s3RetryableCase{
		{name: "nil is not retryable", err: nil, want: false},
		{name: "offline is not retryable", err: helpers.ErrOfflineMode, want: false},
		{name: "stalled read in its production shape is retryable", err: stalledProduction, want: true},
		{
			name: "a stall signature that also carries context.Canceled is still retryable",
			err:  stalledSynthetic,
			want: true,
		},
		{name: "raw context.Canceled is not retryable", err: context.Canceled, want: false},
		{name: "raw context.DeadlineExceeded is not retryable", err: context.DeadlineExceeded, want: false},
		{
			name: "retryable status is retryable",
			err:  wrapRetryableStatus(http.StatusServiceUnavailable, errS3BucketRequestFailed),
			want: true,
		},
		{name: "not found is not retryable", err: errS3NotFound, want: false},
		{name: "precondition failed is not retryable", err: errS3PreconditionFailed, want: false},
		{name: "an oversized listing or batch-delete response is never retried", err: helpers.ErrResponseTooLarge, want: false},
		// One fixture, retried under a live ctx and refused under a canceled
		// one: the gate discriminates on ctx, not on the error's shape.
		{name: "transport failure is retryable while the caller's ctx is live", err: transportFailure, want: true},
		{
			name:     "the identical transport failure is not retried once the caller's ctx is canceled",
			err:      transportFailure,
			canceled: true,
			want:     false,
		},
		{
			name: "an unmarked raw transport error is never retried",
			err:  &url.Error{Op: "Get", URL: "https://example.invalid", Err: context.DeadlineExceeded},
			want: false,
		},
		{
			name: "a status sentinel carrying ErrCacheBackendUnavailable but no retryable status is not retried",
			err:  errS3GetFailed,
			want: false,
		},
	}
}

// TestS3Retryable pins the retry classification: a stalled read is retried in
// either rendering, a raw caller cancellation is not, and errS3TransportFailed
// is retried only while the caller's ctx is live.
func TestS3Retryable(t *testing.T) {
	t.Parallel()
	for _, tc := range newS3RetryableCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tc.canceled {
				canceledCtx, cancel := context.WithCancel(context.Background())
				cancel()
				ctx = canceledCtx
			}
			if got := s3Retryable(ctx, tc.err); got != tc.want {
				t.Errorf("s3Retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestWrapRetryableStatusFollowsTheSharedSet pins that this package retries
// exactly the statuses helpers.IsRetryableHTTPStatus names, sweeping both
// classes so a drift in either direction fails.
func TestWrapRetryableStatusFollowsTheSharedSet(t *testing.T) {
	t.Parallel()

	statuses := []int{
		http.StatusOK,
		http.StatusMovedPermanently,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusPreconditionFailed,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusNotImplemented,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	}
	// Both classes must be exercised for the sweep to mean anything: an
	// all-retryable or all-terminal list would pass against a predicate stuck
	// at a constant.
	var retryable, terminal int
	for _, status := range statuses {
		if helpers.IsRetryableHTTPStatus(status) {
			retryable++
		} else {
			terminal++
		}
	}
	if retryable == 0 || terminal == 0 {
		t.Fatalf("sweep must cover both classes, got %d retryable and %d terminal", retryable, terminal)
	}

	for _, status := range statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			wrapped := wrapRetryableStatus(status, errS3BucketRequestFailed)
			want := helpers.IsRetryableHTTPStatus(status)
			if got := s3Retryable(context.Background(), wrapped); got != want {
				t.Errorf("s3Retryable(wrapRetryableStatus(%d, err)) = %v, want %v", status, got, want)
			}
			// The wrapper must stay transparent either way: callers outside
			// this package keep matching the underlying sentinel.
			if !errors.Is(wrapped, errS3BucketRequestFailed) {
				t.Errorf("wrapRetryableStatus(%d, err) lost the underlying sentinel", status)
			}
		})
	}
}
