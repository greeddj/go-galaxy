package fakegalaxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/manifest"
	"github.com/psvmcc/hub/pkg/types"
)

// doGet issues a GET against url with a background context, failing the
// test on any transport error. The caller owns the returned response and
// must close its body.
func doGet(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// doGetWithAuth issues a GET with Authorization set to auth unless it is
// empty, failing the test on a transport error; the caller closes the body.
func doGetWithAuth(t *testing.T, client *http.Client, url, auth string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", url, err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// getJSON issues a GET, decodes a 200 body into a non-nil target, closes the
// body and returns the status code.
func getJSON(t *testing.T, client *http.Client, url string, target any) int {
	t.Helper()
	resp := doGet(t, client, url)
	defer func() { _ = resp.Body.Close() }()
	if target != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
			t.Fatalf("decode response from %s: %v", url, err)
		}
	}
	return resp.StatusCode
}

// getConditional issues a GET against url carrying the given If-None-Match
// value, returning the response's status code and its (fully drained)
// body.
func getConditional(t *testing.T, client *http.Client, url, ifNoneMatch string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", url, err)
	}
	req.Header.Set("If-None-Match", ifNoneMatch)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("conditional GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read conditional response from %s: %v", url, err)
	}
	return resp.StatusCode, body
}

// TestRootMetadataReflectsHighestVersion registers two versions and asserts
// root metadata reports the higher one as highest_version.
func TestRootMetadataReflectsHighestVersion(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.AddVersion("ns", "name", "2.5.0", nil)

	var root types.GalaxyCollection
	status := getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name", &root)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	wantVersionsURL := s.URL() + "/api/v3/collections/ns/name/versions/"
	if root.VersionsURL != wantVersionsURL {
		t.Errorf("versions_url = %q, want %q", root.VersionsURL, wantVersionsURL)
	}
	if root.HighestVersion.Version != "2.5.0" {
		t.Errorf("highest_version.version = %q, want %q", root.HighestVersion.Version, "2.5.0")
	}
}

// TestV2AndBareAPIProbes404 asserts a real client's v2/bare-API probes 404
// rather than being served v3 metadata.
func TestV2AndBareAPIProbes404(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)

	for _, path := range []string{"/api/v2/collections/ns/name/", "/api/collections/ns/name/"} {
		if status := getJSON(t, s.Client(), s.URL()+path, nil); status != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want %d", path, status, http.StatusNotFound)
		}
	}
}

// TestVersionsListPagination registers three versions and asserts limit/offset
// paginate them, returning the whole set when neither is given.
func TestVersionsListPagination(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.AddVersion("ns", "name", "1.1.0", nil)
	s.AddVersion("ns", "name", "2.0.0", nil)

	base := s.URL() + "/api/v3/collections/ns/name/versions"
	cases := []struct {
		name      string
		query     string
		wantFirst string
		wantLen   int
	}{
		{name: "first page", query: "?limit=1&offset=0", wantLen: 1, wantFirst: "1.0.0"},
		{name: "third page", query: "?limit=1&offset=2", wantLen: 1, wantFirst: "2.0.0"},
		{name: "no params returns everything", query: "", wantLen: 3, wantFirst: "1.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var versions types.GalaxyCollectionVersions
			status := getJSON(t, s.Client(), base+tc.query, &versions)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want %d", status, http.StatusOK)
			}
			if versions.Meta.Count != 3 {
				t.Errorf("meta.count = %d, want 3", versions.Meta.Count)
			}
			if len(versions.Data) != tc.wantLen {
				t.Fatalf("len(data) = %d, want %d", len(versions.Data), tc.wantLen)
			}
			if versions.Data[0].Version != tc.wantFirst {
				t.Errorf("data[0].version = %q, want %q", versions.Data[0].Version, tc.wantFirst)
			}
		})
	}
}

