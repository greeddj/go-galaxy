package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// testAPIURL is the fixed URL every fixture in this file caches against.
const testAPIURL = "https://example.com/api"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

// assertAPICacheHealed fails the test unless st holds an entry for key whose
// body decodes cleanly and carries wantETag, the shape a corrupt entry must
// end up in once a fetch has healed it.
func assertAPICacheHealed(t *testing.T, st *store.Store, key, wantETag string) {
	t.Helper()
	entry, ok := st.GetAPICache(key)
	if !ok {
		t.Fatalf("expected a healed cache entry")
	}
	var healed map[string]any
	if err := json.Unmarshal(entry.Body, &healed); err != nil {
		t.Fatalf("expected the healed entry body to unmarshal, got error: %v", err)
	}
	if entry.ETag != wantETag {
		t.Fatalf("expected healed entry ETag %s, got %q", wantETag, entry.ETag)
	}
}

func TestFetchJSONWithCachePolicyCacheHit(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	etag := "v1"
	payload := []byte(`{"ok":true}`)

	client := &http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			hits.Add(1)
			header := make(http.Header)
			header.Set("ETag", etag)
			header.Set("Content-Type", "application/json")
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     http.StatusText(http.StatusOK),
				Header:     header,
				Body:       io.NopCloser(bytes.NewReader(payload)),
			}, nil
		}),
	}

	st := store.New()
	policy := Policy{Read: true, Write: true, TTL: time.Minute}
	var out map[string]any
	url := testAPIURL

	if err := FetchJSONWithCachePolicy(context.Background(), client, url, st, &out, policy, 0); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error: %v", err)
	}
	if err := FetchJSONWithCachePolicy(context.Background(), client, url, st, &out, policy, 0); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected 1 request, got %d", got)
	}
}

func TestFetchJSONWithCachePolicyRevalidate(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	etag := "v2"
	payload := []byte(`{"ok":true}`)
	var sawIfNoneMatch atomic.Bool

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hits.Add(1)
			header := make(http.Header)
			if req.Header.Get("If-None-Match") == etag {
				sawIfNoneMatch.Store(true)
				return &http.Response{
					StatusCode: http.StatusNotModified,
					Status:     http.StatusText(http.StatusNotModified),
					Header:     header,
					Body:       io.NopCloser(bytes.NewReader(nil)),
				}, nil
			}
			header.Set("ETag", etag)
			header.Set("Content-Type", "application/json")
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     http.StatusText(http.StatusOK),
				Header:     header,
				Body:       io.NopCloser(bytes.NewReader(payload)),
			}, nil
		}),
	}

	st := store.New()
	policy := Policy{Read: true, Write: true, TTL: time.Millisecond}
	var out map[string]any
	url := testAPIURL

	if err := FetchJSONWithCachePolicy(context.Background(), client, url, st, &out, policy, 0); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error: %v", err)
	}
	key := apiCacheKey(url)
	entry, ok := st.GetAPICache(key)
	if !ok {
		t.Fatalf("expected cache entry")
	}
	entry.FetchedAt = time.Now().Add(-time.Hour)
	st.SetAPICache(key, entry)

	if err := FetchJSONWithCachePolicy(context.Background(), client, url, st, &out, policy, 0); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("expected 2 requests, got %d", got)
	}
	if !sawIfNoneMatch.Load() {
		t.Fatalf("expected If-None-Match on revalidate")
	}
}

// TestFetchJSONWithCachePolicyFutureStampIsRevalidated pins that an entry
// stamped in the future is treated as expired and revalidated, not served as
// permanently fresh.
func TestFetchJSONWithCachePolicyFutureStampIsRevalidated(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	etag := "v2"
	payload := []byte(`{"ok":true}`)
	var sawIfNoneMatch atomic.Bool

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hits.Add(1)
			header := make(http.Header)
			if req.Header.Get("If-None-Match") == etag {
				sawIfNoneMatch.Store(true)
				return &http.Response{
					StatusCode: http.StatusNotModified,
					Status:     http.StatusText(http.StatusNotModified),
					Header:     header,
					Body:       io.NopCloser(bytes.NewReader(nil)),
				}, nil
			}
			header.Set("ETag", etag)
			header.Set("Content-Type", "application/json")
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     http.StatusText(http.StatusOK),
				Header:     header,
				Body:       io.NopCloser(bytes.NewReader(payload)),
			}, nil
		}),
	}

	st := store.New()
	policy := Policy{Read: true, Write: true, TTL: time.Millisecond}
	var out map[string]any
	url := testAPIURL

	if err := FetchJSONWithCachePolicy(context.Background(), client, url, st, &out, policy, 0); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error: %v", err)
	}
	key := apiCacheKey(url)
	entry, ok := st.GetAPICache(key)
	if !ok {
		t.Fatalf("expected cache entry")
	}
	entry.FetchedAt = time.Now().Add(time.Hour)
	st.SetAPICache(key, entry)

	if err := FetchJSONWithCachePolicy(context.Background(), client, url, st, &out, policy, 0); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("expected 2 requests, got %d", got)
	}
	if !sawIfNoneMatch.Load() {
		t.Fatalf("expected If-None-Match on revalidate")
	}
}

