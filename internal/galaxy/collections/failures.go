package collections

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// failureRecorder is a concurrency-safe collector of per-collection and
// per-role failures: an atomic count for the cheap level-break check, plus
// each cause, so the run's error names what went wrong, not only how many.
type failureRecorder struct {
	causes []error
	mu     sync.Mutex
	n      atomic.Int32
	// roles counts the role failures among n; guarded by mu.
	roles int32
}

// record records a collection's failure.
func (r *failureRecorder) record(err error) { r.add(err, false) }

// recordRole records a role's failure, which the headline counts apart from
// the collections'.
func (r *failureRecorder) recordRole(err error) { r.add(err, true) }

// add appends err to the recorder's causes and bumps its counts. It is the
// only writer of any field; every other method only reads.
func (r *failureRecorder) add(err error, role bool) {
	r.mu.Lock()
	r.causes = append(r.causes, err)
	if role {
		r.roles++
	}
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

// summary snapshots the recorder into an immutable failureSummary whose counts
// always match its causes, even while workers still run, because add moves
// every field under one lock.
func (r *failureRecorder) summary() failureSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	// errors.Join of no causes is nil and allocates nothing, so the success
	// path needs no explicit empty check.
	return failureSummary{count: r.n.Load(), roles: r.roles, cause: errors.Join(r.causes...)}
}

// failureSummary is an immutable, copyable snapshot of a failureRecorder; the
// recorder holds a mutex and never leaves its frame, so callers pass this.
type failureSummary struct {
	// cause is the errors.Join of every recorded cause; nil when count == 0.
	cause error
	// count is every recorded failure; roles is how many of them are roles'.
	count int32
	roles int32
}

// join folds another summary into this one, adding counts and joining
// causes, so separately recorded collection and role failures report as one.
func (s failureSummary) join(other failureSummary) failureSummary {
	return failureSummary{
		count: s.count + other.count,
		roles: s.roles + other.roles,
		cause: errors.Join(s.cause, other.cause),
	}
}

// whatFailed names what failed for a headline, leaving out a kind with none:
// "3 collections", "2 roles", or "1 collection and 2 roles".
func (s failureSummary) whatFailed() string {
	var parts []string
	if n := s.count - s.roles; n > 0 {
		parts = append(parts, countOf(n, "collection"))
	}
	if s.roles > 0 {
		parts = append(parts, countOf(s.roles, "role"))
	}
	return strings.Join(parts, " and ")
}

// countOf renders n and noun, the noun plural unless n is one.
func countOf(n int32, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// installError builds the run's headline error for the install command. Its
// "installation failed for 2 roles" wording is output that log-scrapers read.
func (s failureSummary) installError() error {
	return s.wrap(fmt.Errorf("%w for %s", helpers.ErrInstallationFailed, s.whatFailed()))
}

// warmError builds the run's headline error for the warm command; it names
// warm because ErrInstallationFailed alone would not say which command failed.
func (s failureSummary) warmError() error {
	return s.wrap(fmt.Errorf("%w: warm failed for %s", helpers.ErrInstallationFailed, s.whatFailed()))
}

// outdatedError builds the run's headline error for the outdated command, in
// installError's bare shape, since its sentinel already says what failed.
func (s failureSummary) outdatedError() error {
	return s.wrap(fmt.Errorf("%w for %s", helpers.ErrLatestVersionLookupFailed, s.whatFailed()))
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
