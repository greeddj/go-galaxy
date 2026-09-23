package cache

import (
	"context"
	"errors"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// fetchRetryable reports whether a Galaxy API GET failure is worth retrying:
// only a retryable *HTTPStatusError status or helpers.ErrReadStalled is. Unlike
// an artifact download, a raw transport failure fails fast for its caller.
func fetchRetryable(err error) bool {
	if err == nil {
		return false
	}
	if isEarlyTerminalFetchError(err) {
		return false
	}
	// Checked ahead of the context arms as defense in depth, in case a stall
	// cause is ever rendered with %w and carries context.Canceled.
	if errors.Is(err, helpers.ErrReadStalled) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A response that overran its size cap is terminal: retrying would just
	// re-fetch the same oversized body.
	if errors.Is(err, helpers.ErrResponseTooLarge) {
		return false
	}
	if statusErr, ok := errors.AsType[*HTTPStatusError](err); ok {
		return helpers.IsRetryableHTTPStatus(statusErr.Code)
	}
	return false
}

// isEarlyTerminalFetchError reports whether err is offline mode or this
// request's own metadata deadline, checked ahead of ErrReadStalled so a stall
// that raced the deadline stays terminal, as isEarlyTerminalDownloadError does.
func isEarlyTerminalFetchError(err error) bool {
	return errors.Is(err, helpers.ErrOfflineMode) || errors.Is(err, helpers.ErrMetadataFetchDeadline)
}
