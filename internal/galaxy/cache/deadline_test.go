package cache

// This file pins deadlineError's classification table against both sentinels,
// MetadataDeadlineError's HTTP-status pass-through, and idempotence.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// deadlineTestBudget is the fixed budget every deadlineError case in this
// file passes; its value is irrelevant to the classification, only its
// presence in the rendered message matters.
const deadlineTestBudget = time.Second

// errDeadlineCauseWrapsDeadlineExceeded and errDeadlineCauseWrapsCanceled
// mirror a stalled request once dlCtx expires: the ordinary deadline, and the
// watchdog's own cancel racing it.
var (
	errDeadlineCauseWrapsDeadlineExceeded = fmt.Errorf("client.Do: %w", context.DeadlineExceeded)
	errDeadlineCauseWrapsCanceled         = fmt.Errorf("body read: %w", context.Canceled)
)

// deadlineErrorCase is one deadlineErrorCases table row, run against every
// sentinel in deadlineErrorSentinels.
type deadlineErrorCase struct {
	buildParent func() (context.Context, context.CancelFunc)
	buildDl     func(parent context.Context) (context.Context, context.CancelFunc)
	err         error
	name        string
	wantSame    bool
}

// deadlineErrorSentinels is every sentinel deadlineError normalizes into,
// each exercised against the identical table below.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var deadlineErrorSentinels = []error{helpers.ErrMetadataFetchDeadline, helpers.ErrStateObjectDeadline}

// deadlineErrorCases is TestDeadlineErrorClassification's table; its first two
// rows are positive controls, so an "unchanged" row is a real refusal.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var deadlineErrorCases = []deadlineErrorCase{
	{
		name: "own deadline fired, parent live, cause wraps context.DeadlineExceeded: normalized",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err: errDeadlineCauseWrapsDeadlineExceeded,
	},
	{
		name: "own deadline fired, parent live, cause wraps context.Canceled: normalized",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err: errDeadlineCauseWrapsCanceled,
	},
	{
		// An HTTP status error carries no context signal, so it passes through
		// even as the budget expires; relabeling it would break status routing.
		name: "own deadline fired, parent live, cause is an HTTP status error: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      &HTTPStatusError{URL: "https://example.com", Status: "404 Not Found", Code: http.StatusNotFound},
		wantSame: true,
	},
	{
		// The state-object side of the identical precondition: a corrupt
		// registry's actionable message must survive even though the budget
		// expired in the same instant.
		name: "own deadline fired, parent live, cause is a corrupt project registry error: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      helpers.ErrCorruptProjectRegistry,
		wantSame: true,
	},
	{
		name: "parent explicitly canceled: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			parent, cancel := context.WithCancel(context.Background())
			cancel()
			return parent, cancel
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, deadlineTestBudget)
		},
		err:      context.Canceled,
		wantSame: true,
	},
	{
		// dlCtx inherits the parent's expiry, so only the parent.Err() check
		// tells an inherited deadline from this operation's own.
		name: "parent's own deadline (not this operation's budget) expired first: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			parent, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			<-parent.Done()
			return parent, cancel
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, deadlineTestBudget)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      errDeadlineCauseWrapsDeadlineExceeded,
		wantSame: true,
	},
	{
		name: "dlCtx still live: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, time.Hour)
		},
		err:      errDeadlineCauseWrapsDeadlineExceeded,
		wantSame: true,
	},
	{
		name: "nil error stays nil",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      nil,
		wantSame: true,
	},
}

// TestDeadlineErrorClassification pins deadlineError's normalization table,
// run against every sentinel it is used with.
func TestDeadlineErrorClassification(t *testing.T) {
	t.Parallel()
	for _, sentinel := range deadlineErrorSentinels {
		for _, tc := range deadlineErrorCases {
			t.Run(sentinel.Error()+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				parent, parentCancel := tc.buildParent()
				defer parentCancel()
				dlCtx, dlCancel := tc.buildDl(parent)
				defer dlCancel()

				got := deadlineError(parent, dlCtx, deadlineTestBudget, sentinel, tc.err)
				if tc.wantSame {
					assertDeadlineErrorUnchanged(t, got, tc.err)
					return
				}
				assertDeadlineErrorNormalized(t, got, sentinel, tc.err)
			})
		}
	}
}

