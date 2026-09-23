package safeout

import (
	"errors"
	"testing"
	"unicode/utf8"
)

// replacement is U+FFFD, spelled once so every expectation below reads as
// "the replacement character" rather than a bare escape.
const replacement = "\ufffd"

// errUnderlyingWrite is a static sentinel for
// TestNewWriterPropagatesUnderlyingError, in place of an inline
// errors.New call at the assertion site (err113).
var errUnderlyingWrite = errors.New("boom")

// TestCleanTable pins Clean's output at every boundary it draws, and that the
// result is always valid UTF-8 whatever the input.
func TestCleanTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		// Positive control: proves Clean returns content at all, not just
		// an empty or replaced string for anything it is given.
		{"benign ASCII unchanged", "hello world", "hello world"},
		{"newline kept", "a\nb", "a\nb"},
		{"tab kept", "a\tb", "a\tb"},
		{"space kept, unit separator replaced (the C0 boundary)", "a \x1fb", "a " + replacement + "b"},
		{"ESC", "a\x1bb", "a" + replacement + "b"},
		{"lone CR", "a\rb", "a" + replacement + "b"},
		{"CRLF becomes replacement plus newline", "a\r\nb", "a" + replacement + "\nb"},
		{"NUL", "a\x00b", "a" + replacement + "b"},
		{"DEL", "a\x7fb", "a" + replacement + "b"},
		// U+009B (CSI) encoded as valid two-byte UTF-8 (0xC2 0x9B).
		{"U+009B encoded as valid UTF-8", "a\u009bb", "a" + replacement + "b"},
		{"U+0080 range boundary (low)", "a\u0080b", "a" + replacement + "b"},
		{"U+009F range boundary (high)", "a\u009fb", "a" + replacement + "b"},
		{"raw byte 0x9b (invalid UTF-8)", "a\x9bb", "a" + replacement + "b"},
		{"raw byte 0x80 (invalid UTF-8)", "a\x80b", "a" + replacement + "b"},
		{"truncated 0xc2 lead byte at end of string", "a\xc2", "a" + replacement},
		{"U+00A0 NBSP kept (just above the C1 boundary)", "a\u00a0b", "a\u00a0b"},
		// U+2028 and U+2029 are replaced though they sit outside every control
		// range, unlike the bidi controls Clean keeps: both terminate lines.
		{"U+2028 LINE SEPARATOR replaced", "a\u2028b", "a" + replacement + "b"},
		{"U+2029 PARAGRAPH SEPARATOR replaced", "a\u2029b", "a" + replacement + "b"},
		{"legitimately encoded U+FFFD passes through unchanged", "a" + replacement + "b", "a" + replacement + "b"},
		{"box-drawing runes kept", "\u2502\u251c\u2500\u2500", "\u2502\u251c\u2500\u2500"},
		{"emoji kept", "hello \U0001F600", "hello \U0001F600"},
		{"empty string", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Clean(tt.in)
			if string(got) != tt.want {
				t.Fatalf("Clean(%q) = %q, want %q", tt.in, string(got), tt.want)
			}
			if !utf8.ValidString(string(got)) {
				t.Fatalf("Clean(%q) = %q is not valid UTF-8", tt.in, string(got))
			}
		})
	}
}

// TestIsControlTable pins isControl's exact boundary: C0, DEL and C1 report
// true, the rune just past each boundary and U+2028/U+2029 report false.
func TestIsControlTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		r    rune
		want bool
	}{
		{"NUL", '\x00', true},
		{"unit separator (C0 high boundary)", '\x1f', true},
		{"space (just past the C0 boundary)", ' ', false},
		{"newline", '\n', true},
		{"tab", '\t', true},
		{"DEL", '\x7f', true},
		{"C1 low boundary U+0080", '\u0080', true},
		{"C1 high boundary U+009f", '\u009f', true},
		{"NBSP U+00a0 (just past the C1 boundary)", '\u00a0', false},
		{"line separator U+2028 (isLineTerminator's concern, not this one)", '\u2028', false},
		{"paragraph separator U+2029 (isLineTerminator's concern, not this one)", '\u2029', false},
		{"ordinary ASCII letter", 'a', false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isControl(tc.r); got != tc.want {
				t.Errorf("isControl(%U) = %v, want %v", tc.r, got, tc.want)
			}
		})
	}
}

