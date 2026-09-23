// Package cache_test tests WithStateDeadline from outside so it can import the
// real local.Backend, which itself imports internal/galaxy/cache: an in-package
// test could not import it back.
package cache_test

// This file pins WithStateDeadline: the four state operations are bounded,
// Lock and ClearFiles deliberately are not, and it is inert for local.Backend.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// stateDeadlineTestBudget is the fixed per-operation budget every stub-backed
// test in this file uses.
const stateDeadlineTestBudget = 30 * time.Millisecond

// stubStateBackend is a Backend whose state operations block until their
// context ends when blocking is set, and whose Lock and ClearFiles take a
// fixed delay, to prove which methods WithStateDeadline bounds.
type stubStateBackend struct {
	store      *store.Store
	registry   *store.ProjectRegistry
	lockDelay  time.Duration
	clearDelay time.Duration
	blocking   bool
}

func (s *stubStateBackend) Open(_ context.Context) error  { return nil }
func (s *stubStateBackend) Close(_ context.Context) error { return nil }

// Lock waits out s.lockDelay (or the context ending, whichever comes first)
// before reporting success, so a test can prove this call is never bounded
// by the state-object budget.
func (s *stubStateBackend) Lock(ctx context.Context) (context.Context, func() error, error) {
	if s.lockDelay > 0 {
		timer := time.NewTimer(s.lockDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
	}
	// ctx unchanged, matching the local backend's own holder-context contract:
	// this stub's lock cannot be taken away from a live holder either.
	return ctx, func() error { return nil }, nil
}

// LoadStore blocks on ctx until it ends and returns ctx.Err() when blocking
// is set; otherwise it returns s.store immediately.
func (s *stubStateBackend) LoadStore(ctx context.Context) (*store.Store, error) {
	if s.blocking {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.store, nil
}

// SaveStore blocks on ctx until it ends and returns ctx.Err() when blocking
// is set; otherwise it returns nil immediately.
func (s *stubStateBackend) SaveStore(ctx context.Context, _ *store.Store) error {
	if s.blocking {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// ClearFiles waits out s.clearDelay (or the context ending, whichever comes
// first) before reporting success, mirroring Lock.
func (s *stubStateBackend) ClearFiles(ctx context.Context) error {
	if s.clearDelay > 0 {
		timer := time.NewTimer(s.clearDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// RecordProject blocks on ctx until it ends and returns ctx.Err() when
// blocking is set; otherwise it returns nil immediately.
func (s *stubStateBackend) RecordProject(ctx context.Context, _, _, _ string) error {
	if s.blocking {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// LoadProjectRegistry blocks on ctx until it ends and returns ctx.Err() when
// blocking is set; otherwise it returns s.registry immediately.
func (s *stubStateBackend) LoadProjectRegistry(ctx context.Context) (*store.ProjectRegistry, error) {
	if s.blocking {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.registry, nil
}

func (s *stubStateBackend) Artifacts() cacheManager.ArtifactStore { return nil }
func (s *stubStateBackend) SweepTemp(_ context.Context) error     { return nil }

// TestWithStateDeadlineBoundsEveryStateOperation pins that each of the four
// state operations over a blocking stub fails with ErrStateObjectDeadline,
// matching neither context sentinel.
func TestWithStateDeadlineBoundsEveryStateOperation(t *testing.T) {
	t.Parallel()

	blocking := &stubStateBackend{blocking: true}
	wrapped := cacheManager.WithStateDeadline(blocking, stateDeadlineTestBudget)

	cases := []struct {
		call func() error
		name string
	}{
		{name: "LoadStore", call: func() error {
			_, err := wrapped.LoadStore(context.Background())
			return err
		}},
		{name: "SaveStore", call: func() error {
			return wrapped.SaveStore(context.Background(), store.New())
		}},
		{name: "LoadProjectRegistry", call: func() error {
			_, err := wrapped.LoadProjectRegistry(context.Background())
			return err
		}},
		{name: "RecordProject", call: func() error {
			return wrapped.RecordProject(context.Background(), "requirements.yml", "collections", "")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertStateDeadlineFired(t, tc.name, tc.call())
		})
	}

	assertStateDeadlinePositiveControl(t)
}

// assertStateDeadlineFired fails the test unless err matches
// helpers.ErrStateObjectDeadline and neither context sentinel.
func assertStateDeadlineFired(t *testing.T, name string, err error) {
	t.Helper()
	if !errors.Is(err, helpers.ErrStateObjectDeadline) {
		t.Fatalf("%s error = %v, want errors.Is ErrStateObjectDeadline", name, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%s error = %v, must not match context.DeadlineExceeded", name, err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("%s error = %v, must not match context.Canceled", name, err)
	}
}

// assertStateDeadlinePositiveControl pins that the same wrapper over a
// non-blocking stub is transparent: nil errors and the stub's own values.
func assertStateDeadlinePositiveControl(t *testing.T) {
	t.Helper()
	wantStore := store.New()
	wantRegistry := &store.ProjectRegistry{Projects: map[string]store.ProjectRecord{}}
	nonBlocking := &stubStateBackend{store: wantStore, registry: wantRegistry}
	wrappedOK := cacheManager.WithStateDeadline(nonBlocking, stateDeadlineTestBudget)

	gotStore, err := wrappedOK.LoadStore(context.Background())
	if err != nil || gotStore != wantStore {
		t.Fatalf("positive control: LoadStore = (%v, %v), want (%v, nil)", gotStore, err, wantStore)
	}
	if err := wrappedOK.SaveStore(context.Background(), store.New()); err != nil {
		t.Fatalf("positive control: SaveStore = %v, want nil", err)
	}
	gotRegistry, err := wrappedOK.LoadProjectRegistry(context.Background())
	if err != nil || gotRegistry != wantRegistry {
		t.Fatalf("positive control: LoadProjectRegistry = (%v, %v), want (%v, nil)", gotRegistry, err, wantRegistry)
	}
	if err := wrappedOK.RecordProject(context.Background(), "requirements.yml", "collections", ""); err != nil {
		t.Fatalf("positive control: RecordProject = %v, want nil", err)
	}
}

// TestWithStateDeadlineDoesNotBoundLockOrClearFiles pins that Lock and
// ClearFiles taking 3x the budget still succeed through the wrapper, which
// TestWithStateDeadlineBoundsEveryStateOperation shows can fire at that budget.
func TestWithStateDeadlineDoesNotBoundLockOrClearFiles(t *testing.T) {
	t.Parallel()

	stub := &stubStateBackend{
		lockDelay:  3 * stateDeadlineTestBudget,
		clearDelay: 3 * stateDeadlineTestBudget,
	}
	wrapped := cacheManager.WithStateDeadline(stub, stateDeadlineTestBudget)

	_, release, err := wrapped.Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock() error = %v, want nil (Lock must not be bounded by the state-object budget)", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release() error = %v, want nil", err)
	}

	if err := wrapped.ClearFiles(context.Background()); err != nil {
		t.Fatalf("ClearFiles() error = %v, want nil (ClearFiles must not be bounded by the state-object budget)", err)
	}
}

// TestWithStateDeadlineIsInertForTheLocalBackend pins that a real local.Backend
// under a 1ns budget still succeeds on all four state operations, since it
// ignores its context; the blocking stub shows 1ns can still fire.
func TestWithStateDeadlineIsInertForTheLocalBackend(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	backend := local.New(cacheDir)
	wrapped := cacheManager.WithStateDeadline(backend, time.Nanosecond)

	if err := wrapped.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}

	if err := wrapped.RecordProject(context.Background(), reqPath, cacheDir, ""); err != nil {
		t.Fatalf("RecordProject: %v, want nil (the local backend ignores its context parameter)", err)
	}
	if _, err := wrapped.LoadProjectRegistry(context.Background()); err != nil {
		t.Fatalf("LoadProjectRegistry: %v, want nil", err)
	}
	st, err := wrapped.LoadStore(context.Background())
	if err != nil {
		t.Fatalf("LoadStore: %v, want nil", err)
	}
	if err := wrapped.SaveStore(context.Background(), st); err != nil {
		t.Fatalf("SaveStore: %v, want nil", err)
	}

	stub := &stubStateBackend{blocking: true}
	wrappedStub := cacheManager.WithStateDeadline(stub, time.Nanosecond)
	if _, err := wrappedStub.LoadStore(context.Background()); !errors.Is(err, helpers.ErrStateObjectDeadline) {
		t.Fatalf("positive control: LoadStore = %v, want errors.Is ErrStateObjectDeadline (1ns must still be able to fire)", err)
	}
}
