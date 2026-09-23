// Package safeout replaces terminal control characters in text of external
// origin with U+FFFD before it is printed, so hostile input shows up as visible
// garbage instead of acting as an invisible command.
package safeout

import (
	"io"
	"strings"
	"unicode/utf8"
)

// Text is a string that has passed through Clean. Every typed string, so all
// run-time input, needs an explicit conversion to become one; an untyped string
// constant is still assignable, so this is a structural check, not a proof.
type Text string

// Clean replaces with U+FFFD each C0 and C1 control, DEL, U+2028, U+2029 and
// invalid UTF-8 byte, but keeps \n (errors.Join output), \t and bidi controls.
// It replaces rather than deletes, so hostile input stays visible.
func Clean(s string) Text {
	return Text(strings.Map(sanitizeRune, s))
}

// Boundaries of what sanitizeRune replaces: C0 below controlC0Max, DEL, C1 in
// [controlC1Min, controlC1Max], and the Unicode line and paragraph separators.
const (
	controlC0Max       = 0x20
	del                = 0x7f
	controlC1Min       = 0x80
	controlC1Max       = 0x9f
	lineSeparator      = '\u2028'
	paragraphSeparator = '\u2029'
)

// isControl reports whether r is C0 (below controlC0Max), DEL or C1. It is one
// half of IsUnsafeRune, kept separate so that its name stays true.
func isControl(r rune) bool {
	return r < controlC0Max || r == del || (r >= controlC1Min && r <= controlC1Max)
}

// isLineTerminator reports whether r is U+2028 or U+2029, which Clean replaces
// because Unicode-aware consumers such as Python's str.splitlines() break lines
// on them. It is the other half of IsUnsafeRune.
func isLineTerminator(r rune) bool {
	return r == lineSeparator || r == paragraphSeparator
}

// IsUnsafeRune reports whether r is isControl or isLineTerminator, the set Clean
// replaces apart from \n and \t. It is the one place to add a codepoint class:
// Clean, NewWriter and helpers.IsPathElement all consult it.
func IsUnsafeRune(r rune) bool {
	return isControl(r) || isLineTerminator(r)
}

// sanitizeRune is strings.Map's per-rune callback for Clean.
func sanitizeRune(r rune) rune {
	switch {
	case r == '\n' || r == '\t':
		return r
	case IsUnsafeRune(r):
		return utf8.RuneError
	default:
		return r
	}
}

// NewWriter wraps w so that each Write is sanitized through Clean on its own.
// No split lets a control through, but a multi-byte rune split across Writes
// becomes one U+FFFD per byte, so callers write whole lines.
func NewWriter(w io.Writer) io.Writer {
	return &writer{w: w}
}

// writer is the io.Writer NewWriter returns.
type writer struct {
	w io.Writer
}

// Write sanitizes p through Clean into the underlying writer. It returns 0 and
// that writer's error, or len(p) and nil: the pre-sanitization length, since
// io.Writer requires an error whenever n < len(p).
func (s *writer) Write(p []byte) (int, error) {
	if _, err := io.WriteString(s.w, string(Clean(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}
