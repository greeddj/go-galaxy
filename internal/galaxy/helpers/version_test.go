package helpers

import (
	"testing"

	"github.com/Masterminds/semver/v3"
)

// TestIsExactVersion pins the accept/reject boundary: lenient shapes a registry
// may publish (short, "v"-prefixed, leading zero) pass, while ranges,
// traversal fragments and a trailing newline are refused.
func TestIsExactVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"exact three-part version", "1.0.0", true},
		{"short two-part version", "1.0", true},
		{"bare major version", "1", true},
		{"leading v prefix", "v1.0.0", true},
		{"leading zero in major", "01.0.0", true},
		{"wildcard constraint", "*", false},
		{"bare letter", "x", false},
		{"range constraint", ">=1.0.0", false},
		{"named tag", "latest", false},
		{"whitespace padded", " 1.0.0 ", false},
		{"empty", "", false},
		{"parent directory", "..", false},
		{"embedded traversal", "1.0.0/../x", false},
		{"trailing newline", "1.0.0\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsExactVersion(tc.value); got != tc.want {
				t.Errorf("IsExactVersion(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// exactVersionPathAlphabet holds every rune class either semver grammar treats
// specially plus the path separators, NUL, a control byte, space and "*".
//
//nolint:gochecknoglobals // a fixed, immutable alphabet consumed by one test, not mutable shared state.
var exactVersionPathAlphabet = []rune{'0', '1', 'v', '.', '-', '+', '/', '\\', ' ', '\n', '\x00', '*'}

// exactVersionPathAlphabetMaxLen bounds the exhaustive walk so it finishes in a
// fraction of a second and runs in every go test, not as an opt-in fuzz pass.
const exactVersionPathAlphabetMaxLen = 5

// TestIsExactVersionImpliesIsPathElementExhaustive pins the implication callers
// rely on to skip IsPathElement, under both CoerceNewVersion settings. Not
// parallel: it flips that package global, which parallel tests here read.
func TestIsExactVersionImpliesIsPathElementExhaustive(t *testing.T) {
	origCoerce := semver.CoerceNewVersion
	defer func() { semver.CoerceNewVersion = origCoerce }()

	var checked int
	for _, coerce := range []bool{true, false} {
		semver.CoerceNewVersion = coerce
		for length := 1; length <= exactVersionPathAlphabetMaxLen; length++ {
			walkAlphabetStrings(exactVersionPathAlphabet, length, func(s string) {
				checked++
				if IsExactVersion(s) && !IsPathElement(s) {
					t.Fatalf("CoerceNewVersion=%v: IsExactVersion(%q) is true but IsPathElement(%q) is false: the implication does not hold",
						coerce, s, s)
				}
			})
		}
	}
	t.Logf("checked %d strings (both CoerceNewVersion settings, up to length %d over a %d-rune alphabet)",
		checked, exactVersionPathAlphabetMaxLen, len(exactVersionPathAlphabet))
}

// walkAlphabetStrings calls visit once for every string of exactly length runes
// drawn with repetition from alphabet; length 0 visits "" once.
func walkAlphabetStrings(alphabet []rune, length int, visit func(string)) {
	if length == 0 {
		visit("")
		return
	}
	indices := make([]int, length)
	buf := make([]rune, length)
	for {
		for i, idx := range indices {
			buf[i] = alphabet[idx]
		}
		visit(string(buf))
		pos := length - 1
		for pos >= 0 {
			indices[pos]++
			if indices[pos] < len(alphabet) {
				break
			}
			indices[pos] = 0
			pos--
		}
		if pos < 0 {
			return
		}
	}
}

// FuzzIsExactVersionImpliesIsPathElement extends the exhaustive test past its
// fixed alphabet and length under go test -fuzz; a plain go test replays only
// the seeds, which add shapes the alphabet lacks such as a multi-byte rune.
func FuzzIsExactVersionImpliesIsPathElement(f *testing.F) {
	seeds := []string{
		"1.0.0", "1.0", "1", "v1.0.0", "01.0.0", "1.2.3-beta.1+build.5",
		"*", "x", ">=1.0.0", "latest", " 1.0.0 ", "", "..", "1.0.0/../x", "1.0.0\n",
		"../etc/passwd", "1.0.0\r", "1.0.0/", "/1.0.0", "1.0.0\\x",
		"\x00", "\x1b", "\x7f", "é", "1.0.0é", "~", ":", "1:0:0",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if IsExactVersion(s) && !IsPathElement(s) {
			t.Fatalf("IsExactVersion(%q) is true but IsPathElement(%q) is false: the implication does not hold", s, s)
		}
	})
}
