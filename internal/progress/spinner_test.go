package progress

import (
	"bytes"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/safeout"
)

// Expected escape sequences and TERM inputs, spelled by hand rather than taken
// from the production constants: an assertion built from the value it checks
// would stay green whatever that value became.
const (
	hideCursorSeq  = "\x1b[?25l"
	autowrapOffSeq = "\x1b[?7l"
	autowrapOnSeq  = "\x1b[?7h"
	eraseSeq       = "\r\x1b[K"
	leaveSeq       = "\x1b[?25h\r\x1b[K"
	greenSeq       = "\x1b[32m"
	resetSeq       = "\x1b[0m"
	termEnv        = "TERM"
	dumbTerm       = "dumb"
)

// Test timings and workloads: tick delays far below production's so no test
// waits a tenth of a second per frame; the drain budget bounds a scheduler.
const (
	staleTickDelay        = 5 * time.Millisecond
	staleHandoffWait      = 50 * time.Millisecond
	suffixRaceTickDelay   = time.Millisecond
	goroutineDrainTimeout = 2 * time.Second
	goroutinePollInterval = 5 * time.Millisecond
	frameWaitDeadline     = 2 * time.Second
	framePollInterval     = time.Millisecond
	minFramesBeforeHold   = 2
	spinnerRounds         = 3
	suffixRaceWorkers     = 8
	suffixRaceIterations  = 200
)

// syncBuffer is a locked bytes.Buffer a render goroutine and the test may
// share; it counts Write calls because one test asserts one write per frame.
type syncBuffer struct {
	buf    bytes.Buffer
	writes int
	mu     sync.Mutex
}

// Write appends to the buffer under the fixture's lock.
func (b *syncBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writes++
	return b.buf.Write(payload)
}

// String returns everything written so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Writes returns how many Write calls the fixture has received.
func (b *syncBuffer) Writes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.writes
}

// Reset empties the fixture so one buffer can back every row of a table.
func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writes = 0
	b.buf.Reset()
}

// firstFrameGlyph is the glyph a freshly started spinner draws. It is derived
// from the production frame set rather than re-spelled, because which glyph
// comes first is not what any test here is about.
func firstFrameGlyph() string {
	return string([]rune(spinnerFrames)[0])
}

// TestSpinnerRendersOnlyWhenRenderingIsEnabled pins that a non-rendering
// spinner writes nothing and a rendering one has hidden the cursor and drawn
// a frame by the time start returns.
func TestSpinnerRendersOnlyWhenRenderingIsEnabled(t *testing.T) {
	var out syncBuffer
	for _, tc := range []struct {
		name   string
		render bool
	}{
		{name: "rendering enabled", render: true},
		{name: "rendering disabled", render: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			s := newSpinner(&out, tc.render, false)
			s.start()
			defer s.stop()

			got := out.String()
			if !tc.render {
				if got != "" {
					t.Fatalf("rendering disabled, wrote %q", got)
				}
				return
			}
			if got == "" {
				t.Fatal("rendering enabled, wrote nothing")
			}
			if glyph := firstFrameGlyph(); !strings.Contains(got, glyph) {
				t.Fatalf("frame glyph %q missing from %q", glyph, got)
			}
			if !strings.Contains(got, hideCursorSeq) {
				t.Fatalf("cursor was not hidden in %q", got)
			}
		})
	}
}

// TestSpinnerFrameOccupiesOneLine pins that a frame is one physical line
// whatever the suffix, so a one-line erase clears it; the byte match is a
// prefix since a later tick may append a frame.
func TestSpinnerFrameOccupiesOneLine(t *testing.T) {
	var out syncBuffer
	for _, tc := range []struct {
		name   string
		suffix string
		want   string
	}{
		{name: "plain suffix", suffix: " ab", want: " ab"},
		{name: "embedded newline", suffix: " a\nb", want: " a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			s := newSpinner(&out, true, false)
			s.setSuffix(safeout.Clean(tc.suffix))
			s.start()
			defer s.stop()

			got := out.String()
			if strings.ContainsRune(got, '\n') {
				t.Fatalf("frame spans more than one line: %q", got)
			}
			want := hideCursorSeq + autowrapOffSeq + eraseSeq + firstFrameGlyph() + tc.want + autowrapOnSeq
			if !strings.HasPrefix(got, want) {
				t.Fatalf("frame = %q, want prefix %q", got, want)
			}
		})
	}
}

