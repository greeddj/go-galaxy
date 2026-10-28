package cache

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// errPassthroughInner is what every recordingBackend call fails with, so a
// decorator that swallows or replaces the inner error is caught.
var errPassthroughInner = errors.New("inner backend failure")

// errPassthroughHolderReplaced reports a Lock whose holder context is not the
// one the inner backend returned.
var errPassthroughHolderReplaced = errors.New("holder context replaced")

// passthroughKey tags the caller's context; holderKey tags the inner Lock's.
type (
	passthroughKey struct{}
	holderKey      struct{}
)

// recordedCall is one call that reached recordingBackend: its method, the
// caller's tag on the context it got and whether that context had a deadline.
type recordedCall struct {
	tag         any
	method      string
	hasDeadline bool
}

// passthroughArtifacts is the ArtifactStore identity a decorator must return
// unwrapped from Artifacts.
type passthroughArtifacts struct {
	ArtifactStore
}

// recordingBackend is a Backend that records every call and fails each one
// with errPassthroughInner, except Lock, which fails through its release.
type recordingBackend struct {
	artifacts ArtifactStore
	calls     []recordedCall
}

func (r *recordingBackend) Open(ctx context.Context) error {
	r.record(ctx, "Open")
	return errPassthroughInner
}

func (r *recordingBackend) Close(ctx context.Context) error {
	r.record(ctx, "Close")
	return errPassthroughInner
}

// Lock hands back a holder context of its own, which the decorator must
// return as is, and a release that fails with errPassthroughInner.
func (r *recordingBackend) Lock(ctx context.Context) (context.Context, func() error, error) {
	r.record(ctx, "Lock")
	return context.WithValue(ctx, holderKey{}, "inner holder"), func() error { return errPassthroughInner }, nil
}

func (r *recordingBackend) LoadStore(ctx context.Context) (*store.Store, error) {
	r.record(ctx, "LoadStore")
	return nil, errPassthroughInner
}

func (r *recordingBackend) SaveStore(ctx context.Context, _ *store.Store) error {
	r.record(ctx, "SaveStore")
	return errPassthroughInner
}

func (r *recordingBackend) ClearFiles(ctx context.Context) error {
	r.record(ctx, "ClearFiles")
	return errPassthroughInner
}

func (r *recordingBackend) RecordProject(ctx context.Context, _ store.ProjectRun) error {
	r.record(ctx, "RecordProject")
	return errPassthroughInner
}

func (r *recordingBackend) LoadProjectRegistry(ctx context.Context) (*store.ProjectRegistry, error) {
	r.record(ctx, "LoadProjectRegistry")
	return nil, errPassthroughInner
}

func (r *recordingBackend) Artifacts() ArtifactStore {
	r.calls = append(r.calls, recordedCall{method: "Artifacts"})
	return r.artifacts
}

func (r *recordingBackend) SweepTemp(ctx context.Context) error {
	r.record(ctx, "SweepTemp")
	return errPassthroughInner
}

// record appends one call with the caller's tag and deadline state of ctx.
func (r *recordingBackend) record(ctx context.Context, method string) {
	_, hasDeadline := ctx.Deadline()
	r.calls = append(r.calls, recordedCall{method: method, tag: ctx.Value(passthroughKey{}), hasDeadline: hasDeadline})
}

// passthroughCall drives one Backend method and returns its error.
type passthroughCall struct {
	invoke func(ctx context.Context, b Backend) error
	method string
}

// passthroughCalls lists every context-taking Backend method; SaveStore gets a
// dirty store, the one WithCleanSaveSkip lets through.
func passthroughCalls() []passthroughCall {
	dirty := store.New()
	dirty.SetGraph("a.b@1.0.0", nil)
	return []passthroughCall{
		{method: "Open", invoke: func(ctx context.Context, b Backend) error { return b.Open(ctx) }},
		{method: "Close", invoke: func(ctx context.Context, b Backend) error { return b.Close(ctx) }},
		{method: "Lock", invoke: func(ctx context.Context, b Backend) error {
			holder, release, err := b.Lock(ctx)
			if err != nil {
				return err
			}
			if holder.Value(holderKey{}) != "inner holder" {
				return errPassthroughHolderReplaced
			}
			return release()
		}},
		{method: "LoadStore", invoke: func(ctx context.Context, b Backend) error {
			_, err := b.LoadStore(ctx)
			return err
		}},
		{method: "SaveStore", invoke: func(ctx context.Context, b Backend) error { return b.SaveStore(ctx, dirty) }},
		{method: "ClearFiles", invoke: func(ctx context.Context, b Backend) error { return b.ClearFiles(ctx) }},
		{method: "RecordProject", invoke: func(ctx context.Context, b Backend) error {
			return b.RecordProject(ctx, store.ProjectRun{})
		}},
		{method: "LoadProjectRegistry", invoke: func(ctx context.Context, b Backend) error {
			_, err := b.LoadProjectRegistry(ctx)
			return err
		}},
		{method: "SweepTemp", invoke: func(ctx context.Context, b Backend) error { return b.SweepTemp(ctx) }},
	}
}

