package helpers

import (
	"fmt"
	"io"
)

// sizeLimitedReader fails a read past a cumulative byte ceiling, where
// io.LimitReader would report a clean io.EOF and pass a truncated download
// off as complete.
type sizeLimitedReader struct {
	r   io.Reader
	err error
	max int64
	n   int64
}

// NewSizeLimitedReader returns a reader over r that fails with
// ErrResponseTooLarge once more than limit bytes are read. The crossing read
// returns zero bytes: io.ReadAtLeast and io.CopyN drop an error beside data.
func NewSizeLimitedReader(r io.Reader, limit int64) io.Reader {
	return &sizeLimitedReader{r: r, max: limit}
}

// Read counts bytes and, past max, returns a sticky ErrResponseTooLarge in
// place of the wrapped reader's result. It mirrors archive's
// decompressedLimitReader, so the two size caps agree on a crossing read.
func (r *sizeLimitedReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	// Keeps p[:remaining+1] in range: a max below zero drives remaining
	// negative, and at -2 or lower that reslice panics.
	remaining := max(r.max-r.n, 0)
	// Taking this branch means remaining < len(p), hence remaining+1 <= len(p),
	// so the reslice is always within p.
	if remaining < int64(len(p)) {
		p = p[:remaining+1]
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	if r.n > r.max {
		r.err = fmt.Errorf("%w: read %d bytes, limit is %d bytes", ErrResponseTooLarge, r.n, r.max)
		return 0, r.err
	}
	return n, err
}
