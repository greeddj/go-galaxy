package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Client implements minimal S3 operations with SigV4 signing.
type Client struct {
	cfg    config.S3CacheConfig
	client *http.Client
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
	cfg.Endpoint = strings.TrimRight(endpoint, "/")
	return &Client{cfg: cfg, client: httpClient}, nil
}

// getObject performs a GET request for the object key.
func (c *Client) getObject(ctx context.Context, key string) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodGet, key, nil, nil, emptySHA256, nil, false)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, errS3NotFound
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: %s", errS3GetFailed, resp.Status)
	}
	return resp, nil
}

// headObject performs a HEAD request for the object key.
func (c *Client) headObject(ctx context.Context, key string) (http.Header, error) {
	req, err := c.newRequest(ctx, http.MethodHead, key, nil, nil, emptySHA256, nil, false)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errS3NotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s", errS3HeadFailed, resp.Status)
	}
	return resp.Header.Clone(), nil
}

// putObject uploads an object with optional metadata.
func (c *Client) putObject(
	ctx context.Context,
	key string,
	body io.ReadSeeker,
	size int64,
	contentType, contentEncoding string,
	meta map[string]string,
	ifNoneMatch bool,
	payloadHash string,
) error {
	payloadHash, err := resolvePayloadHash(body, payloadHash)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPut, key, nil, body, payloadHash, meta, ifNoneMatch)
	if err != nil {
		return err
	}
	req.ContentLength = size
	applyContentHeaders(req, contentType, contentEncoding)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	return handlePutResponse(resp)
}

// deleteObject deletes an object by key.
func (c *Client) deleteObject(ctx context.Context, key string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, key, nil, nil, emptySHA256, nil, false)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
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
		return fmt.Errorf("%w: %s", errS3DeleteFailed, resp.Status)
	}
	return nil
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

func handlePutResponse(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusPreconditionFailed:
		return errS3PreconditionFailed
	case http.StatusNotFound:
		return errS3BucketNotFound
	case http.StatusOK, http.StatusNoContent:
		return nil
	default:
		return fmt.Errorf("%w: %s", errS3PutFailed, resp.Status)
	}
}

func (c *Client) listObjectsPage(ctx context.Context, prefix, token string) (listBucketResult, error) {
	query := url.Values{}
	query.Set("list-type", "2")
	if prefix != "" {
		query.Set("prefix", prefix)
	}
	if token != "" {
		query.Set("continuation-token", token)
	}
	resp, err := c.bucketRequest(ctx, http.MethodGet, query)
	if err != nil {
		return listBucketResult{}, err
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return listBucketResult{}, err
	}
	var result listBucketResult
	if err := xml.Unmarshal(data, &result); err != nil {
		return listBucketResult{}, err
	}
	return result, nil
}

func appendKeys(dst []string, contents []listBucketContent) []string {
	for _, item := range contents {
		if item.Key != "" {
			dst = append(dst, item.Key)
		}
	}
	return dst
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

// headBucket checks whether the configured bucket exists.
func (c *Client) headBucket(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodHead, "", nil, nil, emptySHA256, nil, false)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
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
		return fmt.Errorf("%w: %s", errS3BucketHeadFailed, resp.Status)
	}
	return nil
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
	req, err := c.newRequest(ctx, http.MethodPut, "", nil, body, payloadHash, nil, false)
	if err != nil {
		return err
	}
	req.ContentLength = contentSize
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.client.Do(req)
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
		return fmt.Errorf("%w: %s", errS3CreateBucketFailed, resp.Status)
	}
	return nil
}

// bucketRequest issues a request against the bucket root.
func (c *Client) bucketRequest(ctx context.Context, method string, query url.Values) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, "", query, nil, emptySHA256, nil, false)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, errS3BucketNotFound
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: %s", errS3BucketRequestFailed, resp.Status)
	}
	return resp, nil
}

// listBucketResult represents the S3 ListBucket XML response.
type listBucketResult struct {
	Contents               []listBucketContent `xml:"Contents"`
	IsTruncated            bool                `xml:"IsTruncated"`
	NextContinuationToken  string              `xml:"NextContinuationToken"`
	ContinuationToken      string              `xml:"ContinuationToken"`
	KeyCount               int                 `xml:"KeyCount"`
	MaxKeys                int                 `xml:"MaxKeys"`
	Prefix                 string              `xml:"Prefix"`
	Delimiter              string              `xml:"Delimiter"`
	CommonPrefixes         []listBucketPrefix  `xml:"CommonPrefixes"`
	StartAfter             string              `xml:"StartAfter"`
	ContinuationTokenStart string              `xml:"ContinuationTokenStart"`
}

// listBucketContent represents an object entry in a ListBucket response.
type listBucketContent struct {
	Key string `xml:"Key"`
}

// listBucketPrefix represents a common prefix entry in a ListBucket response.
type listBucketPrefix struct {
	Prefix string `xml:"Prefix"`
}

// newRequest builds and signs a request for the given object key.
func (c *Client) newRequest(
	ctx context.Context,
	method, key string,
	query url.Values,
	body io.ReadSeeker,
	payloadHash string,
	meta map[string]string,
	ifNoneMatch bool,
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
	if c.cfg.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.cfg.SessionToken)
	}
	for key, value := range meta {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		name := "X-Amz-Meta-" + helpers.UpperFirstRune(strings.TrimSpace(key))
		req.Header.Set(name, trimmed)
	}
	if ifNoneMatch {
		req.Header.Set("If-None-Match", "*")
	}
	canonicalHeaders, signedHeaders := canonicalizeHeaders(host, req.Header)
	req.Header.Set("Authorization", c.signRequest(method, canonicalURI, canonicalQuery, amzDate, payloadHash, canonicalHeaders, signedHeaders))
	return req, nil
}

// requestURL builds the request URL and canonical components.
func (c *Client) requestURL(key string, query url.Values) (string, string, string, string) {
	endpoint := c.cfg.Endpoint
	parsed, _ := url.Parse(endpoint)
	host := parsed.Host
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

	canonicalURI := encodePath(objectPath)
	canonicalQuery := canonicalizeQuery(query)
	reqURL := endpoint + objectPath
	if !c.cfg.PathStyle && parsed.Scheme != "" {
		reqURL = parsed.Scheme + "://" + host + objectPath
	}
	if canonicalQuery != "" {
		reqURL += "?" + canonicalQuery
	}
	return reqURL, host, canonicalURI, canonicalQuery
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

	signingKey := deriveSigningKey(c.cfg.SecretKey, date, c.cfg.Region)
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
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

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
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(values))
	for _, key := range keys {
		vals := values[key]
		sort.Strings(vals)
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

// encodePath encodes each path segment for signature calculations.
func encodePath(value string) string {
	if value == "" {
		return "/"
	}
	segments := strings.Split(value, "/")
	for i, segment := range segments {
		segments[i] = pathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// pathEscape escapes a path segment while preserving slashes.
func pathEscape(value string) string {
	if value == "" {
		return ""
	}
	return strings.ReplaceAll(url.PathEscape(value), "%2F", "/")
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
