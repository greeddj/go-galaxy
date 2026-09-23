package collections

// Tests for signatureDeadlineError, above all its %v-not-%w rendering, which
// keeps a spent signature budget from classifying as the caller's own Ctrl-C.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// signatureBudget is the budget every row below passes. Its value never
// decides a verdict; it only has to appear in the rendered message.
const signatureBudget = time.Second

// The two errors a gather produces once its budget expires: the cause behind
// helpers.ErrSignatureSourceUnavailable is context.DeadlineExceeded, or
// context.Canceled when a watchdog's derived cancel wins the race.
var (
	errSignatureCauseDeadline = fmt.Errorf("%w: %q: %w",
		helpers.ErrSignatureSourceUnavailable, "https://sigs.example/a.asc", context.DeadlineExceeded)
	errSignatureCauseCanceled = fmt.Errorf("%w: %q: %w",
		helpers.ErrSignatureSourceUnavailable, "https://sigs.example/a.asc", context.Canceled)
)

// signatureDeadlineCase is one row of the table below.
type signatureDeadlineCase struct {
	buildParent func() (context.Context, context.CancelFunc)
	buildSig    func(parent context.Context) (context.Context, context.CancelFunc)
	err         error
	name        string
	wantSame    bool
}

// expiredSigCtx builds a signature budget that has already expired under a live
// parent, which is the state every normalizing row needs.
func expiredSigCtx(parent context.Context) (context.Context, context.CancelFunc) {
	sigCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
	<-sigCtx.Done()
	return sigCtx, cancel
}

// liveParent builds a parent context nothing has ended.
func liveParent() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// signatureDeadlineCases is TestSignatureFetchDeadlineClassification's table;
// the first two rows show the fixture can normalize at all.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var signatureDeadlineCases = []signatureDeadlineCase{
	{
		name:        "own budget fired, parent live, cause wraps context.DeadlineExceeded: normalized",
		buildParent: liveParent,
		buildSig:    expiredSigCtx,
		err:         errSignatureCauseDeadline,
	},
	{
		name:        "own budget fired, parent live, cause wraps context.Canceled: normalized",
		buildParent: liveParent,
		buildSig:    expiredSigCtx,
		err:         errSignatureCauseCanceled,
	},
	{
		// The caller's own cancellation outranks the budget: a Ctrl-C during a
		// signature fetch has to stay reachable as one, which is the single
		// exception cmd/go-galaxy/exitcode's isCanceled documents.
		name: "parent canceled first: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			parent, cancel := context.WithCancel(context.Background())
			cancel()
			return parent, cancel
		},
		buildSig: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, signatureBudget)
		},
		err:      errSignatureCauseCanceled,
		wantSame: true,
	},
	{
		// sigCtx inherits its parent's expiry, so sigCtx.Err() alone cannot tell
		// "my own budget expired" from "my parent's did". The parent check is
		// what separates them.
		name: "parent's own deadline expired first: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			parent, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			<-parent.Done()
			return parent, cancel
		},
		buildSig: func(parent context.Context) (context.Context, context.CancelFunc) {
			sigCtx, cancel := context.WithTimeout(parent, signatureBudget)
			<-sigCtx.Done()
			return sigCtx, cancel
		},
		err:      errSignatureCauseDeadline,
		wantSame: true,
	},
	{
		name:        "sigCtx still live: unchanged",
		buildParent: liveParent,
		buildSig: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, time.Hour)
		},
		err:      errSignatureCauseDeadline,
		wantSame: true,
	},
	{
		name:        "nil error stays nil",
		buildParent: liveParent,
		buildSig:    expiredSigCtx,
		err:         nil,
		wantSame:    true,
	},
}

// TestSignatureFetchDeadlineClassification pins that a normalized error matches
// helpers.ErrSignatureFetchDeadline and neither context sentinel, so a slow
// signature host is never reported as a caught Ctrl-C.
func TestSignatureFetchDeadlineClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range signatureDeadlineCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parent, parentCancel := tc.buildParent()
			defer parentCancel()
			sigCtx, sigCancel := tc.buildSig(parent)
			defer sigCancel()

			got := signatureDeadlineError(parent, sigCtx, signatureBudget, tc.err)
			if tc.wantSame {
				if tc.err == nil {
					if got != nil {
						t.Fatalf("signatureDeadlineError = %v, want nil", got)
					}
					return
				}
				if !errors.Is(got, tc.err) {
					t.Fatalf("signatureDeadlineError = %v, want unchanged %v", got, tc.err)
				}
				return
			}
			if !errors.Is(got, helpers.ErrSignatureFetchDeadline) {
				t.Fatalf("signatureDeadlineError = %v, want errors.Is helpers.ErrSignatureFetchDeadline", got)
			}
			if errors.Is(got, context.DeadlineExceeded) {
				t.Fatalf("signatureDeadlineError = %v, must not match context.DeadlineExceeded", got)
			}
			if errors.Is(got, context.Canceled) {
				t.Fatalf("signatureDeadlineError = %v, must not match context.Canceled", got)
			}
			if !strings.Contains(got.Error(), tc.err.Error()) {
				t.Fatalf("signatureDeadlineError = %q, want it to carry the cause %q", got.Error(), tc.err.Error())
			}
		})
	}
}

// TestSignatureDeadlineErrorIsIdempotent pins that re-normalizing leaves the
// message unchanged; the fixture wraps context.DeadlineExceeded with %w so a
// second pass would have a context sentinel to re-wrap.
func TestSignatureDeadlineErrorIsIdempotent(t *testing.T) {
	t.Parallel()
	parent, parentCancel := liveParent()
	defer parentCancel()
	sigCtx, sigCancel := expiredSigCtx(parent)
	defer sigCancel()

	alreadyNormalized := fmt.Errorf("%w after 1s: %w", helpers.ErrSignatureFetchDeadline, context.DeadlineExceeded)
	got := signatureDeadlineError(parent, sigCtx, signatureBudget, alreadyNormalized)
	if got.Error() != alreadyNormalized.Error() {
		t.Fatalf("re-normalizing changed the message: once=%q twice=%q", alreadyNormalized.Error(), got.Error())
	}
	if n := strings.Count(got.Error(), helpers.ErrSignatureFetchDeadline.Error()); n != 1 {
		t.Fatalf("sentinel text appears %d times in %q, want exactly 1", n, got.Error())
	}
}