// TestVersionDetail pins a version detail's download URL, the sha256
// AddVersion returned and metadata.dependencies; an unregistered version 404s.
func TestVersionDetail(t *testing.T) {
	t.Parallel()
	s := New(t)
	deps := map[string]string{"ns2.dep": ">=1.0.0"}
	v := s.AddVersion("ns", "name", "1.0.0", deps)

	var info types.GalaxyCollectionVersionInfo
	status := getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name/versions/1.0.0/", &info)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	wantDownloadURL := s.URL() + "/download/ns-name-1.0.0.tar.gz"
	if info.DownloadURL != wantDownloadURL {
		t.Errorf("download_url = %q, want %q", info.DownloadURL, wantDownloadURL)
	}
	if info.Artifact.Sha256 != v.SHA256 {
		t.Errorf("artifact.sha256 = %q, want %q", info.Artifact.Sha256, v.SHA256)
	}
	if !maps.Equal(info.Metadata.Dependencies, deps) {
		t.Errorf("metadata.dependencies = %v, want %v", info.Metadata.Dependencies, deps)
	}

	status = getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name/versions/9.9.9/", nil)
	if status != http.StatusNotFound {
		t.Errorf("unknown version status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestArtifactChecksumAndExtract asserts the artifact download's bytes hash
// to the sha256 AddVersion reported, and that a real extractor can unpack the
// generated tarball.
func TestArtifactChecksumAndExtract(t *testing.T) {
	t.Parallel()
	s := New(t)
	v := s.AddVersion("ns", "name", "1.0.0", map[string]string{"ns2.dep": "*"})

	resp := doGet(t, s.Client(), s.URL()+"/download/ns-name-1.0.0.tar.gz")
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read artifact body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != v.SHA256 {
		t.Errorf("artifact sha256 = %q, want %q", got, v.SHA256)
	}

	dir := t.TempDir()
	if err := archive.ExtractTarGzStream(context.Background(), bytes.NewReader(body), dir); err != nil {
		t.Fatalf("ExtractTarGzStream: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "MANIFEST.json")); err != nil {
		t.Errorf("MANIFEST.json missing after extraction: %v", err)
	}
}

// TestETagConditionalGet asserts a JSON endpoint's ETag round-trips through
// If-None-Match, yielding 304 with an empty body on a match and 200 with the
// body on a mismatch.
func TestETagConditionalGet(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	url := s.URL() + "/api/v3/collections/ns/name"

	first := doGet(t, s.Client(), url)
	etag := first.Header.Get("ETag")
	_ = first.Body.Close()
	if etag == "" {
		t.Fatal("first response carries no ETag")
	}

	status, body := getConditional(t, s.Client(), url, etag)
	if status != http.StatusNotModified {
		t.Errorf("matching If-None-Match status = %d, want %d", status, http.StatusNotModified)
	}
	if len(body) != 0 {
		t.Errorf("matching If-None-Match body = %d bytes, want empty", len(body))
	}

	status, body = getConditional(t, s.Client(), url, `"0000000000000000"`)
	if status != http.StatusOK {
		t.Errorf("mismatching If-None-Match status = %d, want %d", status, http.StatusOK)
	}
	if len(body) == 0 {
		t.Error("mismatching If-None-Match body is empty, want the full JSON body")
	}
}

// TestFaultStatusOnce asserts a Fault with Count 1 affects exactly the next
// matching request, then stops.
func TestFaultStatusOnce(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.Fail(EndpointRootMetadata, "ns", "name", Fault{Status: http.StatusTooManyRequests, Count: 1})

	url := s.URL() + "/api/v3/collections/ns/name"
	if status := getJSON(t, s.Client(), url, nil); status != http.StatusTooManyRequests {
		t.Errorf("first GET status = %d, want %d", status, http.StatusTooManyRequests)
	}
	if status := getJSON(t, s.Client(), url, nil); status != http.StatusOK {
		t.Errorf("second GET status = %d, want %d", status, http.StatusOK)
	}
}

// TestFaultStatusIndefinite asserts a non-positive Count keeps failing every
// matching request.
func TestFaultStatusIndefinite(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.Fail(EndpointRootMetadata, "ns", "name", Fault{Status: http.StatusInternalServerError, Count: -1})

	url := s.URL() + "/api/v3/collections/ns/name"
	for i := range 2 {
		if status := getJSON(t, s.Client(), url, nil); status != http.StatusInternalServerError {
			t.Errorf("GET #%d status = %d, want %d", i+1, status, http.StatusInternalServerError)
		}
	}
}

// TestFaultHangUnblocksOnContextCancellation asserts a Fault with Hang set
// blocks the handler until the request's context ends, and unblocks (rather
// than hanging forever) once it does.
func TestFaultHangUnblocksOnContextCancellation(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.Fail(EndpointRootMetadata, "ns", "name", Fault{Hang: true, Count: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL()+"/api/v3/collections/ns/name", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := s.Client().Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected the hung request to fail once its context ended, got nil error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected a context deadline exceeded error, got %v", err)
	}
}

// TestFaultStallAfterBytesDeliversRealPrefixThenBlocks pins that a stall
// serves exactly that many real bytes, blocks until the context ends, and
// spends its Count so the next request gets the full artifact.
func TestFaultStallAfterBytesDeliversRealPrefixThenBlocks(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.Fail(EndpointArtifact, "ns", "name", Fault{StallAfterBytes: 4, Count: 1})

	url := s.URL() + "/download/ns-name-1.0.0.tar.gz"

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The 4-byte prefix arrives on loopback in microseconds, well before the
	// 100ms deadline: this read must succeed.
	prefix := make([]byte, 4)
	if _, err := io.ReadFull(resp.Body, prefix); err != nil {
		t.Fatalf("read stalled prefix: %v", err)
	}

	// A further read must block until the context's deadline ends the
	// request, surfacing as a non-nil error rather than more data.
	if n, err := resp.Body.Read(make([]byte, 1)); err == nil {
		t.Fatalf("read past the stalled prefix: n=%d, err=nil, want a non-nil error", n)
	}

	// The fault's Count was consumed by the first request, so a second
	// request serves the full, unstalled artifact, whose first 4 bytes must
	// match the prefix already observed above.
	full := doGet(t, s.Client(), url)
	fullBody, err := io.ReadAll(full.Body)
	_ = full.Body.Close()
	if err != nil {
		t.Fatalf("read full artifact body: %v", err)
	}
	if full.StatusCode != http.StatusOK {
		t.Fatalf("second GET status = %d, want %d", full.StatusCode, http.StatusOK)
	}
	if !bytes.Equal(fullBody[:len(prefix)], prefix) {
		t.Errorf("fullBody[:4] = %x, want %x (the stalled prefix)", fullBody[:len(prefix)], prefix)
	}
}

// TestArtifactDripFaultKeepsWritingUntilTheContextEnds pins that an artifact
// drip writes more than once and ends only in a read error, while the same
// fixture unfaulted downloads in full with the sha256 AddVersion reported.
func TestArtifactDripFaultKeepsWritingUntilTheContextEnds(t *testing.T) {
	t.Parallel()
	s := New(t)
	v := s.AddVersion("ns", "name", "1.0.0", nil)
	s.Fail(EndpointArtifact, "ns", "name", Fault{DripInterval: 5 * time.Millisecond, Count: 1})

	url := s.URL() + "/download/ns-name-1.0.0.tar.gz"
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	n, err := io.ReadFull(resp.Body, make([]byte, 2))
	if err != nil {
		t.Fatalf("read first 2 dripped bytes: n=%d, err=%v, want 2 bytes and no error", n, err)
	}

	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("expected a read error once the context ended, got nil (the drip must never complete the body)")
	}

	// Positive control: the same fixture, no fault armed, must still serve a
	// complete artifact whose bytes hash to the sha256 AddVersion reported.
	full := doGet(t, s.Client(), url)
	body, err := io.ReadAll(full.Body)
	_ = full.Body.Close()
	if err != nil {
		t.Fatalf("read unstalled artifact body: %v", err)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != v.SHA256 {
		t.Errorf("unstalled artifact sha256 = %q, want %q", got, v.SHA256)
	}
}

// TestJSONEndpointDripFaultWritesBytesAndBlocksUntilCanceled pins that a
// drip on a JSON endpoint flushes its opening byte and never completes the
// body, so only the caller aborting it ends the request.
func TestJSONEndpointDripFaultWritesBytesAndBlocksUntilCanceled(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.Fail(EndpointRootMetadata, "ns", "name", Fault{DripInterval: 50 * time.Millisecond, Count: 1})

	url := s.URL() + "/api/v3/collections/ns/name"
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	opening := make([]byte, 1)
	if n, err := io.ReadFull(resp.Body, opening); err != nil || opening[0] != '{' {
		t.Fatalf("read opening byte: n=%d, err=%v, byte=%q, want 1 byte '{' and no error", n, err, opening)
	}

	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("expected a read error once the context ended, got nil (the drip must never complete the body)")
	}

	// Positive control: with the Count spent, the same request completes.
	var root types.GalaxyCollection
	status := getJSON(t, s.Client(), url, &root)
	if status != http.StatusOK {
		t.Fatalf("second request status = %d, want %d", status, http.StatusOK)
	}
	if root.HighestVersion.Version != "1.0.0" {
		t.Errorf("highest_version.version = %q, want %q", root.HighestVersion.Version, "1.0.0")
	}
}

// TestCounters asserts per-endpoint and total request counts, and ResetCounts
// zeroing them.
func TestCounters(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)

	getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name", nil)
	getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name/versions", nil)
	getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name/versions/1.0.0/", nil)
	getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name/versions/1.0.0/", nil)
	resp := doGet(t, s.Client(), s.URL()+"/download/ns-name-1.0.0.tar.gz")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if got := s.Count(EndpointRootMetadata); got != 1 {
		t.Errorf("Count(EndpointRootMetadata) = %d, want 1", got)
	}
	if got := s.Count(EndpointVersionsList); got != 1 {
		t.Errorf("Count(EndpointVersionsList) = %d, want 1", got)
	}
	if got := s.Count(EndpointVersionDetail); got != 2 {
		t.Errorf("Count(EndpointVersionDetail) = %d, want 2", got)
	}
	if got := s.Count(EndpointArtifact); got != 1 {
		t.Errorf("Count(EndpointArtifact) = %d, want 1", got)
	}
	if got := s.Total(); got != 5 {
		t.Errorf("Total() = %d, want 5", got)
	}

	s.ResetCounts()
	if got := s.Total(); got != 0 {
		t.Errorf("Total() after ResetCounts = %d, want 0", got)
	}
	for ep := EndpointRootMetadata; ep <= EndpointArtifact; ep++ {
		if got := s.Count(ep); got != 0 {
			t.Errorf("Count(%d) after ResetCounts = %d, want 0", ep, got)
		}
	}
}

// TestFailWildcardAndNamespaceMatching pins that an empty namespace/name in
// Fail matches any collection and a namespace-pinned rule no other.
func TestFailWildcardAndNamespaceMatching(t *testing.T) {
	t.Parallel()

	t.Run("wildcard fault fires for a collection never named in Fail", func(t *testing.T) {
		t.Parallel()
		s := New(t)
		s.Fail(EndpointRootMetadata, "", "", Fault{Status: http.StatusTooManyRequests, Count: 1})
		s.AddVersion("other", "thing", "1.0.0", nil)

		url := s.URL() + "/api/v3/collections/other/thing"
		if status := getJSON(t, s.Client(), url, nil); status != http.StatusTooManyRequests {
			t.Errorf("status = %d, want %d", status, http.StatusTooManyRequests)
		}
	})

	t.Run("namespace-pinned fault does not fire for a different namespace", func(t *testing.T) {
		t.Parallel()
		s := New(t)
		s.Fail(EndpointRootMetadata, "ns1", "x", Fault{Status: http.StatusTooManyRequests, Count: 1})
		s.AddVersion("ns2", "x", "1.0.0", nil)

		url := s.URL() + "/api/v3/collections/ns2/x"
		if status := getJSON(t, s.Client(), url, nil); status != http.StatusOK {
			t.Errorf("status = %d, want %d", status, http.StatusOK)
		}
	})
}

// unregistered404Case is one table entry for TestUnregistered404Cases.
type unregistered404Case struct {
	// setup registers what the case needs on a fresh server; nil means an
	// empty server, a scenario of its own.
	setup func(s *Server)
	name  string
	path  string
}

// unregistered404Cases lists one route per row that must 404 for an
// unregistered identifier; rows holding an unrelated collection prove the
// route looks its identifier up rather than serving what the server holds.
func unregistered404Cases() []unregistered404Case {
	return []unregistered404Case{
		{
			// Root metadata 404s for a namespace/name that was never
			// registered, even while a different collection is.
			name:  "root metadata",
			path:  "/api/v3/collections/ns/unknown",
			setup: func(s *Server) { s.AddVersion("ns", "name", "1.0.0", nil) },
		},
		{
			// The versions-list route 404s for a namespace/name that was
			// never registered.
			name: "versions list",
			path: "/api/v3/collections/ns/unknown/versions",
		},
		{
			// The download route 404s for a filename that was never
			// registered, even while a different artifact is.
			name:  "artifact",
			path:  "/download/does-not-exist.tar.gz",
			setup: func(s *Server) { s.AddVersion("ns", "name", "1.0.0", nil) },
		},
	}
}

// TestUnregistered404Cases pins a 404 on every route for an unregistered
// identifier, never a zero-valued body or another collection; each row
// builds its own server so no registration leaks between rows.
func TestUnregistered404Cases(t *testing.T) {
	t.Parallel()
	for _, tc := range unregistered404Cases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := New(t)
			if tc.setup != nil {
				tc.setup(s)
			}
			status := getJSON(t, s.Client(), s.URL()+tc.path, nil)
			if status != http.StatusNotFound {
				t.Errorf("status = %d, want %d", status, http.StatusNotFound)
			}
		})
	}
}

