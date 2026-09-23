package s3

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// seedDeleteTestObjects writes a 1-byte object under each key via putObject,
// failing the test immediately on any write error.
func seedDeleteTestObjects(ctx context.Context, t *testing.T, b *Backend, keys []string) {
	t.Helper()
	for _, key := range keys {
		if err := b.client.putObject(ctx, key, bytes.NewReader([]byte("x")), 1,
			putObjectAttrs{contentType: "application/octet-stream"}, putCondition{}); err != nil {
			t.Fatalf("seed object %q: %v", key, err)
		}
	}
}

// assertObjectAbsent fails the test unless key is gone (HEAD reports
// errS3NotFound).
func assertObjectAbsent(ctx context.Context, t *testing.T, b *Backend, key string) {
	t.Helper()
	if _, err := b.client.headObject(ctx, key); !errors.Is(err, errS3NotFound) {
		t.Fatalf("expected key %q to be absent, headObject error = %v", key, err)
	}
}

// assertObjectPresent fails the test unless key still exists (HEAD succeeds).
func assertObjectPresent(ctx context.Context, t *testing.T, b *Backend, key string) {
	t.Helper()
	if _, err := b.client.headObject(ctx, key); err != nil {
		t.Fatalf("expected key %q to still be present, headObject error = %v", key, err)
	}
}

// TestClearFilesBatchesDeletes pins that ClearFiles removes a page of keys with
// exactly one DeleteObjects (POST ?delete) request and no per-key DELETE.
func TestClearFilesBatchesDeletes(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	keys := []string{
		b.key(artifactsPrefix, "a.tar.gz"),
		b.key(artifactsPrefix, "b.tar.gz"),
		b.key(artifactsPrefix, "c.tar.gz"),
		b.key(artifactsPrefix, "d.tar.gz"),
		b.key(artifactsPrefix, "e.tar.gz"),
	}
	seedDeleteTestObjects(ctx, t, b, keys)

	if err := b.ClearFiles(ctx); err != nil {
		t.Fatalf("ClearFiles: %v", err)
	}

	if got := fake.requestCount(bucketDeleteObjectsKey, http.MethodPost); got != 1 {
		t.Fatalf("expected exactly 1 DeleteObjects POST for a single page of 5 keys, got %d", got)
	}
	for _, key := range keys {
		assertObjectAbsent(ctx, t, b, key)
		if got := fake.requestCount(key, http.MethodDelete); got != 0 {
			t.Fatalf("expected key %q to receive 0 per-object DELETEs (batched instead), got %d", key, got)
		}
	}
}

// TestDeleteObjectsChunksAtLimit pins that deleteObjects splits keys into
// ceil(len(keys)/maxKeys) batches, driven through the maxKeys seam.
func TestDeleteObjectsChunksAtLimit(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	keys := []string{"chunk-a", "chunk-b", "chunk-c", "chunk-d", "chunk-e"}
	seedDeleteTestObjects(ctx, t, b, keys)

	if err := b.client.deleteObjects(ctx, keys, 2); err != nil {
		t.Fatalf("deleteObjects: %v", err)
	}

	const wantBatches = 3 // ceil(5/2)
	if got := fake.requestCount(bucketDeleteObjectsKey, http.MethodPost); got != wantBatches {
		t.Fatalf("expected %d DeleteObjects batches for 5 keys at maxKeys=2, got %d", wantBatches, got)
	}
	for _, key := range keys {
		assertObjectAbsent(ctx, t, b, key)
	}
}

// TestClearFilesSurfacesPerKeyError pins that a per-key <Error> inside a 200
// DeleteResult fails ClearFiles with an error naming the key and code.
func TestClearFilesSurfacesPerKeyError(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	okKey := b.key(artifactsPrefix, "ok.tar.gz")
	failKey := b.key(artifactsPrefix, "denied.tar.gz")
	seedDeleteTestObjects(ctx, t, b, []string{okKey, failKey})
	fake.failDeleteObjectsFor(failKey, "AccessDenied", "simulated per-key failure")

	err := b.ClearFiles(ctx)
	if !errors.Is(err, errS3DeleteFailed) {
		t.Fatalf("expected errors.Is(err, errS3DeleteFailed), got %v", err)
	}
	if got := err.Error(); !strings.Contains(got, failKey) || !strings.Contains(got, "AccessDenied") {
		t.Fatalf("expected the error to name the failed key %q and code AccessDenied, got %q", failKey, got)
	}
}

