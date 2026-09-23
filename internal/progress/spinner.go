package progress

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/safeout"
)

// The spinner's delay, glyphs and escape sequences. spinnerFrames is a string
// because a []string cannot be constant; the wrap sequences bracket one frame
// in writeFrameLocked, not the run, so autowrap is off only for those bytes.
const (
	spinnerDelay      = 100 * time.Millisecond
	spinnerFrames     = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
	spinnerColorSeq   = "\x1b[32m"
	spinnerResetSeq   = "\x1b[0m"
	spinnerEraseSeq   = "\r\x1b[K"
	spinnerWrapOffSeq = "\x1b[?7l"
	spinnerWrapOnSeq  = "\x1b[?7h"
	spinnerEnterSeq   = "\x1b[?25l"
	spinnerLeaveSeq   = "\x1b[?25h" + spinnerEraseSeq
	frameBufSize      = 128
)

// The terminal type that renders escape sequences literally instead of acting
// on them, so a frame drawn there must carry no color.
const (
	envTerm      = "TERM"
	dumbTerminal = "dumb"
)

// spinner redraws a one-line indicator on w every delay until stopped. stopCh
// is non-nil exactly while a render goroutine is current; a goroutine holding
// a stale channel writes nothing, so no frame lands after the restore.
type spinner struct {
	w       io.Writer
	stopCh  chan struct{}
	suffix  safeout.Text
	frames  []rune
	delay   time.Duration
	next    int
	mu      sync.Mutex
	render  bool
	colored bool
}

// newSpinner builds a spinner drawing on w. render is the caller's
// character-device verdict (so a stdout on the null device draws there unseen);
// TERM=dumb takes color away even when colorAllowed.
func newSpinner(w io.Writer, render, colorAllowed bool) *spinner {
	return &spinner{
		w:       w,
		frames:  []rune(spinnerFrames),
		delay:   spinnerDelay,
		render:  render,
		colored: colorAllowed && os.Getenv(envTerm) != dumbTerminal,
	}
}

// start hides the cursor, draws the first frame synchronously and starts the
// render goroutine. It is a no-op when not rendering or already running.
func (s *spinner) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.render || s.stopCh != nil {
		return
	}
	s.stopCh = make(chan struct{})
	_, _ = io.WriteString(s.w, spinnerEnterSeq)
	s.writeFrameLocked()
	go s.run(s.stopCh, s.delay)
}

// stop erases the frame and restores the cursor, idempotently. It must not wait
// for the render goroutine, which takes s.mu to draw, or it would deadlock;
// clearing stopCh already keeps that goroutine from writing.
func (s *spinner) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopCh == nil {
		return
	}
	close(s.stopCh)
	s.stopCh = nil
	_, _ = io.WriteString(s.w, spinnerLeaveSeq)
}

// restart stops the spinner and starts it again, which is how a caller prints
// a line of its own without a frame landing in the middle of it.
func (s *spinner) restart() {
	s.stop()
	s.start()
}

// run redraws a frame on every tick until stop is closed, drawing only while
// s.stopCh is still the channel this goroutine was started with.
func (s *spinner) run(stop chan struct{}, delay time.Duration) {
	ticker := time.NewTicker(delay)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.mu.Lock()
			if s.stopCh == stop {
				s.writeFrameLocked()
			}
			s.mu.Unlock()
		}
	}
}

// setSuffix replaces the text drawn right of the frame glyph. safeout.Text
// makes an uncleaned string need an explicit conversion, though an untyped
// string constant is still assignable.
func (s *spinner) setSuffix(text safeout.Text) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suffix = text
}

// suffixText returns the current suffix.
func (s *spinner) suffixText() safeout.Text {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suffix
}

// writeFrameLocked draws one frame (wrap off, erase, glyph, suffix, wrap on) as
// a single write, so no reader sees half an escape sequence and autowrap is off
// for these bytes alone, then advances the glyph. s.mu must be held.
func (s *spinner) writeFrameLocked() {
	var frame strings.Builder
	frame.Grow(frameBufSize)
	frame.WriteString(spinnerWrapOffSeq)
	frame.WriteString(spinnerEraseSeq)
	if s.colored {
		frame.WriteString(spinnerColorSeq)
		frame.WriteRune(s.frames[s.next])
		frame.WriteString(spinnerResetSeq)
	} else {
		frame.WriteRune(s.frames[s.next])
	}
	frame.WriteString(string(firstLine(s.suffix)))
	frame.WriteString(spinnerWrapOnSeq)
	s.next = (s.next + 1) % len(s.frames)
	_, _ = io.WriteString(s.w, frame.String())
}

// firstLine returns text up to its first newline. Slicing preserves the
// safeout.Text type, so a frame is assembled without ever converting a plain
// string back into sanitized text.
func firstLine(text safeout.Text) safeout.Text {
	if i := strings.IndexByte(string(text), '\n'); i >= 0 {
		return text[:i]
	}
	return text
}
