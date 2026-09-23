package s3

// Tests for the cache-backend classes: exhausted retries, transport failures,
// cut-off or undecodable bodies and an overwrite's 412 are ErrCacheBackendUnavailable,
// an Open probe failure only ErrCacheBackendUnusable, and a cancellation neither.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// TestSaveStoreClassifiesOverwritePreconditionFailureAsCacheBackendUnavailable
// proves a 412 to the snapshot's unconditional PUT is errS3PutFailed, after one
// attempt, and never the lock loop's errS3PreconditionFailed.
func TestSaveStoreClassifiesOverwritePreconditionFailureAsCacheBackendUnavailable(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	key := b.key(statePrefix, storeObject)
	fake.failNext(key, http.MethodPut, http.StatusPreconditionFailed, 1)

	err := b.SaveStore(ctx, store.New())
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
	if !errors.Is(err, errS3PutFailed) {
		t.Fatalf("expected errors.Is(err, errS3PutFailed), got %v", err)
	}
	if errors.Is(err, errS3PreconditionFailed) {
		t.Fatalf("expected NOT errors.Is(err, errS3PreconditionFailed), got %v", err)
	}
	if got := fake.requestCount(key, http.MethodPut); got != 1 {
		t.Fatalf("expected exactly 1 PUT attempt for a 412, got %d", got)
	}
}

// TestClearFilesClassifiesUndecodableListingAsCacheBackendUnavailable proves a
// listing page answered 200 with a body that is not XML is errS3BucketRequestFailed
// naming the listing response, after one request.
func TestClearFilesClassifiesUndecodableListingAsCacheBackendUnavailable(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	fake.failNextWithBody(bucketListKey, http.MethodGet, http.StatusOK, 1, []byte("not a listing"))

	err := b.ClearFiles(ctx)
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
	if !errors.Is(err, errS3BucketRequestFailed) {
		t.Fatalf("expected errors.Is(err, errS3BucketRequestFailed), got %v", err)
	}
	if !strings.Contains(err.Error(), "s3 listing response does not decode") {
		t.Fatalf("expected the error to name the s3 listing response, got %q", err.Error())
	}
	if got := fake.requestCount(bucketListKey, http.MethodGet); got != 1 {
		t.Fatalf("expected exactly 1 listing request, got %d", got)
	}
}

// TestClearFilesClassifiesUndecodableBatchDeleteAsCacheBackendUnavailable proves
// a batch delete answered 200 with a body that is not XML is errS3DeleteFailed
// naming the batch-delete response, after one request.
func TestClearFilesClassifiesUndecodableBatchDeleteAsCacheBackendUnavailable(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	key := b.key(artifactsPrefix, "a.tar.gz")
	if err := b.client.putObject(ctx, key, bytes.NewReader([]byte("x")), 1,
		putObjectAttrs{contentType: "application/gzip"}, putCondition{}); err != nil {
		t.Fatalf("putObject(%q): %v", key, err)
	}
	fake.failNextWithBody(bucketDeleteObjectsKey, http.MethodPost, http.StatusOK, 1, []byte("not a delete result"))

	err := b.ClearFiles(ctx)
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
	if !errors.Is(err, errS3DeleteFailed) {
		t.Fatalf("expected errors.Is(err, errS3DeleteFailed), got %v", err)
	}
	if !strings.Contains(err.Error(), "s3 batch-delete response does not decode") {
		t.Fatalf("expected the error to name the s3 batch-delete response, got %q", err.Error())
	}
	if got := fake.requestCount(bucketDeleteObjectsKey, http.MethodPost); got != 1 {
		t.Fatalf("expected exactly 1 batch-delete request, got %d", got)
	}
}