// TestFaultFiresOnEachEndpoint pins that each route matches faults by its own
// Endpoint: a one-shot fault trips the first request and not the second.
func TestFaultFiresOnEachEndpoint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		path       string
		endpoint   Endpoint
		wantStatus int
	}{
		{
			name:       "root metadata",
			path:       "/api/v3/collections/acme/name",
			endpoint:   EndpointRootMetadata,
			wantStatus: http.StatusOK,
		},
		{
			name:       "versions list",
			path:       "/api/v3/collections/acme/name/versions",
			endpoint:   EndpointVersionsList,
			wantStatus: http.StatusOK,
		},
		{
			name:       "version detail",
			path:       "/api/v3/collections/acme/name/versions/1.0.0/",
			endpoint:   EndpointVersionDetail,
			wantStatus: http.StatusOK,
		},
		{
			name:       "artifact",
			path:       "/download/acme-name-1.0.0.tar.gz",
			endpoint:   EndpointArtifact,
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := New(t)
			s.AddVersion("acme", "name", "1.0.0", nil)
			s.Fail(tc.endpoint, "acme", "name", Fault{Status: http.StatusTooManyRequests, Count: 1})

			url := s.URL() + tc.path
			if status := getJSON(t, s.Client(), url, nil); status != http.StatusTooManyRequests {
				t.Errorf("first request status = %d, want %d", status, http.StatusTooManyRequests)
			}
			if status := getJSON(t, s.Client(), url, nil); status != tc.wantStatus {
				t.Errorf("second request status = %d, want %d", status, tc.wantStatus)
			}
		})
	}
}

