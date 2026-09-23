package collections_test

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// lineCapturingPrinter records every line on every tier, Debugf included, so
// a test can assert what a report said and that no line contains a git
// password. It is safe for the download-worker pool's concurrent use.
type lineCapturingPrinter struct {
	lines []string
	mu    sync.Mutex
}

func (p *lineCapturingPrinter) Printf(format string, args ...any) { p.recordf(format, args...) }
func (p *lineCapturingPrinter) PersistentPrintf(format string, args ...any) {
	p.recordf(format, args...)
}
func (p *lineCapturingPrinter) Okf(format string, args ...any) { p.recordf(format, args...) }
func (p *lineCapturingPrinter) OkVersionf(version, format string, args ...any) {
	p.record(renderVersionLine(version, "", format, args...))
}
func (p *lineCapturingPrinter) Updatef(format string, args ...any) { p.recordf(format, args...) }
func (p *lineCapturingPrinter) Errorf(format string, args ...any)  { p.recordf(format, args...) }
func (p *lineCapturingPrinter) ErrorVersionf(version, cause, format string, args ...any) {
	p.record(renderVersionLine(version, cause, format, args...))
}
func (p *lineCapturingPrinter) Warnf(format string, args ...any)  { p.recordf(format, args...) }
func (p *lineCapturingPrinter) Debugf(format string, args ...any) { p.recordf(format, args...) }
func (p *lineCapturingPrinter) DebugSincef(_ time.Time, format string, args ...any) {
	p.recordf(format, args...)
}

func (p *lineCapturingPrinter) recordf(format string, args ...any) {
	p.record(fmt.Sprintf(format, args...))
}

func (p *lineCapturingPrinter) record(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lines = append(p.lines, line)
}

// snapshot returns a copy of every recorded line, in order.
func (p *lineCapturingPrinter) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.lines...)
}

// hasLineContaining reports whether any recorded line, on any tier, contains
// substr.
func (p *lineCapturingPrinter) hasLineContaining(substr string) bool {
	for _, line := range p.snapshot() {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// renderVersionLine renders an OkVersionf or ErrorVersionf call as the line an
// operator reads, version and cause included and color left out, so a leak
// assertion never reads half a line.
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
