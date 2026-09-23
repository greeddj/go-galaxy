package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// slowDownErrorBody is a representative S3 XML <Error> document: a 503
// response body carrying a throttling error code and a human-readable
// message, exactly the shape s3StatusError is meant to surface.
const slowDownErrorBody = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<Error><Code>SlowDown</Code><Message>Please reduce your request rate.</Message></Error>`

// TestGetObjectSurfacesXMLErrorDetails pins that a failed GET folds the S3
// <Error> Code and Message into an errS3GetFailed error, and that a persistent
// 503 still does so after exactly s3RetryMaxAttempts attempts.
func TestGetObjectSurfacesXMLErrorDetails(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "get-error-object"
	fake.failNextWithBody(key, http.MethodGet, http.StatusServiceUnavailable, -1, []byte(slowDownErrorBody))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := getObjectError(ctx, t, b, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3GetFailed) {
		t.Fatalf("expected errors.Is(err, errS3GetFailed), got %v", err)
	}
	assertContainsCodeAndMessage(t, err, "SlowDown", "Please reduce your request rate.")
	if got := fake.requestCount(key, http.MethodGet); got != s3RetryMaxAttempts {
		t.Fatalf("expected exactly s3RetryMaxAttempts=%d GET attempts against a persistently failing key, got %d",
			s3RetryMaxAttempts, got)
	}
}

// TestDeleteObjectSurfacesXMLErrorDetails pins the same enrichment for
// deleteObject. The failure is armed indefinitely: with a bounded count the
// last retry would find the never-created key already deleted.
func TestDeleteObjectSurfacesXMLErrorDetails(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "delete-error-object"
	fake.failNextWithBody(key, http.MethodDelete, http.StatusServiceUnavailable, -1, []byte(slowDownErrorBody))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := b.client.deleteObject(ctx, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3DeleteFailed) {
		t.Fatalf("expected errors.Is(err, errS3DeleteFailed), got %v", err)
	}
	assertContainsCodeAndMessage(t, err, "SlowDown", "Please reduce your request rate.")
}

// TestPutObjectSurfacesXMLErrorDetails pins the same enrichment for
// handlePutResponse's default arm on an unconditional PUT, armed indefinitely
// so that no retry manages to write the object.
func TestPutObjectSurfacesXMLErrorDetails(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "put-error-object"
	fake.failNextWithBody(key, http.MethodPut, http.StatusServiceUnavailable, -1, []byte(slowDownErrorBody))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	payload := []byte("payload")
	err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)), putObjectAttrs{}, putCondition{})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3PutFailed) {
		t.Fatalf("expected errors.Is(err, errS3PutFailed), got %v", err)
	}
	assertContainsCodeAndMessage(t, err, "SlowDown", "Please reduce your request rate.")
}

// TestGetObjectFallsBackToStatusOnlyOnMalformedBody pins that a non-XML error
// body yields the status-only form, never the parse failure, and still matches
// errS3GetFailed.
func TestGetObjectFallsBackToStatusOnlyOnMalformedBody(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "get-malformed-body"
	fake.failNextWithBody(key, http.MethodGet, http.StatusInternalServerError, -1, []byte("not xml at all"))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := getObjectError(ctx, t, b, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3GetFailed) {
		t.Fatalf("expected errors.Is(err, errS3GetFailed), got %v", err)
	}
	if strings.Contains(err.Error(), "(") {
		t.Fatalf("expected a status-only error for a malformed body, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), http.StatusText(http.StatusInternalServerError)) {
		t.Fatalf("expected the status text in the fallback error, got %q", err.Error())
	}
}

// TestGetObjectFallsBackToStatusOnlyOnEmptyBody pins the same status-only
// fallback for an error response with no body, as a misconfigured proxy sends.
func TestGetObjectFallsBackToStatusOnlyOnEmptyBody(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "get-empty-body"
	fake.failNext(key, http.MethodGet, http.StatusBadGateway, -1)

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := getObjectError(ctx, t, b, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3GetFailed) {
		t.Fatalf("expected errors.Is(err, errS3GetFailed), got %v", err)
	}
	if strings.Contains(err.Error(), "(") {
		t.Fatalf("expected a status-only error for an empty body, got %q", err.Error())
	}
}

// TestHeadObjectStaysStatusOnly pins that headObject never appends a code or
// message even when the fake arms a body, since net/http elides a HEAD body.
func TestHeadObjectStaysStatusOnly(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "head-error-object"
	fake.failNextWithBody(key, http.MethodHead, http.StatusServiceUnavailable, -1, []byte(slowDownErrorBody))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err := b.client.headObject(ctx, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3HeadFailed) {
		t.Fatalf("expected errors.Is(err, errS3HeadFailed), got %v", err)
	}
	if strings.Contains(err.Error(), "(") {
		t.Fatalf("expected a status-only error for a HEAD response, got %q", err.Error())
	}
}

// TestGetObjectRetriesTransientFailureThenSucceeds pins that getObject
// recovers from two transient 503s and returns the real body after exactly
// three attempts.
func TestGetObjectRetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "get-retry-then-succeed"
	payload := []byte("recovered payload")
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)),
		putObjectAttrs{contentType: "text/plain"}, putCondition{}); err != nil {
		t.Fatalf("seed object: %v", err)
	}

	fake.failNext(key, http.MethodGet, http.StatusServiceUnavailable, 2)

	resp, err := b.client.getObject(ctx, key)
	if err != nil {
		t.Fatalf("expected getObject to recover after two transient failures, got %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read recovered body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("expected the recovered body %q, got %q", payload, body)
	}
	if got := fake.requestCount(key, http.MethodGet); got != 3 {
		t.Fatalf("expected exactly 3 GET attempts (2 failures + 1 success), got %d", got)
	}
}

// TestHeadObjectRetriesTransientFailureThenSucceeds mirrors
// TestGetObjectRetriesTransientFailureThenSucceeds for headObject.
func TestHeadObjectRetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "head-retry-then-succeed"
	payload := []byte("payload")
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)),
		putObjectAttrs{contentType: "text/plain"}, putCondition{}); err != nil {
		t.Fatalf("seed object: %v", err)
	}

	fake.failNext(key, http.MethodHead, http.StatusServiceUnavailable, 2)

	if _, err := b.client.headObject(ctx, key); err != nil {
		t.Fatalf("expected headObject to recover after two transient failures, got %v", err)
	}
	if got := fake.requestCount(key, http.MethodHead); got != 3 {
		t.Fatalf("expected exactly 3 HEAD attempts (2 failures + 1 success), got %d", got)
	}
}

// TestDeleteObjectRetriesTransientFailureThenSucceeds mirrors
// TestGetObjectRetriesTransientFailureThenSucceeds for deleteObject.
func TestDeleteObjectRetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "delete-retry-then-succeed"
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	fake.failNext(key, http.MethodDelete, http.StatusServiceUnavailable, 2)

	if err := b.client.deleteObject(ctx, key); err != nil {
		t.Fatalf("expected deleteObject to recover after two transient failures, got %v", err)
	}
	if got := fake.requestCount(key, http.MethodDelete); got != 3 {
		t.Fatalf("expected exactly 3 DELETE attempts (2 failures + 1 success), got %d", got)
	}
}

// TestPutObjectUnconditionalRetriesTransientFailureThenSucceeds pins that an
// overwrite PUT recovers from two transient 503s by resending its reseeked
// body, and that the object lands.
func TestPutObjectUnconditionalRetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "put-retry-then-succeed"
	payload := []byte("recovered payload")
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	fake.failNext(key, http.MethodPut, http.StatusServiceUnavailable, 2)

	if err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)),
		putObjectAttrs{contentType: "text/plain"}, putCondition{}); err != nil {
		t.Fatalf("expected putObject to recover after two transient failures, got %v", err)
	}
	if got := fake.requestCount(key, http.MethodPut); got != 3 {
		t.Fatalf("expected exactly 3 PUT attempts (2 failures + 1 success), got %d", got)
	}

	resp, err := b.client.getObject(ctx, key)
	if err != nil {
		t.Fatalf("getObject after recovery: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read written body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("expected the written body %q, got %q", payload, body)
	}
}

// TestPutObjectConditionalDoesNotRetryOnTransientFailure pins that a
// create-if-absent PUT is issued once even on a retryable status: a retried
// lost success would read as 412 contention and stall the lock.
func TestPutObjectConditionalDoesNotRetryOnTransientFailure(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "put-conditional-no-retry"
	payload := []byte("payload")
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	fake.failNext(key, http.MethodPut, http.StatusServiceUnavailable, -1)

	err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)),
		putObjectAttrs{contentType: "text/plain"}, putCondition{ifNoneMatch: true})
	if !errors.Is(err, errS3PutFailed) {
		t.Fatalf("expected errors.Is(err, errS3PutFailed), got %v", err)
	}
	if got := fake.requestCount(key, http.MethodPut); got != 1 {
		t.Fatalf("expected a conditional PUT to be issued exactly once despite a retryable status, got %d attempts", got)
	}
}

// TestGetObjectNotFoundIsNotRetried pins that a never-written key is
// errS3NotFound after exactly one attempt.
func TestGetObjectNotFoundIsNotRetried(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "get-never-written"
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	err := getObjectError(ctx, t, b, key)
	if !errors.Is(err, errS3NotFound) {
		t.Fatalf("expected errors.Is(err, errS3NotFound), got %v", err)
	}
	if got := fake.requestCount(key, http.MethodGet); got != 1 {
		t.Fatalf("expected exactly 1 GET attempt for a not-found key, got %d", got)
	}
}

// TestPutObjectPreconditionFailedIsNotRetried pins that a 412 on a
// conditional PUT is errS3PreconditionFailed after exactly one attempt.
func TestPutObjectPreconditionFailedIsNotRetried(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "put-precondition-no-retry"
	payload := []byte("payload")
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	fake.failNext(key, http.MethodPut, http.StatusPreconditionFailed, 1)

	err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)),
		putObjectAttrs{contentType: "text/plain"}, putCondition{ifNoneMatch: true})
	if !errors.Is(err, errS3PreconditionFailed) {
		t.Fatalf("expected errors.Is(err, errS3PreconditionFailed), got %v", err)
	}
	if got := fake.requestCount(key, http.MethodPut); got != 1 {
		t.Fatalf("expected exactly 1 PUT attempt for a precondition failure, got %d", got)
	}
}

// newRedirectRefusalFixture builds a *Client against a front server that
// answers 302 to a target server when redirect is true, else 200, and returns
// both servers' request counters.
func newRedirectRefusalFixture(t *testing.T, redirect bool) (*Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()

	var target atomic.Int32
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		target.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(targetSrv.Close)

	var front atomic.Int32
	frontSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		front.Add(1)
		if redirect {
			//nolint:gosec // G710: a test double redirecting to the in-process target server.
			http.Redirect(w, r, targetSrv.URL+r.URL.Path, http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(frontSrv.Close)

	cfg := config.S3CacheConfig{
		Endpoint:  frontSrv.URL,
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	c, err := newClient(cfg, frontSrv.Client())
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c, &front, &target
}

// TestClientRefusesEndpointRedirect pins that an endpoint redirect fails with
// ErrCacheBackendUnusable and not also ErrCacheBackendUnavailable, reaches no
// target, and is not retried.
func TestClientRefusesEndpointRedirect(t *testing.T) {
	t.Parallel()
	client, frontRequests, targetRequests := newRedirectRefusalFixture(t, true)

	resp, err := client.getObject(context.Background(), "redirect-object")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, helpers.ErrCacheBackendUnusable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnusable), got %v", err)
	}
	if errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected err to NOT also carry helpers.ErrCacheBackendUnavailable "+
			"(exclusive with helpers.ErrCacheBackendUnusable per variables.go's partition), got %v", err)
	}
	// Documentary: a request reaching the target would already have made
	// getObject succeed, which the nil-error check above catches first.
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("expected zero requests reaching the redirect target, got %d", got)
	}
	// Pinned, and not by the same state as the exclusivity check above: a
	// classifier that retried the refusal would leave the error tree clean
	// and drive this count to s3RetryMaxAttempts, failing here alone.
	if got := frontRequests.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request to the configured endpoint (not retried), got %d", got)
	}
}

// TestClientRefusesEndpointRedirectPositiveControl runs the same fixture
// without the redirect and expects success, so the refusal above is caused by
// the redirect rather than by the fixture.
func TestClientRefusesEndpointRedirectPositiveControl(t *testing.T) {
	t.Parallel()
	client, frontRequests, targetRequests := newRedirectRefusalFixture(t, false)

	resp, err := client.getObject(context.Background(), "redirect-object")
	if err != nil {
		t.Fatalf("expected getObject to succeed against a direct 200 response, got %v", err)
	}
	_ = resp.Body.Close()
	if got := frontRequests.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request to the configured endpoint, got %d", got)
	}
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("expected zero requests to the unused redirect target, got %d", got)
	}
}

// TestS3RedirectPolicyDoesNotAffectTheSharedClient pins that newClient sets
// its redirect refusal on a private copy: the *http.Client it was given, which
// internal/galaxy/fetch shares, still follows a redirect.
func TestS3RedirectPolicyDoesNotAffectTheSharedClient(t *testing.T) {
	t.Parallel()

	var targetRequests atomic.Int32
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(targetSrv.Close)

	frontSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//nolint:gosec // G710: a test double redirecting to the in-process target server.
		http.Redirect(w, r, targetSrv.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(frontSrv.Close)

	shared := frontSrv.Client()

	cfg := config.S3CacheConfig{
		Endpoint:  frontSrv.URL,
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	if _, err := newClient(cfg, shared); err != nil {
		t.Fatalf("newClient: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, frontSrv.URL+"/probe", nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext: %v", err)
	}
	resp, err := shared.Do(req)
	if err != nil {
		t.Fatalf("expected the original, shared client to still follow a redirect, got %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected a 200 from the followed redirect, got %d", resp.StatusCode)
	}
	if got := targetRequests.Load(); got != 1 {
		t.Fatalf("expected the redirect to be followed and reach the target exactly once, got %d", got)
	}
}

// assertContainsCodeAndMessage fails the test unless err's message contains
// both code and message, which s3StatusError appends in parentheses after
// the HTTP status line.
func assertContainsCodeAndMessage(t *testing.T, err error, code, message string) {
	t.Helper()
	got := err.Error()
	if !strings.Contains(got, code) {
		t.Fatalf("expected error to contain code %q, got %q", code, got)
	}
	if !strings.Contains(got, message) {
		t.Fatalf("expected error to contain message %q, got %q", message, got)
	}
}

// getObjectError calls getObject and returns its error, closing a response
// defensively although getObject returns none on failure.
func getObjectError(ctx context.Context, t *testing.T, b *Backend, key string) error {
	t.Helper()
	resp, err := b.client.getObject(ctx, key)
	if resp != nil {
		defer func() {
			_ = resp.Body.Close()
		}()
	}
	return err
}

// getObjectRetryResponseHeaderTimeout is the ResponseHeaderTimeout every row
// below arms: short enough to keep the test fast, long enough that the local
// loopback dial itself never races it.
const getObjectRetryResponseHeaderTimeout = 150 * time.Millisecond

// TestGetObjectRetriesAResponseHeaderTimeout pins that an endpoint accepting
// connections but never answering is tried s3RetryMaxAttempts times, although
// its timeout matches DeadlineExceeded while the caller's context is live.
func TestGetObjectRetriesAResponseHeaderTimeout(t *testing.T) {
	t.Parallel()

	ln, accepted := newAcceptingNeverRespondingListener(t)
	httpClient := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: getObjectRetryResponseHeaderTimeout}}

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

	resp, err := client.getObject(context.Background(), "some-key")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected an error against an endpoint that never answers, got nil")
	}
	if !errors.Is(err, errS3TransportFailed) {
		t.Fatalf("expected errors.Is(err, errS3TransportFailed), got %v", err)
	}
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
	if got := accepted.Load(); got != s3RetryMaxAttempts {
		t.Fatalf("expected exactly s3RetryMaxAttempts=%d accepted connections, got %d", s3RetryMaxAttempts, got)
	}
}

// TestGetObjectRecoversFromATransientTransportFailure pins that the
// errS3TransportFailed retry recovers: two connections close without a status
// line, and the third request is answered normally.
func TestGetObjectRecoversFromATransientTransportFailure(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	const key = "transient-transport-object"
	payload := []byte("recovered after a transport failure")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) <= 2 {
			// t.Errorf, not t.Fatal: a Fatal on the handler goroutine would
			// leave the client waiting and turn the failure into a timeout.
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("ResponseWriter does not support hijacking")
				return
			}
			conn, _, hijackErr := hijacker.Hijack()
			if hijackErr != nil {
				t.Errorf("Hijack: %v", hijackErr)
				return
			}
			_ = conn.Close() // close without writing anything: a genuine transport failure, not a status.
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
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

	resp, err := client.getObject(context.Background(), key)
	if err != nil {
		t.Fatalf("expected getObject to recover from a transient transport failure, got %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read recovered body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("expected the recovered body %q, got %q", payload, body)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("expected exactly 3 requests (2 transport failures + 1 success), got %d", got)
	}
}

// sessionTokenHeaderCase is one row for TestSessionTokenHeader.
type sessionTokenHeaderCase struct {
	name  string
	token config.Secret
	want  string
}

// TestSessionTokenHeader pins when X-Amz-Security-Token is sent: a configured
// token is, and a never-set Secret sends no header at all, since a strict
// endpoint rejects an empty one.
func TestSessionTokenHeader(t *testing.T) {
	t.Parallel()

	cases := []sessionTokenHeaderCase{
		{name: "configured token is sent", token: config.NewSecret("st"), want: "st"},
		{name: "unset token sends no header", token: config.Secret{}, want: ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got string
			var seen atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Amz-Security-Token")
				seen.Store(true)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			cfg := config.S3CacheConfig{
				Endpoint:     srv.URL,
				Bucket:       "test",
				Region:       "us-east-1",
				AccessKey:    "x",
				SecretKey:    config.NewSecret("y"),
				SessionToken: tt.token,
				PathStyle:    true,
				Enabled:      true,
			}
			c, err := newClient(cfg, srv.Client())
			if err != nil {
				t.Fatalf("newClient: %v", err)
			}
			resp, err := c.getObject(context.Background(), "any-object")
			if err != nil {
				t.Fatalf("getObject: %v", err)
			}
			_ = resp.Body.Close()

			if !seen.Load() {
				t.Fatalf("the endpoint was never reached, so the header assertion below proves nothing")
			}
			if got != tt.want {
				t.Fatalf("X-Amz-Security-Token = %q, want %q", got, tt.want)
			}
		})
	}
}
