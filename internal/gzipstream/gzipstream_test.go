package gzipstream

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// gzipMemberHeader is the ten bytes every member below opens with: the gzip
// magic, deflate as the method, no flags, no mtime, no extra flags, and 0xff
// for "unknown" as the operating system.
const gzipMemberHeader = "\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\xff"

// emptyMember is the smallest gzip member there is, 20 bytes: the header, an
// empty fixed-Huffman final block and a trailer over zero bytes. It is
// hand-spelled because its size is what one member of a flood costs.
func emptyMember() []byte {
	member := make([]byte, 0, 20)
	member = append(member, gzipMemberHeader...)
	member = append(member, 0x03, 0x00)
	return append(member, 0, 0, 0, 0, 0, 0, 0, 0)
}

// contentMember renders one ordinary gzip member carrying payload, through
// compress/gzip rather than through the library under test, so a fixture this
// package accepts is one an independent writer produced.
func contentMember(t *testing.T, payload []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("writing a member payload: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing a member: %v", err)
	}
	return buf.Bytes()
}

// storedBlockMember renders a member of padBlocks zero-length stored blocks,
// five wire bytes each producing nothing, then a final stored block carrying
// payload; a nil payload renders a member that produces nothing at all.
func storedBlockMember(padBlocks int, payload []byte) []byte {
	member := make([]byte, 0, len(gzipMemberHeader)+padBlocks*5+5+len(payload)+8)
	member = append(member, gzipMemberHeader...)
	for range padBlocks {
		// BFINAL=0, BTYPE=00 (stored), then LEN=0 and its one's complement.
		member = append(member, 0x00, 0x00, 0x00, 0xff, 0xff)
	}
	size := len(payload)
	lo, hi := byte(size&0xff), byte(size>>8&0xff)
	member = append(member, 0x01, lo, hi, ^lo, ^hi)
	member = append(member, payload...)

	sum := crc32.ChecksumIEEE(payload)
	//nolint:gosec // the same fixture length, rendered little-endian.
	return append(member,
		byte(sum), byte(sum>>8), byte(sum>>16), byte(sum>>24),
		byte(size), byte(size>>8), byte(size>>16), byte(size>>24),
	)
}

// buildStream concatenates members and appends trailing verbatim; every row
// builds through it, so accepted and refused rows differ only in members.
func buildStream(members [][]byte, trailing []byte) []byte {
	size := len(trailing)
	for _, m := range members {
		size += len(m)
	}
	out := make([]byte, 0, size)
	for _, m := range members {
		out = append(out, m...)
	}
	return append(out, trailing...)
}

// emptyTarStream is a well-formed tar with no entries, its two 512-byte zero
// blocks: archive.ProbeTarGz accepts it, so the member rule must not refuse it.
func emptyTarStream() []byte {
	return make([]byte, 1024)
}

// TestReaderMemberRules pins that an empty member is refused wherever it sits
// while content members read back concatenated; "content twice, then an empty
// member" pins the Multistream(false) re-arm after each Reset.
func TestReaderMemberRules(t *testing.T) {
	t.Parallel()

	for _, tt := range memberCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, err := NewReader(t.Context(), bytes.NewReader(tt.stream))
			if err != nil {
				t.Fatalf("%s: NewReader = %v, want a reader", tt.name, err)
			}
			defer func() {
				_ = r.Close()
			}()

			got, err := io.ReadAll(r)
			assertMemberOutcome(t, tt, got, err)
		})
	}
}

