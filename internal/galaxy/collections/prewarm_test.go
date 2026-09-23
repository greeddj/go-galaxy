package collections

// Tests drive prewarmRootMetadata through resolveCollectionsInternal, since it
// returns nothing; a concurrencyBarrier transport observes how many metadata
// requests were in flight at once, not merely how many were made.

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// concurrencyBarrierTimeout bounds how long a request waits for the barrier
// to open; only a run that never reaches want pays it, at most once per test,
// so it is generous enough not to flake under CI load.
const concurrencyBarrierTimeout = 2 * time.Second

// concurrencyBarrier is an http.RoundTripper that holds each request until
// want are in flight at once, then opens for good. The base transport must not
// cap per-host connections below want, or requests queue instead of overlap.
type concurrencyBarrier struct {
	base     http.RoundTripper
	open     chan struct{}
	mu       sync.Mutex
	timeout  time.Duration
	want     int
	inFlight int
	peak     int
	total    int
	opened   bool
}

// RoundTrip blocks a request behind the barrier until want requests are
// simultaneously in flight (or until timeout elapses, or the request's own
// context ends - whichever comes first), then delegates to base.
func (b *concurrencyBarrier) RoundTrip(req *http.Request) (*http.Response, error) {
	if ch := b.enter(); ch != nil {
		timer := time.NewTimer(b.timeout)
		defer timer.Stop()
		select {
		case <-ch:
		case <-timer.C:
			b.release()
		case <-req.Context().Done():
			b.release()
		}
	}
	resp, err := b.base.RoundTrip(req)
	b.leave()
	return resp, err
}

// enter counts one more in-flight request, updating total and peak, and opens
// the barrier once inFlight first reaches want. It returns the channel to wait
// on while the barrier is closed, or nil once it is open.
func (b *concurrencyBarrier) enter() chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inFlight++
	b.total++
	b.peak = max(b.peak, b.inFlight)
	if b.inFlight >= b.want && !b.opened {
		b.opened = true
		close(b.open)
	}
	if b.opened {
		return nil
	}
	return b.open
}

// release opens the barrier early, when a request gives up waiting on timeout
// or cancellation, so no other request stays blocked forever. It is
// idempotent: releasing an already-open barrier is a no-op.
func (b *concurrencyBarrier) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.opened {
		b.opened = true
		close(b.open)
	}
}

// leave records that one in-flight request completed, once RoundTrip's own
// call to the wrapped base transport returns.
func (b *concurrencyBarrier) leave() {
	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
}

// peakConcurrency reports the highest inFlight value enter ever observed:
// the largest number of requests this barrier ever saw outstanding through
// it at the same instant.
func (b *concurrencyBarrier) peakConcurrency() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak
}

// requests reports the total number of RoundTrip calls seen: the only count
// that includes a request a hub-shaped fake server 404s before routing it to
// a counted endpoint (see TestPrewarmSharesTheResolvePhaseAPIRootMemo).
func (b *concurrencyBarrier) requests() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// newPrewarmBarrierRuntime builds an *infra.Infra whose client is a fresh
// http.Client over a concurrencyBarrier armed for want, leaving srv.Client()
// itself unmodified for any other caller.
func newPrewarmBarrierRuntime(srv *fakegalaxy.Server, want int) (*infra.Infra, *concurrencyBarrier) {
	barrier := &concurrencyBarrier{
		base:    srv.Client().Transport,
		open:    make(chan struct{}),
		timeout: concurrencyBarrierTimeout,
		want:    want,
	}
	return infra.New(noopPrinter{}, &http.Client{Transport: barrier}), barrier
}

// unpinnedPrewarmRoots registers n single-version collections
// (acme.c0..acme.cN-1, each at "1.0.0") against srv and returns their roots,
// each an unpinned ("*") requirement pointing at source.
func unpinnedPrewarmRoots(srv *fakegalaxy.Server, source string, n int) []collection {
	roots := make([]collection, n)
	for i := range roots {
		name := fmt.Sprintf("c%d", i)
		srv.AddVersion("acme", name, "1.0.0", nil)
		roots[i] = collection{Namespace: "acme", Name: name, Version: "*", Constraint: "*", Source: source}
	}
	return roots
}

