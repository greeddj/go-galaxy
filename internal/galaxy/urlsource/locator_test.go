package urlsource

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const testSHA = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func TestLocatorRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		loc  Locator
		want string
	}{
		{
			name: "unpinned",
			loc:  Locator{URL: "https://h/x.tar.gz"},
			want: "url+https://h/x.tar.gz#",
		},
		{
			name: "pinned",
			loc:  Locator{URL: "https://h/x.tar.gz", SHA256: testSHA},
			want: "url+https://h/x.tar.gz#sha256:" + testSHA,
		},
		{
			name: "pinned with query",
			loc:  Locator{URL: "https://h/x.tar.gz?v=1", SHA256: testSHA},
			want: "url+https://h/x.tar.gz?v=1#sha256:" + testSHA,
		},
		{
			name: "pinned with port and deep path",
			loc:  Locator{URL: "http://h:8080/a/b/x.tar.gz", SHA256: testSHA},
			want: "url+http://h:8080/a/b/x.tar.gz#sha256:" + testSHA,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.loc.String()
			if got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
			if !IsLocator(got) {
				t.Fatalf("IsLocator(%q) = false", got)
			}
			parsed, err := ParseLocator(got)
			if err != nil {
				t.Fatalf("ParseLocator(%q): %v", got, err)
			}
			if parsed != tt.loc {
				t.Fatalf("ParseLocator(%q) = %+v, want %+v", got, parsed, tt.loc)
			}
			if parsed.Pinned() != (tt.loc.SHA256 != "") {
				t.Fatalf("Pinned() = %t", parsed.Pinned())
			}
		})
	}
}

func TestParseLocatorRefusals(t *testing.T) {
	t.Parallel()
	refused := []string{
		"https://h/x.tar.gz",                                        // no prefix
		"url+https://h/x.tar.gz",                                    // no "#"
		"url+HTTPS://h/x.tar.gz#",                                   // url not canonical
		"url+https://h:443/x.tar.gz#",                               // url not canonical (default port)
		"url+https://h/x.tar.gz#" + testSHA,                         // pin missing its sha256: tag
		"url+https://h/x.tar.gz#sha256:abc",                         // short digest
		"url+https://h/x.tar.gz#sha256:" + strings.ToUpper(testSHA), // uppercase digest
		"url+https://h/x.tar.gz#md5:" + testSHA,                     // wrong tag
		"url+https://u:p@h/x.tar.gz#sha256:" + testSHA,              // credential in url
		"url+https://h#sha256:" + testSHA,                           // origin-only url
		"url+https://h/a#b#sha256:" + testSHA,                       // "#" inside the url part
		"url+ftp://h/x.tar.gz#",                                     // refused scheme
	}
	for _, raw := range refused {
		if _, err := ParseLocator(raw); !errors.Is(err, helpers.ErrInvalidURLLocator) {
			t.Fatalf("ParseLocator(%q) error = %v, want ErrInvalidURLLocator", raw, err)
		}
	}
	if IsLocator("https://h/x.tar.gz") {
		t.Fatalf("IsLocator accepted a Galaxy base")
	}
	if IsLocator("git+https://h/r.git#") {
		t.Fatalf("IsLocator accepted a git locator")
	}
}

func TestPinKeyIsTheURL(t *testing.T) {
	t.Parallel()
	const u = "https://h/x.tar.gz?v=1"
	if PinKey(u) != u {
		t.Fatalf("PinKey(%q) = %q", u, PinKey(u))
	}
}