// memberCases builds the table TestReaderMemberRules drives, every row out of
// buildStream and the member constructors above.
func memberCases(t *testing.T) []memberCase {
	t.Helper()

	content := []byte("collection artifact bytes")
	// One rendered member, reused wherever a row wants a second copy of it, so
	// a refusal row and its positive control differ only in what follows.
	carrier := contentMember(t, content)

	return []memberCase{
		{
			name:   "one member carrying content",
			stream: buildStream([][]byte{carrier}, nil),
			want:   content,
		},
		{
			name:   "two members carrying content read back concatenated",
			stream: buildStream([][]byte{carrier, carrier}, nil),
			want:   append(append([]byte{}, content...), content...),
		},
		{
			name:   "a well-formed empty tar",
			stream: buildStream([][]byte{contentMember(t, emptyTarStream())}, nil),
			want:   emptyTarStream(),
		},
		{
			name:    "a single empty member",
			stream:  buildStream([][]byte{emptyMember()}, nil),
			wantErr: helpers.ErrEmptyGzipMember,
		},
		{
			name:    "content then an empty member",
			stream:  buildStream([][]byte{carrier, emptyMember()}, nil),
			wantErr: helpers.ErrEmptyGzipMember,
		},
		{
			name:    "content twice, then an empty member",
			stream:  buildStream([][]byte{carrier, carrier, emptyMember()}, nil),
			wantErr: helpers.ErrEmptyGzipMember,
		},
		{
			name:    "a member of zero-length stored blocks producing nothing",
			stream:  buildStream([][]byte{storedBlockMember(4096, nil)}, nil),
			wantErr: helpers.ErrEmptyGzipMember,
		},
		{
			name:     "content then trailing garbage",
			stream:   buildStream([][]byte{carrier}, []byte("no member starts here")),
			wantText: "gzip: invalid header",
		},
	}
}

// memberCase is one row of the table above: a stream, and the one outcome it
// must produce - a sentinel, a verbatim error message, or bytes.
type memberCase struct {
	wantErr  error
	name     string
	wantText string
	stream   []byte
	want     []byte
}

// assertMemberOutcome checks one row against the one expectation it declared:
// a sentinel, a verbatim error text, or bytes.
func assertMemberOutcome(t *testing.T, c memberCase, got []byte, err error) {
	t.Helper()

	switch {
	case c.wantErr != nil:
		if !errors.Is(err, c.wantErr) {
			t.Fatalf("%s: error = %v, want %v", c.name, err, c.wantErr)
		}
	case c.wantText != "":
		if err == nil || err.Error() != c.wantText {
			t.Fatalf("%s: error = %v, want %q", c.name, err, c.wantText)
		}
	default:
		if err != nil {
			t.Fatalf("%s: error = %v, want nil", c.name, err)
		}
		if !bytes.Equal(got, c.want) {
			t.Fatalf("%s: read %q, want %q", c.name, got, c.want)
		}
	}
}

// tarAcrossMembers gzips one tar stream of entryCount entries as memberCount
// members cut without regard to tar framing, so entries straddle boundaries.
func tarAcrossMembers(t *testing.T, entryCount, memberCount int) ([]byte, []string) {
	t.Helper()

	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	names := make([]string, 0, entryCount)
	for i := range entryCount {
		name := fmt.Sprintf("file-%02d.txt", i)
		body := bytes.Repeat([]byte{byte('a' + i)}, 700)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatalf("writing a tar header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("writing a tar body: %v", err)
		}
		names = append(names, name)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing the tar writer: %v", err)
	}

	tarBytes := raw.Bytes()
	chunk := len(tarBytes) / memberCount
	members := make([][]byte, 0, memberCount)
	for i := range memberCount {
		start := i * chunk
		end := start + chunk
		if i == memberCount-1 {
			end = len(tarBytes)
		}
		members = append(members, contentMember(t, tarBytes[start:end]))
	}
	return buildStream(members, nil), names
}

