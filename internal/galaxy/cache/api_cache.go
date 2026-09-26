package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
		return decodeMetadata(url, body, out)
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
	if err := decodeMetadata(url, body, out); err != nil {
		return false, err
	}
	if policy.Write {
		st.SetAPICache(key, newAPICacheEntry(url, body, etag, lastModified, policy.TTL))
	}
	return true, nil
}

// fetchAndStore downloads JSON and stores it in the cache when policy.Write is
// set and it decoded, so a body no later run could decode is never kept.
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
	if err := decodeMetadata(url, body, out); err != nil {
		return err
	}
	if policy.Write {
		st.SetAPICache(key, newAPICacheEntry(url, body, etag, lastModified, policy.TTL))
	}
	return nil
}

// decodeMetadata unmarshals body into out. No JSON at all is a *notJSONError, a
// web page when it is markup (the one shape IsWebPage lets a walk skip); JSON that
// does not fit out is helpers.ErrMetadataMalformed. Both name url's display form.
func decodeMetadata(url string, body []byte, out any) error {
	err := json.Unmarshal(body, out)
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*json.SyntaxError](err); ok {
		return &notJSONError{err: err, url: helpers.URLForMessage(url), webPage: isMarkup(body)}
	}
	// A value of the wrong type, or a timestamp time.Time refuses; only a
	// non-pointer out, the caller's defect and not the document's, stays bare.
	if _, ok := errors.AsType[*json.InvalidUnmarshalError](err); ok {
		return err
	}
	return fmt.Errorf("%w: %s: %w", helpers.ErrMetadataMalformed, helpers.URLForMessage(url), err)
}

// isMarkup reports whether body's first byte past JSON whitespace is '<',
// which opens HTML or XML and never a JSON document.
func isMarkup(body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '<'
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
// the request, covers every attempt and backoff; a revalidation's 304 is success.
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
		err = deadlineError(ctx, dlCtx, budget, helpers.ErrMetadataFetchDeadline, err)
		return nil, "", "", false, unreachableError(ctx, err)
	}
	return body, etag, lastModified, notModified, nil
}

// unreachableError wraps a transport failure, any net.Error (every failed
// client.Do's *url.Error is one: a refused dial, DNS, TLS), as the server being
// unavailable, unless err already has a class of its own.
func unreachableError(ctx context.Context, err error) error {
	// A caller's ended ctx, such as the versions pager's shared budget, is the
	// caller's to classify: its deadline sentinel renders the cause with %v.
	if ctx.Err() != nil || isClassifiedFetchError(err) {
		return err
	}
	if _, ok := errors.AsType[net.Error](err); !ok {
		return err
	}
	return fmt.Errorf("%w: %w", helpers.ErrGalaxyServerUnavailable, err)
}

// isClassifiedFetchError reports whether err already names why the fetch
// failed, which a server-unavailable wrap would only blur: a cancellation, the
// request's deadline, a stall, offline mode, an overrun cap or an HTTP status.
func isClassifiedFetchError(err error) bool {
	if _, ok := errors.AsType[*HTTPStatusError](err); ok {
		return true
	}
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, helpers.ErrMetadataFetchDeadline) ||
		errors.Is(err, helpers.ErrReadStalled) ||
		errors.Is(err, helpers.ErrOfflineMode) ||
		errors.Is(err, helpers.ErrResponseTooLarge)
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

	// A 304 answers only a revalidation; to a request with no cached copy behind
	// it, it is a status like any other, not an empty document.
	if resp.StatusCode == http.StatusNotModified && entry != nil {
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

// HTTPStatusError is a Galaxy metadata answer other than 200 or a revalidation's
// 304. Its exit class rides on Unwrap (see StatusClass), so a caller routes a
// 404 by Code and wraps no class sentinel around any status itself.
type HTTPStatusError struct {
	// URL is the fetched URL with its userinfo and query already cut by
	// fetchJSONBodyOnce, the only production constructor.
	URL    string
	Status string
	Code   int
}

// Error renders the status and URL, led by the status's class when it has one,
// so a message carries that sentinel's text exactly once.
func (e *HTTPStatusError) Error() string {
	msg := "failed to fetch metadata: " + e.Status
	if e.URL != "" {
		msg += " (" + e.URL + ")"
	}
	if class := StatusClass(e.Code); class != nil {
		return class.Error() + ": " + msg
	}
	return msg
}

// Unwrap exposes StatusClass(e.Code), so a status classifies wherever it
// surfaces; a 404 unwraps to nothing, leaving its meaning to the caller.
func (e *HTTPStatusError) Unwrap() error {
	return StatusClass(e.Code)
}

// StatusClass is the class of a Galaxy metadata status: none for a 404, which
// each caller reads as the source lacking something, helpers.ErrGalaxyAuthFailed
// for 401 and 403, and helpers.ErrGalaxyServerUnavailable for any other.
func StatusClass(code int) error {
	switch code {
	case http.StatusNotFound:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return helpers.ErrGalaxyAuthFailed
	default:
		return helpers.ErrGalaxyServerUnavailable
	}
}

// notJSONError is a metadata body that is no JSON at all. It unwraps to
// helpers.ErrMetadataNotJSON and the decoder's *json.SyntaxError; webPage
// marks markup, which a truncated or empty body never is.
type notJSONError struct {
	err     error
	url     string
	webPage bool
}

// Error renders the sentinel, the URL's display form and the decoder's error.
func (e *notJSONError) Error() string {
	return fmt.Sprintf("%s: %s: %s", helpers.ErrMetadataNotJSON, e.url, e.err)
}

// Unwrap exposes helpers.ErrMetadataNotJSON and the decoder's error.
func (e *notJSONError) Unwrap() []error {
	return []error{helpers.ErrMetadataNotJSON, e.err}
}

// IsWebPage reports whether err is a metadata body that was markup, as a web
// UI serves with 200 at a path its API does not own: the one non-JSON answer
// a server walk may skip at an API root.
func IsWebPage(err error) bool {
	notJSON, ok := errors.AsType[*notJSONError](err)
	return ok && notJSON.webPage
}