// TestMetadataDeadlineErrorPreservesHTTPStatusRouting pins that an
// *HTTPStatusError keeps its Code through errors.As without gaining the
// sentinel, while a context-carrying cause on the same fixture does gain it.
func TestMetadataDeadlineErrorPreservesHTTPStatusRouting(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	dlCtx, dlCancel := context.WithTimeout(parent, time.Nanosecond)
	defer dlCancel()
	<-dlCtx.Done()

	statusErr := &HTTPStatusError{URL: "https://example.com", Status: "404 Not Found", Code: http.StatusNotFound}
	got := MetadataDeadlineError(parent, dlCtx, deadlineTestBudget, statusErr)

	var route *HTTPStatusError
	if !errors.As(got, &route) {
		t.Fatalf("errors.As(got, &*HTTPStatusError) failed on %v, want success (the server walk consumes this property)", got)
	}
	if route.Code != http.StatusNotFound {
		t.Fatalf("route.Code = %d, want %d", route.Code, http.StatusNotFound)
	}
	if errors.Is(got, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("MetadataDeadlineError(%v) = %v, must not carry the sentinel", statusErr, got)
	}

	positiveControl := MetadataDeadlineError(parent, dlCtx, deadlineTestBudget, errDeadlineCauseWrapsDeadlineExceeded)
	if !errors.Is(positiveControl, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("positive control: MetadataDeadlineError = %v, want errors.Is ErrMetadataFetchDeadline", positiveControl)
	}
}

// TestDeadlineErrorIsIdempotent pins that re-normalizing never doubles the
// sentinel. alreadyNormalized double-wraps with %w, a shape no producer builds,
// because only it reaches the idempotence check past the causal precondition.
func TestDeadlineErrorIsIdempotent(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	dlCtx, dlCancel := context.WithTimeout(parent, time.Nanosecond)
	defer dlCancel()
	<-dlCtx.Done()

	once := deadlineError(parent, dlCtx, deadlineTestBudget, helpers.ErrMetadataFetchDeadline, errDeadlineCauseWrapsDeadlineExceeded)
	twice := deadlineError(parent, dlCtx, deadlineTestBudget, helpers.ErrMetadataFetchDeadline, once)
	if twice.Error() != once.Error() {
		t.Fatalf("re-normalizing the real shape changed the message: once=%q twice=%q", once.Error(), twice.Error())
	}

	alreadyNormalized := fmt.Errorf("%w after 1s: %w", helpers.ErrMetadataFetchDeadline, context.DeadlineExceeded)
	reNormalized := deadlineError(parent, dlCtx, deadlineTestBudget, helpers.ErrMetadataFetchDeadline, alreadyNormalized)
	if reNormalized.Error() != alreadyNormalized.Error() {
		t.Fatalf("re-normalizing the isolating fixture changed the message: once=%q twice=%q",
			alreadyNormalized.Error(), reNormalized.Error())
	}
	if n := strings.Count(reNormalized.Error(), helpers.ErrMetadataFetchDeadline.Error()); n != 1 {
		t.Fatalf("sentinel text appears %d times in %q, want exactly 1 (idempotency must not double-wrap)", n, reNormalized.Error())
	}
}

// assertDeadlineErrorUnchanged fails the test unless got is want, left
// untouched by deadlineError.
func assertDeadlineErrorUnchanged(t *testing.T, got, want error) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("deadlineError = %v, want nil", got)
		}
		return
	}
	if !errors.Is(got, want) {
		t.Fatalf("deadlineError = %v, want unchanged %v", got, want)
	}
}

// assertDeadlineErrorNormalized fails unless got matches sentinel, matches
// neither context error (the %v-not-%w contract), and contains cause's text.
func assertDeadlineErrorNormalized(t *testing.T, got, sentinel, cause error) {
	t.Helper()
	if !errors.Is(got, sentinel) {
		t.Fatalf("deadlineError = %v, want errors.Is %v", got, sentinel)
	}
	if errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("deadlineError = %v, must not match context.DeadlineExceeded", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("deadlineError = %v, must not match context.Canceled", got)
	}
	if !strings.Contains(got.Error(), cause.Error()) {
		t.Fatalf("deadlineError = %q, want it to contain the cause %q", got.Error(), cause.Error())
	}
}
