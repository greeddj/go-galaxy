// Package progress implements output.Printer and the stdlib log sink: results
// on stdout, warnings and failures on stderr, color decided per stream. Every
// line goes through writeLine: payload sanitized first, decorated after.
package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/safeout"
)

const (
	ansiRed    = "\x1b[1m\x1b[31m"
	ansiGreen  = "\x1b[1m\x1b[32m"
	ansiYellow = "\x1b[1m\x1b[33m"
	ansiGray   = "\x1b[1m\x1b[90m"
	ansiReset  = "\x1b[0m"
	okGlyph    = "✔"
	failGlyph  = "✗"
	warnGlyph  = "!"
	// updateGlyph marks a result that is intact but superseded. It is one
	// column wide like okGlyph and failGlyph, so a report's verdicts align.
	updateGlyph = "↑"
	// stepGlyph marks every line saying what the run is doing rather than what
	// it concluded; one gray glyph for all of them keeps a log easy to scan.
	stepGlyph   = "·"
	debugPrefix = "Debug: "
	// versionMark introduces a result line's exact version, spelled as the
	// pinning constraint so the pair pastes straight into a requirements file.
	versionMark = "== "
)

// Environment variables that override the terminal check, in the precedence
// this package applies: envNoColor first, then either force variable.
const (
	// envNoColor disables color whatever the destination is, per no-color.org:
	// present and non-empty is enough, whatever the value.
	envNoColor = "NO_COLOR"
	// envClicolorForce and envForceColor force color onto a non-terminal, for
	// a CI log viewer that renders it; a literal "0" means "do not force" and
	// falls through to the terminal check.
	envClicolorForce = "CLICOLOR_FORCE"
	envForceColor    = "FORCE_COLOR"
)

// stream is one destination and whether it may carry color, bound together
// because `go-galaxy install > install.log` leaves stderr a terminal while
// stdout is a file.
type stream struct {
	w     io.Writer
	color bool
}

// ok, fail, warn, update and step return this stream's marker prefix,
// colored only when this destination accepts color.
func (s stream) ok() string     { return marker(okGlyph, ansiGreen, s.color) }
func (s stream) fail() string   { return marker(failGlyph, ansiRed, s.color) }
func (s stream) warn() string   { return marker(warnGlyph, ansiYellow, s.color) }
func (s stream) update() string { return marker(updateGlyph, ansiYellow, s.color) }
func (s stream) step() string   { return marker(stepGlyph, ansiGray, s.color) }

// versionTag returns the version decoration for this stream, colored only
// when this destination accepts color.
func (s stream) versionTag(version safeout.Text) string { return versionTag(version, s.color) }

// marker builds one status prefix: the glyph, wrapped in seq and a full reset
// when colored, then one space. The reset precedes the payload, so no color
// state leaks into the text that follows.
func marker(glyph, seq string, colored bool) string {
	if !colored {
		return glyph + " "
	}
	return seq + glyph + ansiReset + " "
}

// versionTag builds " == <version>", gray and reset-closed when colored so the
// color never reaches a failure line's cause. An empty version yields no tag,
// which keeps the version optional at every call site.
func versionTag(version safeout.Text, colored bool) string {
	if version == "" {
		return ""
	}
	if !colored {
		return " " + versionMark + string(version)
	}
	return " " + ansiGray + versionMark + string(version) + ansiReset
}

// spaced returns s cleaned behind one separating space, or nothing for an
// empty s, so a line with no cause carries no trailing space. The space is
// cleaned with s so no plain string reaches safeout.Text uncleaned.
func spaced(s string) safeout.Text {
	if s == "" {
		return ""
	}
	return safeout.Clean(" " + s)
}

// colorEnabled reports whether lines written to f may carry color. NO_COLOR
// beats the force variables, since an opt-out another variable can override
// is not one; then a force wins, else the terminal check decides.
func colorEnabled(f *os.File) bool {
	if os.Getenv(envNoColor) != "" {
		return false
	}
	if forcesColor(os.Getenv(envClicolorForce)) || forcesColor(os.Getenv(envForceColor)) {
		return true
	}
	return isTerminal(f)
}

// forcesColor reports whether an environment value is a request to force color
// on. Empty means unset; "0" is the conventional "do not force" and neither
// forces nor disables, so it falls through to the terminal check.
func forcesColor(value string) bool {
	return value != "" && value != "0"
}

