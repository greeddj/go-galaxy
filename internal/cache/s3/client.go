package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // used only for the S3-mandated Content-MD5 integrity header, not as a security primitive
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// signingKeyCache memoizes the SigV4 signing key for the current UTC date;
// secret and region are fixed for the client's lifetime, so the date is the
// only cache key.
type signingKeyCache struct {
	date string
	key  []byte
	mu   sync.Mutex
}

// Client implements minimal S3 operations with SigV4 signing.
type Client struct {
	client         *http.Client
	endpointHost   string
	endpointScheme string
	cfg            config.S3CacheConfig
	signing        signingKeyCache
}

// newClient constructs an S3 client from configuration.
func newClient(cfg config.S3CacheConfig, httpClient *http.Client) (*Client, error) {
	if cfg.Bucket == "" {
		return nil, errS3BucketEmpty
	}
	if httpClient == nil {
		return nil, errS3HTTPClientNil
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", cfg.Region)
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%w: %s", errS3InvalidEndpoint, endpoint)
	}
	// The trailing-slash trim leaves host and scheme untouched, so the parsed
	// values are safe to cache once for requestURL.
	cfg.Endpoint = strings.TrimRight(endpoint, "/")

	// Refuse redirects on a private shallow copy: httpClient is the shared
	// Galaxy client, whose own CheckRedirect must keep following redirects,
	// and the copy still shares its Transport and connection pool.
	redirectless := *httpClient
	redirectless.CheckRedirect = refuseRedirect
	return &Client{cfg: cfg, client: &redirectless, endpointHost: parsed.Host, endpointScheme: parsed.Scheme}, nil
}

// refuseRedirect refuses every redirect, a same-host scheme upgrade included:
// SigV4 signs Host and path, and a followed hop would carry the X-Amz-*
// headers, session token included, and a replayed body to a new party.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	status := "redirect"
	if req.Response != nil {
		status = req.Response.Status
	}
	return fmt.Errorf("%w: %s redirected to %s; configure --s3-endpoint as that origin",
		errS3RedirectRefused, status, helpers.Origin(req.URL))
}

// do sends req and wraps a transport failure in errS3TransportFailed unless the
// caller's own context ended. It tests req.Context().Err(), never the error's
// shape: dial and response-header timeouts also match DeadlineExceeded.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	// #nosec G704 -- req's own Host header is always this Client's configured
	// S3 endpoint, built by requestURL and never from a remote value, and
	// refuseRedirect keeps any hop from moving it.
	resp, err := c.client.Do(req)
	if err == nil {
		return resp, nil
	}
	// The refusal already carries ErrCacheBackendUnusable; wrapping it would
	// add a second class and make s3Retryable retry it.
	if errors.Is(err, errS3RedirectRefused) {
		return nil, err
	}
	// A cancellation racing a transport error wins over blaming the backend.
	if req.Context().Err() != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %w", errS3TransportFailed, err)
}

// s3ErrorResponse captures the fields S3 puts in the XML <Error> document
// that many non-2xx responses carry in their body, alongside the HTTP status
// line.
type s3ErrorResponse struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// s3StatusError wraps sentinel with the status line and, when the body is an
// S3 <Error> document, its Code and Message; an empty or malformed body falls
// back to the status alone. The caller still closes resp.Body.
func s3StatusError(sentinel error, resp *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, s3ErrorBodyLimit))
	if err != nil {
		return fmt.Errorf("%w: %s", sentinel, resp.Status)
	}
	var parsed s3ErrorResponse
	if err := xml.Unmarshal(data, &parsed); err != nil || parsed.Code == "" {
		return fmt.Errorf("%w: %s", sentinel, resp.Status)
	}
	return fmt.Errorf("%w: %s (%s: %s)", sentinel, resp.Status, parsed.Code, parsed.Message)
}

// getObject GETs key, retrying a transient failure with a freshly signed
// request per attempt; only the successful response is returned open, for the
// caller to read and close.
func (c *Client) getObject(ctx context.Context, key string) (*http.Response, error) {
	var success *http.Response
	err := helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newReadRequest(ctx, http.MethodGet, key, nil)
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			return errS3NotFound
		}
		if resp.StatusCode != http.StatusOK {
			statusErr := wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3GetFailed, resp))
			_ = resp.Body.Close()
			return statusErr
		}
		success = resp
		return nil
	}, s3RetryableFor(ctx))
	if err != nil {
		return nil, err
	}
	return success, nil
}

