package collections

import (
	"errors"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
)

// offlineConflictNote is appended to a resolution conflict under --offline,
// where only cached metadata is visible, so the conflict may reflect stale or
// incomplete cache coverage rather than real unsatisfiability.
const offlineConflictNote = "note: offline mode restricts resolution to cached metadata; retry without --offline to fetch fresh metadata"

// offlineConflictError appends offlineConflictNote to a *solver.ConflictError's
// message and stays transparent through Unwrap, so errors.Is/errors.As and
// exitcode.FromError classify it exactly as the error it wraps.
type offlineConflictError struct {
	inner error
}

// Error renders the inner conflict's own message followed by the offline
// note on its own line.
func (e *offlineConflictError) Error() string {
	return e.inner.Error() + "\n" + offlineConflictNote
}

// Unwrap exposes the inner error so errors.Is/errors.As - and therefore
// exitcode.FromError - classify offlineConflictError identically to the
// *solver.ConflictError it wraps.
func (e *offlineConflictError) Unwrap() error {
	return e.inner
}

// annotateOfflineConflict wraps err in offlineConflictError when cfg is
// --offline and err is a *solver.ConflictError, and otherwise returns err
// unchanged.
func annotateOfflineConflict(cfg *config.Config, err error) error {
	if err == nil || !cfg.Offline {
		return err
	}
	if _, ok := errors.AsType[*solver.ConflictError](err); !ok {
		return err
	}
	return &offlineConflictError{inner: err}
}
