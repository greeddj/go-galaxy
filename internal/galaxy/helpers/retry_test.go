package helpers

import (
	"context"
	"errors"
	"testing"
	"time"
)

// errRetryTestTransient is the failure every Retry test's attempt reports.
var errRetryTestTransient = errors.New("transient attempt failure")

// instantRetryPolicy bounds the backoff to [0, 1ns), which BackoffDelay
// rounds to zero, so attempt-counting tests never wait on a timer.
func instantRetryPolicy(maxAttempts int) RetryPolicy {
	return RetryPolicy{Base: time.Nanosecond, Cap: time.Nanosecond, MaxAttempts: maxAttempts}
}

// retryCounters counts how often Retry called attempt and retryable.
type retryCounters struct {
	attempts   int
	retryables int
}

// failingAttempt fails with errRetryTestTransient until it has failed
// failures times, then succeeds.
func (c *retryCounters) failingAttempt(failures int) func() error {
	return func() error {
		c.attempts++
		if c.attempts <= failures {
			return errRetryTestTransient
		}
		return nil
	}
}

// retryableAnswer counts each consultation and returns answer.
func (c *retryCounters) retryableAnswer(answer bool) func(error) bool {
	return func(error) bool {
		c.retryables++
		return answer
	}
}

// TestRetryStopsAtTheFirstSuccess pins that two transient failures are
// retried and the third, successful attempt ends the run with nil.
func TestRetryStopsAtTheFirstSuccess(t *testing.T) {
	t.Parallel()
	var c retryCounters
	err := Retry(context.Background(), instantRetryPolicy(4), c.failingAttempt(2), c.retryableAnswer(true))
	if err != nil {
		t.Fatalf("Retry = %v, want nil after the third attempt succeeds", err)
	}
	if c.attempts != 3 || c.retryables != 2 {
		t.Fatalf("attempts = %d, retryable consulted %d times; want 3 and 2", c.attempts, c.retryables)
	}
}

// TestRetryReturnsTheLastErrorOnceAttemptsRunOut pins that MaxAttempts bounds
// the total tries and that retryable is not consulted for the final failure.
func TestRetryReturnsTheLastErrorOnceAttemptsRunOut(t *testing.T) {
	t.Parallel()
	var c retryCounters
	err := Retry(context.Background(), instantRetryPolicy(4), c.failingAttempt(100), c.retryableAnswer(true))
	if !errors.Is(err, errRetryTestTransient) {
		t.Fatalf("Retry = %v, want errRetryTestTransient", err)
	}
	if c.attempts != 4 || c.retryables != 3 {
		t.Fatalf("attempts = %d, retryable consulted %d times; want 4 and 3", c.attempts, c.retryables)
	}
}

// TestRetryStopsWhenRetryableRefuses pins that a refused error is returned
// after one attempt even though attempts remain.
func TestRetryStopsWhenRetryableRefuses(t *testing.T) {
	t.Parallel()
	var c retryCounters
	err := Retry(context.Background(), instantRetryPolicy(4), c.failingAttempt(100), c.retryableAnswer(false))
	if !errors.Is(err, errRetryTestTransient) {
		t.Fatalf("Retry = %v, want errRetryTestTransient", err)
	}
	if c.attempts != 1 || c.retryables != 1 {
		t.Fatalf("attempts = %d, retryable consulted %d times; want 1 and 1", c.attempts, c.retryables)
	}
}

// TestRetryRunsOnceWithoutARetryBudget pins that MaxAttempts of 1, 0 or a
// negative value still runs the attempt exactly once and never retries.
func TestRetryRunsOnceWithoutARetryBudget(t *testing.T) {
	t.Parallel()
	for _, maxAttempts := range []int{1, 0, -3} {
		var c retryCounters
		err := Retry(context.Background(), instantRetryPolicy(maxAttempts), c.failingAttempt(100), c.retryableAnswer(true))
		if !errors.Is(err, errRetryTestTransient) {
			t.Fatalf("MaxAttempts %d: Retry = %v, want errRetryTestTransient", maxAttempts, err)
		}
		if c.attempts != 1 || c.retryables != 0 {
			t.Fatalf("MaxAttempts %d: attempts = %d, retryable consulted %d times; want 1 and 0",
				maxAttempts, c.attempts, c.retryables)
		}
	}
}

// TestRetryReturnsTheContextErrorDuringBackoff pins that a context ended
// before the backoff wins over an hour-long timer and is returned as is.
func TestRetryReturnsTheContextErrorDuringBackoff(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	policy := RetryPolicy{Base: time.Hour, Cap: time.Hour, MaxAttempts: 4}
	var c retryCounters
	err := Retry(ctx, policy, c.failingAttempt(100), c.retryableAnswer(true))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Retry = %v, want context.Canceled", err)
	}
	if errors.Is(err, errRetryTestTransient) {
		t.Fatalf("Retry = %v, want the context error, not the attempt's", err)
	}
	if c.attempts != 1 {
		t.Fatalf("attempts = %d, want 1: no attempt may start after the context ended", c.attempts)
	}
}

// TestBackoffDelayIsZeroWithoutABase pins the zero answer for a base or cap
// that leaves no positive ceiling.
func TestBackoffDelayIsZeroWithoutABase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		base     time.Duration
		delayCap time.Duration
		attempt  int
	}{
		{name: "zero base", base: 0, delayCap: time.Second, attempt: 2},
		{name: "negative base", base: -time.Millisecond, delayCap: time.Second, attempt: 0},
		{name: "zero cap", base: time.Millisecond, delayCap: 0, attempt: 0},
		{name: "negative cap", base: time.Millisecond, delayCap: -time.Second, attempt: 5},
	}
	for _, tc := range cases {
		if got := BackoffDelay(tc.base, tc.delayCap, tc.attempt); got != 0 {
			t.Errorf("%s: BackoffDelay(%v, %v, %d) = %v, want 0", tc.name, tc.base, tc.delayCap, tc.attempt, got)
		}
	}
}

// TestBackoffDelayStaysUnderItsCeiling samples each case and pins the jitter
// to [0, min(cap, base*2^attempt)), the cap standing in for a shift that
// would overflow or is out of range.
func TestBackoffDelayStaysUnderItsCeiling(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		base     time.Duration
		delayCap time.Duration
		ceiling  time.Duration
		attempt  int
	}{
		{name: "first attempt", base: time.Millisecond, delayCap: time.Second, attempt: 0, ceiling: time.Millisecond},
		{name: "doubled per attempt", base: time.Millisecond, delayCap: time.Second, attempt: 3, ceiling: 8 * time.Millisecond},
		{name: "cap reached", base: time.Millisecond, delayCap: 5 * time.Millisecond, attempt: 4, ceiling: 5 * time.Millisecond},
		{name: "negative attempt", base: time.Millisecond, delayCap: 7 * time.Millisecond, attempt: -1, ceiling: 7 * time.Millisecond},
		{name: "attempt past the shift", base: time.Millisecond, delayCap: 9 * time.Millisecond, attempt: 63, ceiling: 9 * time.Millisecond},
		{name: "shift overflows", base: 1 << 61, delayCap: 3 * time.Millisecond, attempt: 4, ceiling: 3 * time.Millisecond},
	}
	for _, tc := range cases {
		for range 200 {
			got := BackoffDelay(tc.base, tc.delayCap, tc.attempt)
			if got < 0 || got >= tc.ceiling {
				t.Fatalf("%s: BackoffDelay(%v, %v, %d) = %v, want within [0, %v)",
					tc.name, tc.base, tc.delayCap, tc.attempt, got, tc.ceiling)
			}
		}
	}
}