// headObject HEADs key, retrying like getObject; a HEAD response has no body,
// so its failure stays status-only rather than going through s3StatusError.
func (c *Client) headObject(ctx context.Context, key string) (http.Header, error) {
	var headers http.Header
	err := helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newReadRequest(ctx, http.MethodHead, key, nil)
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		if resp.StatusCode == http.StatusNotFound {
			return errS3NotFound
		}
		if resp.StatusCode != http.StatusOK {
			return wrapRetryableStatus(resp.StatusCode, fmt.Errorf("%w: %s", errS3HeadFailed, resp.Status))
		}
		headers = resp.Header.Clone()
		return nil
	}, s3RetryableFor(ctx))
	if err != nil {
		return nil, err
	}
	return headers, nil
}

// putCondition is a write's precondition: ifNoneMatch for create-if-absent,
// ifMatch (an ETag) for compare-and-swap, the zero value for an overwrite. A
// type rather than two flags, so no caller misjudges whether a write may retry.
type putCondition struct {
	ifMatch     string
	ifNoneMatch bool
}

// isConditional reports whether this write carries any precondition, which is
// what decides whether it may be retried.
func (c putCondition) isConditional() bool {
	return c.ifNoneMatch || c.ifMatch != ""
}

// putObjectAttrs holds a PUT's Content-Type, Content-Encoding, X-Amz-Meta-*
// metadata and precomputed sha256 hex; a zero field sets nothing, and an empty
// payloadHash makes putObject hash the body itself.
type putObjectAttrs struct {
	meta            map[string]string
	contentType     string
	contentEncoding string
	payloadHash     string
}

// putObject uploads an object. A conditional PUT is single-shot even on a
// transport failure: it may have landed, and a retry would see 412 and misread
// its own success as contention, so only the lock loop retries one.
func (c *Client) putObject(
	ctx context.Context,
	key string,
	body io.ReadSeeker,
	size int64,
	attrs putObjectAttrs,
	cond putCondition,
) error {
	payloadHash, err := resolvePayloadHash(body, attrs.payloadHash)
	if err != nil {
		return err
	}
	attempt := func() error {
		req, err := c.newRequest(ctx, http.MethodPut, key, nil, body, payloadHash, attrs.meta, cond)
		if err != nil {
			return err
		}
		req.ContentLength = size
		applyContentHeaders(req, attrs.contentType, attrs.contentEncoding)
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		return handlePutResponse(resp, cond)
	}
	if cond.isConditional() {
		return attempt()
	}
	return helpers.Retry(ctx, s3RetryPolicy(), func() error {
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return attempt()
	}, s3RetryableFor(ctx))
}

// deleteObject deletes an object by key, retrying a transient failure like
// getObject. A missing object (404) is treated as already deleted, matching
// S3's own idempotent DELETE semantics.
func (c *Client) deleteObject(ctx context.Context, key string) error {
	return helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newReadRequest(ctx, http.MethodDelete, key, nil)
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			return wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3DeleteFailed, resp))
		}
		return nil
	}, s3RetryableFor(ctx))
}

// deleteObjectsMaxKeys is the maximum number of keys S3's Multi-Object
// Delete (DeleteObjects) accepts in a single request.
const deleteObjectsMaxKeys = 1000

