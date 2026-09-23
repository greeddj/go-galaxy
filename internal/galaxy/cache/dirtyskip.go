package cache

import (
	"context"

	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// cleanSaveSkipBackend is a Backend decorator making SaveStore a no-op for a
// store this process did not write; every other method passes through.
type cleanSaveSkipBackend struct {
	inner Backend
}

// WithCleanSaveSkip wraps b so SaveStore skips a non-nil store with Dirty()
// false; nil b returns nil. Methods are written out by hand, not embedded, so a
// new Backend method fails the build here until it is decided.
func WithCleanSaveSkip(b Backend) Backend {
	if b == nil {
		return nil
	}
	return &cleanSaveSkipBackend{inner: b}
}

// SaveStore skips b.inner.SaveStore when st is non-nil and clean. A nil st
// passes through, since nil Dirty() reads false: local.Backend refuses it with
// helpers.ErrStoreNil, and a skip here would turn that into success.
func (b *cleanSaveSkipBackend) SaveStore(ctx context.Context, st *store.Store) error {
	if st != nil && !st.Dirty() {
		return nil
	}
	return b.inner.SaveStore(ctx, st)
}

// Open passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) Open(ctx context.Context) error {
	return b.inner.Open(ctx)
}

// Close passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) Close(ctx context.Context) error {
	return b.inner.Close(ctx)
}

// Lock passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) Lock(ctx context.Context) (context.Context, func() error, error) {
	return b.inner.Lock(ctx)
}

// LoadStore passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) LoadStore(ctx context.Context) (*store.Store, error) {
	return b.inner.LoadStore(ctx)
}

// ClearFiles passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) ClearFiles(ctx context.Context) error {
	return b.inner.ClearFiles(ctx)
}

// RecordProject passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) RecordProject(ctx context.Context, requirementsFile, downloadPath, rolesPath string) error {
	return b.inner.RecordProject(ctx, requirementsFile, downloadPath, rolesPath)
}

// LoadProjectRegistry passes through unmodified; only SaveStore is decided
// here.
func (b *cleanSaveSkipBackend) LoadProjectRegistry(ctx context.Context) (*store.ProjectRegistry, error) {
	return b.inner.LoadProjectRegistry(ctx)
}

// Artifacts passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) Artifacts() ArtifactStore {
	return b.inner.Artifacts()
}

// SweepTemp passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) SweepTemp(ctx context.Context) error {
	return b.inner.SweepTemp(ctx)
}
