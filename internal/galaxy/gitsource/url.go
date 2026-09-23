package gitsource

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/safeout"
)

const (
	schemeHTTPS = "https"
	schemeHTTP  = "http"
	schemeSSH   = "ssh"

	// gitPlusPrefix is the spelling ansible strips before looking at the
	// URL: "git+https://..." names the same repository as "https://...".
	gitPlusPrefix = "git+"
	// headRef is what an absent, empty or "*" version means for a git
	// source: the remote's HEAD, exactly as ansible's parse_scm substitutes.
	headRef = "HEAD"
)

// URL is a canonical repository URL: User only for ssh, Host lower-cased, Port
// empty at the scheme default. SCPLike user@host:path is never rewritten to
// ssh://, since its path is relative to the remote user's home.
type URL struct {
	Scheme  string
	User    string
	Host    string
	Port    string
	Path    string
	SCPLike bool
}

// scpLikePattern is the user@host:path spelling; the user is required, unlike
// in go-git's parser, so a bare host:path is never mistaken for one, which is
// also why ansible's auto-detection keys on "git@".
var scpLikePattern = regexp.MustCompile(`^([A-Za-z0-9._-]+)@([^:/@\s]+):(.+)$`)

// sshUserPattern bounds an ssh user name to what a remote accepts in a login
// name and what is safe to render.
var sshUserPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// hostPattern bounds a host name: DNS labels, an IPv4 literal, or a bracketed
// IPv6 literal.
var hostPattern = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+)$`)

// ParseURL parses raw as an https, http, ssh or scp-like repository URL into
// its canonical form; anything else is helpers.ErrInvalidGitURL, and a
// credential in the URL is helpers.ErrGitURLUserinfo.
func ParseURL(raw string) (URL, error) {
	return parseURL(raw, false)
}

// ParsePrefix parses a credential binding with ParseURL's grammar, except an
// empty path covers the whole host and an ssh binding names no user, since the
// requirement URL supplies it.
func ParsePrefix(raw string) (URL, error) {
	return parseURL(raw, true)
}

func parseURL(raw string, prefix bool) (URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return URL{}, fmt.Errorf("%w: empty", helpers.ErrInvalidGitURL)
	}
	if strings.Contains(trimmed, "://") {
		return parseSchemeURL(trimmed, prefix)
	}
	if m := scpLikePattern.FindStringSubmatch(trimmed); m != nil {
		if prefix {
			return URL{}, fmt.Errorf("%w: a credential binding is written as ssh://host[:port][/prefix], not as user@host:path",
				helpers.ErrInvalidGitURL)
		}
		return parseSCPLike(m[1], m[2], m[3])
	}
	return URL{}, fmt.Errorf("%w: %q is neither a https, http or ssh URL nor a user@host:path spelling",
		helpers.ErrInvalidGitURL, helpers.URLForMessage(trimmed))
}

func parseSchemeURL(raw string, prefix bool) (URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return URL{}, fmt.Errorf("%w: %s", helpers.ErrInvalidGitURL, helpers.URLForMessage(raw))
	}
	scheme := strings.ToLower(parsed.Scheme)
	if err := checkSchemeAndQuery(raw, scheme, parsed); err != nil {
		return URL{}, err
	}
	user, err := checkUserinfo(scheme, parsed, prefix)
	if err != nil {
		return URL{}, err
	}
	if parsed.Hostname() == "" || !hostPattern.MatchString(hostWithBrackets(parsed)) {
		return URL{}, fmt.Errorf("%w: missing or invalid host in %s", helpers.ErrInvalidGitURL, helpers.URLForMessage(raw))
	}
	path, err := schemeURLPath(parsed.EscapedPath(), prefix)
	if err != nil {
		return URL{}, err
	}
	return URL{Scheme: scheme, User: user, Host: hostWithBrackets(parsed), Port: explicitPort(scheme, parsed.Port()), Path: path}, nil
}

// checkSchemeAndQuery refuses a scheme outside https, http and ssh, and any
// query or fragment, which are never part of a repository URL.
func checkSchemeAndQuery(raw, scheme string, parsed *url.URL) error {
	if scheme != schemeHTTPS && scheme != schemeHTTP && scheme != schemeSSH {
		return fmt.Errorf("%w: scheme %q is not supported (https, http, ssh or user@host:path)",
			helpers.ErrInvalidGitURL, scheme)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return fmt.Errorf("%w: a query or fragment is not part of a repository URL: %s",
			helpers.ErrInvalidGitURL, helpers.URLForMessage(raw))
	}
	return nil
}

// explicitPort drops a port equal to the scheme's default so two spellings of
// the same origin compare equal.
func explicitPort(scheme, port string) string {
	if port == defaultPort(scheme) {
		return ""
	}
	return port
}

// schemeURLPath validates the path. A credential-binding prefix may be empty
// and loses its trailing slashes; a repository path must be present.
func schemeURLPath(path string, prefix bool) (string, error) {
	if !prefix {
		return path, checkPath(path)
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "", nil
	}
	return path, checkPath(path)
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

func checkUserinfo(scheme string, parsed *url.URL, prefix bool) (string, error) {
	if parsed.User == nil {
		if scheme == schemeSSH && !prefix {
			return "", fmt.Errorf("%w: an ssh URL names its user explicitly (ssh://git@%s/<path>)",
				helpers.ErrInvalidGitURL, strings.ToLower(parsed.Hostname()))
		}
		return "", nil
	}
	if scheme != schemeSSH {
		return "", fmt.Errorf("%w: %s", helpers.ErrGitURLUserinfo, helpers.URLForMessage(parsed.String()))
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return "", fmt.Errorf("%w: %s", helpers.ErrGitURLUserinfo, helpers.URLForMessage(parsed.String()))
	}
	if prefix {
		return "", fmt.Errorf("%w: a credential binding names no user; the requirement URL supplies it",
			helpers.ErrInvalidGitURL)
	}
	user := parsed.User.Username()
	if !sshUserPattern.MatchString(user) {
		return "", fmt.Errorf("%w: invalid ssh user", helpers.ErrInvalidGitURL)
	}
	return user, nil
}

func parseSCPLike(user, host, path string) (URL, error) {
	if !hostPattern.MatchString(host) {
		return URL{}, fmt.Errorf("%w: invalid host %q", helpers.ErrInvalidGitURL, safeout.Clean(host))
	}
	if err := checkPath(path); err != nil {
		return URL{}, err
	}
	return URL{Scheme: schemeSSH, User: user, Host: strings.ToLower(host), Path: path, SCPLike: true}, nil
}

// checkPath refuses an empty path, a first segment starting with "-" (an
// upload-pack argument), and any rune outside a conservative alphabet, keeping
// quotes, whitespace and control runes out of go-git's git-upload-pack line.
func checkPath(path string) error {
	if path == "" || path == "/" {
		return fmt.Errorf("%w: missing repository path", helpers.ErrInvalidGitURL)
	}
	first := strings.TrimLeft(path, "/")
	if strings.HasPrefix(first, "-") {
		return fmt.Errorf("%w: repository path must not start with \"-\"", helpers.ErrInvalidGitURL)
	}
	for _, r := range path {
		if !isPathRune(r) {
			return fmt.Errorf("%w: repository path carries a character this tool does not send to a remote: %q",
				helpers.ErrInvalidGitURL, r)
		}
	}
	return checkPathSegments(path)
}

// checkPathSegments refuses empty and dot segments, percent-encoded included:
// the remote resolves "/org/../other" while a credential binding matches the
// path as written, so a dot segment could spend a credential on another prefix.
func checkPathSegments(path string) error {
	for segment := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		folded := strings.ReplaceAll(strings.ToLower(segment), "%2e", ".")
		if folded == "" || folded == "." || folded == ".." {
			return fmt.Errorf("%w: repository path carries an empty or dot segment", helpers.ErrInvalidGitURL)
		}
	}
	return nil
}

func isPathRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '/', r == '.', r == '-', r == '_', r == '~', r == '+', r == '%', r == '@':
		return true
	default:
		return false
	}
}

func defaultPort(scheme string) string {
	switch scheme {
	case schemeHTTPS:
		return "443"
	case schemeHTTP:
		return "80"
	case schemeSSH:
		return "22"
	default:
		return ""
	}
}

// String renders the canonical spelling: user@host:path for the scp-like
// form, scheme://[user@]host[:port]path otherwise. A URL never carries a
// password, so the result is always safe to persist and to print.
func (u URL) String() string {
	if u.SCPLike {
		return u.User + "@" + u.Host + ":" + u.Path
	}
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString("://")
	if u.User != "" {
		b.WriteString(u.User)
		b.WriteByte('@')
	}
	b.WriteString(u.Host)
	if u.Port != "" {
		b.WriteByte(':')
		b.WriteString(u.Port)
	}
	b.WriteString(u.Path)
	return b.String()
}

// Origin is scheme://host:port with the default port filled in, the value
// two URLs must share for one credential to apply to both.
func (u URL) Origin() string {
	port := u.Port
	if port == "" {
		port = defaultPort(u.Scheme)
	}
	return u.Scheme + "://" + u.Host + ":" + port
}

// IsLoopback reports whether the host is "localhost" or a loopback literal,
// the one case a plaintext http credential is tolerated for; a DNS name never
// counts, since it resolves wherever its owner points it.
func (u URL) IsLoopback() bool {
	host := strings.Trim(u.Host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SplitSCM splits a requirement pointer into raw URL, ref and subdir in the
// order ansible's parse_scm does: ",<ref>" is cut first and wins over version,
// then one "git+" is dropped, then "#<subdir>" is cut at the first "#".
func SplitSCM(spec, version string) (string, string, string) {
	rawURL := strings.TrimSpace(spec)
	ref := strings.TrimSpace(version)
	subdir := ""
	if before, after, ok := strings.Cut(rawURL, ","); ok {
		rawURL, ref = before, after
	} else if ref == "" || ref == "*" {
		ref = headRef
	}
	if len(rawURL) >= len(gitPlusPrefix) && strings.EqualFold(rawURL[:len(gitPlusPrefix)], gitPlusPrefix) {
		rawURL = rawURL[len(gitPlusPrefix):]
	}
	if before, after, ok := strings.Cut(rawURL, "#"); ok {
		rawURL, subdir = before, strings.Trim(after, "/")
	}
	return rawURL, ref, subdir
}

// IsPointer reports whether value is what ansible auto-detects as a git source
// with no type: a case-insensitive "git+" or "git@" prefix, never a bare
// https:// URL, which ansible reads as a tarball.
func IsPointer(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, gitPlusPrefix) || strings.HasPrefix(lower, "git@")
}
