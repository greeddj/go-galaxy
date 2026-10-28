package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// errTestReadRefused and errTestSeekRefused are what the failing bodies below
// report from Read and Seek.
var (
	errTestReadRefused = errors.New("body read refused")
	errTestSeekRefused = errors.New("body seek refused")
)

// readRefusingBody fails every Read, so hashing it fails.
type readRefusingBody struct{}

func (readRefusingBody) Read([]byte) (int, error)       { return 0, errTestReadRefused }
func (readRefusingBody) Seek(int64, int) (int64, error) { return 0, nil }

// seekRefusingBody reads normally but fails every Seek, so it cannot be
// rewound for a send.
type seekRefusingBody struct {
	*bytes.Reader
}

func (seekRefusingBody) Seek(int64, int) (int64, error) { return 0, errTestSeekRefused }

// newUnbuildableRequestClient returns a virtual-host client whose bucket name
// cannot form a host, so no request can be built, and the count of requests
// its server received.
func newUnbuildableRequestClient(t *testing.T) (*Client, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	cfg := config.S3CacheConfig{
		Endpoint:  srv.URL,
		Bucket:    "bad bucket",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
	}
	client, err := newClient(cfg, srv.Client())
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return client, &hits
}

// clientGetError calls c.getObject and returns its error, closing any
// response, though getObject returns none alongside an error.
func clientGetError(ctx context.Context, c *Client, key string) error {
	resp, err := c.getObject(ctx, key)
	if resp != nil {
		_ = resp.Body.Close()
	}
	return err
}

// unbuildableVerbs drives every client verb once against c.
func unbuildableVerbs(c *Client) map[string]func(context.Context) error {
	body := []byte("payload")
	return map[string]func(context.Context) error{
		"getObject": func(ctx context.Context) error { return clientGetError(ctx, c, "k") },
		"headObject": func(ctx context.Context) error {
			_, err := c.headObject(ctx, "k")
			return err
		},
		"putObject": func(ctx context.Context) error {
			return c.putObject(ctx, "k", bytes.NewReader(body), int64(len(body)), putObjectAttrs{}, putCondition{})
		},
		"conditional putObject": func(ctx context.Context) error {
			return c.putObject(ctx, "k", bytes.NewReader(body), int64(len(body)), putObjectAttrs{}, putCondition{ifNoneMatch: true})
		},
		"deleteObject":  func(ctx context.Context) error { return c.deleteObject(ctx, "k") },
		"deleteObjects": func(ctx context.Context) error { return c.deleteObjects(ctx, []string{"k"}, deleteObjectsMaxKeys) },
		"listObjects": func(ctx context.Context) error {
			_, err := c.listObjects(ctx, "")
			return err
		},
		"ensureBucket": c.ensureBucket,
		"createBucket": c.createBucket,
	}
}

// TestUnbuildableRequestFailsEveryVerbWithoutSending pins that a request that
// cannot be built, a bucket name no host can carry here, fails each verb with
// the URL error before anything is sent.
func TestUnbuildableRequestFailsEveryVerbWithoutSending(t *testing.T) {
	t.Parallel()
	c, hits := newUnbuildableRequestClient(t)
	for name, verb := range unbuildableVerbs(c) {
		err := verb(context.Background())
		if _, ok := errors.AsType[*url.Error](err); !ok {
			t.Errorf("%s = %v, want the *url.Error the build reported", name, err)
		}
		if errors.Is(err, errS3TransportFailed) {
			t.Errorf("%s = %v, a request never sent is not a transport failure", name, err)
		}
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("the server received %d requests, want none", got)
	}
}

// TestPutObjectBodyFailuresStopBeforeSending pins that a body putObject cannot
// hash, or cannot rewind for a send, fails the write with that cause and
// sends no PUT.
func TestPutObjectBodyFailuresStopBeforeSending(t *testing.T) {
	t.Parallel()
	payload := []byte("snapshot bytes")
	sum := sha256.Sum256(payload)
	cases := []struct {
		body  io.ReadSeeker
		want  error
		name  string
		attrs putObjectAttrs
	}{
		{name: "hash read fails", body: readRefusingBody{}, want: errTestReadRefused},
		{name: "rewind after hashing fails", body: seekRefusingBody{bytes.NewReader(payload)}, want: errTestSeekRefused},
		{
			name:  "rewind before the send fails",
			body:  seekRefusingBody{bytes.NewReader(payload)},
			attrs: putObjectAttrs{payloadHash: hex.EncodeToString(sum[:])},
			want:  errTestSeekRefused,
		},
	}
	for _, tc := range cases {
		b, fake := newTestBackendAndFake(t)
		if err := b.Open(context.Background()); err != nil {
			t.Fatalf("Open: %v", err)
		}
		err := b.client.putObject(context.Background(), "obj", tc.body, int64(len(payload)), tc.attrs, putCondition{})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: putObject = %v, want %v", tc.name, err, tc.want)
		}
		if got := fake.requestCount("obj", http.MethodPut); got != 0 {
			t.Errorf("%s: %d PUTs sent, want none", tc.name, got)
		}
	}
}

