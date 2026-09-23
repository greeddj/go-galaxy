package helpers

import (
	"strings"
	"testing"
)

// validSHA256Hex is a real 64-character lowercase hex digest (a sha256 of
// "hello world"), used as the one accepted shape in TestIsSHA256Hex.
const validSHA256Hex = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"

// TestIsSHA256Hex pins the exact shape IsSHA256Hex accepts and rejects,
// including the ".." and "." shapes markerRel relies on it to refuse.
func TestIsSHA256Hex(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"valid lowercase hex", validSHA256Hex, true},
		{"empty", "", false},
		{"one char short", validSHA256Hex[:63], false},
		{"one char long", validSHA256Hex + "a", false},
		{"uppercase hex rejected", strings.ToUpper(validSHA256Hex), false},
		{"one invalid hex character", validSHA256Hex[:63] + "g", false},
		{"path traversal fragment", "../../x", false},
		{"single dot", ".", false},
		{"double dot", "..", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsSHA256Hex(tc.value); got != tc.want {
				t.Errorf("IsSHA256Hex(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