// TestDeleteAllUnderPrefixClassifiesTruncatedListingAsCacheBackendUnavailable
// proves a listing body that breaks off partway is errS3BucketRequestFailed
// naming the listing response, not a bare unexpected EOF.
func TestDeleteAllUnderPrefixClassifiesTruncatedListingAsCacheBackendUnavailable(t *testing.T) {
	t.Parallel()
	client := newHandlerTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeTruncatedBody(w, `<?xml version="1.0"?><ListBucketResult>`)
	}, nil)

	err := client.deleteAllUnderPrefix(t.Context(), "artifacts")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("precondition: expected the listing read to fail with io.ErrUnexpectedEOF, got %v", err)
	}
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
	if !errors.Is(err, errS3BucketRequestFailed) {
		t.Fatalf("expected errors.Is(err, errS3BucketRequestFailed), got %v", err)
	}
	if !strings.Contains(err.Error(), "s3 listing response") {
		t.Fatalf("expected the error to name the s3 listing response, got %q", err.Error())
	}
}

// TestDeleteAllUnderPrefixClassifiesTruncatedBatchDeleteAsCacheBackendUnavailable
// proves a batch-delete body that breaks off partway is errS3DeleteFailed
// naming the batch-delete response, not a bare unexpected EOF.
func TestDeleteAllUnderPrefixClassifiesTruncatedBatchDeleteAsCacheBackendUnavailable(t *testing.T) {
	t.Parallel()
	client := newHandlerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeTruncatedBody(w, `<?xml version="1.0"?><DeleteResult>`)
			return
		}
		_, _ = io.WriteString(w, `<ListBucketResult><Contents><Key>artifacts/a.tar.gz</Key></Contents></ListBucketResult>`)
	}, nil)

	err := client.deleteAllUnderPrefix(t.Context(), "artifacts")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("precondition: expected the batch-delete read to fail with io.ErrUnexpectedEOF, got %v", err)
	}
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
	if !errors.Is(err, errS3DeleteFailed) {
		t.Fatalf("expected errors.Is(err, errS3DeleteFailed), got %v", err)
	}
	if !strings.Contains(err.Error(), "s3 batch-delete response") {
		t.Fatalf("expected the error to name the s3 batch-delete response, got %q", err.Error())
	}
}

// TestListingBodyReadCanceledPartwayStaysUnclassified proves a listing body read
// the caller cancels partway reaches it as context.Canceled and not as
// ErrCacheBackendUnavailable, as a canceled round trip does in do.
func TestListingBodyReadCanceledPartwayStaysUnclassified(t *testing.T) {
	t.Parallel()
	headers := make(chan struct{}, 1)
	client := newHandlerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `<ListBucketResult>`)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}, func(base http.RoundTripper) http.RoundTripper {
		return headersSeenTransport{base: base, seen: headers}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.deleteAllUnderPrefix(ctx, "artifacts") }()

	// Cancel only once the response is in hand, so the cancellation lands in
	// the body read rather than in the round trip do already covers.
	select {
	case <-headers:
	case <-time.After(5 * time.Second):
		t.Fatal("the listing response headers never arrived")
	}
	cancel()

	var err error
	select {
	case err = <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("deleteAllUnderPrefix did not return after the caller canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected errors.Is(err, context.Canceled), got %v", err)
	}
	if errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected NOT errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
}

// headersSeenTransport signals seen, without blocking, each time base returns
// a response, which is when the caller starts reading its body.
type headersSeenTransport struct {
	base http.RoundTripper
	seen chan<- struct{}
}

// RoundTrip delegates to base and signals seen on a response.
func (h headersSeenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := h.base.RoundTrip(req)
	if err == nil {
		select {
		case h.seen <- struct{}{}:
		default:
		}
	}
	return resp, err
}

// newHandlerTestClient builds a *Client against a server running handler, over
// the server's transport as wrap returns it (unchanged for a nil wrap), for a
// response the fake cannot shape, such as a body that breaks off partway.
func newHandlerTestClient(
	t *testing.T,
	handler http.HandlerFunc,
	wrap func(http.RoundTripper) http.RoundTripper,
) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	httpClient := srv.Client()
	if wrap != nil {
		httpClient.Transport = wrap(httpClient.Transport)
	}
	cfg := config.S3CacheConfig{
		Endpoint:  srv.URL,
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
	return client
}

// writeTruncatedBody answers 200 promising more bytes than partial holds, so
// the server drops the connection after it and the client's read fails partway.
func writeTruncatedBody(w http.ResponseWriter, partial string) {
	w.Header().Set("Content-Length", strconv.Itoa(len(partial)+1024))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, partial)
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
