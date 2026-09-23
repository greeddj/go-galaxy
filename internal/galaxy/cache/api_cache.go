package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// apiCacheKey generates a stable cache key for a URL.
func apiCacheKey(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

// FetchJSONWithCachePolicy fetches JSON under policy and unmarshals it into
// out. budget bounds the network fetch end to end; non-positive means
// helpers.MetadataFetchDeadline.
func FetchJSONWithCachePolicy(
	ctx context.Context,
	client *http.Client,
	url string,
	st *store.Store,
	out any,
	policy Policy,
	budget time.Duration,
) error {
	if st == nil || (!policy.Read && !policy.Write) {
		body, _, _, _, err := fetchJSONBody(ctx, client, url, nil, budget)
		if err != nil {
			return err
		}
		return json.Unmarshal(body, out)
	}

	key := apiCacheKey(url)
	if policy.Read {
		if ok, err := tryServeFromCache(ctx, client, url, st, key, out, policy, budget); ok || err != nil {
			return err
		}
	}
	return fetchAndStore(ctx, client, url, st, key, out, policy, budget)
}

// tryServeFromCache serves url from the cache and reports whether it did. A
// body that fails to decode is a miss, refetched unconditionally: revalidating
// it would get a 304 and keep the corrupt bytes forever.
func tryServeFromCache(
	ctx context.Context,
	client *http.Client,
	url string,
	st *store.Store,
	key string,
	out any,
	policy Policy,
	budget time.Duration,
) (bool, error) {
	entry, ok := st.GetAPICache(key)
	if !isValidCacheEntry(ok, entry, url) {
		return false, nil
	}
	if err := json.Unmarshal(entry.Body, out); err != nil {
		return false, nil
	}
	// A FetchedAt in the future is corrupt or clock-skewed, not fresh: its
	// negative age would pass the TTL test forever, so it is revalidated.
	age := time.Since(entry.FetchedAt)
	if policy.TTL != 0 && (age > policy.TTL || age < 0) {
		// On a changed response revalidateCache decodes again over out,
		// replacing the stale body decoded above.
		return revalidateCache(ctx, client, url, st, key, entry, out, policy, budget)
	}
	return true, nil
}

func isValidCacheEntry(ok bool, entry store.APICacheEntry, url string) bool {
	if !ok || entry.URL != url || len(entry.Body) == 0 {
		return false
	}
	return true
}

// revalidateCache issues a conditional GET for an expired entry already
// decoded into out; a 304 keeps that decode rather than unmarshaling again.
func revalidateCache(
	ctx context.Context,
	client *http.Client,
	url string,
	st *store.Store,
	key string,
	entry store.APICacheEntry,
	out any,
	policy Policy,
	budget time.Duration,
) (bool, error) {
	body, etag, lastModified, notModified, err := fetchJSONBody(ctx, client, url, &entry, budget)
	if err != nil {
		return false, err
	}
	if notModified {
		if policy.Write {
			st.SetAPICache(key, refreshAPICacheEntry(entry, etag, lastModified))
		}
		return true, nil
	}
	if policy.Write {
		st.SetAPICache(key, newAPICacheEntry(url, body, etag, lastModified, policy.TTL))
	}
	return true, json.Unmarshal(body, out)
}

// fetchAndStore downloads JSON and optionally stores it in the cache.
func fetchAndStore(
	ctx context.Context,
	client *http.Client,
	url string,
	st *store.Store,
	key string,
	out any,
	policy Policy,
	budget time.Duration,
) error {
	body, etag, lastModified, _, err := fetchJSONBody(ctx, client, url, nil, budget)
	if err != nil {
		return err
	}
	if policy.Write {
		st.SetAPICache(key, newAPICacheEntry(url, body, etag, lastModified, policy.TTL))
	}
	return json.Unmarshal(body, out)
}

// newAPICacheEntry builds a cache entry storing body verbatim. A presigned
// download_url in it keeps its query: this program fetches it back, and a cut
// one would 403 on every cache-served download.
func newAPICacheEntry(url string, body []byte, etag, lastModified string, ttl time.Duration) store.APICacheEntry {
	return store.APICacheEntry{
		URL:          url,
		FetchedAt:    time.Now().UTC(),
		TTL:          ttl,
		Body:         body,
		ETag:         etag,
		LastModified: lastModified,
	}
}

// refreshAPICacheEntry updates timestamps and validators for a cached entry.
func refreshAPICacheEntry(entry store.APICacheEntry, etag, lastModified string) store.APICacheEntry {
	entry.FetchedAt = time.Now().UTC()
	if etag != "" {
		entry.ETag = etag
	}
	if lastModified != "" {
		entry.LastModified = lastModified
	}
	return entry
}

// fetchJSONBody fetches JSON bytes and validators for url, retrying transient
// failures with fresh conditional headers. One budget, set here by the owner of
// the request, covers every attempt and backoff; a 304 is success, not retried.
func fetchJSONBody(
	ctx context.Context,
	client *http.Client,
	url string,
	entry *store.APICacheEntry,
	budget time.Duration,
) ([]byte, string, string, bool, error) {
	budget = metadataBudget(budget)
	dlCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var (
		body               []byte
		etag, lastModified string
		notModified        bool
	)
	err := helpers.Retry(dlCtx, helpers.FetchRetryPolicy(), func() error {
		var attemptErr error
		body, etag, lastModified, notModified, attemptErr = fetchJSONBodyOnce(dlCtx, client, url, entry)
		return deadlineError(ctx, dlCtx, budget, helpers.ErrMetadataFetchDeadline, attemptErr)
	}, fetchRetryable)
	if err != nil {
		return nil, "", "", false, deadlineError(ctx, dlCtx, budget, helpers.ErrMetadataFetchDeadline, err)
	}
	return body, etag, lastModified, notModified, nil
}

// metadataBudget returns budget when positive, else
// helpers.MetadataFetchDeadline, so a zero value degrades to the real ceiling.
func metadataBudget(budget time.Duration) time.Duration {
	if budget <= 0 {
		return helpers.MetadataFetchDeadline
	}
	return budget
}

// fetchJSONBodyOnce performs one request-and-read attempt for url. No error it
// returns names url's credentials: a build failure names no part of url, and a
// transport or status failure carries its userinfo- and query-cut form.
func fetchJSONBodyOnce(
	ctx context.Context,
	client *http.Client,
	url string,
	entry *store.APICacheEntry,
) ([]byte, string, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		// The build error is dropped: it is a *url.Error naming the whole raw
		// value, password included (see helpers.ErrMetadataRequestBuildFailed).
		return nil, "", "", false, helpers.ErrMetadataRequestBuildFailed
	}
	if entry != nil {
		if entry.ETag != "" {
			req.Header.Set("If-None-Match", entry.ETag)
		}
		if entry.LastModified != "" {
			req.Header.Set("If-Modified-Since", entry.LastModified)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		// net/http masks only a password, leaving a declared query whole, so the
		// error is re-rendered over the cut URL; it still unwraps, which
		// fetchRetryable and deadlineError classify through.
		return nil, "", "", false, helpers.CutTransportURL(url, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotModified {
		return nil, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), true, nil
	}
	if resp.StatusCode != http.StatusOK {
		// Cut at construction, so no consumer rendering URL can leak a
		// capability; both cuts, since userinfo is kept out only upstream, by
		// collections.normalizeVersionsURL.
		return nil, "", "", false, &HTTPStatusError{
			URL:    helpers.WithoutCredentials(url),
			Status: resp.Status,
			Code:   resp.StatusCode,
		}
	}

	body, err := io.ReadAll(helpers.NewSizeLimitedReader(resp.Body, helpers.MetadataMaxSize))
	if err != nil {
		// helpers.ErrResponseTooLarge is shared with other surfaces; the wrap
		// names the metadata document while keeping errors.Is intact.
		return nil, "", "", false, fmt.Errorf("galaxy metadata document: %w", err)
	}
	return body, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), false, nil
}

// HTTPStatusError describes a non-200 HTTP response.
type HTTPStatusError struct {
	// URL is the fetched URL with its userinfo and query already cut by
	// fetchJSONBodyOnce, the only production constructor.
	URL    string
	Status string
	Code   int
}

// Error implements the error interface.
func (e *HTTPStatusError) Error() string {
	if e.URL != "" {
		return fmt.Sprintf("failed to fetch metadata: %s (%s)", e.Status, e.URL)
	}
	return "failed to fetch metadata: " + e.Status
}
