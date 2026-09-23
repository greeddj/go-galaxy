package cache

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// errTestTransport is a static stand-in for a raw transport-level failure
// (e.g. a dial or connection reset), used only to exercise fetchRetryable's
// classification without ever being returned by production code.
var errTestTransport = errors.New("dial tcp: connection refused")

// TestFetchRetryable pins the retry classification: a stall is retryable in
// both renderings, while a raw caller cancellation and a raw transport error
// (no HTTPStatusError) are not, for a Galaxy API GET.
func TestFetchRetryable(t *testing.T) {
	t.Parallel()

	// stalledProduction mirrors watchdogBody.Read: the cause is rendered with
	// %v, so it does not carry context.Canceled through errors.Is.
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape watchdogBody.Read builds.
	stalledProduction := fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, time.Second, context.Canceled)
	// stalledSynthetic wraps context.Canceled with %w, a shape no producer
	// builds, to pin that ErrReadStalled is classified before context.Canceled.
	stalledSynthetic := fmt.Errorf("%w: no data for %s: %w", helpers.ErrReadStalled, time.Second, context.Canceled)

	cases := []struct {
		err  error
		name string
		want bool
	}{
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
			name: "retryable status 503 is retryable",
			err:  &HTTPStatusError{URL: "https://example.com", Status: "503 Service Unavailable", Code: http.StatusServiceUnavailable},
			want: true,
		},
		{
			name: "not found 404 is not retryable",
			err:  &HTTPStatusError{URL: "https://example.com", Status: "404 Not Found", Code: http.StatusNotFound},
			want: false,
		},
		{name: "a raw transport error is not retryable for an API GET", err: errTestTransport, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fetchRetryable(tc.err); got != tc.want {
				t.Errorf("fetchRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestFetchRetryableTreatsTheMetadataDeadlineAsTerminal pins that an error
// carrying both ErrMetadataFetchDeadline and ErrReadStalled is terminal; only
// the combined error distinguishes the check order, a bare sentinel does not.
func TestFetchRetryableTreatsTheMetadataDeadlineAsTerminal(t *testing.T) {
	t.Parallel()

	combined := fmt.Errorf("%w: %w", helpers.ErrMetadataFetchDeadline, helpers.ErrReadStalled)
	if got := fetchRetryable(combined); got {
		t.Errorf("fetchRetryable(%v) = %v, want false (the deadline must resolve deterministically toward terminal)", combined, got)
	}

	// Control: a bare ErrReadStalled, with no deadline sentinel in the tree,
	// is retryable - proving this table can produce true at all.
	if got := fetchRetryable(helpers.ErrReadStalled); !got {
		t.Errorf("fetchRetryable(%v) = %v, want true (control: proves the table can produce true)", helpers.ErrReadStalled, got)
	}
}