// TestFetchJSONWithCachePolicyCorruptBodyRefetchesUnconditional pins that an
// undecodable cached body is refetched without validators and healed in place.
func TestFetchJSONWithCachePolicyCorruptBodyRefetchesUnconditional(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	var sawIfNoneMatch atomic.Bool
	payload := []byte(`{"ok":true}`)

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hits.Add(1)
			if req.Header.Get("If-None-Match") != "" {
				sawIfNoneMatch.Store(true)
			}
			header := make(http.Header)
			header.Set("ETag", "v2")
			header.Set("Content-Type", "application/json")
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     http.StatusText(http.StatusOK),
				Header:     header,
				Body:       io.NopCloser(bytes.NewReader(payload)),
			}, nil
		}),
	}

	st := store.New()
	url := testAPIURL
	key := apiCacheKey(url)
	st.SetAPICache(key, store.APICacheEntry{
		URL:       url,
		Body:      []byte("{not-json"),
		ETag:      "v1",
		FetchedAt: time.Now().UTC(),
	})

	policy := Policy{Read: true, Write: true, TTL: time.Minute}
	var out map[string]any
	if err := FetchJSONWithCachePolicy(context.Background(), client, url, st, &out, policy, 0); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error: %v", err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("expected out to decode {ok:true}, got %v", out)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected 1 request, got %d", got)
	}
	if sawIfNoneMatch.Load() {
		t.Fatalf("expected no If-None-Match on a corrupt-body refetch")
	}
	assertAPICacheHealed(t, st, key, "v2")

	if err := FetchJSONWithCachePolicy(context.Background(), client, url, st, &out, policy, 0); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error on second call: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected the entry to be healed with no further hits, got %d total", got)
	}
}

// TestFetchJSONWithCachePolicyCorruptExpiredBodyDoesNotRide304 pins that an
// expired, corrupt entry is refetched unconditionally, never revalidated into a
// 304 that would keep its unusable bytes.
func TestFetchJSONWithCachePolicyCorruptExpiredBodyDoesNotRide304(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	var saw304 atomic.Bool
	payload := []byte(`{"ok":true}`)

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hits.Add(1)
			if req.Header.Get("If-None-Match") == "v1" {
				// The trap: if the caller ever revalidates conditionally
				// against the stale ETag, the server confirms "unchanged"
				// and the caller would be stuck with the corrupt bytes.
				saw304.Store(true)
				return &http.Response{
					StatusCode: http.StatusNotModified,
					Status:     http.StatusText(http.StatusNotModified),
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewReader(nil)),
				}, nil
			}
			header := make(http.Header)
			header.Set("ETag", "v2")
			header.Set("Content-Type", "application/json")
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     http.StatusText(http.StatusOK),
				Header:     header,
				Body:       io.NopCloser(bytes.NewReader(payload)),
			}, nil
		}),
	}

	st := store.New()
	url := testAPIURL
	key := apiCacheKey(url)
	st.SetAPICache(key, store.APICacheEntry{
		URL:       url,
		Body:      []byte("{not-json"),
		ETag:      "v1",
		FetchedAt: time.Now().Add(-time.Hour),
	})

	policy := Policy{Read: true, Write: true, TTL: time.Minute}
	var out map[string]any
	if err := FetchJSONWithCachePolicy(context.Background(), client, url, st, &out, policy, 0); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error: %v", err)
	}
	if saw304.Load() {
		t.Fatalf("expected the corrupt expired entry to never ride a conditional 304")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected 1 request, got %d", got)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("expected out to decode {ok:true}, got %v", out)
	}
	assertAPICacheHealed(t, st, key, "v2")
}
