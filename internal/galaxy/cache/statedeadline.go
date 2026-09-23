package cache

import (
	"context"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// stateDeadlineBackend is a Backend decorator bounding LoadStore, SaveStore,
// LoadProjectRegistry and RecordProject with a wall-clock budget and passing
// every other method through unmodified.
type stateDeadlineBackend struct {
	inner  Backend
	budget time.Duration
}

// WithStateDeadline bounds b's four state operations by budget (<= 0 means
// helpers.StateObjectDeadline) as ErrStateObjectDeadline; nil b returns nil.
// Methods are hand-written so a new Backend method fails the build here.
func WithStateDeadline(b Backend, budget time.Duration) Backend {
	if b == nil {
		return nil
	}
	if budget <= 0 {
		budget = helpers.StateObjectDeadline
	}
	return &stateDeadlineBackend{inner: b, budget: budget}
}

// LoadStore bounds b.LoadStore with this decorator's budget.
func (b *stateDeadlineBackend) LoadStore(ctx context.Context) (*store.Store, error) {
	dlCtx, cancel := context.WithTimeout(ctx, b.budget)
	defer cancel()
	st, err := b.inner.LoadStore(dlCtx)
	return st, deadlineError(ctx, dlCtx, b.budget, helpers.ErrStateObjectDeadline, err)
}

// SaveStore bounds b.SaveStore with this decorator's budget, the largest
// operation it bounds: on S3 the snapshot encode, gzip and PUT share it.
func (b *stateDeadlineBackend) SaveStore(ctx context.Context, st *store.Store) error {
	dlCtx, cancel := context.WithTimeout(ctx, b.budget)
	defer cancel()
	err := b.inner.SaveStore(dlCtx, st)
	return deadlineError(ctx, dlCtx, b.budget, helpers.ErrStateObjectDeadline, err)
}

// LoadProjectRegistry bounds b.LoadProjectRegistry with this decorator's
// budget.
func (b *stateDeadlineBackend) LoadProjectRegistry(ctx context.Context) (*store.ProjectRegistry, error) {
	dlCtx, cancel := context.WithTimeout(ctx, b.budget)
	defer cancel()
	registry, err := b.inner.LoadProjectRegistry(dlCtx)
	return registry, deadlineError(ctx, dlCtx, b.budget, helpers.ErrStateObjectDeadline, err)
}

// RecordProject bounds b.RecordProject with this decorator's budget.
func (b *stateDeadlineBackend) RecordProject(ctx context.Context, requirementsFile, downloadPath, rolesPath string) error {
	dlCtx, cancel := context.WithTimeout(ctx, b.budget)
	defer cancel()
	err := b.inner.RecordProject(dlCtx, requirementsFile, downloadPath, rolesPath)
	return deadlineError(ctx, dlCtx, b.budget, helpers.ErrStateObjectDeadline, err)
}

// Open passes through unmodified: its bucket check and conditional-write probe
// are tiny fixed-size bodies with no drip surface worth a budget.
func (b *stateDeadlineBackend) Open(ctx context.Context) error {
	return b.inner.Open(ctx)
}

// Close passes through unmodified: it releases in-process resources and
// issues no network call in either backend.
func (b *stateDeadlineBackend) Close(ctx context.Context) error {
	return b.inner.Close(ctx)
}

// Lock passes through unmodified: the lock protocol has its own timings and a
// wait may outlast the budget, and the holder context spans the whole run, so
// bounding either would cancel legitimate runs.
func (b *stateDeadlineBackend) Lock(ctx context.Context) (context.Context, func() error, error) {
	return b.inner.Lock(ctx)
}

// ClearFiles passes through unmodified: --clear-cache's bulk delete grows with
// the cache, so no fixed budget fits, and a dripped listing page holds the lock
// for as long as the drip lasts.
func (b *stateDeadlineBackend) ClearFiles(ctx context.Context) error {
	return b.inner.ClearFiles(ctx)
}

// SweepTemp passes through unmodified: it is local-only work (the S3
// backend's implementation is a no-op), with no state-object read or write
// for a budget to bound.
func (b *stateDeadlineBackend) SweepTemp(ctx context.Context) error {
	return b.inner.SweepTemp(ctx)
}

// Artifacts passes through unmodified, returning the store unwrapped: artifact
// I/O has its own budget, helpers.ArtifactDownloadDeadline.
func (b *stateDeadlineBackend) Artifacts() ArtifactStore {
	return b.inner.Artifacts()
}
