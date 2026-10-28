package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// Validator fixtures for the revalidation tests below: the cached entry's and
// the server's newer Last-Modified values.
const (
	testCachedLastModified = "Mon, 01 Jan 2024 00:00:00 GMT"
	testServerLastModified = "Tue, 02 Jan 2024 00:00:00 GMT"
)

// seedExpiredAPIEntry stores an hour-old entry for testAPIURL whose body is
// {"v":1}, with ETag v1 and testCachedLastModified as its validators.
func seedExpiredAPIEntry(st *store.Store) string {
	key := apiCacheKey(testAPIURL)
	st.SetAPICache(key, store.APICacheEntry{
		URL:          testAPIURL,
		Body:         []byte(`{"v":1}`),
		ETag:         "v1",
		LastModified: testCachedLastModified,
		FetchedAt:    time.Now().Add(-time.Hour).UTC(),
	})
	return key
}

// validatorRecordingClient answers every request with status, body and the
// given validators, recording the conditional headers of the last request.
func validatorRecordingClient(status int, body, etag, lastModified string, seen *http.Header) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			*seen = req.Header.Clone()
			header := make(http.Header)
			header.Set("ETag", etag)
			header.Set("Last-Modified", lastModified)
			return &http.Response{
				StatusCode: status,
				Status:     http.StatusText(status),
				Header:     header,
				Body:       io.NopCloser(bytes.NewReader([]byte(body))),
			}, nil
		}),
	}
}

// assertConditionalHeaders fails unless seen carries the seeded entry's ETag
// and Last-Modified as If-None-Match and If-Modified-Since.
func assertConditionalHeaders(t *testing.T, seen http.Header) {
	t.Helper()
	if got := seen.Get("If-None-Match"); got != "v1" {
		t.Errorf("If-None-Match = %q, want v1", got)
	}
	if got := seen.Get("If-Modified-Since"); got != testCachedLastModified {
		t.Errorf("If-Modified-Since = %q, want %q", got, testCachedLastModified)
	}
}

// TestFetchJSONWithCachePolicyRevalidationTakesAChangedBody pins that a 200 to
// a revalidation is decoded over out and, only under policy.Write, replaces
// the entry with the new body and validators.
func TestFetchJSONWithCachePolicyRevalidationTakesAChangedBody(t *testing.T) {
	t.Parallel()
	for _, write := range []bool{true, false} {
		var seen http.Header
		client := validatorRecordingClient(http.StatusOK, `{"v":2}`, "v2", testServerLastModified, &seen)
		st := store.New()
		key := seedExpiredAPIEntry(st)
		policy := Policy{Read: true, Write: write, TTL: time.Minute}

		var out struct{ V int }
		if err := FetchJSONWithCachePolicy(context.Background(), client, testAPIURL, st, &out, policy, 0); err != nil {
			t.Fatalf("write=%v: FetchJSONWithCachePolicy error: %v", write, err)
		}
		assertConditionalHeaders(t, seen)
		if out.V != 2 {
			t.Errorf("write=%v: out.V = %d, want the changed body's 2", write, out.V)
		}
		assertRevalidatedEntry(t, st, key, policy)
	}
}

// assertRevalidatedEntry fails unless key holds the server's new body and
// validators under a fresh stamp when policy.Write is set, and the seeded
// entry untouched otherwise.
func assertRevalidatedEntry(t *testing.T, st *store.Store, key string, policy Policy) {
	t.Helper()
	entry, _ := st.GetAPICache(key)
	wantBody, wantETag, wantLM := `{"v":1}`, "v1", testCachedLastModified
	if policy.Write {
		wantBody, wantETag, wantLM = `{"v":2}`, "v2", testServerLastModified
	}
	if string(entry.Body) != wantBody || entry.ETag != wantETag || entry.LastModified != wantLM {
		t.Errorf("write=%v: entry = body %s, ETag %q, Last-Modified %q; want %s, %q, %q",
			policy.Write, entry.Body, entry.ETag, entry.LastModified, wantBody, wantETag, wantLM)
	}
	if policy.Write && (entry.TTL != policy.TTL || time.Since(entry.FetchedAt) > time.Minute) {
		t.Errorf("entry TTL %v FetchedAt %v, want the policy TTL and a fresh stamp", entry.TTL, entry.FetchedAt)
	}
}

// TestFetchJSONWithCachePolicyNotModifiedRefreshesValidators pins that a 304
// keeps the cached body and decode, takes the server's new validators and
// restamps FetchedAt.
func TestFetchJSONWithCachePolicyNotModifiedRefreshesValidators(t *testing.T) {
	t.Parallel()
	var seen http.Header
	client := validatorRecordingClient(http.StatusNotModified, "", "v1b", testServerLastModified, &seen)
	st := store.New()
	key := seedExpiredAPIEntry(st)
	policy := Policy{Read: true, Write: true, TTL: time.Minute}

	var out struct{ V int }
	if err := FetchJSONWithCachePolicy(context.Background(), client, testAPIURL, st, &out, policy, 0); err != nil {
		t.Fatalf("FetchJSONWithCachePolicy error: %v", err)
	}
	assertConditionalHeaders(t, seen)
	if out.V != 1 {
		t.Errorf("out.V = %d, want the cached body's 1", out.V)
	}
	entry, _ := st.GetAPICache(key)
	if string(entry.Body) != `{"v":1}` {
		t.Errorf("entry body = %s, want the cached body kept", entry.Body)
	}
	if entry.ETag != "v1b" || entry.LastModified != testServerLastModified {
		t.Errorf("entry validators = %q, %q; want v1b, %q", entry.ETag, entry.LastModified, testServerLastModified)
	}
	if time.Since(entry.FetchedAt) > time.Minute {
		t.Errorf("entry FetchedAt = %v, want it restamped by the 304", entry.FetchedAt)
	}
}

// TestFetchJSONWithCachePolicyRevalidationFailureKeepsTheEntry pins that a
// failed revalidation returns the status error and leaves the entry alone.
func TestFetchJSONWithCachePolicyRevalidationFailureKeepsTheEntry(t *testing.T) {
	t.Parallel()
	var seen http.Header
	client := validatorRecordingClient(http.StatusNotFound, "", "", "", &seen)
	st := store.New()
	key := seedExpiredAPIEntry(st)
	before, _ := st.GetAPICache(key)
	policy := Policy{Read: true, Write: true, TTL: time.Minute}

	var out struct{ V int }
	err := FetchJSONWithCachePolicy(context.Background(), client, testAPIURL, st, &out, policy, 0)
	statusErr, ok := errors.AsType[*HTTPStatusError](err)
	if !ok || statusErr.Code != http.StatusNotFound {
		t.Fatalf("FetchJSONWithCachePolicy = %v, want a 404 *HTTPStatusError", err)
	}
	after, _ := st.GetAPICache(key)
	if string(after.Body) != string(before.Body) || after.ETag != before.ETag || !after.FetchedAt.Equal(before.FetchedAt) {
		t.Errorf("entry = %+v after a failed revalidation, want %+v unchanged", after, before)
	}
}
