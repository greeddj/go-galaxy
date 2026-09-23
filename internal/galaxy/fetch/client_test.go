package fetch_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// tinyTimeout is small enough that a hung response fails the
// ResponseHeaderTimeout test in well under a second, but large enough that a
// normal in-process fakegalaxy round trip never trips it by accident.
const tinyTimeout = 50 * time.Millisecond

// waitBound bounds how long this file's tests wait for an HTTP round trip
// to return, so a regression that reintroduces an unbounded hang fails the
// test instead of hanging the suite.
const waitBound = 5 * time.Second

// TestNew_ResponseHeaderTimeout_FiresOnHang pins that a server which never
// writes headers fails the request once ResponseHeaderTimeout elapses: the
// client has no whole-response Timeout to fall back on.
func TestNew_ResponseHeaderTimeout_FiresOnHang(t *testing.T) {
	t.Parallel()

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "hangs", "1.0.0", nil)
	// Count must be positive: ruleMatches treats a zero Count as a rule
	// already used up, so the Hang would never fire.
	s.Fail(fakegalaxy.EndpointRootMetadata, "acme", "hangs", fakegalaxy.Fault{Hang: true, Count: 1})

	client := fetch.New(tinyTimeout, nil)
	url := fmt.Sprintf("%s/api/v3/collections/%s/%s/", s.URL(), "acme", "hangs")

	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext error = %v, want nil", err)
	}

	done := make(chan error, 1)
	go func() {
		resp, doErr := client.Do(req)
		if doErr == nil {
			// Not expected on this path, but close defensively so a
			// surprising success never leaks a connection.
			_ = resp.Body.Close()
		}
		done <- doErr
	}()

	select {
	case doErr := <-done:
		if doErr == nil {
			t.Fatal("Do error = nil, want a ResponseHeaderTimeout failure since the fake server hung before writing headers")
		}
	case <-time.After(waitBound):
		t.Fatal("Do did not return within the outer bound; ResponseHeaderTimeout failed to fire")
	}
}

// TestNew_SuccessfulRoundTrip_ReadsBodyAndCloses pins that the watchdog-wrapped
// body leaves an ordinary request intact: the payload reads whole and closes.
func TestNew_SuccessfulRoundTrip_ReadsBodyAndCloses(t *testing.T) {
	t.Parallel()

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "widgets", "1.0.0", nil)

	client := fetch.New(time.Second, nil)
	url := fmt.Sprintf("%s/api/v3/collections/%s/%s/", s.URL(), "acme", "widgets")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext error = %v, want nil", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do error = %v, want nil", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("Body.Close error = %v, want nil", cerr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error = %v, want nil", err)
	}
	if len(body) == 0 {
		t.Fatal("ReadAll returned an empty body, want the root metadata JSON payload")
	}
	if got := s.Count(fakegalaxy.EndpointRootMetadata); got != 1 {
		t.Fatalf("EndpointRootMetadata count = %d, want 1", got)
	}
}

// mustGetRequest builds a GET request bound to t's context (as noctx wants),
// failing the test on a parse error.
func mustGetRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext(%q) error = %v, want nil", target, err)
	}
	return req
}

// TestNewOffline_AttachesNothing_ReachesNoTransport pins that the offline
// client refuses a request with ErrOfflineMode before any auth or TLS layer
// runs, so nothing is attached to it.
func TestNewOffline_AttachesNothing_ReachesNoTransport(t *testing.T) {
	t.Parallel()

	client := fetch.NewOffline(time.Second)
	req := mustGetRequest(t, "https://galaxy.example.com/api/v3/")

	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("Do returned a non-nil response, want nil: an offline client must never reach a transport")
	}
	if !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("Do error = %v, want errors.Is(err, helpers.ErrOfflineMode)", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization header = %q, want empty: nothing must be attached before the offline rejection", got)
	}
}

