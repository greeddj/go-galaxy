package helpers

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/greeddj/go-galaxy/internal/safeout"
)

// IsPathElement reports whether name is one safe path segment: not empty,
// "." or "..", no separator, and no safeout.IsUnsafeRune rune (\n and \t
// included), so a value built only from such segments prints safely with %s.
func IsPathElement(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsRune(name, '/') {
		return false
	}
	// Covers the Windows backslash separator; unreachable on POSIX, where
	// os.PathSeparator is '/' and the check above already handles it.
	if os.PathSeparator != '/' && strings.ContainsRune(name, os.PathSeparator) {
		return false
	}
	// Unreachable on POSIX given the guards above; kept as a backstop for a
	// platform whose filepath.Base rewrites a separator-free name.
	if filepath.Base(name) != name {
		return false
	}
	return !strings.ContainsFunc(name, safeout.IsUnsafeRune)
}

// WithinDir reports whether target is base itself or lies inside base once
// both are cleaned: a defense-in-depth recheck before a destructive
// filesystem operation, even on a path built from validated elements.
func WithinDir(base, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}
