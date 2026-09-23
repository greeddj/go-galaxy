package collections

import (
	"context"
	"errors"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// downloadAttemptError tags one artifact-download attempt's failure with its
// HTTP status (0 when no response arrived) for downloadRetryable, and unwraps
// to the original error so sentinel matching is unchanged.
type downloadAttemptError struct {
	err    error
	status int
}

// Error returns the wrapped error's message unchanged.
func (e *downloadAttemptError) Error() string {
	return e.err.Error()
}

// Unwrap exposes the wrapped sentinel-carrying error.
func (e *downloadAttemptError) Unwrap() error {
	return e.err
}

// downloadRetryable reports whether a whole artifact download attempt should
// be retried: yes for a read stall, a retryable status or no response at all
// (unlike an API GET), never for offline, deadline, cancellation or bad bytes.
func downloadRetryable(err error) bool {
	if err == nil {
		return false
	}
	if isEarlyTerminalDownloadError(err) {
		return false
	}
	// Ahead of the context arms as defense in depth, should a stall cause ever
	// be rendered with %w and carry context.Canceled.
	if errors.Is(err, helpers.ErrReadStalled) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if isLateTerminalDownloadError(err) {
		return false
	}
	return isRetryableAttemptError(err)
}

// isEarlyTerminalDownloadError reports offline mode or this acquisition's
// deadline. It runs ahead of the ErrReadStalled check, so a stall that raced
// the deadline is terminal rather than burning a backoff.
func isEarlyTerminalDownloadError(err error) bool {
	return errors.Is(err, helpers.ErrOfflineMode) || errors.Is(err, helpers.ErrArtifactDownloadDeadline)
}

// isLateTerminalDownloadError reports a terminal content verdict: a sha256
// mismatch, an oversized body, non-tar.gz bytes, or no tar header within the
// probe's bound. The same URL would serve the same bytes again.
func isLateTerminalDownloadError(err error) bool {
	return errors.Is(err, helpers.ErrSHA256Mismatch) ||
		errors.Is(err, helpers.ErrResponseTooLarge) ||
		errors.Is(err, helpers.ErrArtifactNotTarGz) ||
		errors.Is(err, helpers.ErrArtifactTarHeaderNotFound)
}

// isRetryableAttemptError reports a *downloadAttemptError with no response
// (status 0) or a retryable HTTP status; any other error is not retryable.
func isRetryableAttemptError(err error) bool {
	if attemptErr, ok := errors.AsType[*downloadAttemptError](err); ok {
		if attemptErr.status == 0 {
			return true
		}
		return helpers.IsRetryableHTTPStatus(attemptErr.status)
	}
	return false
}