// TestNewUnauthenticatedAttachesNoAuthorization pins that a client with no
// server configuration sends no credential; the fetch.New control on the same
// server proves the fixture would have seen a token had one been sent.
func TestNewUnauthenticatedAttachesNoAuthorization(t *testing.T) {
	t.Parallel()

	// Buffered, and read after each round trip rather than shared as a slice:
	// the handler runs on the server's own goroutine, so a channel handoff is
	// what makes the read ordered with respect to it under -race.
	headers := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v, want nil", srv.URL, err)
	}

	resp, err := fetch.NewUnauthenticated(waitBound, false).Do(mustGetRequest(t, srv.URL))
	if err != nil {
		t.Fatalf("Do error = %v, want nil", err)
	}
	_ = resp.Body.Close()
	if got := <-headers; got != "" {
		t.Fatalf("unauthenticated client sent Authorization = %q, want empty", got)
	}

	control := fetch.New(waitBound, []fetch.ServerAuth{{Origin: helpers.Origin(parsed), Token: "t"}})
	controlResp, err := control.Do(mustGetRequest(t, srv.URL))
	if err != nil {
		t.Fatalf("positive control: Do error = %v, want nil", err)
	}
	_ = controlResp.Body.Close()
	if got, want := <-headers, "Token t"; got != want {
		t.Fatalf("positive control: Authorization = %q, want %q; the fixture cannot show a header at all", got, want)
	}
}

// TestNewUnauthenticatedOfflineRefuses covers the offline parameter this
// constructor takes and NewOffline's own does not: it must select the same
// refuse-everything transport rather than quietly building a live client.
func TestNewUnauthenticatedOfflineRefuses(t *testing.T) {
	t.Parallel()

	resp, err := fetch.NewUnauthenticated(waitBound, true).Do(mustGetRequest(t, "https://galaxy.example.com/sig.asc"))
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("Do returned a non-nil response, want nil: an offline client must never reach a transport")
	}
	if !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("Do error = %v, want errors.Is(err, helpers.ErrOfflineMode)", err)
	}
}

// redirectQuery is a presigned URL's capability, which net/http's refererForURL
// keeps in the Referer it hands a redirect target.
const redirectQuery = "?X-Amz-Signature=deadbeef&X-Amz-Expires=900"

// redirectBody is what the redirect target answers with, so a test can tell
// "the hop arrived carrying no Referer" from "the hop never happened".
const redirectBody = "redirect-target-body"

// TestClientsSendNoRefererAcrossARedirect pins that both New and
// NewUnauthenticated follow a redirect with no Referer; the target's own body
// must arrive, so "no Referer" cannot mean "never reached".
func TestClientsSendNoRefererAcrossARedirect(t *testing.T) {
	t.Parallel()

	// Buffered and read after each round trip: the handler runs on the
	// server's own goroutine, so the channel handoff is what orders the read
	// with respect to it under -race.
	referers := make(chan string, 2)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		referers <- r.Header.Get("Referer")
		_, _ = w.Write([]byte(redirectBody))
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/dest", http.StatusFound)
	}))
	defer source.Close()

	sourceURL, err := url.Parse(source.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v, want nil", source.URL, err)
	}

	clients := []struct {
		client *http.Client
		name   string
	}{
		// The shared client is built with the redirecting origin configured as
		// a Galaxy server, which is the shape a presigned artifact download
		// actually takes: a configured host answering with a hop elsewhere.
		{name: "New", client: fetch.New(waitBound, []fetch.ServerAuth{{Origin: helpers.Origin(sourceURL), Token: "t"}})},
		{name: "NewUnauthenticated", client: fetch.NewUnauthenticated(waitBound, false)},
	}

	for _, tc := range clients {
		// The Referer goes to the log rather than the failure message: it
		// carries the fixture's ephemeral port, so no two runs would agree.
		resp, err := tc.client.Do(mustGetRequest(t, source.URL+"/sig.asc"+redirectQuery))
		if err != nil {
			t.Fatalf("%s: Do error = %v, want nil", tc.name, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("%s: ReadAll error = %v, want nil", tc.name, err)
		}
		if string(body) != redirectBody {
			t.Fatalf("%s: body = %q, want the redirect target's own body; the hop did not arrive", tc.name, body)
		}
		if got := <-referers; got != "" {
			t.Logf("Referer = %q", got)
			t.Fatalf("%s: the redirect target received a Referer", tc.name)
		}
	}
}