// exactPinnedPrewarmRoots registers n distinct single-version collections
// (acme.p0..acme.pN-1, each at "1.0.0") against srv and returns their roots,
// each pinned to that exact version.
func exactPinnedPrewarmRoots(srv *fakegalaxy.Server, source string, n int) []collection {
	roots := make([]collection, n)
	for i := range roots {
		name := fmt.Sprintf("p%d", i)
		srv.AddVersion("acme", name, "1.0.0", nil)
		roots[i] = collection{Namespace: "acme", Name: name, Version: "1.0.0", Constraint: "1.0.0", Source: source}
	}
	return roots
}

// TestPrewarmFetchesRootMetadataConcurrently pins that prewarmRootMetadata
// overlaps root-metadata requests up to cfg.Workers for more unpinned roots
// than workers, and never warms the versions list.
func TestPrewarmFetchesRootMetadataConcurrently(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	roots := unpinnedPrewarmRoots(srv, srv.URL(), 8)

	runtime, barrier := newPrewarmBarrierRuntime(srv, 4)
	cfg := &config.Config{Server: srv.URL(), Workers: 4, NoDeps: true}
	deps := newCollectionDeps(cfg, runtime, store.New())

	_, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial)
	if err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}
	// Without the prewarm, the solve makes these requests one at a time.
	if peak := barrier.peakConcurrency(); peak < 4 {
		t.Fatalf("peak concurrency = %d, want >= 4", peak)
	}
	// One request per root with or without the prewarm: this guards the
	// resolve's request shape, not the prewarm's presence.
	if got := srv.Count(fakegalaxy.EndpointRootMetadata); got != 8 {
		t.Fatalf("root metadata requests = %d, want 8", got)
	}
	// Highest alone settles each "*" root, so a warmed versions list would be
	// a request the solve never needed.
	if got := srv.Count(fakegalaxy.EndpointVersionsList); got != 0 {
		t.Fatalf("versions list requests = %d, want 0", got)
	}
}

// TestPrewarmSkippedUnderRefresh pins that --refresh (write-only non-exact
// policy) skips the prewarm for unpinned roots; peak 1 also proves the
// barrier can observe a sequential run.
func TestPrewarmSkippedUnderRefresh(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	roots := unpinnedPrewarmRoots(srv, srv.URL(), 8)

	runtime, barrier := newPrewarmBarrierRuntime(srv, 4)
	cfg := &config.Config{Server: srv.URL(), Workers: 4, NoDeps: true, Refresh: true}
	deps := newCollectionDeps(cfg, runtime, store.New())

	_, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial)
	if err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}
	// Any prewarm goroutine calling Highest would push peak to Workers.
	if peak := barrier.peakConcurrency(); peak != 1 {
		t.Fatalf("peak concurrency = %d, want 1 (prewarm must be skipped under --refresh)", peak)
	}
	// --refresh does not change the solve's own request count.
	if got := srv.Count(fakegalaxy.EndpointRootMetadata); got != 8 {
		t.Fatalf("root metadata requests = %d, want 8", got)
	}
}

// TestPrewarmSkippedWhenSnapshotReplays pins that a snapshot replay issues zero
// requests, so the prewarm must sit below the replay return. The replay Store
// holds only the resolution, so no warm document cache can mask a hoist.
func TestPrewarmSkippedWhenSnapshotReplays(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	roots := unpinnedPrewarmRoots(srv, srv.URL(), 4)

	// The barrier is unasserted here: a replay reaches no transport call.
	runtime, _ := newPrewarmBarrierRuntime(srv, 1)
	cfg := &config.Config{Server: srv.URL(), Workers: 4, NoDeps: true}
	deps := newCollectionDeps(cfg, runtime, store.New())

	resolved, graph, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveTopLevel)
	if err != nil {
		t.Fatalf("first resolveCollectionsInternal: %v", err)
	}
	srv.ResetCounts()

	replaySt := store.New()
	reqSpec := buildRequirementsSpec(roots)
	reqHash := requirementsSignatureFromSpec(reqSpec, cfg.NoDeps, serversSignature(cfg))
	recordResolution(replaySt, resolved, graph, reqHash, cfg.Server, reqSpec)
	replayDeps := newCollectionDeps(cfg, runtime, replaySt)

	if _, _, err := resolveCollectionsInternal(context.Background(), replayDeps, roots, resolveTopLevel); err != nil {
		t.Fatalf("second resolveCollectionsInternal: %v", err)
	}
	// A prewarm above the replay return would cost one request per root.
	if total := srv.Total(); total != 0 {
		t.Fatalf("srv.Total() = %d, want 0 (a snapshot-replay resolve must issue zero metadata requests)", total)
	}
}

