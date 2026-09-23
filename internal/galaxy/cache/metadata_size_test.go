package cache

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// TestMetadataFetchRejectsOversizedResponse pins that a response over
// helpers.MetadataMaxSize is refused uncached and unretried, with a message
// naming the metadata surface, since ErrResponseTooLarge is shared.
func TestMetadataFetchRejectsOversizedResponse(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32

	// A single small chunk reused for every write, so the server itself never
	// allocates anywhere near helpers.MetadataMaxSize; only the transferred
	// byte count grows across repeated writes of the same buffer.
	chunk := bytes.Repeat([]byte("a"), 64<<10)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		var written int64
		for written <= helpers.MetadataMaxSize {
			n, err := w.Write(chunk)
			written += int64(n)
			if err != nil {
				// The client aborts its read once the cumulative count
				// crosses the cap and closes the body, so a write past that
				// point is expected to fail; stop sending rather than spin.
				break
			}
		}
	}))
	defer srv.Close()

	st := store.New()
	var out map[string]any
	policy := Policy{Read: true, Write: true}

	err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), srv.URL, st, &out, policy, 0)
	if !errors.Is(err, helpers.ErrResponseTooLarge) {
		t.Fatalf("FetchJSONWithCachePolicy() error = %v, want ErrResponseTooLarge", err)
	}
	if !strings.Contains(err.Error(), "galaxy metadata document") {
		t.Fatalf("FetchJSONWithCachePolicy() error = %q, want it to name the galaxy metadata document surface", err.Error())
	}

	key := apiCacheKey(srv.URL)
	if _, ok := st.GetAPICache(key); ok {
		t.Fatalf("expected no cache entry to be written after an over-cap fetch")
	}

	if got := requests.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request (an over-cap response is terminal, not retried), got %d", got)
	}
}

// TestMetadataFetchAcceptsResponseWithinCeiling is the positive control for
// TestMetadataFetchRejectsOversizedResponse: the same fixture under the cap is
// fetched, decoded and cached.
func TestMetadataFetchAcceptsResponseWithinCeiling(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	payload := []byte(`{"ok":true,"count":42}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	st := store.New()
	var out map[string]any
	policy := Policy{Read: true, Write: true}

	if err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), srv.URL, st, &out, policy, 0); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy() unexpected error: %v", err)
	}
	if got, want := out["count"], float64(42); got != want {
		t.Fatalf("FetchJSONWithCachePolicy() decoded count = %v, want %v", got, want)
	}

	key := apiCacheKey(srv.URL)
	if _, ok := st.GetAPICache(key); !ok {
		t.Fatalf("expected a cache entry to be written after a within-ceiling fetch")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request, got %d", got)
	}
}
