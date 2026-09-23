package cache_test

// This file exercises WithCleanSaveSkip from the external test package, the
// way a caller in another package uses it.

import (
	"context"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// saveCountingBackend is a minimal cacheManager.Backend that counts the
// SaveStore calls WithCleanSaveSkip lets through.
type saveCountingBackend struct {
	saveCalls int
}

func (s *saveCountingBackend) Open(_ context.Context) error  { return nil }
func (s *saveCountingBackend) Close(_ context.Context) error { return nil }

// Lock returns ctx unchanged, matching the local backend's own
// cannot-lose-a-lock contract; this stub is never used to exercise Lock
// itself.
func (s *saveCountingBackend) Lock(ctx context.Context) (context.Context, func() error, error) {
	return ctx, func() error { return nil }, nil
}

// LoadStore returns a fresh, empty store rather than nil: this stub is never
// used to exercise LoadStore itself, and a real Backend never reports success
// alongside a nil store.
func (s *saveCountingBackend) LoadStore(_ context.Context) (*store.Store, error) {
	return store.New(), nil
}

// SaveStore records that it was called and always succeeds.
func (s *saveCountingBackend) SaveStore(_ context.Context, _ *store.Store) error {
	s.saveCalls++
	return nil
}

func (s *saveCountingBackend) ClearFiles(_ context.Context) error { return nil }

func (s *saveCountingBackend) RecordProject(_ context.Context, _, _, _ string) error { return nil }

// LoadProjectRegistry returns a fresh, empty registry rather than nil, for
// the identical reason LoadStore above does.
func (s *saveCountingBackend) LoadProjectRegistry(_ context.Context) (*store.ProjectRegistry, error) {
	return &store.ProjectRegistry{Projects: map[string]store.ProjectRecord{}}, nil
}

func (s *saveCountingBackend) Artifacts() cacheManager.ArtifactStore { return nil }
func (s *saveCountingBackend) SweepTemp(_ context.Context) error     { return nil }

// TestCleanSaveSkipSkipsUnmutatedStore pins that a clean store's save never
// reaches the wrapped backend, and that one mutator call makes it reach it.
func TestCleanSaveSkipSkipsUnmutatedStore(t *testing.T) {
	t.Parallel()

	inner := &saveCountingBackend{}
	wrapped := cacheManager.WithCleanSaveSkip(inner)

	st := store.New()
	if err := wrapped.SaveStore(context.Background(), st); err != nil {
		t.Fatalf("SaveStore(clean store) error = %v, want nil", err)
	}
	if inner.saveCalls != 0 {
		t.Fatalf("SaveStore(clean store) reached the wrapped backend %d times, want 0", inner.saveCalls)
	}

	// Positive control on the same store: one mutator makes it dirty, so the
	// identical save must now reach the wrapped backend.
	st.SetGraph("a.b@1.0.0", []string{"c.d@1.2.3"})
	if err := wrapped.SaveStore(context.Background(), st); err != nil {
		t.Fatalf("SaveStore(dirty store) error = %v, want nil", err)
	}
	if inner.saveCalls != 1 {
		t.Fatalf("SaveStore(dirty store) reached the wrapped backend %d times, want 1", inner.saveCalls)
	}
}

// TestCleanSaveSkipPassesThroughNilStore pins that a nil store reaches the
// wrapped backend rather than reading as clean, so local.Backend's
// helpers.ErrStoreNil is not swallowed.
func TestCleanSaveSkipPassesThroughNilStore(t *testing.T) {
	t.Parallel()

	inner := &saveCountingBackend{}
	wrapped := cacheManager.WithCleanSaveSkip(inner)

	if err := wrapped.SaveStore(context.Background(), nil); err != nil {
		t.Fatalf("SaveStore(nil) error = %v, want nil", err)
	}
	if inner.saveCalls != 1 {
		t.Fatalf(
			"SaveStore(nil) reached the wrapped backend %d times, want 1 (the st != nil guard must let a nil store through)",
			inner.saveCalls,
		)
	}
}