// TestClientsStillRefuseAnEndlessRedirectChain pins that a redirect loop stops
// after exactly 10 hops, as net/http's default did: CheckRedirect replaces that
// check, and refusing the first hop would break presigned downloads.
func TestClientsStillRefuseAnEndlessRedirectChain(t *testing.T) {
	t.Parallel()

	var hops atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops.Add(1)
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	defer srv.Close()

	// A hook that never refuses hangs this test rather than failing it, so a
	// hang here means the hop ceiling is gone.
	resp, err := fetch.New(waitBound, nil).Do(mustGetRequest(t, srv.URL+"/sig.asc"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("Do error = nil, want a refusal: a self-redirecting server must not be followed forever")
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("Do error = %v, want net/http's own redirect-ceiling wording", err)
	}
	if got := hops.Load(); got != 10 {
		t.Fatalf("server served %d requests, want 10: the ceiling must bite where net/http's own default did", got)
	}
}

// TestNew_InsecureIsNeverGlobal pins that validate_certs=false on one
// self-signed TLS server leaves a second one in the same client failing
// certificate verification: the policy is scoped to one origin.
func TestNew_InsecureIsNeverGlobal(t *testing.T) {
	t.Parallel()

	insecureSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer insecureSrv.Close()

	secureSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer secureSrv.Close()

	insecureURL, err := url.Parse(insecureSrv.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v, want nil", insecureSrv.URL, err)
	}

	client := fetch.New(5*time.Second, []fetch.ServerAuth{
		{Origin: helpers.Origin(insecureURL), InsecureTLS: true},
	})

	respInsecure, err := client.Do(mustGetRequest(t, insecureSrv.URL))
	if err != nil {
		t.Fatalf("Do(insecureSrv.URL) error = %v, want nil: this origin is configured validate_certs=false", err)
	}
	_ = respInsecure.Body.Close()

	respSecure, err := client.Do(mustGetRequest(t, secureSrv.URL))
	if respSecure != nil {
		_ = respSecure.Body.Close()
	}
	if err == nil {
		t.Fatal("Do(secureSrv.URL) error = nil, want a certificate error: this origin was never configured insecure")
	}
	if !strings.Contains(err.Error(), "x509") {
		t.Fatalf("Do(secureSrv.URL) error = %v, want an x509 certificate verification failure", err)
	}
}

// TestNew_RedirectFromInsecureOriginToSecureOriginUsesSecureTransport pins
// that a redirect out of the insecure origin fails certificate verification:
// the hop is routed to the verifying pool, not the first hop's policy.
func TestNew_RedirectFromInsecureOriginToSecureOriginUsesSecureTransport(t *testing.T) {
	t.Parallel()

	secureSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer secureSrv.Close()

	var insecureHitToken string
	insecureSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		insecureHitToken = r.Header.Get("Authorization")
		http.Redirect(w, r, secureSrv.URL, http.StatusFound)
	}))
	defer insecureSrv.Close()

	insecureURL, err := url.Parse(insecureSrv.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v, want nil", insecureSrv.URL, err)
	}

	client := fetch.New(5*time.Second, []fetch.ServerAuth{
		{Origin: helpers.Origin(insecureURL), InsecureTLS: true, Token: "secret-a"},
	})

	resp, err := client.Do(mustGetRequest(t, insecureSrv.URL))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("Do error = nil, want a certificate error once the redirect crosses into secureSrv's unconfigured origin")
	}
	if !strings.Contains(err.Error(), "x509") {
		t.Fatalf("Do error = %v, want an x509 certificate verification failure on the redirect hop", err)
	}
	const wantToken = "Token secret-a"
	if insecureHitToken != wantToken {
		t.Fatalf("insecureSrv received Authorization = %q, want %q (its own origin is configured with a token)", insecureHitToken, wantToken)
	}
}