// TestFaultNoopFallsThroughToNormalResponse pins that a Fault with no action
// set is consumed but serves the normal response.
func TestFaultNoopFallsThroughToNormalResponse(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("acme", "name", "1.0.0", nil)
	s.Fail(EndpointRootMetadata, "acme", "name", Fault{Count: 1})

	var root types.GalaxyCollection
	status := getJSON(t, s.Client(), s.URL()+"/api/v3/collections/acme/name", &root)
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
	if root.HighestVersion.Version != "1.0.0" {
		t.Errorf("highest_version.version = %q, want %q", root.HighestVersion.Version, "1.0.0")
	}
}

// TestCompareDottedVersionsAndComponents pins numeric components compared by
// magnitude, a lexical fallback for non-numeric ones, and "1.2" sorting
// before "1.2.0".
func TestCompareDottedVersionsAndComponents(t *testing.T) {
	t.Parallel()

	if got := compareDottedComponent("2", "10"); got != -1 {
		t.Errorf(`compareDottedComponent("2", "10") = %d, want -1 (numeric, not lexical)`, got)
	}
	if got := compareDottedComponent("beta", "alpha"); got <= 0 {
		t.Errorf(`compareDottedComponent("beta", "alpha") = %d, want > 0 (lexical fallback)`, got)
	}
	if got := compareDottedVersions("1.2", "1.2.0"); got >= 0 {
		t.Errorf(`compareDottedVersions("1.2", "1.2.0") = %d, want < 0 (shorter arity sorts first)`, got)
	}
}

