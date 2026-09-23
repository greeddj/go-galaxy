package helpers

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// IsCollectionNamePart reports whether part is a namespace or name half under
// ^[a-z][a-z0-9_]*$, the alphabet Galaxy servers accept. It is deliberately
// separate from IsPathElement: naming policy and path safety evolve apart.
func IsCollectionNamePart(part string) bool {
	if part == "" {
		return false
	}
	for i, r := range part {
		switch {
		case r >= 'a' && r <= 'z':
		case i > 0 && (r >= '0' && r <= '9' || r == '_'):
		default:
			return false
		}
	}
	return true
}

// IsCollectionName reports whether value is a well-formed collection name:
// exactly two dot-separated halves, each satisfying IsCollectionNamePart.
func IsCollectionName(value string) bool {
	namespace, name, ok := SplitFQDN(value)
	return ok && IsCollectionNamePart(namespace) && IsCollectionNamePart(name)
}

// IsURLCollectionNamePart reports whether part is one half of a url
// collection's identity under ansible's FQCN word rule [A-Za-z0-9_]+, wider
// than the Galaxy alphabet because real url artifacts carry mixed-case names.
func IsURLCollectionNamePart(part string) bool {
	if part == "" {
		return false
	}
	for _, r := range part {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// SplitFQDN splits a "namespace.collection" string at its one dot into two
// non-empty halves. It checks shape only, no alphabet and no path safety.
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

// NormalizeConstraint trims a version constraint for Masterminds/semver v3,
// maps "" and "*" to match-all, and rewrites ansible's "==" clause operator
// to "=", which v3 needs; a constraint without "==" is returned as trimmed.
func NormalizeConstraint(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "*" {
		return ""
	}
	if !strings.Contains(trimmed, "==") {
		return trimmed
	}
	parts := strings.Split(trimmed, ",")
	for i, part := range parts {
		p := strings.TrimSpace(part)
		// "===" (or any run of more than two '=') is left untouched: it is not
		// ansible's exact-match operator and must still fail to parse as an
		// unrecognized operator rather than be silently coerced into "=".
		if strings.HasPrefix(p, "==") && !strings.HasPrefix(p, "===") {
			p = "=" + p[2:]
		}
		parts[i] = p
	}
	return strings.Join(parts, ",")
}
