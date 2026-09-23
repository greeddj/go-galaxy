package collections

// Unit tests for failureRecorder, failureSummary and summaryError, one layer
// below TestFrozenInstallCorruptedPinPropagatesBothSentinels.

import (
	"errors"
	"fmt"
	"slices"
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

	errTestRoleCause0 = errors.New("role cause 0")
	errTestRoleCause1 = errors.New("role cause 1")

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
// collection and role causes two recorders observe, the failureSummary method
// under test, and the exact message and sentinel that method must produce.
type summaryHeadlineCase struct {
	name         string
	build        func(s failureSummary) error
	wantMsg      string
	wantSentinel error
	causes       []error
	roleCauses   []error
}

// summaryHeadlineCases enumerates each failureSummary headline method over
// collection failures, role failures and both, each row recording its own
// causes so its counts are visible in the message it demands.
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
		{
			// Role failures alone are named as roles, the collections left out.
			name:         "install roles only",
			build:        func(s failureSummary) error { return s.installError() },
			wantMsg:      "installation failed for 2 roles",
			wantSentinel: helpers.ErrInstallationFailed,
			roleCauses:   []error{errTestRoleCause0, errTestRoleCause1},
		},
		{
			name:         "warm one role",
			build:        func(s failureSummary) error { return s.warmError() },
			wantMsg:      "installation failed: warm failed for 1 role",
			wantSentinel: helpers.ErrInstallationFailed,
			roleCauses:   []error{errTestRoleCause0},
		},
		{
			name:         "outdated roles only",
			build:        func(s failureSummary) error { return s.outdatedError() },
			wantMsg:      "latest version lookup failed for 2 roles",
			wantSentinel: helpers.ErrLatestVersionLookupFailed,
			roleCauses:   []error{errTestRoleCause0, errTestRoleCause1},
		},
		{
			// Both kinds are named, each with its own count and number.
			name:         "install one collection and roles",
			build:        func(s failureSummary) error { return s.installError() },
			wantMsg:      "installation failed for 1 collection and 2 roles",
			wantSentinel: helpers.ErrInstallationFailed,
			causes:       []error{errTestCause0},
			roleCauses:   []error{errTestRoleCause0, errTestRoleCause1},
		},
	}
}

// TestSummaryHeadlineCases pins that each headline method renders only its
// headline, over a collection summary joined with a role summary, while
// errors.Is reaches its sentinel and every recorded cause.
func TestSummaryHeadlineCases(t *testing.T) {
	t.Parallel()

	for _, tc := range summaryHeadlineCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var cols, roles failureRecorder
			for _, c := range tc.causes {
				cols.record(c)
			}
			for _, c := range tc.roleCauses {
				roles.recordRole(c)
			}
			err := tc.build(cols.summary().join(roles.summary()))
			if got := err.Error(); got != tc.wantMsg {
				t.Fatalf("Error() = %q, want %q", got, tc.wantMsg)
			}
			if !errors.Is(err, tc.wantSentinel) {
				t.Fatalf("expected errors.Is %v, got %v", tc.wantSentinel, err)
			}
			for i, c := range slices.Concat(tc.causes, tc.roleCauses) {
				if !errors.Is(err, c) {
					t.Fatalf("expected errors.Is causes[%d] = %v, got %v", i, c, err)
				}
			}
		})
	}
}

// TestFailureRecorderCountsRolesApart pins that one recorder fed both kinds,
// as outdated feeds it, keeps the role count apart from the total.
func TestFailureRecorderCountsRolesApart(t *testing.T) {
	t.Parallel()
	var r failureRecorder
	r.recordRole(errTestRoleCause0)
	r.record(errTestCause0)
	r.recordRole(errTestRoleCause1)

	if got := r.count(); got != 3 {
		t.Fatalf("count() = %d, want 3", got)
	}
	summary := r.summary()
	if summary.count != 3 || summary.roles != 2 {
		t.Fatalf("summary counts = %d total, %d roles; want 3 and 2", summary.count, summary.roles)
	}
	const want = "latest version lookup failed for 1 collection and 2 roles"
	if got := summary.outdatedError().Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
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