// TestIsLineTerminatorTable pins isLineTerminator to exactly the two runes
// it exists for, rejecting a handful of neighbors that could plausibly be
// mistaken for it.
func TestIsLineTerminatorTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		r    rune
		want bool
	}{
		{"line separator U+2028", '\u2028', true},
		{"paragraph separator U+2029", '\u2029', true},
		{"just below U+2028", '\u2027', false},
		{"just above U+2029", '\u202a', false},
		{"newline is not a line terminator by this predicate", '\n', false},
		{"NUL is not a line terminator by this predicate", '\x00', false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isLineTerminator(tc.r); got != tc.want {
				t.Errorf("isLineTerminator(%U) = %v, want %v", tc.r, got, tc.want)
			}
		})
	}
}

// TestIsUnsafeRuneTable pins IsUnsafeRune to exactly the union of isControl
// and isLineTerminator: every rune either alone reports true carries through
// to the union, and a rune neither reports true on stays false here too.
func TestIsUnsafeRuneTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		r    rune
		want bool
	}{
		{"NUL (isControl's concern)", '\x00', true},
		{"DEL (isControl's concern)", '\x7f', true},
		{"C1 low boundary U+0080 (isControl's concern)", '\u0080', true},
		{"line separator U+2028 (isLineTerminator's concern)", '\u2028', true},
		{"paragraph separator U+2029 (isLineTerminator's concern)", '\u2029', true},
		{"newline (neither predicate's concern)", '\n', true},
		{"tab (neither predicate's concern)", '\t', true},
		{"NBSP U+00a0 (neither predicate's concern)", '\u00a0', false},
		{"ordinary ASCII letter", 'a', false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsUnsafeRune(tc.r); got != tc.want {
				t.Errorf("IsUnsafeRune(%U) = %v, want %v", tc.r, got, tc.want)
			}
		})
	}
}

// TestCleanRawC1ByteVersusEncodedC1Rune pins both C1 spellings: a raw 0x9b is
// replaced by strings.Map as invalid UTF-8, and only the encoded U+009B reaches
// sanitizeRune's C1 arm, so neither TestCleanTable row is redundant.
func TestCleanRawC1ByteVersusEncodedC1Rune(t *testing.T) {
	t.Parallel()
	raw := Clean("\x9b")
	encoded := Clean("\u009b")
	if string(raw) != replacement || string(encoded) != replacement {
		t.Fatalf("raw = %q, encoded = %q, want both %q", string(raw), string(encoded), replacement)
	}
}

// TestCleanReturnsInputWhenNothingToRemove pins that clean input comes back
// equal; that no copy is made is TestCleanDoesNotAllocateForCleanInput's job.
func TestCleanReturnsInputWhenNothingToRemove(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"hello world", "", "\u2502\u251c", "line one\nline two\ttabbed"} {
		if got := Clean(in); got != Text(in) {
			t.Fatalf("Clean(%q) = %q, want identical %q", in, string(got), in)
		}
	}
}

// cleanSink keeps the compiler from eliding the Clean call inside
// TestCleanDoesNotAllocateForCleanInput.
//
//nolint:gochecknoglobals // required by testing.AllocsPerRun's own pattern: a local sink can be optimized away.
var cleanSink Text

// TestCleanDoesNotAllocateForCleanInput pins zero allocations for input that
// needs no replacing. testing.AllocsPerRun panics in a parallel test, so
// neither this test nor its subtests call t.Parallel.
func TestCleanDoesNotAllocateForCleanInput(t *testing.T) {
	inputs := []struct {
		name string
		in   string
	}{
		{"ASCII", "hello world, this is a benign sixty byte test line."},
		{"box-drawing", "\u2502\u251c\u2500\u2500\u2502\u251c\u2500\u2500"},
		{"emoji", "hello \U0001F600 world"},
		{"newline and tab", "line one\nline two\ttabbed"},
		{"empty", ""},
	}

	for _, tt := range inputs {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			allocs := testing.AllocsPerRun(100, func() {
				cleanSink = Clean(in)
			})
			if allocs != 0 {
				t.Fatalf("Clean(%q) allocated %.1f times per run, want 0", in, allocs)
			}
		})
	}
}

