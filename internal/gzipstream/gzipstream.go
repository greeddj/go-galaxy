// Package gzipstream is the one place a gzip reader opens over bytes this
// program did not produce: members are walked iteratively, an empty member is
// refused (helpers.ErrEmptyGzipMember), and ctx is checked on compressed reads.
package gzipstream

import (
	"bufio"
	"context"
	"errors"
	"io"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/klauspost/pgzip"
)

// Reader reads a gzip stream member by member under a context. src must stay
// the *bufio.Reader the decompressor was built on: a Reset over any other
// reader rebuffers each member and loses the bytes read past its boundary.
type Reader struct {
	z   *pgzip.Reader
	src *bufio.Reader
	// err is the terminal verdict, repeated by every later Read without
	// touching pgzip, which blocks forever on a Read after a member's end.
	err error
	// n counts the decompressed bytes the CURRENT member has produced, and is
	// reset at every member boundary. Zero at a member's end is the refusal.
	n int64
}

// NewReader opens r as a gzip stream under ctx with pgzip's default block
// sizing. The context reader sits under the buffer, and the buffer directly
// under pgzip, so the decompressor and every Reset share one buffered view.
func NewReader(ctx context.Context, r io.Reader) (*Reader, error) {
	src := bufio.NewReader(&contextReader{ctx: ctx, r: r})
	z, err := pgzip.NewReader(src)
	if err != nil {
		return nil, err
	}
	// Multistream(false) is what turns pgzip's own member loop off, so the one
	// below replaces it rather than running alongside it.
	z.Multistream(false)
	return &Reader{z: z, src: src}, nil
}

// NewReaderN opens r as a gzip stream under ctx with the caller's own block
// sizing, for a caller reading the front of an archive rather than unpacking
// one. It wraps r exactly as NewReader does, for the same reasons.
func NewReaderN(ctx context.Context, r io.Reader, blockSize, blocks int) (*Reader, error) {
	src := bufio.NewReader(&contextReader{ctx: ctx, r: r})
	z, err := pgzip.NewReaderN(src, blockSize, blocks)
	if err != nil {
		return nil, err
	}
	z.Multistream(false)
	return &Reader{z: z, src: src}, nil
}

// Read fills p from the current member and advances to the next one in a loop,
// not by pgzip's recursion. Every terminal return sets r.err: pgzip under
// Multistream(false) blocks uncancellably on any Read after a member's end.
func (r *Reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	for {
		n, err := r.z.Read(p)
		r.n += int64(n)
		switch {
		case n > 0:
			return n, nil
		case err == nil:
			return 0, nil
		case !errors.Is(err, io.EOF):
			r.err = err
			return 0, err
		}

		// The member ended. A member that produced nothing is the refusal this
		// package exists for, and it is checked before the Reset that would
		// otherwise move on to the next one.
		if r.n == 0 {
			r.err = helpers.ErrEmptyGzipMember
			return 0, r.err
		}
		if err := r.z.Reset(r.src); err != nil {
			// io.EOF means the stream ended; anything else is pgzip's verdict on
			// what follows the member ("gzip: invalid header" for trailing
			// garbage), returned unchanged.
			r.err = err
			return 0, err
		}
		// Reset re-enables multistream (gunzip.go sets z.multistream = true
		// unconditionally), so the loop has to disarm it again or pgzip
		// resumes the recursion this type replaced.
		r.z.Multistream(false)
		r.n = 0
	}
}

// Close releases the decompressor's readahead goroutine and block pool. It
// does not close the source reader, which the caller owns.
func (r *Reader) Close() error {
	return r.z.Close()
}

// contextReader fails a compressed-side read once ctx is done, returning
// ctx.Err() unwrapped so exitcode still classifies it through errors.Is.
//
//nolint:containedctx // io.Reader cannot take a context per call
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
