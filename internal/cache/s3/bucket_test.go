package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// accessDeniedErrorBody is an S3 <Error> document a refusing endpoint sends.
const accessDeniedErrorBody = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`

// bucketFake answers HEAD and PUT on the bucket root "/test" with fixed
// statuses and records each CreateBucket request it receives.
type bucketFake struct {
	createHeader http.Header
	createBody   []byte
	headStatus   int
	createStatus int
	heads        int
	creates      int
	mu           sync.Mutex
}

// ServeHTTP answers a bucket-root HEAD or PUT; anything else is a 404.
func (f *bucketFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/test" {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodHead:
		f.heads++
		w.WriteHeader(f.headStatus)
	case http.MethodPut:
		f.creates++
		f.createBody, _ = io.ReadAll(r.Body)
		f.createHeader = r.Header.Clone()
		w.WriteHeader(f.createStatus)
		if f.createStatus == http.StatusForbidden {
			_, _ = io.WriteString(w, accessDeniedErrorBody)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// counts returns how many HEADs and creates arrived, read under mu since the
// handler runs on the server's goroutine.
func (f *bucketFake) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heads, f.creates
}

// lastCreate returns the body and headers of the last CreateBucket request.
func (f *bucketFake) lastCreate() ([]byte, http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createBody, f.createHeader
}

// newBucketTestClient builds a path-style *Client for bucket "test" in region
// against a server running handler.
func newBucketTestClient(t *testing.T, handler http.Handler, region string) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := config.S3CacheConfig{
		Endpoint:  srv.URL,
		Bucket:    "test",
		Region:    region,
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
	}
	client, err := newClient(cfg, srv.Client())
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return client
}

// TestEnsureBucketLeavesAnExistingBucketAlone pins that a 200 or 204 to the
// bucket HEAD is existence, so no CreateBucket is ever sent.
func TestEnsureBucketLeavesAnExistingBucketAlone(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusOK, http.StatusNoContent} {
		fake := &bucketFake{headStatus: status}
		if err := newBucketTestClient(t, fake, "us-east-1").ensureBucket(context.Background()); err != nil {
			t.Fatalf("HEAD %d: ensureBucket = %v, want nil", status, err)
		}
		if heads, creates := fake.counts(); heads != 1 || creates != 0 {
			t.Fatalf("HEAD %d: %d HEADs and %d creates, want 1 and 0", status, heads, creates)
		}
	}
}

// TestEnsureBucketCreatesAMissingBucketInItsRegion pins that a 404 HEAD
// triggers CreateBucket: us-east-1 sends no body, any other region a signed
// LocationConstraint document.
func TestEnsureBucketCreatesAMissingBucketInItsRegion(t *testing.T) {
	t.Parallel()

	fake := &bucketFake{headStatus: http.StatusNotFound, createStatus: http.StatusOK}
	if err := newBucketTestClient(t, fake, "us-east-1").ensureBucket(context.Background()); err != nil {
		t.Fatalf("us-east-1: ensureBucket = %v, want nil", err)
	}
	_, creates := fake.counts()
	body, header := fake.lastCreate()
	if creates != 1 || len(body) != 0 || header.Get("Content-Type") != "" {
		t.Fatalf("us-east-1: %d creates with body %q and Content-Type %q, want one bare create",
			creates, body, header.Get("Content-Type"))
	}
	if got := header.Get("X-Amz-Content-Sha256"); got != emptySHA256 {
		t.Fatalf("us-east-1: X-Amz-Content-Sha256 = %q, want the empty payload hash", got)
	}

	fake = &bucketFake{headStatus: http.StatusNotFound, createStatus: http.StatusOK}
	if err := newBucketTestClient(t, fake, "eu-west-1").ensureBucket(context.Background()); err != nil {
		t.Fatalf("eu-west-1: ensureBucket = %v, want nil", err)
	}
	body, header = fake.lastCreate()
	if !strings.Contains(string(body), "<LocationConstraint>eu-west-1</LocationConstraint>") {
		t.Fatalf("eu-west-1: create body = %q, want a LocationConstraint naming the region", body)
	}
	if got := header.Get("Content-Type"); got != "application/xml" {
		t.Fatalf("eu-west-1: Content-Type = %q, want application/xml", got)
	}
	sum := sha256.Sum256(body)
	if got := header.Get("X-Amz-Content-Sha256"); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("eu-west-1: X-Amz-Content-Sha256 = %q, want the body's sha256", got)
	}
}

// TestCreateBucketOutcomes pins that a 409 means the bucket is already ours,
// and that any other refusal is errS3CreateBucketFailed carrying S3's code.
func TestCreateBucketOutcomes(t *testing.T) {
	t.Parallel()

	conflict := &bucketFake{headStatus: http.StatusNotFound, createStatus: http.StatusConflict}
	if err := newBucketTestClient(t, conflict, "us-east-1").ensureBucket(context.Background()); err != nil {
		t.Fatalf("409 to CreateBucket: ensureBucket = %v, want nil", err)
	}

	refused := &bucketFake{headStatus: http.StatusNotFound, createStatus: http.StatusForbidden}
	err := newBucketTestClient(t, refused, "us-east-1").ensureBucket(context.Background())
	if !errors.Is(err, errS3CreateBucketFailed) || !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("403 to CreateBucket: ensureBucket = %v, want errS3CreateBucketFailed", err)
	}
	assertContainsCodeAndMessage(t, err, "AccessDenied", "Access Denied")
	if _, creates := refused.counts(); creates != 1 {
		t.Fatalf("403 to CreateBucket: %d creates, want 1", creates)
	}
}

// TestHeadBucketRefusalIsNotRetriedOrCreated pins that a non-retryable HEAD
// status is errS3BucketHeadFailed after one request, with no create attempted.
func TestHeadBucketRefusalIsNotRetriedOrCreated(t *testing.T) {
	t.Parallel()
	fake := &bucketFake{headStatus: http.StatusForbidden}
	err := newBucketTestClient(t, fake, "us-east-1").ensureBucket(context.Background())
	if !errors.Is(err, errS3BucketHeadFailed) {
		t.Fatalf("ensureBucket = %v, want errS3BucketHeadFailed", err)
	}
	if heads, creates := fake.counts(); heads != 1 || creates != 0 {
		t.Fatalf("%d HEADs and %d creates, want 1 and 0", heads, creates)
	}
}

// TestListingStatusFailuresAreClassifiedAndNotRetried pins bucketRequest's two
// refusals: a 404 listing is errS3BucketNotFound and a 403 is
// errS3BucketRequestFailed carrying S3's code, each after one request.
func TestListingStatusFailuresAreClassifiedAndNotRetried(t *testing.T) {
	t.Parallel()
	cases := []struct {
		want   error
		body   []byte
		status int
	}{
		{status: http.StatusNotFound, want: errS3BucketNotFound},
		{status: http.StatusForbidden, body: []byte(accessDeniedErrorBody), want: errS3BucketRequestFailed},
	}
	for _, tc := range cases {
		b, fake := newTestBackendAndFake(t)
		if err := b.Open(context.Background()); err != nil {
			t.Fatalf("Open: %v", err)
		}
		fake.failNextWithBody(bucketListKey, http.MethodGet, tc.status, -1, tc.body)

		keys, err := b.client.listObjects(context.Background(), "artifacts")
		if !errors.Is(err, tc.want) || keys != nil {
			t.Fatalf("listing answered %d: listObjects = %v, %v; want nil and %v", tc.status, keys, err, tc.want)
		}
		if tc.body != nil {
			assertContainsCodeAndMessage(t, err, "AccessDenied", "Access Denied")
		}
		if got := fake.requestCount(bucketListKey, http.MethodGet); got != 1 {
			t.Fatalf("listing answered %d: %d list requests, want 1", tc.status, got)
		}
	}
}
