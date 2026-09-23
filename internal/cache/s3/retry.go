package s3

import (
	"context"
	"errors"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// retryableStatusError marks a per-verb HTTP failure as safe to retry, while
// Unwrap still exposes the original sentinel-wrapped error, so callers keep
// matching the underlying sentinel with errors.Is.
type retryableStatusError struct {
	err    error
	status int
}

// Error returns the wrapped error's message unchanged.
func (e *retryableStatusError) Error() string {
	return e.err.Error()
}

// Unwrap exposes the wrapped sentinel-carrying error.
func (e *retryableStatusError) Unwrap() error {
	return e.err
}

// wrapRetryableStatus marks err retryable when helpers.IsRetryableHTTPStatus
// accepts status and returns it unchanged otherwise. The set is shared with the
// Galaxy paths on purpose: a private copy here could drift from it.
func wrapRetryableStatus(status int, err error) error {
	if err == nil || !helpers.IsRetryableHTTPStatus(status) {
		return err
	}
	return &retryableStatusError{status: status, err: err}
}

// s3RetryPolicy is the fixed policy of every idempotent S3 verb (GET, HEAD,
// DELETE, list, unconditional PUT): bounded attempts with full-jitter backoff.
// Unlike the lock's own backoff, it is not configurable per call site.
func s3RetryPolicy() helpers.RetryPolicy {
	return helpers.RetryPolicy{
		Base:        s3RetryBackoffBase,
		Cap:         s3RetryBackoffCap,
		MaxAttempts: s3RetryMaxAttempts,
	}
}

// s3RetryableFor binds ctx to s3Retryable for helpers.Retry; every caller
// passes the same ctx it threads into the attempt's request.
func s3RetryableFor(ctx context.Context) func(error) bool {
	return func(err error) bool { return s3Retryable(ctx, err) }
}

// s3Retryable is default-deny: it retries a stalled read, a retryable status,
// and errS3TransportFailed while ctx is live (judged by ctx, not error shape).
// Gating the status arm on ctx too would report a status error, not ctx.Err().
func s3Retryable(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, helpers.ErrOfflineMode) {
		return false
	}
	if errors.Is(err, helpers.ErrReadStalled) {
		return true
	}
	// An oversized listing or batch-delete response is refused explicitly, so
	// a reordering of the default-deny fallthrough cannot start retrying it.
	if errors.Is(err, helpers.ErrResponseTooLarge) {
		return false
	}
	if errors.Is(err, errS3TransportFailed) {
		return ctx.Err() == nil
	}
	_, ok := errors.AsType[*retryableStatusError](err)
	return ok
}
