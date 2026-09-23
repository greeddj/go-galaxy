// Package fetch builds every *http.Client this program uses and owns their
// per-origin policy: a Galaxy token or relaxed TLS applies only on an exact
// helpers.Origin match, and a client for repository content holds neither.
package fetch

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// ServerAuth is fetch's view of one configured server's origin, token and TLS
// policy, so fetch never imports config, the layer above it. The command layer
// builds it just before New, the one place a Galaxy token is revealed.
type ServerAuth struct {
	// Origin is helpers.Origin(parsed server URL), already normalized, so
	// New never has to reparse or renormalize a server's configured URL.
	Origin string
	// Token is the server's revealed API token, or "" if it carries none.
	Token string
	// InsecureTLS mirrors the server's validate_certs=false setting.
	InsecureTLS bool
}

// New creates the shared client: a request on a configured server's origin
// gets that server's TLS policy and token, and any other origin, the S3 cache
// backend's included, gets full verification and no credential.
func New(timeout time.Duration, servers []ServerAuth) *http.Client {
	return newClient(timeout, false, servers)
}

// NewOffline creates a client whose transport fails every request with
// helpers.ErrOfflineMode before any auth or TLS layer runs, so a code path
// that needs the network fails fast while cache reads keep working.
func NewOffline(timeout time.Duration) *http.Client {
	return newClient(timeout, true, nil)
}

// NewUnauthenticated creates a client built with no server configuration, so
// no origin gets a credential or relaxed TLS, as a repository-authored URL such
// as a signatures: source needs; offline refuses every request instead.
func NewUnauthenticated(timeout time.Duration, offline bool) *http.Client {
	return newClient(timeout, offline, nil)
}

// checkRedirect, run by every client here, deletes the Referer net/http gives a
// redirect hop, which would hand a presigned query to the target, and keeps the
// hop ceiling, since setting CheckRedirect replaces net/http's default check.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= helpers.FetchMaxRedirects {
		// net/http's own wording rather than a sentinel, so an exhausted
		// budget classifies exactly as it did when net/http raised it.
		//nolint:err113 // mirrors net/http's own defaultCheckRedirect error, never a sentinel to match
		return fmt.Errorf("stopped after %d redirects", helpers.FetchMaxRedirects)
	}
	req.Header.Del("Referer")

	return nil
}

func newClient(timeout time.Duration, offline bool, servers []ServerAuth) *http.Client {
	if offline {
		// No body ever comes back for a watchdog to guard; CheckRedirect is
		// still set, so every client this package builds has a redirect policy.
		return &http.Client{Timeout: timeout, Transport: offlineTransport{}, CheckRedirect: checkRedirect}
	}

	dispatch := tlsDispatchTransport{secure: newTransport(timeout, nil), insecureOrigins: insecureOriginSet(servers)}
	if len(dispatch.insecureOrigins) > 0 {
		dispatch.insecure = newTransport(timeout, &tls.Config{
			// Built only for a server's validate_certs=false, and
			// tlsDispatchTransport routes only that server's origin here.
			InsecureSkipVerify: true, // #nosec G402 -- opt-in per-origin via validate_certs=false, never a blanket default
		})
	}

	auth := authTransport{base: dispatch, tokens: tokensByOrigin(servers)}
	// No client Timeout, which would cap total transfer time: the watchdog
	// bounds each body read instead, so a stall fails and a large body does not.
	return &http.Client{Transport: watchdogTransport{base: auth, idle: timeout}, CheckRedirect: checkRedirect}
}

// newTransport builds a *http.Transport with the dial, pooling and HTTP/2
// settings every fetch transport shares; a nil tlsConfig verifies fully. Both
// pools come from here so their settings cannot drift apart.
func newTransport(timeout time.Duration, tlsConfig *tls.Config) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   helpers.FetchDialContextTimeout,
			KeepAlive: helpers.FetchDialContextKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     helpers.FetchForceAttemptHTTP2,
		MaxIdleConns:          helpers.FetchMaxIdleConns,
		MaxIdleConnsPerHost:   helpers.FetchMaxIdleConnsPerHost,
		IdleConnTimeout:       helpers.FetchIdleConnTimeout,
		TLSHandshakeTimeout:   helpers.FetchTLSHandshakeTimeout,
		ExpectContinueTimeout: helpers.FetchExpectContinueTimeout,
		// ResponseHeaderTimeout bounds time-to-first-byte: timeout is a
		// no-progress budget rather than a whole-response cap, so a slow but
		// steadily streaming artifact download is not truncated by it.
		ResponseHeaderTimeout: timeout,
		TLSClientConfig:       tlsConfig,
	}
}

// insecureOriginSet collects the origin of every server with InsecureTLS set;
// an empty set tells newClient not to build the insecure pool at all.
func insecureOriginSet(servers []ServerAuth) map[string]struct{} {
	origins := make(map[string]struct{}, len(servers))
	for _, s := range servers {
		if s.InsecureTLS {
			origins[s.Origin] = struct{}{}
		}
	}
	return origins
}

// tokensByOrigin collects the normalized origin -> token map for every
// server in servers that carries a token, pre-sized to len(servers).
func tokensByOrigin(servers []ServerAuth) map[string]string {
	tokens := make(map[string]string, len(servers))
	for _, s := range servers {
		if s.Token != "" {
			tokens[s.Origin] = s.Token
		}
	}
	return tokens
}

// tlsDispatchTransport sends a request to the insecure pool only when its exact
// origin set validate_certs=false. Two pools, not one DialTLSContext: the idle
// pool key omits tls.Config, and DialTLSContext would disable ALPN HTTP/2.
type tlsDispatchTransport struct {
	secure   *http.Transport
	insecure *http.Transport // nil unless at least one configured server sets validate_certs=false
	// insecureOrigins is built once at construction (see insecureOriginSet)
	// and never mutated afterward, so concurrent RoundTrip calls need no
	// lock to read it.
	insecureOrigins map[string]struct{}
}

// RoundTrip dispatches req to the insecure transport only when one was built
// and req's normalized origin is in insecureOrigins; all else goes to secure.
func (t tlsDispatchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.insecure != nil {
		if _, ok := t.insecureOrigins[helpers.Origin(req.URL)]; ok {
			return t.insecure.RoundTrip(req)
		}
	}
	return t.secure.RoundTrip(req)
}

type offlineTransport struct{}

// RoundTrip fails every request with ErrOfflineMode, naming the URL through
// helpers.WithoutCredentials: every RoundTripper must cut its own URL, since
// wrappers print its error verbatim and net/http masks only its own *url.Error.
func (offlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("%w: %s %s", helpers.ErrOfflineMode, req.Method, helpers.WithoutCredentials(req.URL.String()))
}