// Progress renders CLI progress output with optional spinner. Regular output
// goes to out; error and failure lines go to errOut so diagnostics do not
// contaminate stdout consumers.
type Progress struct {
	s      *spinner
	out    stream
	errOut stream
	mu     sync.Mutex
	v      bool
	q      bool
}

// newProgress builds a Progress on out and errOut with one color verdict for
// both, starting a spinner only when neither quiet nor verbose and terminal
// is true, so a CI log never gets escapes or interleaved frames.
func newProgress(verbose, quiet, terminal bool, out, errOut io.Writer) *Progress {
	return newStreamProgress(verbose, quiet, terminal,
		stream{w: out, color: terminal}, stream{w: errOut, color: terminal})
}

// newStreamProgress is newProgress with each stream's color decided on its
// own and apart from the spinner: NO_COLOR asks for plain text, not for losing
// the spinner on a terminal that can still redraw a line.
func newStreamProgress(verbose, quiet, terminal bool, out, errOut stream) *Progress {
	if quiet || verbose || !terminal {
		return &Progress{
			v:      verbose,
			q:      quiet,
			out:    out,
			errOut: errOut,
		}
	}

	// Frames are gated on os.Stdout itself, not on terminal: tests pass
	// terminal true to build the spinner state while `go test` always gives
	// the test binary a pipe on stdout.
	spin := newSpinner(os.Stdout, isTerminal(os.Stdout), out.color)

	p := &Progress{
		v:      verbose,
		q:      quiet,
		s:      spin,
		out:    out,
		errOut: errOut,
	}
	p.s.start()
	return p
}

// New creates a Progress printer configured for verbose/quiet output.
func New(verbose, quiet bool) *Progress {
	return newStreamProgress(verbose, quiet, isTerminal(os.Stdout),
		stream{w: os.Stdout, color: colorEnabled(os.Stdout)},
		stream{w: os.Stderr, color: colorEnabled(os.Stderr)})
}

// isTerminal reports whether f is connected to a terminal.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// Okf prints a success line to stdout for a caller that owns no Progress,
// resolving color on every call as Errorf does.
func Okf(format string, args ...any) {
	s := stream{w: os.Stdout, color: colorEnabled(os.Stdout)}
	writeLine(s.w, decorated{prefix: s.ok(), msg: safeout.Clean(fmt.Sprintf(format, args...))})
}

// Errorf prints a failure line to stderr for a caller that owns no Progress.
// main prints a run's final error here, so color is resolved per call: escapes
// in a redirected log would break `grep '^✗'` on that line.
func Errorf(format string, args ...any) {
	s := stream{w: os.Stderr, color: colorEnabled(os.Stderr)}
	writeLine(s.w, decorated{prefix: s.fail(), msg: safeout.Clean(fmt.Sprintf(format, args...))})
}

// Printf updates the spinner suffix when a spinner is active, otherwise
// prints a log line unless quiet mode is enabled.
func (p *Progress) Printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.s != nil {
		// The leading space parts the suffix from the frame glyph; it is
		// cleaned with the message so no plain string reaches safeout.Text.
		p.s.setSuffix(safeout.Clean(" " + fmt.Sprintf(format, args...)))
		return
	}
	if p.q {
		return
	}
	// The marker goes on the line and never on the suffix above: a spinner
	// draws its own frame glyph in that column already, and a second one
	// behind it would read as two cursors.
	p.emit(p.out, decorated{prefix: p.out.step(), msg: safeout.Clean(fmt.Sprintf(format, args...))})
}

// PersistentPrintf prints a persistent line to stdout that survives spinner
// updates.
func (p *Progress) PersistentPrintf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(p.out, decorated{prefix: p.out.step(), msg: safeout.Clean(fmt.Sprintf(format, args...))})
}

// Okf prints a success message with a colored marker to stdout.
func (p *Progress) Okf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(p.out, decorated{prefix: p.out.ok(), msg: safeout.Clean(fmt.Sprintf(format, args...))})
}

// OkVersionf is Okf plus a dimmed "== <version>"; an empty version prints
// Okf's line byte for byte. The version is a parameter because an escape
// spelled into format would be sanitized to U+FFFD rather than colored.
func (p *Progress) OkVersionf(version, format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(p.out, decorated{
		prefix: p.out.ok(),
		msg:    safeout.Clean(fmt.Sprintf(format, args...)),
		tag:    p.out.versionTag(safeout.Clean(version)),
	})
}

