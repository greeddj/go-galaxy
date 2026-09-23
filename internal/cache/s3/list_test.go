package s3

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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

// paginationTestTimeout bounds a paginated call, so a client that never gets
// past the first page fails the test instead of hanging it.
const paginationTestTimeout = 30 * time.Second

// seedPaginationKeys writes five objects under the artifacts prefix, sorted,
// so a page size of 2 splits their listing into pages of 2, 2 and 1.
func seedPaginationKeys(ctx context.Context, t *testing.T, b *Backend) []string {
	t.Helper()
	keys := []string{
		b.key(artifactsPrefix, "a.tar.gz"),
		b.key(artifactsPrefix, "b.tar.gz"),
		b.key(artifactsPrefix, "c.tar.gz"),
		b.key(artifactsPrefix, "d.tar.gz"),
		b.key(artifactsPrefix, "e.tar.gz"),
	}
	seedDeleteTestObjects(ctx, t, b, keys)
	return keys
}

// assertContinuationTokensEchoed fails the test unless the fake issued
// wantIssued tokens and the client sent none first, then each one verbatim.
func assertContinuationTokensEchoed(t *testing.T, fake *fakeS3, wantIssued int) {
	t.Helper()
	issued, received := fake.listContinuationTokens()
	if len(issued) != wantIssued {
		t.Fatalf("expected the fake to issue %d continuation tokens, got %v", wantIssued, issued)
	}
	if want := append([]string{""}, issued...); !slices.Equal(received, want) {
		t.Fatalf("expected list requests to carry continuation tokens %q, got %q", want, received)
	}
}

// TestListObjectsFollowsContinuationToken pins that listObjects walks a
// truncated listing to its last page, sending each NextContinuationToken back
// verbatim, and returns every page's keys in order.
func TestListObjectsFollowsContinuationToken(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx, cancel := context.WithTimeout(t.Context(), paginationTestTimeout)
	defer cancel()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := seedPaginationKeys(ctx, t, b)
	fake.setListPageSize(2)

	got, err := b.client.listObjects(ctx, b.key(artifactsPrefix))
	if err != nil {
		t.Fatalf("listObjects() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listObjects() = %v, want %v", got, want)
	}
	if got := fake.requestCount(bucketListKey, http.MethodGet); got != 3 {
		t.Fatalf("expected 3 list requests for 5 keys at 2 per page, got %d", got)
	}
	assertContinuationTokensEchoed(t, fake, 2)
}

// TestListingLoopsStopAtTruncatedPageWithoutToken pins that both listing loops
// end at a truncated page carrying no NextContinuationToken, rather than
// listing again from the first page with no token.
func TestListingLoopsStopAtTruncatedPageWithoutToken(t *testing.T) {
	t.Parallel()
	walks := map[string]func(context.Context, *Backend) ([]string, error){
		"listObjects": func(ctx context.Context, b *Backend) ([]string, error) {
			return b.client.listObjects(ctx, b.key(artifactsPrefix))
		},
		"deleteAllUnderPrefix": func(ctx context.Context, b *Backend) ([]string, error) {
			return nil, b.client.deleteAllUnderPrefix(ctx, b.key(artifactsPrefix))
		},
	}
	for name, walk := range walks {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, fake := newTestBackendAndFake(t)
			ctx, cancel := context.WithTimeout(t.Context(), paginationTestTimeout)
			defer cancel()

			if err := b.Open(ctx); err != nil {
				t.Fatalf("Open: %v", err)
			}
			keys := seedPaginationKeys(ctx, t, b)
			fake.setListPageSize(2)
			fake.setListOmitNextToken(true)

			listed, err := walk(ctx, b)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if name == "listObjects" && !slices.Equal(listed, keys[:2]) {
				t.Fatalf("listObjects() = %v, want the first page %v", listed, keys[:2])
			}
			if got := fake.requestCount(bucketListKey, http.MethodGet); got != 1 {
				t.Fatalf("expected 1 list request before stopping, got %d", got)
			}
		})
	}
}