// TestReaderReadsOneTarSplitAcrossManyMembers pins that a tar split across
// eight members reads back whole, which fails if Reset gets any source but the
// one *bufio.Reader; under -race it covers pgzip's readahead restarts too.
func TestReaderReadsOneTarSplitAcrossManyMembers(t *testing.T) {
	t.Parallel()

	const (
		entryCount  = 24
		memberCount = 8
	)
	stream, names := tarAcrossMembers(t, entryCount, memberCount)

	r, err := NewReader(t.Context(), bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewReader = %v, want a reader", err)
	}
	defer func() {
		_ = r.Close()
	}()

	var got []string
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next after %d entries = %v, want the next entry", len(got), err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading the body of %s: %v", header.Name, err)
		}
		if len(body) != 700 {
			t.Fatalf("%s carries %d bytes, want 700", header.Name, len(body))
		}
		got = append(got, header.Name)
	}

	if len(got) != entryCount {
		t.Fatalf("read %d entries, want %d", len(got), entryCount)
	}
	for i, name := range names {
		if got[i] != name {
			t.Fatalf("entry %d is %s, want %s", i, got[i], name)
		}
	}
}

// cancelOnRead cancels ctx as it serves read number after, so a test can stop
// a decompression at a chosen point on the COMPRESSED side deterministically
// instead of racing a timer against it.
type cancelOnRead struct {
	r      io.Reader
	cancel context.CancelFunc
	after  int
	reads  int
}

func (c *cancelOnRead) Read(p []byte) (int, error) {
	c.reads++
	if c.reads == c.after {
		c.cancel()
	}
	return c.r.Read(p)
}

// TestReaderObservesCancellationOnTheCompressedSide pins that cancellation
// stops a member of zero-length stored blocks, which yields no decompressed
// byte; it fires on the source's second read, already inside the member.
func TestReaderObservesCancellationOnTheCompressedSide(t *testing.T) {
	t.Parallel()

	stream := buildStream([][]byte{storedBlockMember(13107, []byte("payload"))}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &cancelOnRead{r: bytes.NewReader(stream), cancel: cancel, after: 2}

	r, err := NewReader(ctx, src)
	if err != nil {
		t.Fatalf("NewReader = %v, want a reader", err)
	}
	defer func() {
		_ = r.Close()
	}()

	if _, err := io.ReadAll(r); !errors.Is(err, context.Canceled) {
		t.Fatalf("reading a canceled stored-block member = %v, want errors.Is context.Canceled", err)
	}
}

// TestReaderAcceptsTheSameStoredBlockMemberUncanceled is the positive control
// for TestReaderObservesCancellationOnTheCompressedSide: the same fixture read
// under a live context hands back its payload.
func TestReaderAcceptsTheSameStoredBlockMemberUncanceled(t *testing.T) {
	t.Parallel()

	stream := buildStream([][]byte{storedBlockMember(13107, []byte("payload"))}, nil)

	r, err := NewReader(t.Context(), bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewReader = %v, want a reader", err)
	}
	defer func() {
		_ = r.Close()
	}()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading the same member uncanceled = %v, want nil", err)
	}
	if string(got) != "payload" {
		t.Fatalf("read %q, want %q", got, "payload")
	}
}

// TestNewReaderNAppliesTheSameMemberRules pins that NewReaderN also disables
// pgzip's member loop; its context reader is pinned instead by
// archive.TestProbeTarGzReportsCancellationAsItself.
func TestNewReaderNAppliesTheSameMemberRules(t *testing.T) {
	t.Parallel()

	content := []byte("collection artifact bytes")
	carrier := contentMember(t, content)

	for _, tt := range []memberCase{
		{
			name:    "content then an empty member",
			stream:  buildStream([][]byte{carrier, emptyMember()}, nil),
			wantErr: helpers.ErrEmptyGzipMember,
		},
		{
			name:   "two members carrying content read back concatenated",
			stream: buildStream([][]byte{carrier, carrier}, nil),
			want:   append(append([]byte{}, content...), content...),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, err := NewReaderN(t.Context(), bytes.NewReader(tt.stream), 64<<10, 1)
			if err != nil {
				t.Fatalf("%s: NewReaderN = %v, want a reader", tt.name, err)
			}
			defer func() {
				_ = r.Close()
			}()

			got, err := io.ReadAll(r)
			assertMemberOutcome(t, tt, got, err)
		})
	}
}
