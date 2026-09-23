package collections

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// failureRecorder is a concurrency-safe collector of per-collection install
// failures: an atomic count for the cheap level-break check, plus each cause
// so the run's error names what went wrong, not only how many failed.
type failureRecorder struct {
	causes []error
	mu     sync.Mutex
	n      atomic.Int32
}

// record appends err to the recorder's causes and bumps its count. It is the
// only writer of either field; every other method only reads.
func (r *failureRecorder) record(err error) {
	r.mu.Lock()
	r.causes = append(r.causes, err)
	// Bumped under the lock so summary never sees a count that trails its
	// causes, whatever order callers run in.
	r.n.Add(1)
	r.mu.Unlock()
}

// count reports the number of failures recorded so far, as a lock-free atomic
// load for the level-break check between install levels.
func (r *failureRecorder) count() int32 {
	return r.n.Load()
}

// summary snapshots the recorder into an immutable failureSummary whose count
// always matches its causes, even while workers still run, because record
// moves both fields under one lock.
func (r *failureRecorder) summary() failureSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	// errors.Join of no causes is nil and allocates nothing, so the success
	// path needs no explicit empty check.
	return failureSummary{count: r.n.Load(), cause: errors.Join(r.causes...)}
}

// failureSummary is an immutable, copyable snapshot of a failureRecorder; the
// recorder holds a mutex and never leaves its frame, so callers pass this.
type failureSummary struct {
	// cause is the errors.Join of every recorded cause; nil when count == 0.
	cause error
	count int32
}

// join folds another summary into this one, adding counts and joining
// causes, so separately recorded collection and role failures report as one.
func (s failureSummary) join(other failureSummary) failureSummary {
	return failureSummary{count: s.count + other.count, cause: errors.Join(s.cause, other.cause)}
}

// installError builds the run's headline error for the install command. Its
// "%w for %d collections" wording is user-facing output that log-scrapers read.
func (s failureSummary) installError() error {
	return s.wrap(fmt.Errorf("%w for %d collections", helpers.ErrInstallationFailed, s.count))
}

// warmError builds the run's headline error for the warm command; it names
// warm because ErrInstallationFailed alone would not say which command failed.
func (s failureSummary) warmError() error {
	return s.wrap(fmt.Errorf("%w: warm failed for %d collections", helpers.ErrInstallationFailed, s.count))
}

// outdatedError builds the run's headline error for the outdated command, in
// installError's bare shape, since its sentinel already says what failed.
func (s failureSummary) outdatedError() error {
	return s.wrap(fmt.Errorf("%w for %d collections", helpers.ErrLatestVersionLookupFailed, s.count))
}

// wrap folds headline together with the recorded causes: nil when nothing
// failed, and a *summaryError joining both otherwise. The bare-headline branch
// is defensive; record never leaves a nonzero count without a cause.
func (s failureSummary) wrap(headline error) error {
	if s.count == 0 {
		return nil
	}
	if s.cause == nil {
		return headline
	}
	return &summaryError{msg: headline.Error(), joined: errors.Join(headline, s.cause)}
}

// summaryError prints only the one-line headline, since workers already
// printed each cause, while Unwrap exposes every per-collection sentinel so
// the exit code is classified by the real cause.
type summaryError struct {
	joined error
	msg    string
}

// Error returns the one-line headline, never the full joined tree.
func (e *summaryError) Error() string { return e.msg }

// Unwrap exposes the errors.Join tree of headline plus every recorded cause,
// so errors.Is/errors.As walk through to any sentinel behind either.
func (e *summaryError) Unwrap() error { return e.joined }
