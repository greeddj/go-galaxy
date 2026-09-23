package fetch

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestURLAuthTransport_MatchByOriginAndLongestPrefix pins that the Bearer
// header attaches only on an exact origin with the path at or beneath a prefix
// on a "/" boundary, the longest matching prefix winning.
func TestURLAuthTransport_MatchByOriginAndLongestPrefix(t *testing.T) {
	t.Parallel()
	bindings := []URLBinding{
		{Origin: "https://h.example:443", PathPrefix: "", Token: "origin-wide"},
		{Origin: "https://h.example:443", PathPrefix: "/org", Token: "org-scoped"},
		{Origin: "https://h.example:443", PathPrefix: "/org/deep", Token: "deep-scoped"},
		{Origin: "https://other.example:443", PathPrefix: "", Token: "other"},
	}
	cases := []struct {
		name    string
		url     string
		wantHdr string
	}{
		{"origin-wide fallback", "https://h.example/x.tar.gz", "Bearer origin-wide"},
		{"prefix match", "https://h.example/org/x.tar.gz", "Bearer org-scoped"},
		{"prefix equals path", "https://h.example/org", "Bearer org-scoped"},
		{"longest prefix wins", "https://h.example/org/deep/x.tar.gz", "Bearer deep-scoped"},
		{"boundary rule: organ is not org", "https://h.example/organ/x.tar.gz", "Bearer origin-wide"},
		{"explicit default port", "https://h.example:443/org/x.tar.gz", "Bearer org-scoped"},
		{"different port", "https://h.example:8443/org/x.tar.gz", ""},
		{"different scheme", "http://h.example/org/x.tar.gz", ""},
		{"unbound origin", "https://cdn.example/x.tar.gz", ""},
		{"dot segment attaches nothing", "https://h.example/org/%2e%2e/secret/x.tar.gz", ""},
		{"empty segment attaches nothing", "https://h.example/org//x.tar.gz", ""},
		{"caching-proxy path attaches origin-wide", "https://h.example/https://up.example/x.tar.gz", "Bearer origin-wide"},
		{"caching-proxy path under prefix", "https://h.example/org/https://up.example/x.tar.gz", "Bearer org-scoped"},
		{"dot segment inside embedded url attaches nothing", "https://h.example/https://up.example/%2e%2e/x.tar.gz", ""},
		{"empty segment after embedded url attaches nothing", "https://h.example/https://up.example//x.tar.gz", ""},
		{"uppercase scheme separator attaches nothing", "https://h.example/HTTPS://up.example/x.tar.gz", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := &stubTransport{}
			transport := urlAuthTransport{base: base, bindings: bindings}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.url, nil)
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			_ = resp.Body.Close()
			if got := base.reqs[0].Header.Get("Authorization"); got != tc.wantHdr {
				t.Fatalf("Authorization = %q, want %q", got, tc.wantHdr)
			}
			if req.Header.Get("Authorization") != "" {
				t.Fatalf("caller request was mutated")
			}
		})
	}
}

// TestURLAuthTransport_DoesNotOverwriteExistingAuthorization pins the
// caller-precedence rule authTransport also holds.
func TestURLAuthTransport_DoesNotOverwriteExistingAuthorization(t *testing.T) {
	t.Parallel()
	base := &stubTransport{}
	transport := urlAuthTransport{base: base, bindings: []URLBinding{{Origin: "https://h.example:443", Token: "tok"}}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://h.example/x.tar.gz", nil)
	req.Header.Set("Authorization", "Bearer caller-owned")
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	if got := base.reqs[0].Header.Get("Authorization"); got != "Bearer caller-owned" {
		t.Fatalf("Authorization = %q, want the caller's own", got)
	}
}

// TestURLAuthTransport_StringLeaksNoToken pins the leak-proof rendering.
func TestURLAuthTransport_StringLeaksNoToken(t *testing.T) {
	t.Parallel()
	transport := urlAuthTransport{bindings: []URLBinding{{Origin: "https://h.example:443", Token: "secret-token"}}}
	rendered := transport.String()
	if strings.Contains(rendered, "secret-token") || strings.Contains(rendered, "h.example") {
		t.Fatalf("String() leaks binding material: %s", rendered)
	}
	if rendered != "fetch.urlAuthTransport{bindings: 1}" {
		t.Fatalf("String() = %q", rendered)
	}
}

// TestNewURLDownload_CrossOriginRedirect pins the GitHub release-asset shape:
// the bound origin sees the Bearer header, the unbound redirect target sees no
// Authorization and no Referer, and the download succeeds.
func TestNewURLDownload_CrossOriginRedirect(t *testing.T) {
	t.Parallel()
	var targetAuth, targetReferer atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present := r.Header["Authorization"]
		targetAuth.Store(present)
		targetReferer.Store(r.Header.Get("Referer"))
		_, _ = w.Write([]byte("artifact-bytes"))
	}))
	defer target.Close()

	var originAuth atomic.Value
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originAuth.Store(r.Header.Get("Authorization"))
		http.Redirect(w, r, target.URL+"/asset?X-Sig=presigned", http.StatusFound)
	}))
	defer origin.Close()

	client := NewURLDownload(30*time.Second, false, []URLBinding{{Origin: originOf(t, origin.URL), Token: "tok"}})
	resp, err := doGet(t, client, origin.URL+"/releases/x.tar.gz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != "artifact-bytes" {
		t.Fatalf("body = %q (%v)", body, err)
	}
	if got := originAuth.Load(); got != "Bearer tok" {
		t.Fatalf("bound origin saw Authorization %q, want the Bearer token", got)
	}
	if present := targetAuth.Load(); present != false {
		t.Fatalf("redirect target saw an Authorization header")
	}
	if referer := targetReferer.Load(); referer != "" {
		t.Fatalf("redirect target saw a Referer: %q", referer)
	}
}