// TestPaginateClamps pins paginate's clamps: negative offset, offset past the
// end, negative limit, and an offset+limit that overflows int, which an
// unbounded ?limit= from parseQueryInt can produce.
func TestPaginateClamps(t *testing.T) {
	t.Parallel()
	versions := []string{"1.0.0", "1.1.0", "2.0.0"}

	cases := []struct {
		name   string
		want   []string
		offset int
		limit  int
	}{
		{name: "negative offset clamped to zero", want: []string{"1.0.0", "1.1.0"}, offset: -5, limit: 2},
		{name: "offset past end yields empty slice", want: []string{}, offset: 10, limit: 2},
		{name: "negative limit returns through end of slice", want: []string{"1.1.0", "2.0.0"}, offset: 1, limit: -1},
		{name: "overflowing sum wraps below offset", want: []string{}, offset: 1, limit: math.MaxInt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := paginate(versions, tc.offset, tc.limit)
			if got == nil {
				t.Fatal("paginate returned a nil slice, want non-nil")
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("paginate(versions, %d, %d) = %v, want %v", tc.offset, tc.limit, got, tc.want)
			}
		})
	}
}

// TestAuthAnonymousByDefault pins that without RequireAuth no header is
// required, and one sent anyway is captured without affecting the response.
func TestAuthAnonymousByDefault(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)

	url := s.URL() + "/api/v3/collections/ns/name"
	if status := getJSON(t, s.Client(), url, nil); status != http.StatusOK {
		t.Errorf("no Authorization header: status = %d, want %d", status, http.StatusOK)
	}

	resp := doGetWithAuth(t, s.Client(), url, "Token whatever")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Authorization header present but unenforced: status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	value, ok := s.SeenAuth(EndpointRootMetadata)
	if !ok || value != "Token whatever" {
		t.Errorf("SeenAuth = (%q, %v), want (%q, true)", value, ok, "Token whatever")
	}
}

// TestAuthMissingOrWrongHeaderRejected pins that RequireAuth answers 401 to
// a missing Authorization header and to one not matching byte for byte.
func TestAuthMissingOrWrongHeaderRejected(t *testing.T) {
	t.Parallel()

	t.Run("missing header", func(t *testing.T) {
		t.Parallel()
		s := New(t)
		s.RequireAuth("Token s3cr3t")
		s.AddVersion("ns", "name", "1.0.0", nil)

		url := s.URL() + "/api/v3/collections/ns/name"
		if status := getJSON(t, s.Client(), url, nil); status != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
		}

		value, ok := s.SeenAuth(EndpointRootMetadata)
		if ok || value != "" {
			t.Errorf("SeenAuth = (%q, %v), want (%q, false)", value, ok, "")
		}
	})

	t.Run("wrong header", func(t *testing.T) {
		t.Parallel()
		s := New(t)
		s.RequireAuth("Token s3cr3t")
		s.AddVersion("ns", "name", "1.0.0", nil)

		url := s.URL() + "/api/v3/collections/ns/name"
		resp := doGetWithAuth(t, s.Client(), url, "Token wrong")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
		}

		value, ok := s.SeenAuth(EndpointRootMetadata)
		if !ok || value != "Token wrong" {
			t.Errorf("SeenAuth = (%q, %v), want (%q, true)", value, ok, "Token wrong")
		}
	})
}

// TestAuthCorrectHeaderSucceeds asserts a request carrying exactly the
// expected Authorization header value is served normally.
func TestAuthCorrectHeaderSucceeds(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.RequireAuth("Token s3cr3t")
	s.AddVersion("ns", "name", "1.0.0", nil)

	url := s.URL() + "/api/v3/collections/ns/name"
	resp := doGetWithAuth(t, s.Client(), url, "Token s3cr3t")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestAuthFailStatusOverridesTo403 asserts AuthFailStatus lets a caller
// exercise the 403 Forbidden branch instead of the default 401.
func TestAuthFailStatusOverridesTo403(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.RequireAuth("Token s3cr3t")
	s.AuthFailStatus(http.StatusForbidden)
	s.AddVersion("ns", "name", "1.0.0", nil)

	url := s.URL() + "/api/v3/collections/ns/name"
	if status := getJSON(t, s.Client(), url, nil); status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestAuthEmptyHeaderDistinctFromAbsent asserts SeenAuth distinguishes a
// request that carried an empty Authorization header from one that carried
// none at all.
func TestAuthEmptyHeaderDistinctFromAbsent(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	url := s.URL() + "/api/v3/collections/ns/name"

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "")
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()

	value, ok := s.SeenAuth(EndpointRootMetadata)
	if !ok || value != "" {
		t.Errorf("empty header: SeenAuth = (%q, %v), want (%q, true)", value, ok, "")
	}

	// A brand new endpoint that has never received a request reports absent.
	value, ok = s.SeenAuth(EndpointArtifact)
	if ok || value != "" {
		t.Errorf("never requested endpoint: SeenAuth = (%q, %v), want (%q, false)", value, ok, "")
	}
}

// TestAuthRejectedRequestStillCounted asserts an endpoint's request counter
// still counts a request that RequireAuth rejected.
func TestAuthRejectedRequestStillCounted(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.RequireAuth("Token s3cr3t")
	s.AddVersion("ns", "name", "1.0.0", nil)

	url := s.URL() + "/api/v3/collections/ns/name"
	if status := getJSON(t, s.Client(), url, nil); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", status, http.StatusUnauthorized)
	}
	if got := s.Count(EndpointRootMetadata); got != 1 {
		t.Errorf("Count(EndpointRootMetadata) = %d, want 1", got)
	}
}