// deleteAllUnderPrefix deletes every object under prefix one listing page at a
// time, so memory stays bounded to a single page of keys.
func (c *Client) deleteAllUnderPrefix(ctx context.Context, prefix string) error {
	var token string
	for {
		page, err := c.listObjectsPage(ctx, prefix, token)
		if err != nil {
			return err
		}
		if err := c.deleteObjects(ctx, keysFrom(page.Contents), deleteObjectsMaxKeys); err != nil {
			return err
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	return nil
}

// deleteObjects deletes keys in DeleteObjects batches of at most maxKeys, a
// test seam that production sets to deleteObjectsMaxKeys.
func (c *Client) deleteObjects(ctx context.Context, keys []string, maxKeys int) error {
	for start := 0; start < len(keys); start += maxKeys {
		end := min(start+maxKeys, len(keys))
		if err := c.deleteObjectsBatch(ctx, keys[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// deleteRequest is the S3 Multi-Object Delete request body. Quiet suppresses
// per-key <Deleted> entries in the response, so only <Error> entries come
// back - all deleteObjectsBatch needs to detect a partial failure.
type deleteRequest struct {
	XMLName xml.Name            `xml:"Delete"`
	Objects []deleteObjectEntry `xml:"Object"`
	Quiet   bool                `xml:"Quiet"`
}

// deleteObjectEntry names one object key in a deleteRequest. It is named
// deleteObjectEntry, rather than deleteObject, to avoid clashing with the
// Client's existing single-object deleteObject method.
type deleteObjectEntry struct {
	Key string `xml:"Key"`
}

// deleteResult is the S3 Multi-Object Delete response body in Quiet mode:
// only failed keys are reported, each as an <Error> entry.
type deleteResult struct {
	XMLName xml.Name      `xml:"DeleteResult"`
	Errors  []deleteError `xml:"Error"`
}

// deleteError is one per-key failure reported by a DeleteObjects call.
type deleteError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// deleteObjectsBatch issues one DeleteObjects request, retried like the other
// idempotent verbs. Keys are bucket-derived, so the body is built with
// encoding/xml; a per-key <Error> inside a 200 body fails the call.
func (c *Client) deleteObjectsBatch(ctx context.Context, keys []string) error {
	objects := make([]deleteObjectEntry, len(keys))
	for i, key := range keys {
		objects[i] = deleteObjectEntry{Key: key}
	}
	body, err := xml.Marshal(deleteRequest{Quiet: true, Objects: objects})
	if err != nil {
		return err
	}
	payload := append([]byte(xml.Header), body...)

	hash := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(hash[:])
	// S3 rejects DeleteObjects without Content-MD5; it is an integrity header,
	// not a security primitive, since SigV4 already signs the sha256.
	sum := md5.Sum(payload) //nolint:gosec // see the Content-MD5 comment above
	contentMD5 := base64.StdEncoding.EncodeToString(sum[:])

	return helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newRequest(ctx, http.MethodPost, "", url.Values{"delete": {""}},
			bytes.NewReader(payload), payloadHash, nil, putCondition{})
		if err != nil {
			return err
		}
		req.ContentLength = int64(len(payload))
		req.Header.Set("Content-Type", "application/xml")
		req.Header.Set("Content-MD5", contentMD5)
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		if resp.StatusCode != http.StatusOK {
			return wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3DeleteFailed, resp))
		}
		data, err := io.ReadAll(helpers.NewSizeLimitedReader(resp.Body, helpers.S3ListMaxSize))
		if err != nil {
			return bodyReadError(ctx, errS3DeleteFailed, "s3 batch-delete response", err)
		}
		var result deleteResult
		if err := xml.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("%w: s3 batch-delete response does not decode: %w", errS3DeleteFailed, err)
		}
		if len(result.Errors) > 0 {
			e := result.Errors[0]
			return fmt.Errorf("%w: %d of %d keys failed (first: %q %s: %s)",
				errS3DeleteFailed, len(result.Errors), len(keys), e.Key, e.Code, e.Message)
		}
		return nil
	}, s3RetryableFor(ctx))
}

// listObjects returns object keys under the given prefix.
func (c *Client) listObjects(ctx context.Context, prefix string) ([]string, error) {
	keys := []string{}
	var token string
	for {
		result, err := c.listObjectsPage(ctx, prefix, token)
		if err != nil {
			return nil, err
		}
		keys = appendKeys(keys, result.Contents)
		if !result.IsTruncated || result.NextContinuationToken == "" {
			break
		}
		token = result.NextContinuationToken
	}
	return keys, nil
}

func resolvePayloadHash(body io.ReadSeeker, payloadHash string) (string, error) {
	if payloadHash != "" {
		return payloadHash, nil
	}
	hash, err := hashReader(body)
	if err != nil {
		return "", err
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hash, nil
}

func applyContentHeaders(req *http.Request, contentType, contentEncoding string) {
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
}

// handlePutResponse maps a PUT response to this package's errors. A 412 or 409
// is control flow only for a conditional write, and an overwrite's is a failure;
// a 404 on an If-Match write is the object deleted after its HEAD, not the bucket.
func handlePutResponse(resp *http.Response, cond putCondition) error {
	switch resp.StatusCode {
	case http.StatusPreconditionFailed:
		if cond.isConditional() {
			return errS3PreconditionFailed
		}
		return wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3PutFailed, resp))
	case http.StatusNotFound:
		if cond.ifMatch != "" {
			return errS3NotFound
		}
		return errS3BucketNotFound
	case http.StatusConflict:
		if cond.isConditional() {
			return errS3ConditionalConflict
		}
		return wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3PutFailed, resp))
	case http.StatusOK, http.StatusNoContent:
		return nil
	default:
		return wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3PutFailed, resp))
	}
}