// TestNewURLDownload_RedirectIntoBoundOriginGainsToken pins the other side of
// the per-hop rule: a chain that hops INTO a bound origin attaches that
// binding's token on the hop.
func TestNewURLDownload_RedirectIntoBoundOriginGainsToken(t *testing.T) {
	t.Parallel()
	var boundAuth atomic.Value
	bound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		boundAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("ok"))
	}))
	defer bound.Close()
	unbound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, bound.URL+"/asset", http.StatusFound)
	}))
	defer unbound.Close()

	client := NewURLDownload(30*time.Second, false, []URLBinding{{Origin: originOf(t, bound.URL), Token: "tok"}})
	resp, err := doGet(t, client, unbound.URL+"/x.tar.gz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = resp.Body.Close()
	if got := boundAuth.Load(); got != "Bearer tok" {
		t.Fatalf("bound origin saw Authorization %q on the hop, want the Bearer token", got)
	}
}

// TestNewURLDownload_RefusesHTTPSDowngrade pins the downgrade refusal: a
// chain that started on https may never drop to plaintext http, and the http
// listener never sees the request.
func TestNewURLDownload_RefusesHTTPSDowngrade(t *testing.T) {
	t.Parallel()
	var plainHits atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		plainHits.Add(1)
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/x.tar.gz", http.StatusFound)
	}))
	defer secure.Close()

	client := NewURLDownload(30*time.Second, false, nil)
	client.Transport = secure.Client().Transport // trust the test server's certificate; CheckRedirect is the subject
	resp, err := doGet(t, client, secure.URL+"/x.tar.gz")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("Get followed an https to http downgrade")
	}
	if !errors.Is(err, helpers.ErrDownloadFailed) {
		t.Fatalf("Get error = %v, want errors.Is ErrDownloadFailed", err)
	}
	if plainHits.Load() != 0 {
		t.Fatalf("the plaintext listener saw %d requests, want 0", plainHits.Load())
	}
}

// TestNewURLDownload_StopsAtRedirectCeiling pins that the hop ceiling
// checkRedirect re-imposes still holds on this client.
func TestNewURLDownload_StopsAtRedirectCeiling(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, server.URL+"/again", http.StatusFound)
	}))
	defer server.Close()

	client := NewURLDownload(30*time.Second, false, nil)
	resp, err := doGet(t, client, server.URL+"/x.tar.gz")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("Get survived an endless redirect chain")
	}
	if !strings.Contains(err.Error(), "stopped after") {
		t.Fatalf("Get error = %v, want the redirect-ceiling refusal", err)
	}
	if hits.Load() > int64(helpers.FetchMaxRedirects)+1 {
		t.Fatalf("server saw %d requests, want at most the ceiling plus the first", hits.Load())
	}
}

// TestNewURLDownload_OfflineRefusesEveryRequest pins the offline arm.
func TestNewURLDownload_OfflineRefusesEveryRequest(t *testing.T) {
	t.Parallel()
	client := NewURLDownload(30*time.Second, true, []URLBinding{{Origin: "https://h.example:443", Token: "tok"}})
	resp, err := doGet(t, client, "https://h.example/x.tar.gz")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("Get succeeded offline")
	}
	if !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("Get error = %v, want errors.Is ErrOfflineMode", err)
	}
}

// doGet issues one GET with the test's context, standing in for the plain
// client.Get the linter refuses.
func doGet(t *testing.T, client *http.Client, rawURL string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	return client.Do(req)
}

// originOf normalizes a test server's URL to the helpers.Origin form a
// binding carries.
func originOf(t *testing.T, raw string) string {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, raw+"/", nil)
	return helpers.Origin(req.URL)
}