// TestDeleteObjectsBatchRetriesTransientFailureThenSucceeds pins that
// deleteObjectsBatch recovers from two transient 503s like the other
// idempotent verbs, and that the keys end up deleted.
func TestDeleteObjectsBatchRetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	keys := []string{
		b.key(artifactsPrefix, "retry-a.tar.gz"),
		b.key(artifactsPrefix, "retry-b.tar.gz"),
	}
	seedDeleteTestObjects(ctx, t, b, keys)

	fake.failNext(bucketDeleteObjectsKey, http.MethodPost, http.StatusServiceUnavailable, 2)

	if err := b.client.deleteObjectsBatch(ctx, keys); err != nil {
		t.Fatalf("expected deleteObjectsBatch to recover after two transient failures, got %v", err)
	}
	if got := fake.requestCount(bucketDeleteObjectsKey, http.MethodPost); got != 3 {
		t.Fatalf("expected exactly 3 DeleteObjects attempts (2 failures + 1 success), got %d", got)
	}
	for _, key := range keys {
		assertObjectAbsent(ctx, t, b, key)
	}
}

// TestDeleteObjectsResponseBounded pins that the DeleteResult read is capped at
// helpers.S3ListMaxSize, with an error naming the batch-delete surface; its
// positive control is TestDeleteObjectsBatchRetriesTransientFailureThenSucceeds.
func TestDeleteObjectsResponseBounded(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	fake.oversizedDeleteResult = true

	err := b.client.deleteObjectsBatch(ctx, []string{b.key(artifactsPrefix, "whatever.tar.gz")})
	if !errors.Is(err, helpers.ErrResponseTooLarge) {
		t.Fatalf("deleteObjectsBatch() error = %v, want ErrResponseTooLarge", err)
	}
	if !strings.Contains(err.Error(), "s3 batch-delete response") {
		t.Fatalf("deleteObjectsBatch() error = %q, want it to name the s3 batch-delete response surface", err.Error())
	}
}

// TestDeleteObjectsXMLSafeKeys pins that a key carrying XML markup stays one
// literal <Object> entry deleting only itself, and that the request carries
// Content-MD5, X-Amz-Content-Sha256 and an XML Content-Type.
func TestDeleteObjectsXMLSafeKeys(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	const maliciousKey = "foo</Key></Object><Object><Key>bar"
	safeKey := b.key(artifactsPrefix, "safe.tar.gz")
	seedDeleteTestObjects(ctx, t, b, []string{maliciousKey, safeKey})

	if err := b.client.deleteObjectsBatch(ctx, []string{maliciousKey}); err != nil {
		t.Fatalf("deleteObjectsBatch: %v", err)
	}

	req := fake.deleteObjectsRequest()
	if len(req.Objects) != 1 {
		t.Fatalf("expected exactly 1 <Object> entry (no XML injection splitting), got %d: %+v", len(req.Objects), req.Objects)
	}
	if req.Objects[0].Key != maliciousKey {
		t.Fatalf("expected the single literal key %q, got %q", maliciousKey, req.Objects[0].Key)
	}

	assertObjectAbsent(ctx, t, b, maliciousKey)
	assertObjectPresent(ctx, t, b, safeKey)

	headers := fake.deleteObjectsHeaders()
	if headers.Get("Content-MD5") == "" {
		t.Fatal("expected a non-empty Content-MD5 header on the DeleteObjects request")
	}
	if headers.Get("X-Amz-Content-Sha256") == "" {
		t.Fatal("expected a non-empty X-Amz-Content-Sha256 header on the DeleteObjects request")
	}
	if got := headers.Get("Content-Type"); got != "application/xml" {
		t.Fatalf("expected Content-Type: application/xml, got %q", got)
	}
}