// TestNew_S3ShapedOriginIsIsolatedFromGalaxyServerConfig pins that the shared
// client, which the S3 cache backend also uses, sends an unconfigured origin
// no Galaxy token even beside an insecure, token-bearing server.
func TestNew_S3ShapedOriginIsIsolatedFromGalaxyServerConfig(t *testing.T) {
	t.Parallel()

	var s3Header string
	s3Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s3Header = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer s3Srv.Close()

	insecureSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer insecureSrv.Close()

	insecureURL, err := url.Parse(insecureSrv.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v, want nil", insecureSrv.URL, err)
	}

	client := fetch.New(5*time.Second, []fetch.ServerAuth{
		{Origin: helpers.Origin(insecureURL), InsecureTLS: true, Token: "galaxy-secret"},
	})

	resp, err := client.Do(mustGetRequest(t, s3Srv.URL))
	if err != nil {
		t.Fatalf("Do(s3Srv.URL) error = %v, want nil: an unconfigured plain-HTTP origin must still work through secure", err)
	}
	_ = resp.Body.Close()

	if s3Header != "" {
		t.Fatalf("s3-shaped origin received Authorization = %q, want empty (never configured with a token)", s3Header)
	}
}

// offlineFixturePassword is the credential the offline fixture smuggles into
// its URL, distinctive so a substring match cannot be a coincidence.
const offlineFixturePassword = "pa55w0rd-must-not-be-rendered"

// offlineFixtureHostPath is the part of that fixture an operator reading the
// refusal actually needs, and the part no cut here removes.
const offlineFixtureHostPath = "hub.example/api/v3/collections/acme/widgets/"

// offlineFixtureQuery is the capability half of the same fixture: a presigned
// query is what a message naming a URL must drop even where the URL itself is
// worth naming.
const offlineFixtureQuery = "?X-Amz-Signature=deadbeefcafe&X-Amz-Expires=900"

// offlineFixtureURL carries both credential-bearing parts of a URL at once, so
// one fixture covers both halves of the cut under test.
const offlineFixtureURL = "https://u:" + offlineFixturePassword + "@" + offlineFixtureHostPath + offlineFixtureQuery

// TestNewOffline_RefusalNamesTheURLWithItsCredentialsCut pins that the offline
// transport's own error, which wrappers print verbatim, names host and path but
// no password, userinfo or presigned query, and still classifies as offline.
func TestNewOffline_RefusalNamesTheURLWithItsCredentialsCut(t *testing.T) {
	t.Parallel()

	resp, err := fetch.NewOffline(0).Do(mustGetRequest(t, offlineFixtureURL))
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("Do returned a non-nil response, want nil: an offline client must never reach a transport")
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("errors.As(err, &*url.Error) failed on %v, so this fixture never reached the transport", err)
	}
	msg := urlErr.Err.Error()

	if strings.Contains(msg, offlineFixturePassword) {
		t.Errorf("offline refusal carries the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("offline refusal carries the userinfo prefix %q: %s", "u:", msg)
	}
	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("offline refusal carries the presigned query: %s", msg)
	}
	if !strings.Contains(msg, offlineFixtureHostPath) {
		t.Errorf("offline refusal does not name the host and path it refused: %s", msg)
	}
	if !errors.Is(err, helpers.ErrOfflineMode) {
		t.Errorf("offline refusal does not classify as helpers.ErrOfflineMode: %v", err)
	}

	assertOfflineRefusalNamesACleanURL(t)
}

// assertOfflineRefusalNamesACleanURL is the control described on
// TestNewOffline_RefusalNamesTheURLWithItsCredentialsCut: the same transport,
// handed a URL with nothing to cut, must still name that URL whole.
func assertOfflineRefusalNamesACleanURL(t *testing.T) {
	t.Helper()

	clean := "https://" + offlineFixtureHostPath
	resp, err := fetch.NewOffline(0).Do(mustGetRequest(t, clean))
	if resp != nil {
		_ = resp.Body.Close()
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("control: errors.As(err, &*url.Error) failed on %v", err)
	}
	if got := urlErr.Err.Error(); !strings.Contains(got, clean) {
		t.Errorf("control: a refusal over a credential-free URL does not name it: %s", got)
	}
}
