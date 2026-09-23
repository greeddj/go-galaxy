package urlsource

import (
	"errors"
	"net/url"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// testReleaseAsset is a real GitHub release-asset URL shape, already in its
// canonical spelling.
const testReleaseAsset = "https://github.com/StephenSorriaux/ansible-kafka-admin" +
	"/releases/download/0.24.0/StephenSorriaux-ansible_kafka_admin-0.24.0.tar.gz"

func TestParseURLAccepted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "github release asset", raw: testReleaseAsset, want: testReleaseAsset},
		{name: "scheme and host fold to lowercase", raw: "HTTPS://Host/x.tar.gz", want: "https://host/x.tar.gz"},
		{name: "default port dropped", raw: "https://h:443/x.tar.gz", want: "https://h/x.tar.gz"},
		{name: "explicit port kept", raw: "http://h:8080/x.tar.gz", want: "http://h:8080/x.tar.gz"},
		{name: "query kept", raw: "https://h/x.tar.gz?token=abc&v=1", want: "https://h/x.tar.gz?token=abc&v=1"},
		{name: "query on bare slash", raw: "https://h/?a=1", want: "https://h/?a=1"},
		{name: "query with no path", raw: "https://h?a=1", want: "https://h?a=1"},
		{name: "ipv6 host", raw: "https://[::1]:8443/x.tar.gz", want: "https://[::1]:8443/x.tar.gz"},
		{name: "space canonicalizes percent-encoded", raw: "https://h/a b.tar.gz", want: "https://h/a%20b.tar.gz"},
		{name: "sub-delims in path", raw: "https://h/v(1),final=x!/x.tar.gz", want: "https://h/v(1),final=x!/x.tar.gz"},
		{name: "surrounding space trimmed", raw: "  https://h/x.tar.gz  ", want: "https://h/x.tar.gz"},
		{
			name: "caching-proxy path embeds an upstream url",
			raw:  "http://cacheproxy.mirror.example.com/" + testReleaseAsset,
			want: "http://cacheproxy.mirror.example.com/" + testReleaseAsset,
		},
		{
			name: "caching-proxy mounted under a path prefix",
			raw:  "https://front/mirror/" + testReleaseAsset,
			want: "https://front/mirror/" + testReleaseAsset,
		},
		{
			name: "chained caching proxies",
			raw:  "http://p1/http://p2/https://h/x.tar.gz",
			want: "http://p1/http://p2/https://h/x.tar.gz",
		},
		{
			name: "proxy query stays the outer url's",
			raw:  "http://proxy/https://h/x.tar.gz?sig=1",
			want: "http://proxy/https://h/x.tar.gz?sig=1",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u, err := ParseURL(tt.raw)
			if err != nil {
				t.Fatalf("ParseURL(%q): %v", tt.raw, err)
			}
			if got := u.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
			again, err := ParseURL(u.String())
			if err != nil || again != u {
				t.Fatalf("canonical form does not round-trip: %+v vs %+v (%v)", again, u, err)
			}
			if !IsHTTPURL(tt.raw) {
				t.Fatalf("IsHTTPURL(%q) = false", tt.raw)
			}
		})
	}
}

func TestParseURLRefused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		want error
		name string
		raw  string
	}{
		{name: "empty", raw: "", want: helpers.ErrInvalidURLRequirement},
		{name: "not absolute", raw: "h/x.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "ftp scheme", raw: "ftp://h/x.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "ssh scheme", raw: "ssh://git@h/r", want: helpers.ErrInvalidURLRequirement},
		{name: "missing host", raw: "https:///x.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "origin only", raw: "https://h", want: helpers.ErrInvalidURLRequirement},
		{name: "origin only with slash", raw: "https://h/", want: helpers.ErrInvalidURLRequirement},
		{name: "origin only with bare query marker", raw: "https://h/?", want: helpers.ErrInvalidURLRequirement},
		{name: "fragment", raw: "https://h/x.tar.gz#frag", want: helpers.ErrInvalidURLRequirement},
		{name: "empty fragment", raw: "https://h/x.tar.gz#", want: helpers.ErrInvalidURLRequirement},
		{name: "control rune", raw: "https://h/x\x01.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "quote in path", raw: "https://h/a'b.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "dot segment", raw: "https://h/../x.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "encoded dot segment", raw: "https://h/a/%2e%2e/x.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "empty segment", raw: "https://h//x.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "empty segment before embedded url", raw: "http://proxy//https://h/x.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "embedded url with uppercase host", raw: "http://proxy/https://GitHub.com/x.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "embedded url with uppercase scheme", raw: "http://proxy/HTTPS://h/x.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "embedded url with default port", raw: "http://proxy/https://h:443/x.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "embedded url with dot segment", raw: "http://proxy/https://h/../x.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "embedded url naming only an origin", raw: "http://proxy/https://h", want: helpers.ErrInvalidURLRequirement},
		{name: "embedded ftp url", raw: "http://proxy/ftp://h/x.tar.gz", want: helpers.ErrInvalidURLRequirement},
		{name: "embedded url with userinfo", raw: "http://proxy/https://u@h/x.tar.gz", want: helpers.ErrURLRequirementUserinfo},
		{name: "angle bracket in query", raw: "https://h/x.tar.gz?a=<b>", want: helpers.ErrInvalidURLRequirement},
		//nolint:gosec // the userinfo is the fixture under test, not a credential
		{name: "userinfo", raw: "https://user:pw@h/x.tar.gz", want: helpers.ErrURLRequirementUserinfo},
		{name: "bare user", raw: "https://user@h/x.tar.gz", want: helpers.ErrURLRequirementUserinfo},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseURL(tt.raw); !errors.Is(err, tt.want) {
				t.Fatalf("ParseURL(%q) error = %v, want %v", tt.raw, err, tt.want)
			}
		})
	}
}