// TestAuthWinsOverArmedFault pins that an armed matching Fault never masks
// an auth failure: the request gets the auth status, not the fault's.
func TestAuthWinsOverArmedFault(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.RequireAuth("Token s3cr3t")
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.Fail(EndpointRootMetadata, "ns", "name", Fault{Status: http.StatusTooManyRequests, Count: -1})

	url := s.URL() + "/api/v3/collections/ns/name"
	if status := getJSON(t, s.Client(), url, nil); status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (auth must win over the armed fault)", status, http.StatusUnauthorized)
	}
}

// TestAuthServersAreIndependent pins that two servers with different tokens
// each enforce, count and capture only their own requests.
func TestAuthServersAreIndependent(t *testing.T) {
	t.Parallel()
	s1 := New(t)
	s1.RequireAuth("Token one")
	s1.AddVersion("ns", "name", "1.0.0", nil)

	s2 := New(t)
	s2.RequireAuth("Token two")
	s2.AddVersion("ns", "name", "1.0.0", nil)

	url1 := s1.URL() + "/api/v3/collections/ns/name"
	url2 := s2.URL() + "/api/v3/collections/ns/name"

	resp := doGetWithAuth(t, s1.Client(), url1, "Token one")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("s1 with its own token: status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// s2's own token, presented to s1, must be rejected: each server only
	// accepts the token it was configured with.
	resp = doGetWithAuth(t, s1.Client(), url1, "Token two")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("s1 with s2's token: status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	assertServersIndependentAfterS1Traffic(t, s1, s2)

	// s2 still enforces its own auth requirement on an anonymous request,
	// independently of everything done against s1 above.
	status := getJSON(t, s2.Client(), url2, nil)
	if status != http.StatusUnauthorized {
		t.Errorf("s2 anonymous GET status = %d, want %d", status, http.StatusUnauthorized)
	}
}

// assertServersIndependentAfterS1Traffic asserts s1 counted both requests
// while s2's counter and captured Authorization stayed untouched.
func assertServersIndependentAfterS1Traffic(t *testing.T, s1, s2 *Server) {
	t.Helper()
	if got := s1.Count(EndpointRootMetadata); got != 2 {
		t.Errorf("s1 Count(EndpointRootMetadata) = %d, want 2", got)
	}
	if got := s2.Count(EndpointRootMetadata); got != 0 {
		t.Errorf("s2 Count(EndpointRootMetadata) = %d, want 0", got)
	}
	if value, ok := s2.SeenAuth(EndpointRootMetadata); ok || value != "" {
		t.Errorf("s2 SeenAuth = (%q, %v), want (%q, false)", value, ok, "")
	}
}

// TestBasePathRouting pins NewAtBasePath's hub shape: routes and generated
// URLs under "<prefix>/v3", and a 404 for both the unprefixed path and
// "<prefix>/api/v3", so API-root probing must fall through to "<base>/v3".
func TestBasePathRouting(t *testing.T) {
	t.Parallel()
	s := NewAtBasePath(t, "/api/automation-hub")
	v := s.AddVersion("ns", "name", "1.0.0", map[string]string{"ns2.dep": "*"})
	prefix := s.URL() + "/api/automation-hub"

	assertBasePathRootMetadata(t, s, prefix)
	assertBasePathVersionsList(t, s, prefix)
	assertBasePathVersionDetail(t, s, prefix, v)
	assertBasePathArtifact(t, s, prefix, v)

	// The unprefixed path must 404: this server only answers under its
	// configured base path.
	status := getJSON(t, s.Client(), s.URL()+"/v3/collections/ns/name", nil)
	if status != http.StatusNotFound {
		t.Errorf("unprefixed path status = %d, want %d", status, http.StatusNotFound)
	}

	// The galaxy.ansible.com shaped path must 404 even under the correct
	// prefix: a hub does not mount its v3 API under a further "/api/v3".
	status = getJSON(t, s.Client(), prefix+"/api/v3/collections/ns/name", nil)
	if status != http.StatusNotFound {
		t.Errorf("galaxy-shaped path status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestNewServesGalaxyShapeNotHubShape mirrors TestBasePathRouting: New
// serves collections under "/api/v3" and 404s the hub-shaped "/v3".
func TestNewServesGalaxyShapeNotHubShape(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)

	var root types.GalaxyCollection
	if status := getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name", &root); status != http.StatusOK {
		t.Fatalf("galaxy-shaped root metadata status = %d, want %d", status, http.StatusOK)
	}
	if want := s.URL() + "/api/v3/collections/ns/name/versions/"; root.VersionsURL != want {
		t.Errorf("versions_url = %q, want %q", root.VersionsURL, want)
	}
	if status := getJSON(t, s.Client(), s.URL()+"/v3/collections/ns/name", nil); status != http.StatusNotFound {
		t.Errorf("hub-shaped path status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestNewAtBasePathEmptyPrefixServesHubShapeAtRoot pins that the constructor,
// not the prefix, fixes the shape: an empty base path serves "/v3" and still
// 404s "/api/v3".
func TestNewAtBasePathEmptyPrefixServesHubShapeAtRoot(t *testing.T) {
	t.Parallel()
	s := NewAtBasePath(t, "")
	s.AddVersion("ns", "name", "1.0.0", nil)

	var root types.GalaxyCollection
	if status := getJSON(t, s.Client(), s.URL()+"/v3/collections/ns/name", &root); status != http.StatusOK {
		t.Fatalf("hub-shaped root metadata status = %d, want %d", status, http.StatusOK)
	}
	if want := s.URL() + "/v3/collections/ns/name/versions/"; root.VersionsURL != want {
		t.Errorf("versions_url = %q, want %q", root.VersionsURL, want)
	}
	if status := getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name", nil); status != http.StatusNotFound {
		t.Errorf("galaxy-shaped path status = %d, want %d", status, http.StatusNotFound)
	}
}

// assertBasePathRootMetadata asserts the root metadata route resolves
// under prefix and its embedded versions_url/highest_version.href URLs
// carry the same prefix.
func assertBasePathRootMetadata(t *testing.T, s *Server, prefix string) {
	t.Helper()
	var root types.GalaxyCollection
	status := getJSON(t, s.Client(), prefix+"/v3/collections/ns/name", &root)
	if status != http.StatusOK {
		t.Fatalf("root metadata status = %d, want %d", status, http.StatusOK)
	}
	if want := prefix + "/v3/collections/ns/name/versions/"; root.VersionsURL != want {
		t.Errorf("versions_url = %q, want %q", root.VersionsURL, want)
	}
	if want := prefix + "/v3/collections/ns/name/versions/1.0.0/"; root.HighestVersion.Href != want {
		t.Errorf("highest_version.href = %q, want %q", root.HighestVersion.Href, want)
	}
}

// assertBasePathVersionsList asserts the versions-list route resolves
// under prefix and its one entry's href carries the same prefix.
func assertBasePathVersionsList(t *testing.T, s *Server, prefix string) {
	t.Helper()
	var versions types.GalaxyCollectionVersions
	status := getJSON(t, s.Client(), prefix+"/v3/collections/ns/name/versions", &versions)
	if status != http.StatusOK {
		t.Fatalf("versions list status = %d, want %d", status, http.StatusOK)
	}
	if len(versions.Data) != 1 {
		t.Fatalf("len(versions.Data) = %d, want 1", len(versions.Data))
	}
	if want := prefix + "/v3/collections/ns/name/versions/1.0.0/"; versions.Data[0].Href != want {
		t.Errorf("versions.Data[0].href = %q, want %q", versions.Data[0].Href, want)
	}
}

// assertBasePathVersionDetail asserts the version-detail route resolves
// under prefix and its download_url carries the same prefix.
func assertBasePathVersionDetail(t *testing.T, s *Server, prefix string, v Version) {
	t.Helper()
	var info types.GalaxyCollectionVersionInfo
	status := getJSON(t, s.Client(), prefix+"/v3/collections/ns/name/versions/1.0.0/", &info)
	if status != http.StatusOK {
		t.Fatalf("version detail status = %d, want %d", status, http.StatusOK)
	}
	if want := prefix + "/download/ns-name-1.0.0.tar.gz"; info.DownloadURL != want {
		t.Errorf("download_url = %q, want %q", info.DownloadURL, want)
	}
	if info.Artifact.Sha256 != v.SHA256 {
		t.Errorf("artifact.sha256 = %q, want %q", info.Artifact.Sha256, v.SHA256)
	}
}

// assertBasePathArtifact asserts the download route resolves under prefix
// and serves bytes hashing to v's recorded sha256.
func assertBasePathArtifact(t *testing.T, s *Server, prefix string, v Version) {
	t.Helper()
	resp := doGet(t, s.Client(), prefix+"/download/ns-name-1.0.0.tar.gz")
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read artifact body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("artifact status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != v.SHA256 {
		t.Errorf("artifact sha256 = %q, want %q", got, v.SHA256)
	}
}

// TestParseDigitsAndQueryInt pins parseDigits refusing empty and partly
// numeric input, and parseQueryInt falling back to its default when the
// parameter is malformed or absent.
func TestParseDigitsAndQueryInt(t *testing.T) {
	t.Parallel()

	digitCases := []struct {
		name   string
		input  string
		wantN  int
		wantOK bool
	}{
		{name: "empty string", input: "", wantN: 0, wantOK: false},
		{name: "trailing non-digit", input: "12a", wantN: 0, wantOK: false},
		{name: "plain digits", input: "42", wantN: 42, wantOK: true},
	}
	for _, tc := range digitCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, ok := parseDigits(tc.input)
			if n != tc.wantN || ok != tc.wantOK {
				t.Errorf("parseDigits(%q) = (%d, %v), want (%d, %v)", tc.input, n, ok, tc.wantN, tc.wantOK)
			}
		})
	}

	t.Run("malformed query value falls back to default", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/?limit=abc", nil)
		if got := parseQueryInt(req, "limit", 5); got != 5 {
			t.Errorf("parseQueryInt = %d, want 5", got)
		}
	})
	t.Run("absent query value falls back to default", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		if got := parseQueryInt(req, "limit", 7); got != 7 {
			t.Errorf("parseQueryInt = %d, want 7", got)
		}
	})
}

