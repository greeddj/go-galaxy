package fetch

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// stubTransport is a minimal http.RoundTripper double that never touches the
// network: it records every request it receives (letting a test inspect
// exactly what a wrapping transport did to it) and returns a fixed 200 OK.
type stubTransport struct {
	reqs []*http.Request
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.reqs = append(s.reqs, req)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
}

// TestAuthTransport_RoundTrip_OriginExactMatch pins that the token attaches
// only on an exact normalized origin match: another host, another port or the
// http variant of the same host is a different endpoint and gets nothing.
func TestAuthTransport_RoundTrip_OriginExactMatch(t *testing.T) {
	t.Parallel()

	tokens := map[string]string{"https://galaxy.example.com:443": "secret-a"}

	cases := []struct {
		name    string
		url     string
		wantHdr string
	}{
		{"configured origin, implicit default port", "https://galaxy.example.com/api/v3/", "Token secret-a"},
		{"configured origin, explicit default port", "https://galaxy.example.com:443/api/v3/", "Token secret-a"},
		{"different host", "https://other.example.com/api/v3/", ""},
		{"different port", "https://galaxy.example.com:8443/api/v3/", ""},
		{"http instead of https, same host", "http://galaxy.example.com/api/v3/", ""},
		{"off-server download host", "https://cdn.example.org/artifact.tar.gz", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			base := &stubTransport{}
			at := authTransport{base: base, tokens: tokens}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.url, nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext error = %v, want nil", err)
			}
			resp, err := at.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip error = %v, want nil", err)
			}
			_ = resp.Body.Close()

			if len(base.reqs) != 1 {
				t.Fatalf("base received %d requests, want 1", len(base.reqs))
			}
			if got := base.reqs[0].Header.Get("Authorization"); got != tc.wantHdr {
				t.Fatalf("Authorization header = %q, want %q", got, tc.wantHdr)
			}
		})
	}
}

// TestAuthTransport_RoundTrip_DoesNotOverwriteExistingAuthorization checks
// that a caller-supplied Authorization header always wins, regardless of
// whether the request's origin also has a configured token.
func TestAuthTransport_RoundTrip_DoesNotOverwriteExistingAuthorization(t *testing.T) {
	t.Parallel()

	base := &stubTransport{}
	at := authTransport{base: base, tokens: map[string]string{"https://galaxy.example.com:443": "secret-a"}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://galaxy.example.com/api/v3/", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext error = %v, want nil", err)
	}
	req.Header.Set("Authorization", "Bearer caller-supplied")

	resp, err := at.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip error = %v, want nil", err)
	}
	_ = resp.Body.Close()
	if len(base.reqs) != 1 {
		t.Fatalf("base received %d requests, want 1", len(base.reqs))
	}
	if got := base.reqs[0].Header.Get("Authorization"); got != "Bearer caller-supplied" {
		t.Fatalf("Authorization header = %q, want the caller's own %q untouched", got, "Bearer caller-supplied")
	}
}

// TestAuthTransport_RoundTrip_DoesNotMutateCallerRequest pins that RoundTrip
// clones rather than mutates the caller's request, per http.RoundTripper's
// contract: http.Client may reuse it across a redirect.
func TestAuthTransport_RoundTrip_DoesNotMutateCallerRequest(t *testing.T) {
	t.Parallel()

	base := &stubTransport{}
	at := authTransport{base: base, tokens: map[string]string{"https://galaxy.example.com:443": "secret-a"}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://galaxy.example.com/api/v3/", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext error = %v, want nil", err)
	}

	resp, err := at.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip error = %v, want nil", err)
	}
	_ = resp.Body.Close()

	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("caller's original request Authorization = %q, want empty: RoundTrip must not mutate its input", got)
	}
	if len(base.reqs) != 1 {
		t.Fatalf("base received %d requests, want 1", len(base.reqs))
	}
	if base.reqs[0] == req {
		t.Fatal("base received the exact same *http.Request pointer the caller passed in, want a clone")
	}
}

// TestAuthTransport_RoundTrip_MultipleServersDistinctTokens proves the
// origin -> token map dispatches each configured server's own token, never
// mixing them up.
func TestAuthTransport_RoundTrip_MultipleServersDistinctTokens(t *testing.T) {
	t.Parallel()

	tokens := map[string]string{
		"https://a.example.com:443": "token-a",
		"https://b.example.com:443": "token-b",
	}
	base := &stubTransport{}
	at := authTransport{base: base, tokens: tokens}

	for _, host := range []string{"a.example.com", "b.example.com"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+host+"/api/v3/", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext error = %v, want nil", err)
		}
		resp, err := at.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip error = %v, want nil", err)
		}
		_ = resp.Body.Close()
	}

	want := []string{"Token token-a", "Token token-b"}
	for i, w := range want {
		if got := base.reqs[i].Header.Get("Authorization"); got != w {
			t.Fatalf("request %d Authorization = %q, want %q", i, got, w)
		}
	}
}

