package collections

// Unit tests for failureRecorder, failureSummary and summaryError, one layer
// below TestFrozenInstallCorruptedPinPropagatesBothSentinels.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Distinct per-collection causes for this file's tests, declared as static
// sentinels only to satisfy err113.
var (
	errTestCause0 = errors.New("cause 0")
	errTestCause1 = errors.New("cause 1")
	errTestCause2 = errors.New("cause 2")

	errTestWarmCause0 = errors.New("warm cause 0")
	errTestWarmCause1 = errors.New("warm cause 1")

	errTestOutdatedCause0 = errors.New("outdated cause 0")
	errTestOutdatedCause1 = errors.New("outdated cause 1")

	errTestSaveFailure     = errors.New("simulated save failure")
	errTestCollectionCause = errors.New("collection cause")
)

// TestFailureRecorderSummaryIsEmptyWhenNothingRecorded pins that a zero-value
// recorder reports a zero count, a nil cause and nil headline errors.
func TestFailureRecorderSummaryIsEmptyWhenNothingRecorded(t *testing.T) {
	t.Parallel()
	var r failureRecorder

	if got := r.count(); got != 0 {
		t.Fatalf("count() = %d, want 0", got)
	}

	summary := r.summary()
	if summary.count != 0 {
		t.Fatalf("summary.count = %d, want 0", summary.count)
	}
	if summary.cause != nil {
		t.Fatalf("summary.cause = %v, want nil", summary.cause)
	}
	if err := summary.installError(); err != nil {
		t.Fatalf("installError() = %v, want nil", err)
	}
	if err := summary.warmError(); err != nil {
		t.Fatalf("warmError() = %v, want nil", err)
	}
}

// TestFailureRecorderIsConcurrencySafe pins that 64 concurrent record calls
// are all counted and all reachable via errors.Is through installError; under
// -race it also catches record appending without the mutex.
func TestFailureRecorderIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	const workers = 64

	var r failureRecorder
	sentinels := make([]error, workers)
	for i := range sentinels {
		sentinels[i] = fmt.Errorf("worker %d failed: %w", i, helpers.ErrDownloadFailed)
	}

	var wg sync.WaitGroup
	for i := range sentinels {
		wg.Go(func() {
			r.record(sentinels[i])
		})
	}
	wg.Wait()

	if got := r.count(); got != workers {
		t.Fatalf("count() = %d, want %d", got, workers)
	}

	summary := r.summary()
	if summary.count != workers {
		t.Fatalf("summary.count = %d, want %d", summary.count, workers)
	}
	err := summary.installError()
	for i, sentinel := range sentinels {
		if !errors.Is(err, sentinel) {
			t.Fatalf("installError() does not match sentinels[%d] = %v", i, sentinel)
		}
	}
}

// summaryHeadlineCase is one table entry for TestSummaryHeadlineCases: the
// causes a failureRecorder observes, the failureSummary method under test,
// and the exact message and sentinel that method must produce.
type summaryHeadlineCase struct {
	name         string
	build        func(s failureSummary) error
	wantMsg      string
	wantSentinel error
	causes       []error
}

// summaryHeadlineCases enumerates one row per failureSummary headline method,
// each recording its own causes so a row's count is visible in the message it
// demands.
func summaryHeadlineCases() []summaryHeadlineCase {
	return []summaryHeadlineCase{
		{
			// installError's message is the bare headline with no cause text,
			// while Unwrap still reaches the sentinel and every cause.
			name:         "install",
			build:        func(s failureSummary) error { return s.installError() },
			wantMsg:      "installation failed for 3 collections",
			wantSentinel: helpers.ErrInstallationFailed,
			causes:       []error{errTestCause0, errTestCause1, errTestCause2},
		},
		{
			// warmError renders its own headline under the same one-line contract.
			name:         "warm",
			build:        func(s failureSummary) error { return s.warmError() },
			wantMsg:      "installation failed: warm failed for 2 collections",
			wantSentinel: helpers.ErrInstallationFailed,
			causes:       []error{errTestWarmCause0, errTestWarmCause1},
		},
		{
			// outdatedError renders its own headline and sentinel under the
			// same one-line contract.
			name:         "outdated",
			build:        func(s failureSummary) error { return s.outdatedError() },
			wantMsg:      "latest version lookup failed for 2 collections",
			wantSentinel: helpers.ErrLatestVersionLookupFailed,
			causes:       []error{errTestOutdatedCause0, errTestOutdatedCause1},
		},
	}
}

// TestSummaryHeadlineCases pins that each headline method renders only its
// headline while errors.Is reaches its sentinel and every recorded cause.
func TestSummaryHeadlineCases(t *testing.T) {
	t.Parallel()

	for _, tc := range summaryHeadlineCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var r failureRecorder
			for _, c := range tc.causes {
				r.record(c)
			}
			err := tc.build(r.summary())
			if got := err.Error(); got != tc.wantMsg {
				t.Fatalf("Error() = %q, want %q", got, tc.wantMsg)
			}
			if !errors.Is(err, tc.wantSentinel) {
				t.Fatalf("expected errors.Is %v, got %v", tc.wantSentinel, err)
			}
			for i, c := range tc.causes {
				if !errors.Is(err, c) {
					t.Fatalf("expected errors.Is causes[%d] = %v, got %v", i, c, err)
				}
			}
		})
	}
}

// TestFailureSummarySurvivesSaveAnnotation pins that annotateSaveFailure keeps
// the headline, the per-collection cause and the save failure matchable, and
// the message on one line.
func TestFailureSummarySurvivesSaveAnnotation(t *testing.T) {
	t.Parallel()

	var r failureRecorder
	r.record(errTestCollectionCause)
	summary := r.summary()

	err := annotateSaveFailure(summary.installError(), errTestSaveFailure)
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is helpers.ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, errTestCollectionCause) {
		t.Fatalf("expected errors.Is cause, got %v", err)
	}
	if !errors.Is(err, errTestSaveFailure) {
		t.Fatalf("expected errors.Is errSaveSentinel, got %v", err)
	}

	const wantSubstring = "; snapshot save failed:"
	got := err.Error()
	if !strings.Contains(got, wantSubstring) {
		t.Fatalf("Error() = %q, want it to contain %q", got, wantSubstring)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("Error() = %q, want a single line", got)
	}
}