// TestManifestJSONMatchesArtifactAndVerifiesChain pins that ManifestJSON
// returns the served artifact's exact MANIFEST.json, so signing it signs that
// artifact, and that the artifact's digest chain verifies end to end.
func TestManifestJSONMatchesArtifactAndVerifiesChain(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)

	resp := doGet(t, s.Client(), s.URL()+"/download/ns-name-1.0.0.tar.gz")
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read artifact body: %v", err)
	}

	tarPath := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(tarPath, body, 0o600); err != nil {
		t.Fatalf("write artifact to disk: %v", err)
	}

	fromArchive, err := manifest.ReadFromTarGz(t.Context(), tarPath)
	if err != nil {
		t.Fatalf("manifest.ReadFromTarGz() error = %v, want nil", err)
	}
	fromServer := s.ManifestJSON("ns", "name", "1.0.0")
	if !bytes.Equal(fromArchive, fromServer) {
		t.Fatalf("ManifestJSON() = %s, want the archive's own MANIFEST.json bytes %s", fromServer, fromArchive)
	}

	if err := manifest.VerifyChain(context.Background(), tarPath, fromServer); err != nil {
		t.Fatalf("manifest.VerifyChain() = %v, want nil", err)
	}

	if got := s.ManifestJSON("ns", "unknown", "1.0.0"); got != nil {
		t.Errorf("ManifestJSON() for an unregistered collection = %v, want nil", got)
	}
	if got := s.ManifestJSON("ns", "name", "9.9.9"); got != nil {
		t.Errorf("ManifestJSON() for an unregistered version = %v, want nil", got)
	}
}

