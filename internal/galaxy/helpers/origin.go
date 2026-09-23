package helpers

import (
	"net/url"
	"strings"
)

// Origin returns u's normalized network origin, scheme://host:port with
// scheme and host lower-cased and a default port made explicit; path and
// query are ignored. Token and TLS-policy matching key on this value.
func Origin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = defaultPortForScheme(scheme)
	}
	return scheme + "://" + host + ":" + port
}

// defaultPortForScheme returns the port implied by scheme when a URL omits
// one, so "https://h" and "https://h:443" share an Origin; an unknown scheme
// yields "".
func defaultPortForScheme(scheme string) string {
	switch scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}
