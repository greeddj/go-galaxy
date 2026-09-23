package urlsource

import (
	"fmt"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// LocatorPrefix marks a persisted source string as a url locator. It is the
// one durable signal a consumer holding only a Source string has that the
// string is neither a Galaxy server base nor a git locator.
const LocatorPrefix = "url+"

const (
	locatorPinSep = "#"
	locatorPinTag = "sha256:"
)

// Locator identifies a url source: the canonical tarball URL and the sha256 of
// the artifact it served ("" before resolution). Its String form is what every
// persisted source record carries.
type Locator struct {
	URL    string
	SHA256 string
}

// String renders url+<url>#[sha256:<hex>]. The "#" is always written so the
// grammar stays unambiguous: a canonical URL never contains "#" (ParseURL
// refuses a fragment outright), so the pin is exactly the suffix after it.
func (l Locator) String() string {
	var b strings.Builder
	b.WriteString(LocatorPrefix)
	b.WriteString(l.URL)
	b.WriteString(locatorPinSep)
	if l.SHA256 != "" {
		b.WriteString(locatorPinTag)
		b.WriteString(l.SHA256)
	}
	return b.String()
}

// Pinned reports whether the locator carries a sha256.
func (l Locator) Pinned() bool { return l.SHA256 != "" }

// IsLocator reports whether s is a url locator rather than a Galaxy server
// base or a git locator: the cheap prefix test every Source-holding consumer
// dispatches on.
func IsLocator(s string) bool {
	return strings.HasPrefix(s, LocatorPrefix)
}

// ParseLocator parses the String form back, accepting only the canonical
// spelling: a URL that round-trips through ParseURL and an optional lowercase
// sha256 pin. Anything else, a hand-edited record, is ErrInvalidURLLocator.
func ParseLocator(s string) (Locator, error) {
	if !IsLocator(s) {
		return Locator{}, fmt.Errorf("%w: missing %q prefix", helpers.ErrInvalidURLLocator, LocatorPrefix)
	}
	rawURL, pin, ok := strings.Cut(s[len(LocatorPrefix):], locatorPinSep)
	if !ok {
		return Locator{}, fmt.Errorf("%w: missing %q separator", helpers.ErrInvalidURLLocator, locatorPinSep)
	}
	u, err := ParseURL(rawURL)
	if err != nil || u.String() != rawURL {
		return Locator{}, fmt.Errorf("%w: url part is not canonical", helpers.ErrInvalidURLLocator)
	}
	if pin == "" {
		return Locator{URL: rawURL}, nil
	}
	sha, tagged := strings.CutPrefix(pin, locatorPinTag)
	if !tagged || !helpers.IsSHA256Hex(sha) {
		return Locator{}, fmt.Errorf("%w: pin is not sha256: followed by a lowercase 64-hex digest",
			helpers.ErrInvalidURLLocator)
	}
	return Locator{URL: rawURL, SHA256: sha}, nil
}

// PinKey is the store key of a url pin: what the URL last served. It is the
// canonical URL alone, because the URL is the whole requirement line - a url
// source has no ref or subdir dimension for the key to carry.
func PinKey(url string) string { return url }