// listObjectsPage fetches one ListObjectsV2 page. bucketRequest is
// single-shot, so this retry alone covers both a transient status and a
// stalled body read, within one attempt budget.
func (c *Client) listObjectsPage(ctx context.Context, prefix, token string) (listBucketResult, error) {
	query := url.Values{}
	query.Set("list-type", "2")
	if prefix != "" {
		query.Set("prefix", prefix)
	}
	if token != "" {
		query.Set("continuation-token", token)
	}
	var result listBucketResult
	err := helpers.Retry(ctx, s3RetryPolicy(), func() error {
		resp, err := c.bucketRequest(ctx, http.MethodGet, query)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(helpers.NewSizeLimitedReader(resp.Body, helpers.S3ListMaxSize))
		_ = resp.Body.Close()
		if err != nil {
			return bodyReadError(ctx, errS3BucketRequestFailed, "s3 listing response", err)
		}
		var parsed listBucketResult
		if err := xml.Unmarshal(data, &parsed); err != nil {
			return fmt.Errorf("%w: s3 listing response does not decode: %w", errS3BucketRequestFailed, err)
		}
		result = parsed
		return nil
	}, s3RetryableFor(ctx))
	if err != nil {
		return listBucketResult{}, err
	}
	return result, nil
}

// bodyReadError names surface in a failed read of a 200 body and wraps it in
// sentinel unless ctx ended, which do leaves unclassified too; %w keeps a cause
// such as helpers.ErrResponseTooLarge or helpers.ErrReadStalled matching.
func bodyReadError(ctx context.Context, sentinel error, surface string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", surface, err)
	}
	return fmt.Errorf("%w: %s: %w", sentinel, surface, err)
}

func appendKeys(dst []string, contents []listBucketContent) []string {
	for _, item := range contents {
		if item.Key != "" {
			dst = append(dst, item.Key)
		}
	}
	return dst
}

// keysFrom returns one listing page's non-empty keys; unlike appendKeys it
// does not accumulate, so deleteAllUnderPrefix holds one page at a time.
func keysFrom(contents []listBucketContent) []string {
	keys := make([]string, 0, len(contents))
	for _, item := range contents {
		if item.Key != "" {
			keys = append(keys, item.Key)
		}
	}
	return keys
}

// ensureBucket creates the bucket when it does not exist.
func (c *Client) ensureBucket(ctx context.Context) error {
	if err := c.headBucket(ctx); err != nil {
		if errors.Is(err, errS3BucketNotFound) {
			return c.createBucket(ctx)
		}
		return err
	}
	return nil
}

// headBucket checks that the bucket exists, retrying like the other idempotent
// verbs; a HEAD has no body, so its failure stays status-only.
func (c *Client) headBucket(ctx context.Context) error {
	return helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newReadRequest(ctx, http.MethodHead, "", nil)
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		if resp.StatusCode == http.StatusNotFound {
			return errS3BucketNotFound
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			return wrapRetryableStatus(resp.StatusCode, fmt.Errorf("%w: %s", errS3BucketHeadFailed, resp.Status))
		}
		return nil
	}, s3RetryableFor(ctx))
}

// createBucket sends a CreateBucket request with region configuration.
func (c *Client) createBucket(ctx context.Context) error {
	var (
		body        io.ReadSeeker
		contentType string
		contentSize int64
		payloadHash = emptySHA256
	)
	if c.cfg.Region != "" && c.cfg.Region != "us-east-1" {
		payload := fmt.Appendf(nil,
			"<CreateBucketConfiguration xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\">"+
				"<LocationConstraint>%s</LocationConstraint>"+
				"</CreateBucketConfiguration>",
			c.cfg.Region,
		)
		hash := sha256.Sum256(payload)
		payloadHash = hex.EncodeToString(hash[:])
		body = bytes.NewReader(payload)
		contentType = "application/xml"
		contentSize = int64(len(payload))
	}
	req, err := c.newRequest(ctx, http.MethodPut, "", nil, body, payloadHash, nil, putCondition{})
	if err != nil {
		return err
	}
	req.ContentLength = contentSize
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusConflict {
		return nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return s3StatusError(errS3CreateBucketFailed, resp)
	}
	return nil
}

