package helpers

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SplitFQDN splits a "namespace.collection" string into parts.
func SplitFQDN(value string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(value), ".")
	if len(parts) != CollectionNameParts {
		return "", "", false
	}
	if parts[0] == "" || parts[1] == "" {
		return "", "", false
	}

	return parts[0], parts[1], true
}

// UpperFirstRune returns s with the first rune converted to upper case.
func UpperFirstRune(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size == 1 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}
