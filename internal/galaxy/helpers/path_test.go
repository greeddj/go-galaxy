package helpers

import (
	"path/filepath"
	"testing"
)

func TestIsPathElement(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"empty", "", false},
		{"dot", ".", false},
		{"dotdot", "..", false},
		{"simple", "ns", true},
		{"semver with prerelease and build", "1.0.0-rc.1+build", true},
		{"contains slash", "a/b", false},
		{"absolute unix path", "/etc/passwd", false},
		{"traversal", "../../../../tmp/pwn", false},
		{"traversal single segment", "..", false},
		{"dotted name", "my.collection", true},

		// Rejected: every safeout.IsUnsafeRune rune, including \n and \t,
		// which Clean keeps for whole-line output but a path element may not.
		{"newline", "1.0.0\nX", false},
		{"tab", "1.0.0\tX", false},
		{"carriage return", "1.0.0\rX", false},
		{"escape", "1.0.0\x1bX", false},
		{"delete", "1.0.0\x7fX", false},
		{"C1 control byte", "1.0.0\u0080X", false},
		{"unicode line separator", "1.0.0\u2028X", false},
		{"unicode paragraph separator", "1.0.0\u2029X", false},

		// Positive control: ordinary identifiers, printable non-ASCII
		// included, still pass the control-character check.
		{"namespace", "ns", true},
		{"name", "name", true},
		{"exact version", "1.0.0", true},
		{"version with prerelease and build metadata", "1.0.0-rc.1+build", true},
		{"printable non-ASCII name", "café", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsPathElement(tc.value); got != tc.want {
				t.Errorf("IsPathElement(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestWithinDir(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	cases := []struct {
		name   string
		target string
		want   bool
	}{
		{"same as base", base, true},
		{"direct child", filepath.Join(base, "child"), true},
		{"nested child", filepath.Join(base, "a", "b"), true},
		{"escapes via traversal", filepath.Join(base, "..", "sibling"), false},
		{"escapes to unrelated absolute path", filepath.Join(filepath.Dir(base), "other"), false},
		{"escapes to root", "/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := WithinDir(base, tc.target); got != tc.want {
				t.Errorf("WithinDir(%q, %q) = %v, want %v", base, tc.target, got, tc.want)
			}
		})
	}
}

// TestWithinDirRelError pins that WithinDir returns false when filepath.Rel
// fails, here for a relative base against an absolute target.
func TestWithinDirRelError(t *testing.T) {
	t.Parallel()
	if got := WithinDir("relative/dir", "/abs/other"); got {
		t.Errorf("WithinDir(%q, %q) = true, want false (filepath.Rel error)", "relative/dir", "/abs/other")
	}
}
