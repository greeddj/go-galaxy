package helpers

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestNewSizeLimitedReaderWholeStream pins, through io.ReadAll, that a
// stream under or exactly at the cap reads whole and any overrun fails with
// ErrResponseTooLarge rather than truncating.
func TestNewSizeLimitedReaderWholeStream(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		data      []byte
		max       int64
		wantErr   bool
		wantBytes int
	}{
		{name: "well under the cap", data: bytes.Repeat([]byte{'a'}, 10), max: 100, wantErr: false, wantBytes: 10},
		{name: "exactly at the cap", data: bytes.Repeat([]byte{'b'}, 10), max: 10, wantErr: false, wantBytes: 10},
		{name: "one byte over the cap", data: bytes.Repeat([]byte{'c'}, 6), max: 5, wantErr: true, wantBytes: 0},
		{name: "far over the cap", data: bytes.Repeat([]byte{'d'}, 4096), max: 5, wantErr: true, wantBytes: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewSizeLimitedReader(bytes.NewReader(tc.data), tc.max)
			got, err := io.ReadAll(r)
			if tc.wantErr {
				if !errors.Is(err, ErrResponseTooLarge) {
					t.Fatalf("io.ReadAll() error = %v, want ErrResponseTooLarge", err)
				}
			} else if err != nil {
				t.Fatalf("io.ReadAll() unexpected error: %v", err)
			}
			// wantBytes < 0 skips the count: far over the cap, how much
			// io.ReadAll gets before the error depends on its buffer growth.
			if tc.wantBytes >= 0 && len(got) != tc.wantBytes {
				t.Fatalf("io.ReadAll() returned %d bytes, want %d", len(got), tc.wantBytes)
			}
			if !tc.wantErr && !bytes.Equal(got, tc.data) {
				t.Fatalf("io.ReadAll() = %q, want %q", got, tc.data)
			}
		})
	}
}

// TestSizeLimitedReaderCrossesBoundaryMidStream pins that reads under the
// cap pass through and the very call that first exceeds it returns zero
// bytes with ErrResponseTooLarge.
func TestSizeLimitedReaderCrossesBoundaryMidStream(t *testing.T) {
	t.Parallel()

	data := []byte("0123456789") // 10 bytes
	const limit = int64(7)
	r := NewSizeLimitedReader(bytes.NewReader(data), limit)
	buf := make([]byte, 3)

	n, err := r.Read(buf)
	if err != nil || n != 3 {
		t.Fatalf("first read = (%d, %v), want (3, nil)", n, err)
	}
	if got, want := string(buf[:n]), "012"; got != want {
		t.Fatalf("first read data = %q, want %q", got, want)
	}

	n, err = r.Read(buf)
	if err != nil || n != 3 {
		t.Fatalf("second read (cumulative 6, still under cap 7) = (%d, %v), want (3, nil)", n, err)
	}

	// The third read crosses the cap of 7. p is clamped so it can overrun by
	// at most one byte, and the call reports zero bytes with the error.
	n, err = r.Read(buf)
	if n != 0 {
		t.Fatalf("crossing read returned %d bytes, want 0", n)
	}
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("crossing read error = %v, want ErrResponseTooLarge", err)
	}
}

// TestSizeLimitedReaderZeroMax verifies the degenerate case of a zero-byte
// budget: any read that returns data at all overruns it immediately, and
// overruns it with zero bytes handed back.
func TestSizeLimitedReaderZeroMax(t *testing.T) {
	t.Parallel()

	r := NewSizeLimitedReader(bytes.NewReader([]byte("x")), 0)
	buf := make([]byte, 1)
	n, err := r.Read(buf)
	if n != 0 {
		t.Fatalf("Read() n = %d, want 0", n)
	}
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("Read() error = %v, want ErrResponseTooLarge", err)
	}
}

// countingReader records how many Read calls reach the underlying stream, so
// a test can prove the refusal is sticky rather than merely repeated.
type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

// TestSizeLimitedReaderSurvivesSatisfyingCallers pins that io.ReadFull and
// io.CopyN, which drop an error beside a satisfied request, still see the
// refusal, and that the refusal is sticky.
func TestSizeLimitedReaderSurvivesSatisfyingCallers(t *testing.T) {
	t.Parallel()

	const ceiling = int64(8)
	data := bytes.Repeat([]byte{'z'}, 32)

	t.Run("io.ReadFull cannot erase the refusal", func(t *testing.T) {
		t.Parallel()
		r := NewSizeLimitedReader(bytes.NewReader(data), ceiling)
		buf := make([]byte, ceiling+1)

		n, err := io.ReadFull(r, buf)

		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("io.ReadFull over a cap of %d = (%d, %v), want an ErrResponseTooLarge", ceiling, n, err)
		}
	})

	t.Run("io.CopyN cannot erase the refusal", func(t *testing.T) {
		t.Parallel()
		r := NewSizeLimitedReader(bytes.NewReader(data), ceiling)

		n, err := io.CopyN(io.Discard, r, ceiling+1)

		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("io.CopyN over a cap of %d = (%d, %v), want an ErrResponseTooLarge", ceiling, n, err)
		}
	})

	t.Run("the refusal is sticky", func(t *testing.T) {
		t.Parallel()
		counting := &countingReader{r: bytes.NewReader(data)}
		r := NewSizeLimitedReader(counting, ceiling)
		buf := make([]byte, 32)

		// Drain until the cap fires, then keep reading: no further call may
		// reach the underlying stream.
		var fired bool
		for range 4 {
			if _, err := r.Read(buf); errors.Is(err, ErrResponseTooLarge) {
				fired = true
				break
			}
		}
		if !fired {
			t.Fatalf("the cap never fired, so the stickiness assertion below proves nothing")
		}
		before := counting.reads

		for range 3 {
			n, err := r.Read(buf)
			if n != 0 || !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("post-refusal read = (%d, %v), want (0, ErrResponseTooLarge)", n, err)
			}
		}
		if counting.reads != before {
			t.Fatalf("underlying reader was touched %d more times after the refusal, want 0", counting.reads-before)
		}
	})
}