// TestDecoratorsForwardEveryMethodOnce pins that both decorators reach the
// inner method exactly once with the caller's context, return its error and
// artifact store as is, and bound only the four state operations.
func TestDecoratorsForwardEveryMethodOnce(t *testing.T) {
	t.Parallel()
	stateOps := []string{"LoadStore", "SaveStore", "LoadProjectRegistry", "RecordProject"}
	decorators := []struct {
		wrap    func(Backend) Backend
		name    string
		bounded []string
	}{
		{name: "WithCleanSaveSkip", wrap: WithCleanSaveSkip},
		{name: "WithStateDeadline", wrap: func(b Backend) Backend { return WithStateDeadline(b, time.Hour) }, bounded: stateOps},
	}
	for _, d := range decorators {
		for _, call := range passthroughCalls() {
			inner := &recordingBackend{}
			ctx := context.WithValue(context.Background(), passthroughKey{}, "caller")
			if err := call.invoke(ctx, d.wrap(inner)); !errors.Is(err, errPassthroughInner) {
				t.Errorf("%s.%s = %v, want the inner backend's error", d.name, call.method, err)
			}
			want := []recordedCall{{method: call.method, tag: "caller", hasDeadline: slices.Contains(d.bounded, call.method)}}
			if !slices.Equal(inner.calls, want) {
				t.Errorf("%s.%s reached the inner backend as %+v, want %+v", d.name, call.method, inner.calls, want)
			}
		}

		artifacts := &passthroughArtifacts{}
		inner := &recordingBackend{artifacts: artifacts}
		if got := d.wrap(inner).Artifacts(); got != ArtifactStore(artifacts) {
			t.Errorf("%s.Artifacts() = %v, want the inner store unwrapped", d.name, got)
		}
	}
}

// TestCleanSaveSkipPassesANilStoreThrough pins that a nil store is not
// mistaken for a clean one: the inner backend decides what nil means.
func TestCleanSaveSkipPassesANilStoreThrough(t *testing.T) {
	t.Parallel()
	inner := &recordingBackend{}
	if err := WithCleanSaveSkip(inner).SaveStore(context.Background(), nil); !errors.Is(err, errPassthroughInner) {
		t.Fatalf("SaveStore(nil) = %v, want the inner backend's error", err)
	}
	if len(inner.calls) != 1 || inner.calls[0].method != "SaveStore" {
		t.Fatalf("inner calls = %+v, want exactly one SaveStore", inner.calls)
	}
}

// TestDecoratorsOfNilAreNil pins that neither decorator wraps a missing
// backend into a non-nil one whose every call would panic.
func TestDecoratorsOfNilAreNil(t *testing.T) {
	t.Parallel()
	if got := WithCleanSaveSkip(nil); got != nil {
		t.Errorf("WithCleanSaveSkip(nil) = %v, want nil", got)
	}
	if got := WithStateDeadline(nil, time.Second); got != nil {
		t.Errorf("WithStateDeadline(nil) = %v, want nil", got)
	}
}

// TestWithStateDeadlineDefaultsANonPositiveBudget pins that zero and negative
// budgets fall back to helpers.StateObjectDeadline, and a positive one is kept.
func TestWithStateDeadlineDefaultsANonPositiveBudget(t *testing.T) {
	t.Parallel()
	cases := []struct{ budget, want time.Duration }{
		{budget: 0, want: helpers.StateObjectDeadline},
		{budget: -time.Second, want: helpers.StateObjectDeadline},
		{budget: 3 * time.Second, want: 3 * time.Second},
	}
	for _, tc := range cases {
		wrapped, ok := WithStateDeadline(&recordingBackend{}, tc.budget).(*stateDeadlineBackend)
		if !ok {
			t.Fatalf("WithStateDeadline(%v) is not a *stateDeadlineBackend", tc.budget)
		}
		if wrapped.budget != tc.want {
			t.Errorf("WithStateDeadline(%v).budget = %v, want %v", tc.budget, wrapped.budget, tc.want)
		}
	}
}
