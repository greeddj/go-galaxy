package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingPrinter is an output.Printer that keeps what was written to it.
// Every method is safe to call from the repainting goroutine and the test at
// the same time, which is the condition the live line is built around.
type recordingPrinter struct {
	lines []string
	mu    sync.Mutex
}

func (p *recordingPrinter) Printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.lines = append(p.lines, fmt.Sprintf(format, args...))
}

func (p *recordingPrinter) PersistentPrintf(format string, args ...any) { p.Printf(format, args...) }
func (p *recordingPrinter) Okf(format string, args ...any)              { p.Printf(format, args...) }
func (p *recordingPrinter) Updatef(format string, args ...any)          { p.Printf(format, args...) }
func (p *recordingPrinter) Errorf(format string, args ...any)           { p.Printf(format, args...) }
func (p *recordingPrinter) Warnf(format string, args ...any)            { p.Printf(format, args...) }
func (p *recordingPrinter) Debugf(format string, args ...any)           { p.Printf(format, args...) }

// OkVersionf and ErrorVersionf render the version tag as internal/progress
// does, minus color. This binary prints no such line; they satisfy
// output.Printer without a tier that silently drops what it is handed.
func (p *recordingPrinter) OkVersionf(version, format string, args ...any) {
	p.Printf("%s", renderVersionLine(version, "", format, args...))
}

func (p *recordingPrinter) ErrorVersionf(version, cause, format string, args ...any) {
	p.Printf("%s", renderVersionLine(version, cause, format, args...))
}

func renderVersionLine(version, cause, format string, args ...any) string {
	line := fmt.Sprintf(format, args...)
	if version != "" {
		line += " == " + version
	}
	if cause != "" {
		line += " " + cause
	}
	return line
}

func (p *recordingPrinter) DebugSincef(_ time.Time, format string, args ...any) {
	p.Printf(format, args...)
}

// count returns how many lines have been written so far.
func (p *recordingPrinter) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.lines)
}

// last returns the most recent line, or the empty string.
func (p *recordingPrinter) last() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.lines) == 0 {
		return ""
	}

	return p.lines[len(p.lines)-1]
}

func TestLiveLineRepaintsWhileWorkRuns(t *testing.T) {
	printer := &recordingPrinter{}
	live := liveLine{out: printer, started: time.Now(), enabled: true}

	_, err := live.track("go-galaxy cold size 1: run 1/1", func() (time.Duration, error) {
		time.Sleep(3 * livePeriod)

		return time.Second, nil
	})
	if err != nil {
		t.Fatalf("track: %v", err)
	}

	if got := printer.count(); got < 2 {
		t.Fatalf("printer saw %d lines, want the prefix plus at least one repaint", got)
	}

	last := printer.last()
	if !strings.Contains(last, "go-galaxy cold size 1: run 1/1") || !strings.Contains(last, "(total ") {
		t.Fatalf("last line = %q, want the prefix followed by both elapsed times", last)
	}
}

// TestLiveLineStopsRepaintingBeforeTrackReturns is the reason track closes and
// awaits its goroutine rather than deferring the close: a repaint that lands
// after the caller has moved on would be drawn over the caller's next line.
func TestLiveLineStopsRepaintingBeforeTrackReturns(t *testing.T) {
	printer := &recordingPrinter{}
	live := liveLine{out: printer, started: time.Now(), enabled: true}

	if _, err := live.track("prefix", func() (time.Duration, error) {
		time.Sleep(2 * livePeriod)

		return 0, nil
	}); err != nil {
		t.Fatalf("track: %v", err)
	}

	settled := printer.count()
	time.Sleep(3 * livePeriod)

	if got := printer.count(); got != settled {
		t.Fatalf("printer gained %d lines after track returned; the ticker outlived it", got-settled)
	}
}

// TestLiveLineDisabledWritesTheLineOnce covers the case that keeps a CI log
// readable: with no spinner to repaint, every update would be a new line, so
// the disabled path must write the prefix and nothing more.
func TestLiveLineDisabledWritesTheLineOnce(t *testing.T) {
	printer := &recordingPrinter{}
	live := liveLine{out: printer, started: time.Now(), enabled: false}

	if _, err := live.track("prefix", func() (time.Duration, error) {
		time.Sleep(3 * livePeriod)

		return 0, nil
	}); err != nil {
		t.Fatalf("track: %v", err)
	}

	if got := printer.count(); got != 1 {
		t.Fatalf("printer saw %d lines with the live line disabled, want 1", got)
	}

	if got := printer.last(); got != "prefix" {
		t.Fatalf("line = %q, want the bare prefix", got)
	}
}

func TestLiveLineTrackPassesTheWorkResultThrough(t *testing.T) {
	printer := &recordingPrinter{}

	for _, enabled := range []bool{false, true} {
		live := liveLine{out: printer, started: time.Now(), enabled: enabled}

		elapsed, err := live.track("prefix", func() (time.Duration, error) {
			return 42 * time.Millisecond, errReportEmpty
		})

		if elapsed != 42*time.Millisecond {
			t.Fatalf("enabled=%v: elapsed = %v, want 42ms", enabled, elapsed)
		}

		if err == nil {
			t.Fatalf("enabled=%v: track swallowed the work's error", enabled)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                                     "0.0s",
		189 * time.Millisecond:                "0.2s",
		3849 * time.Millisecond:               "3.8s",
		59*time.Second + 900*time.Millisecond: "59.9s",
		time.Minute:                           "1m00s",
		6*time.Minute + 12*time.Second:        "6m12s",
		72 * time.Minute:                      "72m00s",
	}

	for input, want := range cases {
		if got := humanDuration(input); got != want {
			t.Fatalf("humanDuration(%v) = %q, want %q", input, got, want)
		}
	}
}

func TestSpinnerActiveIsOffWhenTheresNothingToRepaint(t *testing.T) {
	if (options{quiet: true}).spinnerActive() {
		t.Fatal("a quiet run has no spinner, so it must not tick")
	}

	if (options{verbose: true}).spinnerActive() {
		t.Fatal("a verbose run has no spinner, so it must not tick")
	}

	// Under `go test` stdout is a pipe, so the terminal branch is false here
	// too. The assertion that matters is that the flags short-circuit before
	// the terminal is ever consulted.
	if (options{}).spinnerActive() {
		t.Fatal("stdout is a pipe under go test; spinnerActive must report false")
	}
}
