package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// probeFaultHandler answers the nth request of method to a probe object with
// status and passes the rest to the fake, since failNext cannot name the
// probe key's random suffix.
type probeFaultHandler struct {
	inner  http.Handler
	method string
	status int
	nth    int
	seen   int
	mu     sync.Mutex
}

// ServeHTTP fires the fault once, on the nth matching request.
func (h *probeFaultHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == h.method && strings.Contains(r.URL.Path, conditionalProbeObject) {
		h.mu.Lock()
		h.seen++
		fire := h.seen == h.nth
		h.mu.Unlock()
		if fire {
			w.WriteHeader(h.status)
			return
		}
	}
	h.inner.ServeHTTP(w, r)
}

// newTestBackendWithHandler is newTestBackendWithFake over any handler that
// serves the fake's bucket.
func newTestBackendWithHandler(t *testing.T, handler http.Handler, bucket string) *Backend {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := config.S3CacheConfig{
		Endpoint:  srv.URL,
		Bucket:    bucket,
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	backend, err := New(cfg, srv.Client(), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return backend
}

// probeObjectsLeft lists the probe objects still stored in fake.
func probeObjectsLeft(fake *fakeS3) []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var left []string
	for key := range fake.objects {
		if strings.Contains(key, conditionalProbeObject) {
			left = append(left, key)
		}
	}
	return left
}

// TestOpenProbeStepFailureIsReportedAndCleanedUp pins that a failure at any
// of the probe's five requests fails Open with its own sentinel, leaves no
// probe object or client, and the next Open starts over.
func TestOpenProbeStepFailureIsReportedAndCleanedUp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		want   error
		name   string
		method string
		nth    int
	}{
		{name: "the first create-if-absent write", method: http.MethodPut, nth: 1, want: errS3PutFailed},
		{name: "the second create-if-absent write", method: http.MethodPut, nth: 2, want: errS3PutFailed},
		{name: "the version read", method: http.MethodHead, nth: 1, want: errS3HeadFailed},
		{name: "the stale compare-and-swap", method: http.MethodPut, nth: 3, want: errS3PutFailed},
		{name: "the current compare-and-swap", method: http.MethodPut, nth: 4, want: errS3PutFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertProbeStepFailure(t, tc.method, tc.nth, tc.want)
		})
	}
}

// assertProbeStepFailure fails the nth probe request of method once and pins
// Open's error, the unopened state, the cleanup and a clean second Open.
func assertProbeStepFailure(t *testing.T, method string, nth int, want error) {
	t.Helper()
	fake := newFakeS3()
	handler := &probeFaultHandler{inner: fake, method: method, nth: nth, status: http.StatusForbidden}
	b := newTestBackendWithHandler(t, handler, fake.bucket)

	err := b.Open(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Open = %v, want %v", err, want)
	}
	if errors.Is(err, errS3ConditionalPutUnsupported) || errors.Is(err, errS3CompareAndSwapUnsupported) {
		t.Fatalf("Open = %v, a failed request must not read as a missing capability", err)
	}
	if b.client != nil || b.artifacts != nil {
		t.Fatal("a failed probe left the backend opened")
	}
	if left := probeObjectsLeft(fake); len(left) != 0 {
		t.Fatalf("probe objects left behind: %v", left)
	}

	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("second Open after a one-shot fault = %v, want nil", err)
	}
	if b.client == nil || b.artifacts == nil {
		t.Fatal("a successful Open left the backend unopened")
	}
}
