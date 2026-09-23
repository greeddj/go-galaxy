package urlsource

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	schemeHTTPS = "https"
	schemeHTTP  = "http"
)

// URL is a parsed, canonical tarball URL: lower-cased host (IPv6 bracketed),
// Port empty when it is the scheme default, Path escaped as written, and the
// raw Query, which is part of the source's identity.
type URL struct {
	Scheme string
	Host   string
	Port   string
	Path   string
	Query  string
}

// Prefix is a parsed credential binding: the origin a GO_GALAXY_URL_* variable
// names, plus an optional path prefix. It is the url counterpart of a git
// credential's binding URL and carries no query, fragment or userinfo.
type Prefix struct {
	Scheme string
	Host   string
	Port   string
	Path   string
}

// hostPattern bounds a host name: DNS labels, an IPv4 literal, or a bracketed
// IPv6 literal.
var hostPattern = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+)$`)

// IsHTTPURL reports whether value spells an http(s) URL: the cheap dispatch
// test the requirements parser runs before this grammar judges the value.
func IsHTTPURL(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, schemeHTTPS+"://") || strings.HasPrefix(lower, schemeHTTP+"://")
}

// ParseURL parses raw as an absolute http(s) tarball URL in canonical form.
// Anything else is helpers.ErrInvalidURLRequirement, and a credential in the
// URL is helpers.ErrURLRequirementUserinfo.
func ParseURL(raw string) (URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return URL{}, fmt.Errorf("%w: empty", helpers.ErrInvalidURLRequirement)
	}
	if strings.Contains(trimmed, "#") {
		return URL{}, fmt.Errorf("%w: a fragment is not part of a tarball URL: %s",
			helpers.ErrInvalidURLRequirement, helpers.URLForMessage(trimmed))
	}
	parsed, scheme, err := parseSchemeHost(trimmed)
	if err != nil {
		return URL{}, err
	}
	if parsed.User != nil {
		return URL{}, fmt.Errorf("%w: %s", helpers.ErrURLRequirementUserinfo, helpers.URLForMessage(trimmed))
	}
	path := parsed.EscapedPath()
	if err := checkSourcePath(path); err != nil {
		return URL{}, err
	}
	if err := checkQuery(parsed); err != nil {
		return URL{}, err
	}
	if (path == "" || path == "/") && parsed.RawQuery == "" {
		return URL{}, fmt.Errorf("%w: %s names nothing beyond its origin",
			helpers.ErrInvalidURLRequirement, helpers.URLForMessage(trimmed))
	}
	return URL{
		Scheme: scheme,
		Host:   hostWithBrackets(parsed),
		Port:   explicitPort(scheme, parsed.Port()),
		Path:   path,
		Query:  parsed.RawQuery,
	}, nil
}

// ParsePrefix parses a GO_GALAXY_URL_<ID>_URL binding: an http(s) origin with
// an optional path prefix, trailing slashes cut, never a query, fragment or
// userinfo. Every refusal is helpers.ErrURLCredentialInvalid.
func ParsePrefix(raw string) (Prefix, error) {
	parsed, scheme, err := parsePrefixShape(strings.TrimSpace(raw))
	if err != nil {
		return Prefix{}, err
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	if path != "" {
		if err := checkPath(path, true); err != nil {
			return Prefix{}, fmt.Errorf("%w: %w", helpers.ErrURLCredentialInvalid, err)
		}
	}
	return Prefix{
		Scheme: scheme,
		Host:   hostWithBrackets(parsed),
		Port:   explicitPort(scheme, parsed.Port()),
		Path:   path,
	}, nil
}

// parsePrefixShape runs the shape refusals a binding answers to before its
// path is judged: an empty value, a "#" anywhere, a scheme or host
// parseSchemeHost refuses, userinfo, and a query.
func parsePrefixShape(trimmed string) (*url.URL, string, error) {
	if trimmed == "" {
		return nil, "", fmt.Errorf("%w: empty url", helpers.ErrURLCredentialInvalid)
	}
	if strings.Contains(trimmed, "#") {
		return nil, "", fmt.Errorf("%w: a query or fragment is not part of a credential binding",
			helpers.ErrURLCredentialInvalid)
	}
	parsed, scheme, err := parseSchemeHost(trimmed)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", helpers.ErrURLCredentialInvalid, err)
	}
	if parsed.User != nil {
		return nil, "", fmt.Errorf("%w: a credential binding names no user", helpers.ErrURLCredentialInvalid)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return nil, "", fmt.Errorf("%w: a query or fragment is not part of a credential binding",
			helpers.ErrURLCredentialInvalid)
	}
	return parsed, scheme, nil
}

// parseSchemeHost runs the checks ParseURL and ParsePrefix share: an absolute
// URL, a scheme of https or http, and a well-formed host.
func parseSchemeHost(trimmed string) (*url.URL, string, error) {
	if !strings.Contains(trimmed, "://") {
		return nil, "", fmt.Errorf("%w: %q is not an absolute http(s) URL",
			helpers.ErrInvalidURLRequirement, helpers.URLForMessage(trimmed))
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s", helpers.ErrInvalidURLRequirement, helpers.URLForMessage(trimmed))
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != schemeHTTPS && scheme != schemeHTTP {
		return nil, "", fmt.Errorf("%w: scheme %q is not supported (https or http)",
			helpers.ErrInvalidURLRequirement, scheme)
	}
	if parsed.Hostname() == "" || !hostPattern.MatchString(hostWithBrackets(parsed)) {
		return nil, "", fmt.Errorf("%w: missing or invalid host in %s",
			helpers.ErrInvalidURLRequirement, helpers.URLForMessage(trimmed))
	}
	return parsed, scheme, nil
}

// checkSourcePath judges a requirement URL's path by checkPath, admitting one
// embedded upstream URL (the caching-proxy shape) only in canonical spelling,
// so its "//" is the one empty segment and each artifact has one pin.
func checkSourcePath(path string) error {
	head, embedded := splitEmbeddedPath(path)
	if embedded == "" {
		return checkPath(path, false)
	}
	if head != "" {
		if err := checkPath(head, true); err != nil {
			return err
		}
	}
	parsed, err := ParseURL(embedded)
	if err != nil {
		return fmt.Errorf("embedded upstream URL: %w", err)
	}
	if parsed.String() != embedded {
		return fmt.Errorf("%w: embedded upstream URL is not in its canonical spelling: %s",
			helpers.ErrInvalidURLRequirement, helpers.URLForMessage(embedded))
	}
	return nil
}

// splitEmbeddedPath splits path before the first "/http(s)://" into the head
// and the embedded URL ("" when none). Only the lower-case scheme on a "/"
// boundary counts; anything else falls to the plain-segment refusals.
func splitEmbeddedPath(path string) (string, string) {
	idx := len(path)
	for _, marker := range []string{"/" + schemeHTTPS + "://", "/" + schemeHTTP + "://"} {
		if i := strings.Index(path, marker); i >= 0 && i < idx {
			idx = i
		}
	}
	if idx == len(path) {
		return path, ""
	}
	return path[:idx], path[idx+1:]
}

// checkPath refuses a rune outside isPathRune and, past a bare root, any empty
// or dot segment, "%2e" included. Prefixes and requirement URLs share this rule
// so the credential match and the request never read one path differently.
func checkPath(path string, prefix bool) error {
	for _, r := range path {
		if !isPathRune(r) {
			return fmt.Errorf("%w: path carries a character this tool does not send in a URL: %q",
				helpers.ErrInvalidURLRequirement, r)
		}
	}
	if path == "" || (path == "/" && !prefix) {
		return nil
	}
	for segment := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		folded := strings.ReplaceAll(strings.ToLower(segment), "%2e", ".")
		if folded == "" || folded == "." || folded == ".." {
			return fmt.Errorf("%w: path carries an empty or dot segment", helpers.ErrInvalidURLRequirement)
		}
	}
	return nil
}

// checkQuery bounds the query to the path alphabet plus "&", "?" and ";". A
// query reaches only its own host, so the concern is rendering alone.
func checkQuery(parsed *url.URL) error {
	for _, r := range parsed.RawQuery {
		if !isPathRune(r) && r != '&' && r != '?' && r != ';' {
			return fmt.Errorf("%w: query carries a character this tool does not send in a URL: %q",
				helpers.ErrInvalidURLRequirement, r)
		}
	}
	return nil
}

// isPathRune is the url-source path alphabet: the git path alphabet plus the
// sub-delims release paths carry, and nothing that needs quoting in a terminal
// or a shell.
func isPathRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '/', r == '.', r == '-', r == '_', r == '~', r == '+', r == '%', r == '@',
		r == '(', r == ')', r == ',', r == '=', r == ':', r == '!':
		return true
	default:
		return false
	}
}

// hostWithBrackets returns the lower-cased host, re-bracketing an IPv6
// literal the way it has to be written back into a URL.
func hostWithBrackets(parsed *url.URL) string {
	host := strings.ToLower(parsed.Hostname())
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// explicitPort drops a port equal to the scheme's default so two spellings of
// the same origin compare equal.
func explicitPort(scheme, port string) string {
	if port == defaultPort(scheme) {
		return ""
	}
	return port
}

func defaultPort(scheme string) string {
	switch scheme {
	case schemeHTTPS:
		return "443"
	case schemeHTTP:
		return "80"
	default:
		return ""
	}
}

// String renders the canonical spelling scheme://host[:port]path[?query],
// safe to persist; messages still go through helpers.URLForMessage, which cuts
// the query.
func (u URL) String() string {
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString("://")
	b.WriteString(u.Host)
	if u.Port != "" {
		b.WriteByte(':')
		b.WriteString(u.Port)
	}
	b.WriteString(u.Path)
	if u.Query != "" {
		b.WriteByte('?')
		b.WriteString(u.Query)
	}
	return b.String()
}

// String renders the canonical binding spelling: scheme://host[:port][path].
func (p Prefix) String() string {
	var b strings.Builder
	b.WriteString(p.Scheme)
	b.WriteString("://")
	b.WriteString(p.Host)
	if p.Port != "" {
		b.WriteByte(':')
		b.WriteString(p.Port)
	}
	b.WriteString(p.Path)
	return b.String()
}

// Origin is scheme://host:port with the default port filled in and an IPv6
// literal unbracketed; it must render exactly as helpers.Origin does, since the
// credential match compares the two byte for byte.
func (p Prefix) Origin() string {
	port := p.Port
	if port == "" {
		port = defaultPort(p.Scheme)
	}
	return p.Scheme + "://" + strings.Trim(p.Host, "[]") + ":" + port
}

// IsLoopback reports whether the host is "localhost" or a loopback IP literal,
// the one case a plaintext http credential is tolerated for; a DNS name never
// earns the exemption.
func (p Prefix) IsLoopback() bool {
	host := strings.Trim(p.Host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