// TestPrewarmSkippedForExactPinWithNoDeps pins that an exact pin under
// --no-deps is not warmed, since the solve asks nothing for it; its positive
// control is TestPrewarmFetchesVersionMetadataForExactPin.
func TestPrewarmSkippedForExactPinWithNoDeps(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	roots := exactPinnedPrewarmRoots(srv, srv.URL(), 2)

	runtime, _ := newPrewarmBarrierRuntime(srv, 1)
	cfg := &config.Config{Server: srv.URL(), Workers: 2, NoDeps: true}
	deps := newCollectionDeps(cfg, runtime, store.New())

	if _, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial); err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}
	// A warm would cost a root-metadata and a version-detail request per root.
	if total := srv.Total(); total != 0 {
		t.Fatalf("srv.Total() = %d, want 0 (an exactly pinned root under --no-deps must issue zero metadata requests)", total)
	}
}

// TestPrewarmFetchesVersionMetadataForExactPin pins that prewarmOne's exact
// pin arm calls Dependencies and overlaps those calls up to cfg.Workers; it
// is the positive control for TestPrewarmSkippedForExactPinWithNoDeps.
func TestPrewarmFetchesVersionMetadataForExactPin(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	roots := exactPinnedPrewarmRoots(srv, srv.URL(), 4)

	runtime, barrier := newPrewarmBarrierRuntime(srv, 4)
	cfg := &config.Config{Server: srv.URL(), Workers: 4}
	deps := newCollectionDeps(cfg, runtime, store.New())

	if _, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial); err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}
	// Without the exact-pin arm, the solve fetches these one at a time.
	if peak := barrier.peakConcurrency(); peak < 4 {
		t.Fatalf("peak concurrency = %d, want >= 4", peak)
	}
	// One of each per root whoever makes it: this guards the request shape,
	// not the prewarm's presence.
	rootMeta, detail := srv.Count(fakegalaxy.EndpointRootMetadata), srv.Count(fakegalaxy.EndpointVersionDetail)
	if rootMeta != 4 || detail != 4 {
		t.Fatalf("root metadata = %d, version detail = %d, want 4 and 4", rootMeta, detail)
	}
}

// TestPrewarmSharesTheResolvePhaseAPIRootMemo pins that prewarm providers
// share deps.apiRoots with the solve: against a hub base, where two /api/v3
// 404s precede the winner, only the first root may pay that losing pair.
func TestPrewarmSharesTheResolvePhaseAPIRootMemo(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.NewAtBasePath(t, hubBasePath)
	base := srv.URL() + hubBasePath
	roots := unpinnedPrewarmRoots(srv, base, 3)

	runtime, barrier := newPrewarmBarrierRuntime(srv, 1)
	cfg := &config.Config{Server: base, Workers: 1, NoDeps: true}
	deps := newCollectionDeps(cfg, runtime, store.New())

	if _, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial); err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}
	// The prewarm costs 3+1+1 requests and the solve hits the cache. Workers
	// is 1 so the walks run in order (a concurrent wave reads an empty memo),
	// and the barrier sees the hub's 404s, which srv.Count never counts.
	if got := barrier.requests(); got != 5 {
		t.Fatalf("requests() = %d, want 5 (prewarm and the solve must share one apiRootMemo against this base)", got)
	}
}

// TestPrewarmSkippedWithoutStore pins prewarmEnabled's nil-store skip: with
// nothing stored, a warm would double the metadata requests. Positive
// control: TestPrewarmFetchesRootMetadataConcurrently.
func TestPrewarmSkippedWithoutStore(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	roots := unpinnedPrewarmRoots(srv, srv.URL(), 8)

	// The barrier is unasserted here: only the request count matters.
	runtime, _ := newPrewarmBarrierRuntime(srv, 1)
	cfg := &config.Config{Server: srv.URL(), Workers: 4, NoDeps: true}
	deps := newCollectionDeps(cfg, runtime, nil)

	if _, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial); err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}
	// A warm without a store is repeated by the solve: 16 instead of 8.
	if got := srv.Count(fakegalaxy.EndpointRootMetadata); got != 8 {
		t.Fatalf("root metadata requests = %d, want 8 (prewarm must be skipped without a store)", got)
	}
}
