package fetch

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// URLBinding is fetch's view of one url-source credential binding, so fetch
// never imports config. The command layer builds it just before
// NewURLDownload, the one place a url token is revealed.
type URLBinding struct {
	// Origin is rendered exactly as helpers.Origin renders a request URL, IPv6
	// brackets included (urlsource.Prefix.Origin), so matching is byte equality.
	Origin string
	// PathPrefix is the binding's escaped path prefix with no trailing
	// slash, "" when the binding covers the whole origin.
	PathPrefix string
	// Token is the revealed Bearer token.
	Token string
}

// NewURLDownload creates the client url sources download over: no Galaxy token
// or relaxed TLS for any origin, only the operator's GO_GALAXY_URL_* Bearer
// bindings, judged anew on every redirect hop; offline refuses every request.
func NewURLDownload(timeout time.Duration, offline bool, bindings []URLBinding) *http.Client {
	client := newClient(timeout, offline, nil)
	client.CheckRedirect = checkURLRedirect
	if offline {
		return client
	}
	client.Transport = urlAuthTransport{base: client.Transport, bindings: bindings}
	return client
}

// urlAuthTransport attaches "Authorization: Bearer <token>" to a request a
// binding covers. It shares no code with authTransport, whose scheme and match
// rule differ, so each security decision stays reviewable on its own.
type urlAuthTransport struct {
	base http.RoundTripper
	// bindings is built once at construction and never mutated afterward, so
	// concurrent RoundTrip calls need no lock to read it.
	bindings []URLBinding
}

// RoundTrip attaches the token of the longest matching binding on the exact
// origin, judged anew per redirect hop, so an unbound target gets no header; a
// caller-set header wins, and a path pathHasUnsafeSegment flags gets nothing.
func (t urlAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" {
		return t.base.RoundTrip(req)
	}
	token, ok := matchURLBinding(t.bindings, req.URL)
	if !ok {
		return t.base.RoundTrip(req)
	}
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(cloned)
}

// String renders urlAuthTransport without any token material, for the reason
// authTransport.String states: only the count of bindings is shown, never
// which origin or what a token is.
func (t urlAuthTransport) String() string {
	return fmt.Sprintf("fetch.urlAuthTransport{bindings: %d}", len(t.bindings))
}

// matchURLBinding returns the token of the longest path prefix bound on u's
// exact origin, and false when none covers u or its path has an unsafe segment.
func matchURLBinding(bindings []URLBinding, u *url.URL) (string, bool) {
	origin := helpers.Origin(u)
	path := strings.TrimPrefix(u.EscapedPath(), "/")
	if pathHasUnsafeSegment(path) {
		return "", false
	}
	best, bestLen, found := "", -1, false
	for _, b := range bindings {
		if b.Origin != origin {
			continue
		}
		prefix := strings.TrimPrefix(b.PathPrefix, "/")
		if prefix != "" && path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		if len(prefix) > bestLen {
			best, bestLen, found = b.Token, len(prefix), true
		}
	}
	return best, found
}

// pathHasUnsafeSegment reports an empty segment or one folding to "." or ".."
// (%2e read as a dot), as urlsource does; the one empty segment allowed is the
// "//" right after a literal "http:" or "https:" segment (the caching proxy).
func pathHasUnsafeSegment(path string) bool {
	if path == "" {
		return false
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		folded := strings.ReplaceAll(strings.ToLower(segment), "%2e", ".")
		if folded == "." || folded == ".." {
			return true
		}
		if folded != "" {
			continue
		}
		if i == 0 || (segments[i-1] != "http:" && segments[i-1] != "https:") {
			return true
		}
	}
	return false
}

// checkURLRedirect is checkRedirect plus refusing a chain that began on https
// from dropping to http, credential or not: it guards the bytes the sha256 pin
// is first taken from. The refusal wraps helpers.ErrDownloadFailed.
func checkURLRedirect(req *http.Request, via []*http.Request) error {
	if err := checkRedirect(req, via); err != nil {
		return err
	}
	if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("%w: redirect from %s to %s leaves https for plaintext http; refusing to follow",
			helpers.ErrDownloadFailed, helpers.URLForMessage(via[0].URL.String()), helpers.URLForMessage(req.URL.String()))
	}
	return nil
}
