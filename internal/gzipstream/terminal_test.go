package gzipstream

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

// terminalReadDeadline is how long a Read past a stream's ending may take
// before it counts as a hang; generous costs nothing, since a Read that
// returns at all returns in microseconds.
const terminalReadDeadline = 3 * time.Second

// readPastTheEnd calls Read on r from a goroutine and fails the test if it
// does not return within terminalReadDeadline. A parked goroutine is leaked,
// and the caller closes r only afterwards: closing under a live Read races.
func readPastTheEnd(t *testing.T, r *Reader) error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		var buf [32]byte
		_, err := r.Read(buf[:])
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(terminalReadDeadline):
		t.Fatalf("Read past the stream's ending did not return within %v, want its verdict repeated", terminalReadDeadline)
		return nil
	}
}

// terminalCase is one row of the table below: a stream, and whether reading it
// to its end is a refusal or an ordinary ending. Both are terminal, which is
// what this table is about; what differs is the verdict that has to come back.
type terminalCase struct {
	name    string
	stream  []byte
	refused bool
}

// terminalCases builds that table: three accepted endings (one small member,
// one past pgzip's block pool, several members) and Read's two other terminal
// returns, the empty-member refusal and trailing garbage.
func terminalCases(t *testing.T) []terminalCase {
	t.Helper()

	carrier := contentMember(t, []byte("collection artifact bytes"))
	// 8 MiB is far past pgzip's own default block - 1 MiB, four of them
	// pooled - so this row reaches its ending having cycled the block pool many
	// times rather than inside the first block the small rows never leave.
	large := contentMember(t, bytes.Repeat([]byte("x"), 8<<20))
	garbage := []byte("no member starts here")

	return []terminalCase{
		{name: "a single small member", stream: buildStream([][]byte{carrier}, nil)},
		{name: "one 8 MiB member", stream: buildStream([][]byte{large}, nil)},
		{name: "three members", stream: buildStream([][]byte{carrier, carrier, carrier}, nil)},
		{name: "a single empty member", stream: buildStream([][]byte{emptyMember()}, nil), refused: true},
		{name: "content then trailing garbage", stream: buildStream([][]byte{carrier}, garbage), refused: true},
	}
}

// TestReaderRepeatsItsTerminalVerdict pins that a Read past a stream's ending,
// accepted or refused, repeats the first verdict instead of parking forever on
// pgzip's full block pool.
func TestReaderRepeatsItsTerminalVerdict(t *testing.T) {
	t.Parallel()

	for _, tt := range terminalCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertTerminalVerdictRepeats(t, tt)
		})
	}
}

// assertTerminalVerdictRepeats drains one row's stream, checks only that it
// was refused or accepted as declared (TestReaderMemberRules pins which), then
// that one more Read repeats that verdict rather than blocking.
func assertTerminalVerdictRepeats(t *testing.T, tt terminalCase) {
	t.Helper()

	r, err := NewReader(t.Context(), bytes.NewReader(tt.stream))
	if err != nil {
		t.Fatalf("%s: NewReader = %v, want a reader", tt.name, err)
	}

	_, drainErr := io.ReadAll(r)
	switch {
	case tt.refused && drainErr == nil:
		t.Fatalf("%s: reading to the end = nil, want a refusal", tt.name)
	case !tt.refused && drainErr != nil:
		t.Fatalf("%s: reading to the end = %v, want nil", tt.name, drainErr)
	}

	// A refusal has to come back as the very error it came back as the first
	// time; an ordinary ending has to come back as io.EOF, which io.ReadAll
	// swallows above and so is named here rather than taken from drainErr.
	want := drainErr
	if want == nil {
		want = io.EOF
	}
	if again := readPastTheEnd(t, r); !errors.Is(again, want) {
		t.Fatalf("%s: reading once more = %v, want %v", tt.name, again, want)
	}

	// Closed here rather than deferred, for the reason readPastTheEnd states:
	// a reader whose Read is still parked must be left alone.
	if err := r.Close(); err != nil {
		t.Fatalf("%s: Close = %v, want nil", tt.name, err)
	}
}