// TestSpinnerFrameCycleRepeatsAndWritesOnce drives writeFrameLocked twice
// around the frame set and one further, pinning the glyph index wrap, an erase
// on every frame, and one write per frame so no half escape is ever visible.
func TestSpinnerFrameCycleRepeatsAndWritesOnce(t *testing.T) {
	var out syncBuffer
	s := newSpinner(&out, true, false)
	frames := []rune(spinnerFrames)
	count := 2*len(frames) + 1

	var want strings.Builder
	s.mu.Lock()
	for i := range count {
		s.writeFrameLocked()
		want.WriteString(autowrapOffSeq + eraseSeq + string(frames[i%len(frames)]) + autowrapOnSeq)
	}
	s.mu.Unlock()

	if got := out.String(); got != want.String() {
		t.Fatalf("frame stream = %q, want %q", got, want.String())
	}
	if got := out.Writes(); got != count {
		t.Fatalf("%d writes for %d frames, want one write each", got, count)
	}
}

// TestSpinnerFrameBracketsItsOwnAutowrap pins that autowrap is off for one
// frame, not the run: off before the glyph, on after, equal counts, and a
// stopped spinner ends on the cursor-restoring sequence.
func TestSpinnerFrameBracketsItsOwnAutowrap(t *testing.T) {
	var out syncBuffer
	s := newSpinner(&out, true, false)
	s.start()
	s.stop()

	got := out.String()
	off := strings.Index(got, autowrapOffSeq)
	on := strings.Index(got, autowrapOnSeq)
	glyph := strings.Index(got, firstFrameGlyph())

	if off < 0 {
		t.Fatalf("no frame disabled autowrap: %q", got)
	}
	if on < 0 {
		t.Fatalf("no frame restored autowrap: %q", got)
	}
	if glyph < 0 {
		t.Fatalf("no frame glyph in %q", got)
	}
	if off > glyph || on < glyph {
		t.Fatalf("frame does not bracket its glyph: off=%d glyph=%d on=%d", off, glyph, on)
	}
	offCount, onCount := strings.Count(got, autowrapOffSeq), strings.Count(got, autowrapOnSeq)
	if offCount != onCount {
		t.Fatalf("autowrap disabled %d times, restored %d times, in %q", offCount, onCount, got)
	}
	if !strings.HasSuffix(got, leaveSeq) {
		t.Fatalf("a stopped spinner did not end on its restore sequence: %q", got)
	}
}

// TestSpinnerFrameColorFollowsTheTerminal pins both color inputs, the
// destination verdict and TERM=dumb, asserting exact frame bytes after
// checking the glyph was drawn so no uncolored verdict is an empty frame.
func TestSpinnerFrameColorFollowsTheTerminal(t *testing.T) {
	var out syncBuffer
	for _, tc := range []struct {
		name         string
		term         string
		colorAllowed bool
		wantColor    bool
	}{
		{name: "color allowed on a capable terminal", term: "xterm-256color", colorAllowed: true, wantColor: true},
		{name: "dumb terminal", term: dumbTerm, colorAllowed: true},
		{name: "color not allowed by the destination", term: "xterm-256color"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(termEnv, tc.term)
			out.Reset()
			s := newSpinner(&out, true, tc.colorAllowed)
			s.start()
			defer s.stop()

			got := out.String()
			glyph := firstFrameGlyph()
			if !strings.Contains(got, glyph) {
				t.Fatalf("frame glyph %q missing from %q", glyph, got)
			}
			if tc.wantColor {
				glyph = greenSeq + glyph + resetSeq
			}
			want := hideCursorSeq + autowrapOffSeq + eraseSeq + glyph + autowrapOnSeq
			if !strings.HasPrefix(got, want) {
				t.Fatalf("frame = %q, want prefix %q", got, want)
			}
		})
	}
}

// TestSpinnerStaleRenderGoroutineWritesNothing holds s.mu so stop runs before
// a woken render goroutine, and pins that the goroutine then writes nothing
// after the restore; waiting for two frames first keeps the check non-vacuous.
func TestSpinnerStaleRenderGoroutineWritesNothing(t *testing.T) {
	var out syncBuffer
	s := newSpinner(&out, true, false)
	s.delay = staleTickDelay
	s.start()

	deadline := time.Now().Add(frameWaitDeadline)
	for frameCount(&out) < minFramesBeforeHold && time.Now().Before(deadline) {
		time.Sleep(framePollInterval)
	}
	if drawn := frameCount(&out); drawn < minFramesBeforeHold {
		t.Fatalf("%d frames drawn before the hold, want at least %d", drawn, minFramesBeforeHold)
	}

	s.mu.Lock()
	var wg sync.WaitGroup
	wg.Go(s.stop)
	// Long enough for the ticker to fire and the render goroutine to queue on
	// the mutex behind stop, which is what puts it after stop in the handoff.
	time.Sleep(staleHandoffWait)
	s.mu.Unlock()
	wg.Wait()
	time.Sleep(staleHandoffWait)

	if got := out.String(); !strings.HasSuffix(got, leaveSeq) {
		t.Fatalf("a frame landed after the restore sequence: %q", got)
	}
}

