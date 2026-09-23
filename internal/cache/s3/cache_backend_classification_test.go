package s3

// Tests for the cache-backend classes: exhausted retries and transport
// failures are ErrCacheBackendUnavailable, an Open probe failure is
// ErrCacheBackendUnusable, never both, and a cancellation stays unclassified.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// TestLoadStoreClassifiesExhaustedGetRetriesAsCacheBackendUnavailable proves a
// snapshot GET failing with 500 on every retry matches both errS3GetFailed and
// helpers.ErrCacheBackendUnavailable.
func TestLoadStoreClassifiesExhaustedGetRetriesAsCacheBackendUnavailable(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	putStoreObject(ctx, t, b, helpers.StoreSnapshotSchemaVersion, nil)
	key := b.key(statePrefix, storeObject)
	fake.failNext(key, http.MethodGet, http.StatusInternalServerError, s3RetryMaxAttempts)

	_, err := b.LoadStore(ctx)
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
	if !errors.Is(err, errS3GetFailed) {
		t.Fatalf("expected errors.Is(err, errS3GetFailed), got %v", err)
	}
}

// TestSaveStoreClassifiesExhaustedPutRetriesAsCacheBackendUnavailable proves a
// snapshot PUT failing with 500 on every retry matches both errS3PutFailed and
// helpers.ErrCacheBackendUnavailable.
func TestSaveStoreClassifiesExhaustedPutRetriesAsCacheBackendUnavailable(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	key := b.key(statePrefix, storeObject)
	fake.failNext(key, http.MethodPut, http.StatusInternalServerError, s3RetryMaxAttempts)

	err := b.SaveStore(ctx, store.New())
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
	if !errors.Is(err, errS3PutFailed) {
		t.Fatalf("expected errors.Is(err, errS3PutFailed), got %v", err)
	}
}

// TestClientDoClassifiesConnectionFailureAsCacheBackendUnavailable proves a
// bucket nobody listens on is ErrCacheBackendUnavailable from LoadStore, and
// matches neither context.Canceled nor context.DeadlineExceeded.
func TestClientDoClassifiesConnectionFailureAsCacheBackendUnavailable(t *testing.T) {
	t.Parallel()

	// A closed server's port refuses connections at once, where an unroutable
	// address would hang on dial.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	cfg := config.S3CacheConfig{
		Endpoint:  closedURL,
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	backend, err := New(cfg, http.DefaultClient, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = backend.LoadStore(context.Background())

	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("expected NOT errors.Is(err, context.Canceled), got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected NOT errors.Is(err, context.DeadlineExceeded), got %v", err)
	}
}

// TestClientDoClassifiesLiveTimeoutShapesAsCacheBackendUnavailable proves a
// dial or response-header timeout, which matches context.DeadlineExceeded with
// the caller's context live, is still ErrCacheBackendUnavailable.
func TestClientDoClassifiesLiveTimeoutShapesAsCacheBackendUnavailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		newTransport func(t *testing.T, addr string) http.RoundTripper
		name         string
	}{
		{
			// A 1ns dial timeout expires before a local handshake completes,
			// giving a genuine dial timeout rather than a connect refusal.
			name: "dial timeout",
			newTransport: func(t *testing.T, _ string) http.RoundTripper {
				t.Helper()
				return &http.Transport{
					DialContext: (&net.Dialer{Timeout: time.Nanosecond}).DialContext,
				}
			},
		},
		{
			// The dial succeeds but no headers ever arrive: an endpoint that
			// accepts a connection and then never answers.
			name: "response header timeout",
			newTransport: func(t *testing.T, _ string) http.RoundTripper {
				t.Helper()
				return &http.Transport{ResponseHeaderTimeout: 150 * time.Millisecond}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertLiveTimeoutClassifiesAsCacheBackendUnavailable(t, tt.newTransport)
		})
	}
}

// assertLiveTimeoutClassifiesAsCacheBackendUnavailable checks the raw
// transport error really is a DeadlineExceeded with a live caller context,
// then asserts Client.do classifies it as ErrCacheBackendUnavailable.
func assertLiveTimeoutClassifiesAsCacheBackendUnavailable(
	t *testing.T,
	newTransport func(t *testing.T, addr string) http.RoundTripper,
) {
	t.Helper()

	ln, _ := newAcceptingNeverRespondingListener(t)
	transport := newTransport(t, ln.Addr().String())
	httpClient := &http.Client{Transport: transport}

	cfg := config.S3CacheConfig{
		Endpoint:  "http://" + ln.Addr().String(),
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	client, err := newClient(cfg, httpClient)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	req, err := client.newReadRequest(context.Background(), http.MethodGet, "some-key", nil)
	if err != nil {
		t.Fatalf("newReadRequest: %v", err)
	}

	// Precondition control: the raw transport failure, bypassing Client.do
	// entirely, must already satisfy errors.Is(rawErr, context.DeadlineExceeded)
	// with a live caller context - otherwise this row proves nothing.
	rawResp, rawErr := client.client.Do(req)
	if rawResp != nil {
		_ = rawResp.Body.Close()
	}
	if !errors.Is(rawErr, context.DeadlineExceeded) {
		t.Fatalf("precondition: expected errors.Is(rawErr, context.DeadlineExceeded), got %v", rawErr)
	}
	if req.Context().Err() != nil {
		t.Fatalf("precondition: expected req.Context().Err() == nil, got %v", req.Context().Err())
	}

	resp, err := client.do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
}

// TestClientDoExcludesCallerCancellationFromCacheBackendUnavailable proves a
// caller cancellation during an in-flight round trip reaches the caller as
// context.Canceled and not as ErrCacheBackendUnavailable.
func TestClientDoExcludesCallerCancellationFromCacheBackendUnavailable(t *testing.T) {
	t.Parallel()

	var reachedHandler atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reachedHandler.Store(true)
		<-r.Context().Done() // never respond; the server's own context ends once the client tears the connection down.
	}))
	t.Cleanup(srv.Close)

	cfg := config.S3CacheConfig{
		Endpoint:  srv.URL,
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	client, err := newClient(cfg, srv.Client())
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Backstop for early exits: srv.Close waits for the handler, which only
	// returns once this context ends.
	defer cancel()
	req, err := client.newReadRequest(ctx, http.MethodGet, "some-key", nil)
	if err != nil {
		t.Fatalf("newReadRequest: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		resp, doErr := client.do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		errCh <- doErr
	}()

	// Wait for the handler to actually observe the request before canceling,
	// so the cancellation races an in-flight round trip rather than a request
	// that has not even been dispatched yet.
	waitForLockEvent(t, "the handler to receive the in-flight request", reachedHandler.Load)
	cancel()

	select {
	case err = <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Client.do did not return after caller cancellation")
	}

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected errors.Is(err, context.Canceled), got %v", err)
	}
	if errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected NOT errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
}

// TestOpenProbeFailureClassifiesAsCacheBackendUnusableNotUnavailable proves a
// store failing Open's conditional-write probe is ErrCacheBackendUnusable and
// never also ErrCacheBackendUnavailable.
func TestOpenProbeFailureClassifiesAsCacheBackendUnusableNotUnavailable(t *testing.T) {
	t.Parallel()
	b := newNonConformingTestBackend(t)
	ctx := context.Background()

	err := b.Open(ctx)

	if !errors.Is(err, helpers.ErrCacheBackendUnusable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnusable), got %v", err)
	}
	if errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected NOT errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
}
