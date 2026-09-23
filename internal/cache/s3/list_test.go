package s3

import (
	"bytes"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestListObjectsRejectsOversizedResponse pins that a ListObjectsV2 page past
// helpers.S3ListMaxSize fails with ErrResponseTooLarge naming the s3 listing
// response, after exactly one request: the overrun is terminal, not retried.
func TestListObjectsRejectsOversizedResponse(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	fake.oversizedList = true

	_, err := b.client.listObjects(ctx, b.key(artifactsPrefix))
	if !errors.Is(err, helpers.ErrResponseTooLarge) {
		t.Fatalf("listObjects() error = %v, want ErrResponseTooLarge", err)
	}
	if !strings.Contains(err.Error(), "s3 listing response") {
		t.Fatalf("listObjects() error = %q, want it to name the s3 listing response surface", err.Error())
	}

	if got := fake.requestCount(bucketListKey, http.MethodGet); got != 1 {
		t.Fatalf("expected exactly 1 request (an over-cap list response is terminal, not retried), got %d", got)
	}
}

// TestListObjectsAcceptsNormalResponse is the positive control for the list
// size cap: an ordinary multi-object ListObjectsV2 page still returns every
// key.
func TestListObjectsAcceptsNormalResponse(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	want := []string{
		b.key(artifactsPrefix, "a.tar.gz"),
		b.key(artifactsPrefix, "b.tar.gz"),
		b.key(artifactsPrefix, "c.tar.gz"),
	}
	for _, key := range want {
		if err := b.client.putObject(ctx, key, bytes.NewReader([]byte("x")), 1,
			putObjectAttrs{contentType: "application/octet-stream"}, putCondition{}); err != nil {
			t.Fatalf("putObject(%q): %v", key, err)
		}
	}

	got, err := b.client.listObjects(ctx, b.key(artifactsPrefix))
	if err != nil {
		t.Fatalf("listObjects() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listObjects() = %v, want %v", got, want)
	}
}