// bucketRequest issues one unretried request against the bucket root, since
// listObjectsPage owns the retry. A 200 is returned with its body open for the
// caller; any other status closes it.
func (c *Client) bucketRequest(ctx context.Context, method string, query url.Values) (*http.Response, error) {
	req, err := c.newReadRequest(ctx, method, "", query)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, errS3BucketNotFound
	}
	if resp.StatusCode != http.StatusOK {
		statusErr := wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3BucketRequestFailed, resp))
		_ = resp.Body.Close()
		return nil, statusErr
	}
	return resp, nil
}

// listBucketResult holds the three elements of the S3 ListObjectsV2 XML
// response this client reads: the page's object entries and the two
// pagination markers. encoding/xml ignores every element not mapped here.
type listBucketResult struct {
	NextContinuationToken string              `xml:"NextContinuationToken"`
	Contents              []listBucketContent `xml:"Contents"`
	IsTruncated           bool                `xml:"IsTruncated"`
}

// listBucketContent represents an object entry in a ListBucket response.
type listBucketContent struct {
	Key string `xml:"Key"`
}

// newReadRequest builds and signs a body-less request: the empty-payload
// sha256, no user metadata and no write precondition.
func (c *Client) newReadRequest(ctx context.Context, method, key string, query url.Values) (*http.Request, error) {
	return c.newRequest(ctx, method, key, query, nil, emptySHA256, nil, putCondition{})
}

// newRequest builds and signs a request for the given object key.
func (c *Client) newRequest(
	ctx context.Context,
	method, key string,
	query url.Values,
	body io.ReadSeeker,
	payloadHash string,
	meta map[string]string,
	cond putCondition,
) (*http.Request, error) {
	reqURL, host, canonicalURI, canonicalQuery := c.requestURL(key, query)
	if payloadHash == "" {
		payloadHash = emptySHA256
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, err
	}
	req.Host = host
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzDate)
	if c.cfg.SessionToken.IsSet() {
		// Reveal here is the value going onto the wire: this header is where
		// a session token is transmitted, so there is nothing further to
		// protect it from.
		req.Header.Set("X-Amz-Security-Token", c.cfg.SessionToken.Reveal())
	}
	for key, value := range meta {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		name := "X-Amz-Meta-" + helpers.UpperFirstRune(strings.TrimSpace(key))
		req.Header.Set(name, trimmed)
	}
	if cond.ifNoneMatch {
		req.Header.Set("If-None-Match", "*")
	}
	if cond.ifMatch != "" {
		req.Header.Set("If-Match", cond.ifMatch)
	}
	canonicalHeaders, signedHeaders := canonicalizeHeaders(host, req.Header)
	req.Header.Set("Authorization", c.signRequest(method, canonicalURI, canonicalQuery, amzDate, payloadHash, canonicalHeaders, signedHeaders))
	return req, nil
}

// requestURL builds the request URL and canonical components.
func (c *Client) requestURL(key string, query url.Values) (string, string, string, string) {
	endpoint := c.cfg.Endpoint
	host := c.endpointHost
	key = strings.TrimLeft(key, "/")

	var objectPath string
	if c.cfg.PathStyle {
		if key == "" {
			objectPath = "/" + c.cfg.Bucket
		} else {
			objectPath = "/" + c.cfg.Bucket + "/" + key
		}
	} else {
		host = c.cfg.Bucket + "." + host
		objectPath = "/" + key
	}

	// Sign and send the same escaped path: net/url escapes reserved characters
	// differently, and any mismatch is a 403 SignatureDoesNotMatch.
	escapedPath := encodePath(objectPath)
	canonicalURI := escapedPath
	canonicalQuery := canonicalizeQuery(query)
	reqURL := endpoint + escapedPath
	if !c.cfg.PathStyle && c.endpointScheme != "" {
		reqURL = c.endpointScheme + "://" + host + escapedPath
	}
	if canonicalQuery != "" {
		reqURL += "?" + canonicalQuery
	}
	return reqURL, host, canonicalURI, canonicalQuery
}

