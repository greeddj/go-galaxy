package cache

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestFetchJSONWithCachePolicyCacheHit(t *testing.T) {
	t.Parallel()
	var hits int32
	etag := "v1"
	payload := []byte(`{"ok":true}`)

	client := &http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			atomic.AddInt32(&hits, 1)
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
	url := "https://example.com/api"

	if err := FetchJSONWithCachePolicy(context.Background(), client, url, st, &out, policy); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error: %v", err)
	}
	if err := FetchJSONWithCachePolicy(context.Background(), client, url, st, &out, policy); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected 1 request, got %d", got)
	}
}

func TestFetchJSONWithCachePolicyRevalidate(t *testing.T) {
	t.Parallel()
	var hits int32
	etag := "v2"
	payload := []byte(`{"ok":true}`)
	var sawIfNoneMatch atomic.Bool

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			atomic.AddInt32(&hits, 1)
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
	url := "https://example.com/api"

	if err := FetchJSONWithCachePolicy(context.Background(), client, url, st, &out, policy); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error: %v", err)
	}
	key := apiCacheKey(url)
	entry, ok := st.GetAPICache(key)
	if !ok {
		t.Fatalf("expected cache entry")
	}
	entry.FetchedAt = time.Now().Add(-time.Hour)
	st.SetAPICache(key, entry)

	if err := FetchJSONWithCachePolicy(context.Background(), client, url, st, &out, policy); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("expected 2 requests, got %d", got)
	}
	if !sawIfNoneMatch.Load() {
		t.Fatalf("expected If-None-Match on revalidate")
	}
}