// TestNewWriterSanitizesEachWrite pins that a hostile payload is sanitized
// before the underlying writer and a benign one passes through byte for byte.
func TestNewWriterSanitizesEachWrite(t *testing.T) {
	t.Parallel()

	t.Run("hostile", func(t *testing.T) {
		t.Parallel()
		var buf byteSliceWriter
		w := NewWriter(&buf)
		in := []byte("prefix\x1b[31mred\x00suffix")
		n, err := w.Write(in)
		if err != nil {
			t.Fatalf("Write: unexpected error %v", err)
		}
		if n != len(in) {
			t.Fatalf("n = %d, want %d (pre-sanitization length)", n, len(in))
		}
		want := "prefix" + replacement + "[31mred" + replacement + "suffix"
		if string(buf) != want {
			t.Fatalf("underlying write = %q, want %q", string(buf), want)
		}
	})

	t.Run("benign positive control", func(t *testing.T) {
		t.Parallel()
		var buf byteSliceWriter
		w := NewWriter(&buf)
		in := []byte("a perfectly ordinary line")
		n, err := w.Write(in)
		if err != nil {
			t.Fatalf("Write: unexpected error %v", err)
		}
		if n != len(in) {
			t.Fatalf("n = %d, want %d", n, len(in))
		}
		if string(buf) != string(in) {
			t.Fatalf("underlying write = %q, want byte-for-byte %q", string(buf), string(in))
		}
	})
}

// TestNewWriterCannotSmuggleAControlAcrossWrites pins that a C1 rune's two
// bytes split across Writes become two U+FFFD and never a raw 0x9b, while the
// same bytes in one Write become a single U+FFFD.
func TestNewWriterCannotSmuggleAControlAcrossWrites(t *testing.T) {
	t.Parallel()
	leadByte := []byte{0xc2}
	contByte := []byte{0x9b}

	t.Run("split across two writes", func(t *testing.T) {
		t.Parallel()
		var buf byteSliceWriter
		w := NewWriter(&buf)
		if _, err := w.Write(leadByte); err != nil {
			t.Fatalf("first Write: unexpected error %v", err)
		}
		if _, err := w.Write(contByte); err != nil {
			t.Fatalf("second Write: unexpected error %v", err)
		}
		got := []byte(buf)
		for _, b := range got {
			if b == 0x9b {
				t.Fatalf("underlying write %x contains raw byte 0x9b", got)
			}
		}
		if !utf8.Valid(got) {
			t.Fatalf("underlying write %x is not valid UTF-8", got)
		}
		if want := replacement + replacement; string(got) != want {
			t.Fatalf("split write = %q, want %q (one replacement per half)", string(got), want)
		}
	})

	t.Run("same two bytes in one write (positive control)", func(t *testing.T) {
		t.Parallel()
		var buf byteSliceWriter
		w := NewWriter(&buf)
		joined := append(append([]byte{}, leadByte...), contByte...)
		if _, err := w.Write(joined); err != nil {
			t.Fatalf("Write: unexpected error %v", err)
		}
		if want := replacement; string(buf) != want {
			t.Fatalf("joined write = %q, want %q (one replacement for the whole rune)", string(buf), want)
		}
	})
}

// errWriter is an io.Writer whose Write always fails, for
// TestNewWriterPropagatesUnderlyingError.
type errWriter struct{ err error }

func (e errWriter) Write([]byte) (int, error) {
	return 0, e.err
}

// TestNewWriterPropagatesUnderlyingError proves a failure from the
// underlying writer surfaces as 0, err rather than being swallowed or
// reported against the pre-sanitization length.
func TestNewWriterPropagatesUnderlyingError(t *testing.T) {
	t.Parallel()
	wantErr := errUnderlyingWrite
	w := NewWriter(errWriter{err: wantErr})
	n, err := w.Write([]byte("anything"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0", n)
	}
}

// byteSliceWriter is a minimal io.Writer backed by a byte slice, used
// instead of bytes.Buffer to keep this test file's import list to
// exactly what safeout.go itself needs plus the testing package.
type byteSliceWriter []byte

func (b *byteSliceWriter) Write(p []byte) (int, error) {
	*b = append(*b, p...)
	return len(p), nil
}