// TestAuthTransport_RoundTrip_CrossOriginRedirectDropsToken pins, through a
// real http.Client redirect, that the origin match is re-decided on every hop,
// so the token reaches the configured origin and not the redirect target.
func TestAuthTransport_RoundTrip_CrossOriginRedirectDropsToken(t *testing.T) {
	t.Parallel()

	var bHeader string
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer serverB.Close()

	var aHeader string
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHeader = r.Header.Get("Authorization")
		http.Redirect(w, r, serverB.URL, http.StatusFound)
	}))
	defer serverA.Close()

	parsedA, err := url.Parse(serverA.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v, want nil", serverA.URL, err)
	}

	tokens := map[string]string{helpers.Origin(parsedA): "secret-a"}
	client := &http.Client{Transport: authTransport{base: http.DefaultTransport, tokens: tokens}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, serverA.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext error = %v, want nil", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do error = %v, want nil", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if aHeader != "Token secret-a" {
		t.Fatalf("server A received Authorization = %q, want %q", aHeader, "Token secret-a")
	}
	if bHeader != "" {
		t.Fatalf("server B received Authorization = %q, want empty: the redirect crossed origins and must drop the token", bHeader)
	}
}

// stubDialTransport is a real *http.Transport whose dial only counts that this
// pool was chosen, then returns a closed net.Pipe end so the RoundTrip fails
// fast; it observes tlsDispatchTransport's routing with no network or TLS.
type stubDialTransport struct {
	*http.Transport

	hits atomic.Int32
}

func newStubDialTransport() *stubDialTransport {
	s := &stubDialTransport{}
	s.Transport = &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			s.hits.Add(1)
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		},
	}
	return s
}

// TestTLSDispatchTransport_RoutesByExactOrigin pins that only the exact
// configured insecure origin reaches the insecure pool; another host, port or
// scheme, or an off-server download host, reaches the secure pool.
func TestTLSDispatchTransport_RoutesByExactOrigin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		url       string
		wantInsec bool
	}{
		{"configured insecure origin", "http://insecure.example.com/artifact.tar.gz", true},
		{"different host", "http://other.example.com/artifact.tar.gz", false},
		{"different port", "http://insecure.example.com:8080/artifact.tar.gz", false},
		{"https variant of the same host", "https://insecure.example.com/artifact.tar.gz", false},
		{"off-server / S3-shaped download host", "http://s3.example.com/bucket/key", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			secure := newStubDialTransport()
			insecure := newStubDialTransport()
			dispatch := tlsDispatchTransport{
				secure:          secure.Transport,
				insecure:        insecure.Transport,
				insecureOrigins: map[string]struct{}{"http://insecure.example.com:80": {}},
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.url, nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext error = %v, want nil", err)
			}
			// The stub dial never yields a usable connection, so RoundTrip
			// always errors and resp is always nil; only which pool it
			// dialed into matters here.
			resp, _ := dispatch.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}

			gotSecure, gotInsecure := secure.hits.Load(), insecure.hits.Load()
			wantSecure, wantInsecure := int32(1), int32(0)
			if tc.wantInsec {
				wantSecure, wantInsecure = 0, 1
			}
			if gotSecure != wantSecure || gotInsecure != wantInsecure {
				t.Fatalf("dial hits secure=%d insecure=%d, want secure=%d insecure=%d", gotSecure, gotInsecure, wantSecure, wantInsecure)
			}
		})
	}
}

// unwrapDispatch peels client's transport chain down to the
// tlsDispatchTransport at its core, failing the test with a precise "what I
// found instead" message if the chain does not have the expected shape.
func unwrapDispatch(t *testing.T, client *http.Client) tlsDispatchTransport {
	t.Helper()
	wd, ok := client.Transport.(watchdogTransport)
	if !ok {
		t.Fatalf("client.Transport type = %T, want watchdogTransport", client.Transport)
	}
	at, ok := wd.base.(authTransport)
	if !ok {
		t.Fatalf("watchdogTransport.base type = %T, want authTransport", wd.base)
	}
	dispatch, ok := at.base.(tlsDispatchTransport)
	if !ok {
		t.Fatalf("authTransport.base type = %T, want tlsDispatchTransport", at.base)
	}
	return dispatch
}

// TestNewClient_NoInsecureServer_InsecureTransportIsNil pins that with every
// configured server fully verified the insecure pool is never constructed,
// not merely left unused.
func TestNewClient_NoInsecureServer_InsecureTransportIsNil(t *testing.T) {
	t.Parallel()

	client := New(time.Second, []ServerAuth{{Origin: "https://galaxy.example.com:443", Token: "t"}})
	if dispatch := unwrapDispatch(t, client); dispatch.insecure != nil {
		t.Fatal("insecure transport is non-nil with no insecure server configured, want nil")
	}
}

// TestNewClient_InsecureServer_BuildsInsecureTransportForItsOriginOnly pins
// that one insecure server builds the insecure pool and scopes insecureOrigins
// to that server's origin alone.
func TestNewClient_InsecureServer_BuildsInsecureTransportForItsOriginOnly(t *testing.T) {
	t.Parallel()

	client := New(time.Second, []ServerAuth{
		{Origin: "https://secure.example.com:443", Token: "t1"},
		{Origin: "https://insecure.example.com:443", Token: "t2", InsecureTLS: true},
	})

	dispatch := unwrapDispatch(t, client)
	if dispatch.insecure == nil {
		t.Fatal("insecure transport is nil with one insecure server configured, want non-nil")
	}
	if len(dispatch.insecureOrigins) != 1 {
		t.Fatalf("insecureOrigins = %v, want exactly one entry", dispatch.insecureOrigins)
	}
	if _, ok := dispatch.insecureOrigins["https://insecure.example.com:443"]; !ok {
		t.Fatalf("insecureOrigins = %v, want it to contain the insecure server's own origin", dispatch.insecureOrigins)
	}
}
