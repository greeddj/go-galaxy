package collections

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// errTestTransport and errTestLocalIO stand in for a raw transport failure
// and an unrelated local filesystem failure in downloadRetryable's table.
var (
	errTestTransport = errors.New("dial tcp: connection refused")
	errTestLocalIO   = errors.New("write /tmp/x: no space left on device")
)

// TestDownloadRetryable pins the artifact-download retry classification: a
// stall is retryable in either rendering, content failures are terminal, and
// unlike the Galaxy API GET predicate a bare transport failure is retried.
func TestDownloadRetryable(t *testing.T) {
	t.Parallel()

	// stalledProduction mirrors watchdogBody.Read: the cause rendered with %v,
	// so errors.Is does not reach context.Canceled.
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape watchdogBody.Read builds.
	stalledProduction := fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, time.Second, context.Canceled)
	// stalledSynthetic also wraps context.Canceled with %w, pinning that
	// downloadRetryable classifies ErrReadStalled before the context check.
	stalledSynthetic := fmt.Errorf("%w: no data for %s: %w", helpers.ErrReadStalled, time.Second, context.Canceled)
	shaMismatch := fmt.Errorf("%w: aaaa != bbbb", helpers.ErrSHA256Mismatch)
	noTarHeader := fmt.Errorf("%w: /tmp/a", helpers.ErrArtifactTarHeaderNotFound)

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
		{name: "sha256 mismatch after a complete read is terminal", err: shaMismatch, want: false},
		{name: "an oversized artifact download is never retried", err: helpers.ErrResponseTooLarge, want: false},
		{
			// Not a production shape: a shape-probe refusal on a retryable
			// status pins the explicit terminal arm ahead of the status check.
			name: "a shape-probe refusal carried on a retryable status is still terminal",
			err:  &downloadAttemptError{err: noTarHeader, status: http.StatusServiceUnavailable},
			want: false,
		},
		{
			// A regression pin only: the default-deny fallthrough would also
			// answer false, since the deadline error does not wrap its cause.
			name: "an expired artifact download deadline is never retried",
			err:  fmt.Errorf("%w after 1s: boom", helpers.ErrArtifactDownloadDeadline),
			want: false,
		},
		{
			name: "retryable status is retryable",
			err:  &downloadAttemptError{err: fmt.Errorf("%w: boom", helpers.ErrDownloadFailed), status: http.StatusServiceUnavailable},
			want: true,
		},
		{
			name: "not found status is not retryable",
			err:  &downloadAttemptError{err: fmt.Errorf("%w: boom", helpers.ErrDownloadFailed), status: http.StatusNotFound},
			want: false,
		},
		{
			name: "a bare transport-level failure (no response at all) is retryable",
			err:  &downloadAttemptError{err: errTestTransport},
			want: true,
		},
		{name: "an unclassified local error is not retryable", err: errTestLocalIO, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := downloadRetryable(tc.err); got != tc.want {
				t.Errorf("downloadRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