// putResponseCase is one row of TestHandlePutResponse.
type putResponseCase struct {
	want      error
	name      string
	cond      putCondition
	status    int
	retryable bool
}

// putResponseCases covers every status arm under an overwrite, a
// create-if-absent and a compare-and-swap precondition.
func putResponseCases() []putResponseCase {
	create := putCondition{ifNoneMatch: true}
	swap := putCondition{ifMatch: `"etag"`}
	return []putResponseCase{
		{name: "200 overwrite", status: http.StatusOK},
		{name: "204 swap", status: http.StatusNoContent, cond: swap},
		{name: "412 overwrite", status: http.StatusPreconditionFailed, want: errS3PutFailed},
		{name: "412 create", status: http.StatusPreconditionFailed, cond: create, want: errS3PreconditionFailed},
		{name: "412 swap", status: http.StatusPreconditionFailed, cond: swap, want: errS3PreconditionFailed},
		{name: "404 overwrite", status: http.StatusNotFound, want: errS3BucketNotFound},
		{name: "404 create", status: http.StatusNotFound, cond: create, want: errS3BucketNotFound},
		{name: "404 swap", status: http.StatusNotFound, cond: swap, want: errS3NotFound},
		{name: "409 overwrite", status: http.StatusConflict, want: errS3PutFailed},
		{name: "409 create", status: http.StatusConflict, cond: create, want: errS3ConditionalConflict},
		{name: "403 overwrite", status: http.StatusForbidden, want: errS3PutFailed},
		{name: "503 overwrite", status: http.StatusServiceUnavailable, want: errS3PutFailed, retryable: true},
	}
}