// Updatef prints a result that is intact but superseded, with its own marker
// (neither success nor failure) and on stdout, since it belongs to the report
// a caller reads there rather than to the stderr warnings.
func (p *Progress) Updatef(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(p.out, decorated{prefix: p.out.update(), msg: safeout.Clean(fmt.Sprintf(format, args...))})
}

// Errorf prints an error message with a colored marker to stderr, so failures
// do not contaminate stdout consumers.
func (p *Progress) Errorf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(p.errOut, decorated{prefix: p.errOut.fail(), msg: safeout.Clean(fmt.Sprintf(format, args...))})
}

// ErrorVersionf is Errorf printing the message, a dimmed "== <version>", then
// cause; an empty version prints message and cause one space apart. The
// version sits before the cause so it never reads as part of the error text.
func (p *Progress) ErrorVersionf(version, cause, format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(p.errOut, decorated{
		prefix: p.errOut.fail(),
		msg:    safeout.Clean(fmt.Sprintf(format, args...)),
		tag:    p.errOut.versionTag(safeout.Clean(version)),
		tail:   spaced(cause),
	})
}

// Warnf prints a warning message with a colored marker to stderr. Like
// Errorf, it always emits regardless of verbose/quiet mode and never
// touches stdout, consistent with the stdout-purity rule.
func (p *Progress) Warnf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(p.errOut, decorated{prefix: p.errOut.warn(), msg: safeout.Clean(fmt.Sprintf(format, args...))})
}

// Debugf prints a debug message when verbose mode is enabled.
func (p *Progress) Debugf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.v {
		p.emit(p.out, decorated{prefix: p.out.step() + debugPrefix, msg: safeout.Clean(fmt.Sprintf(format, args...))})
	}
}

// DebugSincef prints a debug message with timing info.
func (p *Progress) DebugSincef(start time.Time, format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.v {
		prefix := p.out.step() + debugPrefix + "timing (" + time.Since(start).Round(time.Millisecond).String() + ") "
		p.emit(p.out, decorated{prefix: prefix, msg: safeout.Clean(fmt.Sprintf(format, args...))})
	}
}

// Write implements io.Writer so runCollectionCommand can point the stdlib
// logger here under -v; it sanitizes because nothing here controls what a
// dependency writes to that logger.
func (p *Progress) Write(payload []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	message := strings.TrimRight(string(payload), "\n")
	if message == "" {
		return len(payload), nil
	}
	if p.q {
		return len(payload), nil
	}
	// Marked like every other line this package writes: under -v the stdlib
	// logger's output and the run's own lines share one stream, and two left
	// margins in one log read as two programs talking.
	p.emit(p.out, decorated{prefix: p.out.step(), msg: safeout.Clean(message)})
	return len(payload), nil
}

// Close stops and drops the spinner. Dropping it makes the restore final:
// emit restarts any spinner it finds, so a later line would hide the cursor
// again with nothing left to stop the new render goroutine.
func (p *Progress) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.s != nil {
		p.s.stop()
		p.s = nil
	}
}

// emit writes one decorated line to dst for every *Progress method, stopping
// and restarting an active spinner around it so the line survives. p.mu must
// be held by the caller.
func (p *Progress) emit(dst stream, line decorated) {
	if p.s != nil {
		p.s.stop()
		writeLine(dst.w, line)
		p.s.restart()
		return
	}
	writeLine(dst.w, line)
}

// decorated is one line's parts in write order: prefix and tag are this
// package's decorations, msg and tail are payload already through
// safeout.Clean. tag is an infix so a failure line still ends with its cause.
type decorated struct {
	prefix string
	msg    safeout.Text
	tag    string
	tail   safeout.Text
}

// writeLine is the one place a decorated line becomes bytes: payload is
// sanitized before decoration and a decorated line is never sanitized. The
// safeout.Text fields make an uncleaned payload need an explicit conversion.
func writeLine(w io.Writer, line decorated) {
	_, _ = fmt.Fprintf(w, "%s%s%s%s\n", line.prefix, line.msg, line.tag, line.tail)
}
