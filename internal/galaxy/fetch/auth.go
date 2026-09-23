package fetch

import (
	"fmt"
	"net/http"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// authTransport attaches "Authorization: Token <token>" to a request whose
// origin matches a token-bearing server. It stays apart from
// tlsDispatchTransport so a token and InsecureSkipVerify never share a struct.
type authTransport struct {
	base http.RoundTripper
	// tokens maps a normalized origin (helpers.Origin) to its token; built once
	// by tokensByOrigin and never mutated, so concurrent RoundTrips need no lock.
	tokens map[string]string
}

// RoundTrip attaches the token only on an exact helpers.Origin match, so a
// poisoned snapshot's download_url cannot draw it to a look-alike host. A
// caller-set header wins; req is cloned, and each redirect hop is judged anew.
func (t authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" {
		return t.base.RoundTrip(req)
	}
	token, ok := t.tokens[helpers.Origin(req.URL)]
	if !ok {
		return t.base.RoundTrip(req)
	}

	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Token "+token)
	return t.base.RoundTrip(cloned)
}

// String renders authTransport as a count of token-bearing origins only, so a
// %v of the transport chain (a debug dump, a panic value) never leaks a token.
func (t authTransport) String() string {
	return fmt.Sprintf("fetch.authTransport{origins: %d}", len(t.tokens))
}