// TestSignVersionAddsSignatureToVersionDetail pins the signed blob at
// signatures[0].signature, the shape serverSignatureBlobs reads, while an
// unsigned sibling version's detail carries no signatures at all.
func TestSignVersionAddsSignatureToVersionDetail(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.AddVersion("ns", "name", "2.0.0", nil)

	blob := []byte("-----BEGIN PGP SIGNATURE-----\nfixture\n-----END PGP SIGNATURE-----")
	s.SignVersion(t, "ns", "name", "1.0.0", blob)

	var signed types.GalaxyCollectionVersionInfo
	status := getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name/versions/1.0.0/", &signed)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	list, ok := signed.Signatures.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("signed version Signatures = %#v, want a one-element list", signed.Signatures)
	}
	row, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("signed version Signatures[0] = %#v, want a JSON object", list[0])
	}
	if got, _ := row["signature"].(string); got != string(blob) {
		t.Errorf("Signatures[0][%q] = %q, want %q", "signature", got, blob)
	}

	var unsigned types.GalaxyCollectionVersionInfo
	status = getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name/versions/2.0.0/", &unsigned)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if unsigned.Signatures != nil {
		t.Errorf("unsigned version Signatures = %#v, want nil (SignVersion was never called for it)", unsigned.Signatures)
	}
}

// stubTB is a testing.TB double overriding Helper and Fatalf, whose Fatalf
// records the message and calls runtime.Goexit as testing.T does, so it is
// driven on its own goroutine and SignVersion's deferred unlock still runs.
type stubTB struct {
	testing.TB

	fatalMsg string
}

func (s *stubTB) Helper() {}

func (s *stubTB) Fatalf(format string, args ...any) {
	s.fatalMsg = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// callSignVersion runs SignVersion against stub on its own goroutine and
// waits for it to end, so stub.Fatalf's own runtime.Goexit terminates only
// that goroutine rather than the test's.
func callSignVersion(s *Server, stub *stubTB, namespace, name, version string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.SignVersion(stub, namespace, name, version, []byte("fixture"))
	}()
	<-done
}

// TestSignVersionRefusesAnUnregisteredCollection pins that an unregistered
// namespace/name is reported through tb.Fatalf naming it, never reached as a
// nil *fakeCollection dereference.
func TestSignVersionRefusesAnUnregisteredCollection(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("acme", "app", "1.0.0", nil)

	stub := &stubTB{}
	callSignVersion(s, stub, "ghost", "collection", "1.0.0")

	if !strings.Contains(stub.fatalMsg, "ghost.collection") ||
		!strings.Contains(stub.fatalMsg, "was never registered with AddVersion") {
		t.Fatalf("Fatalf message = %q, want it to report ghost.collection as never registered", stub.fatalMsg)
	}
}

// TestSignVersionRefusesAnUnregisteredVersion pins that an unregistered
// version of a registered collection is reported through tb.Fatalf naming
// ns.name@version, never reached as a nil *fakeVersionEntry dereference.
func TestSignVersionRefusesAnUnregisteredVersion(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("acme", "app", "1.0.0", nil)

	stub := &stubTB{}
	callSignVersion(s, stub, "acme", "app", "9.9.9")

	if !strings.Contains(stub.fatalMsg, "acme.app@9.9.9") ||
		!strings.Contains(stub.fatalMsg, "was never registered with AddVersion") {
		t.Fatalf("Fatalf message = %q, want it to report acme.app@9.9.9 as never registered", stub.fatalMsg)
	}
}