func TestIsHTTPURL(t *testing.T) {
	t.Parallel()
	for _, yes := range []string{"https://h/x", "HTTP://h/x", "  https://h  "} {
		if !IsHTTPURL(yes) {
			t.Fatalf("IsHTTPURL(%q) = false", yes)
		}
	}
	for _, no := range []string{"", "h/x.tar.gz", "git+https://h/r.git", "git@h:r.git", "ftp://h/x", "httpss://h/x"} {
		if IsHTTPURL(no) {
			t.Fatalf("IsHTTPURL(%q) = true", no)
		}
	}
}

func TestParsePrefixAccepted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "origin only", raw: "https://h", want: "https://h"},
		{name: "trailing slash trimmed", raw: "https://h/", want: "https://h"},
		{name: "path prefix", raw: "https://h/org", want: "https://h/org"},
		{name: "path prefix trailing slash", raw: "https://h/org/", want: "https://h/org"},
		{name: "port kept", raw: "http://localhost:8080/x", want: "http://localhost:8080/x"},
		{name: "default port dropped", raw: "https://H:443/Org", want: "https://h/Org"},
		{name: "ipv6", raw: "https://[::1]", want: "https://[::1]"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, err := ParsePrefix(tt.raw)
			if err != nil {
				t.Fatalf("ParsePrefix(%q): %v", tt.raw, err)
			}
			if got := p.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParsePrefixRefused(t *testing.T) {
	t.Parallel()
	refused := []string{
		"",
		"h/org",            // not absolute
		"ssh://h",          // scheme
		"git@h:o/r.git",    // scp-like spelling
		"ftp://h",          // scheme
		"https://u@h",      // userinfo
		"https://u:p@h",    // userinfo with password
		"https://h/x?y=1",  // query
		"https://h/x#f",    // fragment
		"https://h/x#",     // empty fragment
		"https://h/a/../b", // dot segment
		"https://h/a//b",   // empty segment
		"https:///x",       // missing host
		"https://h/a'b",    // rune outside the alphabet
	}
	for _, raw := range refused {
		if _, err := ParsePrefix(raw); !errors.Is(err, helpers.ErrURLCredentialInvalid) {
			t.Fatalf("ParsePrefix(%q) error = %v, want ErrURLCredentialInvalid", raw, err)
		}
	}
}

// TestPrefixOriginMatchesHelpersOrigin pins the contract the credential match
// rests on: Prefix.Origin() and helpers.Origin render one origin byte for byte,
// default port filled in and IPv6 brackets stripped on both sides.
func TestPrefixOriginMatchesHelpersOrigin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		binding string
		request string
	}{
		{binding: "https://h", request: "https://h/x.tar.gz"},
		{binding: "https://h:443/org", request: "https://h/x.tar.gz"},
		{binding: "http://localhost:8080", request: "http://localhost:8080/x.tar.gz"},
		{binding: "https://[::1]", request: "https://[::1]/x.tar.gz"},
		{binding: "https://[::1]:8443/org", request: "https://[::1]:8443/x.tar.gz"},
	}
	for _, tt := range cases {
		p, err := ParsePrefix(tt.binding)
		if err != nil {
			t.Fatalf("ParsePrefix(%q): %v", tt.binding, err)
		}
		req, err := url.Parse(tt.request)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", tt.request, err)
		}
		if got, want := p.Origin(), helpers.Origin(req); got != want {
			t.Fatalf("Origin(%q) = %q, helpers.Origin(%q) = %q", tt.binding, got, tt.request, want)
		}
	}
}

func TestPrefixIsLoopback(t *testing.T) {
	t.Parallel()
	for _, yes := range []string{"http://localhost", "http://127.0.0.1:8080", "http://[::1]"} {
		p, err := ParsePrefix(yes)
		if err != nil {
			t.Fatalf("ParsePrefix(%q): %v", yes, err)
		}
		if !p.IsLoopback() {
			t.Fatalf("IsLoopback(%q) = false", yes)
		}
	}
	for _, no := range []string{"http://h", "http://127.example.com", "http://localhost.example.com"} {
		p, err := ParsePrefix(no)
		if err != nil {
			t.Fatalf("ParsePrefix(%q): %v", no, err)
		}
		if p.IsLoopback() {
			t.Fatalf("IsLoopback(%q) = true", no)
		}
	}
}