// frameCount reports how many frames have reached the fixture, counting the
// one sequence every frame opens with and nothing else writes.
func frameCount(out *syncBuffer) int {
	return strings.Count(out.String(), autowrapOffSeq)
}

// TestSpinnerStopIsIdempotentAndReleasesTheGoroutine pins that a double start,
// repeated restarts and a double stop settle without a panic and with the
// goroutine count back at its baseline.
func TestSpinnerStopIsIdempotentAndReleasesTheGoroutine(t *testing.T) {
	var out syncBuffer
	baseline := runtime.NumGoroutine()

	s := newSpinner(&out, true, false)
	s.delay = staleTickDelay
	s.start()
	s.start()
	for range spinnerRounds {
		s.restart()
	}
	s.stop()
	s.stop()

	deadline := time.Now().Add(goroutineDrainTimeout)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(goroutinePollInterval)
	}
	if got := runtime.NumGoroutine(); got > baseline {
		t.Fatalf("goroutine count %d never returned to the baseline %d", got, baseline)
	}
}

// TestSpinnerSuffixRoundTrips covers the accessor pair on its own: a fresh
// spinner carries no suffix, and what setSuffix stores is what suffixText
// reports.
func TestSpinnerSuffixRoundTrips(t *testing.T) {
	var out syncBuffer
	s := newSpinner(&out, false, false)
	if got := s.suffixText(); got != "" {
		t.Fatalf("fresh spinner suffix = %q, want empty", got)
	}

	want := safeout.Clean(" resolving community.general")
	s.setSuffix(want)
	if got := s.suffixText(); got != want {
		t.Fatalf("suffix = %q, want %q", got, want)
	}
}

// TestSpinnerSuffixRace races suffix writers and readers against the render
// goroutine, as Printf does, and must pass under -race; every writer stores a
// non-empty value, so an empty final suffix means a lost update.
func TestSpinnerSuffixRace(t *testing.T) {
	var out syncBuffer
	s := newSpinner(&out, true, false)
	s.delay = suffixRaceTickDelay
	s.start()
	defer s.stop()

	var wg sync.WaitGroup
	for range suffixRaceWorkers {
		wg.Go(func() {
			for i := range suffixRaceIterations {
				s.setSuffix(safeout.Clean(" tick " + strconv.Itoa(i)))
			}
		})
		wg.Go(func() {
			for range suffixRaceIterations {
				_ = s.suffixText()
			}
		})
	}
	wg.Wait()

	if s.suffixText() == "" {
		t.Fatal("the suffix is empty after every writer stored a non-empty one")
	}
}

// TestEmitRestoresAutowrapAroundThePersistentLine pins that a printed line
// lands between frames, after one restores autowrap and before the next
// disables it, so the line reaches a terminal that can wrap it.
func TestEmitRestoresAutowrapAroundThePersistentLine(t *testing.T) {
	var out syncBuffer
	var errOut bytes.Buffer
	p := newProgress(false, false, true, &out, &errOut)
	// The spinner a Progress builds for itself draws on os.Stdout, where this
	// interleaving is not observable. Pointing one at the same buffer the
	// printed line lands in is what makes the order of the two writes visible.
	p.s = newSpinner(&out, true, false)
	p.s.start()

	p.Okf("hello")
	p.Close()

	got := out.String()
	on := strings.Index(got, autowrapOnSeq)
	line := strings.Index(got, "hello")
	off := strings.LastIndex(got, autowrapOffSeq)

	if on < 0 {
		t.Fatalf("autowrap was never restored: %q", got)
	}
	if off < 0 {
		t.Fatalf("autowrap was never disabled again: %q", got)
	}
	if line < 0 {
		t.Fatalf("the printed line never reached the buffer: %q", got)
	}
	if on > line || line > off {
		t.Fatalf("expected restore at %d before the line at %d before disable at %d", on, line, off)
	}
}

// TestCloseDropsTheSpinnerAndKeepsResultOutput pins that Close drops the
// spinner, so emit cannot restart it and hide the cursor again, while Okf
// after Close still reaches stdout.
func TestCloseDropsTheSpinnerAndKeepsResultOutput(t *testing.T) {
	var out, errOut bytes.Buffer
	p := newProgress(false, false, true, &out, &errOut)
	if p.s == nil {
		t.Fatal("expected a spinner for verbose=false, quiet=false, terminal=true")
	}

	p.Close()
	if p.s != nil {
		t.Fatal("Close left the spinner in place, so a later line would restart it")
	}

	p.Okf("after close")
	if want := okMark() + "after close\n"; out.String() != want {
		t.Fatalf("out = %q, want %q", out.String(), want)
	}
}