// signingKeyForDate returns the SigV4 signing key for date, derived once per
// UTC date, since a key for another date signs the wrong scope and gets a 403.
// The cached slice is never mutated, so returning it uncopied is race-free.
func (c *Client) signingKeyForDate(date string) []byte {
	c.signing.mu.Lock()
	defer c.signing.mu.Unlock()
	if c.signing.key == nil || c.signing.date != date {
		// The HMAC chain is the one operation that needs the plaintext secret.
		c.signing.key = deriveSigningKey(c.cfg.SecretKey.Reveal(), date, c.cfg.Region)
		c.signing.date = date
	}
	return c.signing.key
}

// signRequest builds the AWS SigV4 Authorization header value.
func (c *Client) signRequest(
	method string,
	canonicalURI string,
	canonicalQuery string,
	amzDate string,
	payloadHash string,
	canonicalHeaders string,
	signedHeaders string,
) string {
	date := amzDate[:8]
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", date, c.cfg.Region)
	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(hash[:]),
	}, "\n")

	signingKey := c.signingKeyForDate(date)
	signature := hmacSHA256Hex(signingKey, stringToSign)
	return fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.cfg.AccessKey,
		scope,
		signedHeaders,
		signature,
	)
}

// canonicalizeHeaders returns canonical and signed header strings.
func canonicalizeHeaders(host string, headers http.Header) (string, string) {
	entries := map[string]string{
		"host": normalizeHeaderValue([]string{host}),
	}
	for name, values := range headers {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		entries[lower] = normalizeHeaderValue(values)
	}
	names := slices.Sorted(maps.Keys(entries))

	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name)
		canonical.WriteString(":")
		canonical.WriteString(entries[name])
		canonical.WriteString("\n")
	}

	return canonical.String(), strings.Join(names, ";")
}

// normalizeHeaderValue trims and collapses whitespace in header values.
func normalizeHeaderValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		normalized = append(normalized, strings.Join(strings.Fields(value), " "))
	}
	return strings.Join(normalized, ",")
}

// canonicalizeQuery returns the canonical query string.
func canonicalizeQuery(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	keys := slices.Sorted(maps.Keys(values))
	pairs := make([]string, 0, len(values))
	for _, key := range keys {
		vals := values[key]
		slices.Sort(vals)
		for _, value := range vals {
			pairs = append(pairs, awsEncode(key)+"="+awsEncode(value))
		}
	}
	return strings.Join(pairs, "&")
}

// awsEncode encodes a query value according to AWS canonical rules.
func awsEncode(value string) string {
	escaped := url.QueryEscape(value)
	escaped = strings.ReplaceAll(escaped, "+", "%20")
	escaped = strings.ReplaceAll(escaped, "%7E", "~")
	return escaped
}

// encodePath escapes a path with awsURIEncode, leaving "/" literal; the result
// is both the signed canonical URI and the wire path.
func encodePath(value string) string {
	if value == "" {
		return "/"
	}
	return awsURIEncode(value, false)
}

// upperHex is the hex digit alphabet AWS SigV4 requires for percent-encoding:
// uppercase, unlike net/url's lowercase output.
const upperHex = "0123456789ABCDEF"

// awsURIEncode percent-encodes s per SigV4 UriEncode: A-Za-z0-9 and -._~ stay
// literal, "/" too unless encodeSlash, every other byte becomes uppercase %XX.
// url.PathEscape leaves +$&,;=:@ literal, which S3 would sign differently.
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	// Index raw bytes: ranging over the string would decode UTF-8, and each
	// byte of a multibyte rune needs its own %XX.
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte('/')
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0f])
		}
	}
	return b.String()
}

// deriveSigningKey derives the signing key for the given date and region.
func deriveSigningKey(secret, date, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	return hmacSHA256(kService, "aws4_request")
}

// hmacSHA256 returns the HMAC-SHA256 of data using key.
func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

// hmacSHA256Hex returns the hex-encoded HMAC-SHA256 of data.
func hmacSHA256Hex(key []byte, data string) string {
	return hex.EncodeToString(hmacSHA256(key, data))
}

// hashReader returns the SHA256 hash of the reader's contents.
func hashReader(r io.Reader) (string, error) {
	hasher := sha256.New()
	if _, err := io.Copy(hasher, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
