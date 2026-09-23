package helpers

import (
	"context"
	mathrand "math/rand/v2"
	"time"
)

// RetryPolicy bounds a Retry run: MaxAttempts total tries (the first attempt
// plus up to MaxAttempts-1 retries), with a full-jitter exponential backoff
// bounded by Base and Cap between them.
type RetryPolicy struct {
	Base        time.Duration
	Cap         time.Duration
	MaxAttempts int
}

// Retry runs attempt up to p.MaxAttempts times until it succeeds or
// retryable refuses its error, sleeping BackoffDelay between tries; a ctx
// canceled mid-backoff returns ctx.Err() at once.
func Retry(ctx context.Context, p RetryPolicy, attempt func() error, retryable func(error) bool) error {
	for i := 0; ; i++ {
		err := attempt()
		if err == nil {
			return nil
		}
		if i+1 >= p.MaxAttempts || !retryable(err) {
			return err
		}
		timer := time.NewTimer(BackoffDelay(p.Base, p.Cap, i))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// BackoffDelay returns a full-jitter exponential backoff duration: a
// uniformly random value in [0, min(backoffCap, base*2^attempt)).
func BackoffDelay(base, backoffCap time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	delayCeiling := backoffCap
	// Guard against shifting a duration into overflow for large attempt
	// counts: once the shifted value would meet or exceed backoffCap,
	// clamp immediately instead of computing an undefined/negative shift.
	if attempt >= 0 && attempt < 63 {
		if scaled := base << uint(attempt); scaled > 0 && scaled < backoffCap {
			delayCeiling = scaled
		}
	}
	if delayCeiling <= 0 {
		return 0
	}
	//nolint:gosec // G404: jitter timing only, not security sensitive.
	return time.Duration(mathrand.Int64N(int64(delayCeiling)))
}