// TestHandlePutResponse pins how each PUT status maps to this package's
// sentinels and which of them stay retryable.
func TestHandlePutResponse(t *testing.T) {
	t.Parallel()
	for _, tc := range putResponseCases() {
		resp := &http.Response{
			StatusCode: tc.status,
			Status:     http.StatusText(tc.status),
			Body:       io.NopCloser(strings.NewReader("")),
		}
		err := handlePutResponse(resp, tc.cond)
		if tc.want == nil {
			if err != nil {
				t.Errorf("%s: handlePutResponse = %v, want nil", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: handlePutResponse = %v, want %v", tc.name, err, tc.want)
		}
		if got := s3Retryable(context.Background(), err); got != tc.retryable {
			t.Errorf("%s: s3Retryable = %v, want %v", tc.name, got, tc.retryable)
		}
	}
}

// TestNewRequestHeaders pins that an empty payload hash signs as the empty
// body's, blank metadata is dropped, the rest trimmed under an upper-cased
// name, and If-Match carries the ETag.
func TestNewRequestHeaders(t *testing.T) {
	t.Parallel()
	c := newRequestURLTestClient(t, true)
	meta := map[string]string{"blank": "   "}
	meta[" token "] = " abc "
	req, err := c.newRequest(context.Background(), http.MethodPut, "obj", nil, nil, "", meta, putCondition{ifMatch: `"v1"`})
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}
	checks := map[string]string{
		"X-Amz-Content-Sha256": emptySHA256,
		"X-Amz-Meta-Token":     "abc",
		"X-Amz-Meta-Blank":     "",
		"If-Match":             `"v1"`,
		"If-None-Match":        "",
	}
	for name, want := range checks {
		if got := req.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if auth := req.Header.Get("Authorization"); !strings.Contains(auth, "x-amz-meta-token") {
		t.Errorf("Authorization = %q, want x-amz-meta-token among the signed headers", auth)
	}
}

// newClientCase is one row of TestNewClientValidatesAndDefaults.
type newClientCase struct {
	httpClient   *http.Client
	want         error
	name         string
	endpoint     string
	region       string
	bucket       string
	wantEndpoint string
	wantHost     string
	wantRegion   string
}

// newClientCases covers both refusals, the AWS defaults and endpoint parsing.
func newClientCases() []newClientCase {
	httpClient := &http.Client{}
	return []newClientCase{
		{name: "no bucket", httpClient: httpClient, want: errS3BucketEmpty},
		{name: "no HTTP client", bucket: "b", want: errS3HTTPClientNil},
		{
			name: "AWS default region and endpoint", bucket: "b", httpClient: httpClient,
			wantEndpoint: "https://s3.us-east-1.amazonaws.com", wantHost: "s3.us-east-1.amazonaws.com", wantRegion: "us-east-1",
		},
		{
			name: "AWS endpoint of a region", bucket: "b", region: "eu-west-1", httpClient: httpClient,
			wantEndpoint: "https://s3.eu-west-1.amazonaws.com", wantHost: "s3.eu-west-1.amazonaws.com", wantRegion: "eu-west-1",
		},
		{
			name: "bare host gets https and loses its trailing slash", bucket: "b", endpoint: "minio.local:9000/",
			httpClient: httpClient, wantEndpoint: "https://minio.local:9000", wantHost: "minio.local:9000", wantRegion: "us-east-1",
		},
	}
}

// TestNewClientValidatesAndDefaults pins newClient's refusals and how it
// fills in the region and endpoint a configuration leaves out.
func TestNewClientValidatesAndDefaults(t *testing.T) {
	t.Parallel()
	for _, tc := range newClientCases() {
		cfg := config.S3CacheConfig{Bucket: tc.bucket, Region: tc.region, Endpoint: tc.endpoint}
		c, err := newClient(cfg, tc.httpClient)
		if tc.want != nil {
			if !errors.Is(err, tc.want) || c != nil {
				t.Errorf("%s: newClient = %v, %v; want nil and %v", tc.name, c, err, tc.want)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: newClient = %v", tc.name, err)
			continue
		}
		if c.cfg.Endpoint != tc.wantEndpoint || c.endpointHost != tc.wantHost || c.cfg.Region != tc.wantRegion {
			t.Errorf("%s: endpoint %q host %q region %q, want %q, %q, %q", tc.name,
				c.cfg.Endpoint, c.endpointHost, c.cfg.Region, tc.wantEndpoint, tc.wantHost, tc.wantRegion)
		}
	}
}

// TestNewClientRefusesAnEndpointItCannotUse pins that an endpoint that does
// not parse, or names no host, is refused before any request.
func TestNewClientRefusesAnEndpointItCannotUse(t *testing.T) {
	t.Parallel()
	if _, err := newClient(config.S3CacheConfig{Bucket: "b", Endpoint: "http://[::1"}, &http.Client{}); err == nil {
		t.Error("newClient with an unparsable endpoint = nil error, want the parse error")
	}
	_, err := newClient(config.S3CacheConfig{Bucket: "b", Endpoint: "file:///srv/cache"}, &http.Client{})
	if !errors.Is(err, errS3InvalidEndpoint) || !errors.Is(err, helpers.ErrCacheBackendUnusable) {
		t.Errorf("newClient with a hostless endpoint = %v, want errS3InvalidEndpoint", err)
	}
}

// TestCanonicalHelpersOnEmptyInput pins the empty-input answers SigV4 needs:
// no header values join to "", and an empty path is the root "/".
func TestCanonicalHelpersOnEmptyInput(t *testing.T) {
	t.Parallel()
	if got := normalizeHeaderValue(nil); got != "" {
		t.Errorf("normalizeHeaderValue(nil) = %q, want empty", got)
	}
	if got := normalizeHeaderValue([]string{"  a   b ", "c"}); got != "a b,c" {
		t.Errorf("normalizeHeaderValue = %q, want %q", got, "a b,c")
	}
	if got := encodePath(""); got != "/" {
		t.Errorf("encodePath(\"\") = %q, want /", got)
	}
}

// TestStatusErrorFallsBackWhenTheBodyBreaksOff pins that an error body cut
// short is reported by status alone, after one request for a 403.
func TestStatusErrorFallsBackWhenTheBodyBreaksOff(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	c := newHandlerTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<Error><Code>Partial")
	}, nil)

	err := clientGetError(context.Background(), c, "obj")
	if !errors.Is(err, errS3GetFailed) {
		t.Fatalf("getObject = %v, want errS3GetFailed", err)
	}
	if strings.Contains(err.Error(), "Partial") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("getObject = %q, want the status alone, without the cut body", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("%d requests, want 1: a 403 is not retried", got)
	}
}

// TestDeleteObjectOfAMissingKeyIsDone pins that a 404 to DELETE is success
// after one request: S3's DELETE is idempotent.
func TestDeleteObjectOfAMissingKeyIsDone(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	fake.failNext("gone", http.MethodDelete, http.StatusNotFound, -1)
	if err := b.client.deleteObject(context.Background(), "gone"); err != nil {
		t.Fatalf("deleteObject on a 404 = %v, want nil", err)
	}
	if got := fake.requestCount("gone", http.MethodDelete); got != 1 {
		t.Fatalf("%d DELETEs, want 1", got)
	}
}
