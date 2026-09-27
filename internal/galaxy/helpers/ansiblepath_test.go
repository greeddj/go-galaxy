package helpers

import (
	"os"
	"os/user"
	"strings"
	"testing"
)

// unsetEnvForTest unsets key for the rest of the test, restoring it after;
// t.Setenv registers the restore, which os.Unsetenv alone would not.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("os.Unsetenv(%q) error = %v, want nil", key, err)
	}
}

// TestExpandAnsiblePath pins ExpandAnsiblePath to Python's os.path.expandvars
// then os.path.expanduser, row by row as python3.14 prints them for the same
// environment. Not parallel: t.Setenv.
func TestExpandAnsiblePath(t *testing.T) {
	t.Setenv("HOME", "/home/gg")
	t.Setenv("GG_EXP_A", "/opt/a")
	t.Setenv("GG_EXP_EMPTY", "")
	t.Setenv("GG_EXP_TILDE", "~")
	t.Setenv("GG_EXP_REF", "$GG_EXP_A")
	unsetEnvForTest(t, "GG_EXP_UNSET")
	unsetEnvForTest(t, "GG_EXP_Ac")

	rows := []struct{ in, want string }{
		{in: "", want: ""},
		{in: "plain/path", want: "plain/path"},
		{in: "$GG_EXP_A/c", want: "/opt/a/c"},
		{in: "${GG_EXP_A}c", want: "/opt/ac"},
		{in: "$GG_EXP_Ac", want: "$GG_EXP_Ac"},
		{in: "$GG_EXP_UNSET/c", want: "$GG_EXP_UNSET/c"},
		{in: "${GG_EXP_A", want: "${GG_EXP_A"},
		{in: "${}/c", want: "${}/c"},
		{in: "$/c", want: "$/c"},
		{in: "$$GG_EXP_A", want: "$/opt/a"},
		{in: "$GG_EXP_EMPTY/c", want: "/c"},
		{in: "$GG_EXP_REF", want: "$GG_EXP_A"},
		{in: "~", want: "/home/gg"},
		{in: "~/c", want: "/home/gg/c"},
		{in: "~/", want: "/home/gg/"},
		{in: "$GG_EXP_TILDE/r", want: "/home/gg/r"},
		{in: "a/~/c", want: "a/~/c"},
		{in: "~gg-no-such-user-x9/c", want: "~gg-no-such-user-x9/c"},
	}
	for _, row := range rows {
		if got := ExpandAnsiblePath(row.in); got != row.want {
			t.Errorf("ExpandAnsiblePath(%q) = %q, want %q", row.in, got, row.want)
		}
	}
}

// TestExpandAnsiblePathHomeEdges pins expanduser's handling of a HOME that is
// "/", empty or unset: the trailing "/" is trimmed, an empty result is "/",
// and an unset HOME falls back to the password database. Not parallel: t.Setenv.
func TestExpandAnsiblePathHomeEdges(t *testing.T) {
	t.Run("HOME is the root", func(t *testing.T) {
		t.Setenv("HOME", "/")
		for in, want := range map[string]string{"~": "/", "~/c": "/c"} {
			if got := ExpandAnsiblePath(in); got != want {
				t.Errorf("ExpandAnsiblePath(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("HOME exported empty", func(t *testing.T) {
		t.Setenv("HOME", "")
		for in, want := range map[string]string{"~": "/", "~/c": "/c"} {
			if got := ExpandAnsiblePath(in); got != want {
				t.Errorf("ExpandAnsiblePath(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("HOME unset, and ~name", func(t *testing.T) {
		current, err := user.Current()
		if err != nil || current.HomeDir == "" || current.Username == "" {
			t.Skipf("no password-database entry for this user: %v", err)
		}
		home := strings.TrimRight(current.HomeDir, "/")
		unsetEnvForTest(t, "HOME")
		if got := ExpandAnsiblePath("~/c"); got != home+"/c" {
			t.Errorf("ExpandAnsiblePath(%q) = %q, want %q", "~/c", got, home+"/c")
		}
		named := "~" + current.Username + "/c"
		if got := ExpandAnsiblePath(named); got != home+"/c" {
			t.Errorf("ExpandAnsiblePath(%q) = %q, want %q", named, got, home+"/c")
		}
	})
}
