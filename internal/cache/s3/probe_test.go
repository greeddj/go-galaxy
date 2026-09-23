package s3

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestOpenProbePassesOnConformingBackend pins that Open succeeds against a
// backend enforcing If-None-Match and leaves no probe object, whatever its
// random suffix, under the locks prefix.
func TestOpenProbePassesOnConformingBackend(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	keys, err := b.client.listObjects(ctx, b.key(locksPrefix))
	if err != nil {
		t.Fatalf("listObjects: %v", err)
	}
	for _, key := range keys {
		if strings.Contains(key, conditionalProbeObject) {
			t.Fatalf("expected the probe object to be cleaned up, found %q among %v", key, keys)
		}
	}
}

// TestOpenProbeFailsOnNonConformingBackend pins that a backend ignoring
// If-None-Match fails Open with errS3ConditionalPutUnsupported and leaves the
// backend not opened (b.client == nil).
func TestOpenProbeFailsOnNonConformingBackend(t *testing.T) {
	t.Parallel()
	b := newNonConformingTestBackend(t)
	ctx := context.Background()

	err := b.Open(ctx)
	if !errors.Is(err, errS3ConditionalPutUnsupported) {
		t.Fatalf("expected errS3ConditionalPutUnsupported, got %v", err)
	}
	if b.client != nil {
		t.Fatalf("expected the backend to be left in the not-opened state (b.client == nil) after a failed probe")
	}
}

// TestOpenProbeFailsWithoutCompareAndSwap pins that each of the probe's three
// If-Match checks catches its own defect (a stale swap accepted, no ETag, every
// swap refused) with errS3CompareAndSwapUnsupported, leaving it unopened.
func TestOpenProbeFailsWithoutCompareAndSwap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		arm  func(*fakeS3)
		name string
	}{
		{
			name: "a backend that accepts a swap it should have refused",
			arm:  func(f *fakeS3) { f.ignoreIfMatch = true },
		},
		{
			name: "a backend that never names an object's version",
			arm:  func(f *fakeS3) { f.suppressETag = true },
		},
		{
			name: "a backend that refuses every swap alike",
			arm:  func(f *fakeS3) { f.refuseIfMatch = true },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeS3()
			tc.arm(fake)
			b := newTestBackendWithFake(t, fake)

			err := b.Open(context.Background())
			if !errors.Is(err, errS3CompareAndSwapUnsupported) {
				t.Fatalf("Open error = %v, want one matching %v", err, errS3CompareAndSwapUnsupported)
			}
			if b.client != nil {
				t.Fatalf("expected the backend to be left in the not-opened state (b.client == nil) after a failed probe")
			}
		})
	}
}
