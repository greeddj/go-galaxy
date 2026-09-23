package helpers

import (
	"fmt"
	"strings"
)

// WithoutQuery returns raw up to its first "?", since a query may be a
// presigned capability. The cut is textual so it cannot fail on a value
// url.Parse refuses; userinfo survives it.
func WithoutQuery(raw string) string {
	before, _, _ := strings.Cut(raw, "?")
	return before
}

// WithoutFragment returns raw up to its first "#". A fragment never reaches
// the wire or the filesystem, so a message naming what was opened drops it;
// the cut is textual so it cannot fail.
func WithoutFragment(raw string) string {
	before, _, _ := strings.Cut(raw, "#")
	return before
}

// WithoutUserinfo cuts through the last "@" of raw's authority, textually so a
// value url.Parse refuses still loses its password. The scan stops at "/", "?"
// or "#", so the cuts compose in any order; a userinfo holding one escapes it.
func WithoutUserinfo(raw string) string {
	// authorityPrefix introduces an authority once a scheme, if any, has been
	// consumed.
	const authorityPrefix = "//"

	// A ":" reached before any of "/?#" ends a scheme; one reached after it
	// does not, so the whole delimiter set is scanned once rather than
	// searching for a ":" that may belong to a port or to userinfo.
	rest, off := raw, 0
	if i := strings.IndexAny(rest, ":/?#"); i >= 0 && rest[i] == ':' {
		rest, off = rest[i+1:], off+i+1
	}
	if strings.HasPrefix(rest, authorityPrefix) {
		rest, off = rest[len(authorityPrefix):], off+len(authorityPrefix)
	}

	authority := rest
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}

	// Returning raw itself keeps the common no-userinfo case allocation-free.
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return raw
	}

	return raw[:off] + raw[off+at+1:]
}

// WithoutCredentials cuts both credential-bearing parts of raw, query and
// userinfo: the rule for a value rendered or persisted rather than fetched.
// WithoutQuery stays one cut, since persisted signature sources rely on that.
func WithoutCredentials(raw string) string {
	return WithoutUserinfo(WithoutQuery(raw))
}

// MessageValueMaxLen bounds one attacker-influenced string in an operator
// message, about three times the longest real metadata href: such a value can
// be copied onto every signature blob and rendered once per failure.
const MessageValueMaxLen = 512

// TruncateForMessage bounds value at MessageValueMaxLen, visibly: a longer
// value keeps its first bytes and states its real length. The byte cut may
// split a rune, which safeout renders as U+FFFD at the printer.
func TruncateForMessage(value string) string {
	if len(value) <= MessageValueMaxLen {
		return value
	}

	return value[:MessageValueMaxLen] + fmt.Sprintf("... (%d bytes)", len(value))
}

// URLForMessage is the display form of a refused URL: query, fragment and
// userinfo cut, then TruncateForMessage. Cutting first is load-bearing: a
// credential whose "@" lies past the cap would otherwise survive.
func URLForMessage(raw string) string {
	return TruncateForMessage(WithoutUserinfo(WithoutFragment(WithoutQuery(raw))))
}
