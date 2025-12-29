package progress

import (
	"fmt"
	"strings"
	"time"

	"github.com/briandowns/spinner"
)

const (
	spinnerDelay   = 100 * time.Millisecond
	spinnerCharSet = 14
	spinnerColor   = "green"
	ansiRed        = "\x1b[1m\x1b[31m"
	ansiGreen      = "\x1b[1m\x1b[32m"
	ansiReset      = "\x1b[1m\x1b[0m"
	ok             = ansiGreen + "✔" + ansiReset
	fail           = ansiRed + "✗" + ansiReset
)

// Progress renders CLI progress output with optional spinner.
type Progress struct {
	v bool
	q bool
	s *spinner.Spinner
}

// New creates a Progress printer configured for verbose/quiet output.
func New(verbose, quiet bool) *Progress {
	if quiet || verbose {
		return &Progress{
			v: verbose,
			q: quiet,
			s: nil,
		}
	}

	spin := spinner.New(spinner.CharSets[spinnerCharSet], spinnerDelay)
	_ = spin.Color(spinnerColor)

	p := &Progress{
		v: verbose,
		q: quiet,
		s: spin,
	}
	p.s.Start()
	return p
}

// Okf prints a success message with a colored marker. For standalone use.
func Okf(format string, args ...any) {
	fmt.Printf(ok+" "+format+"\n", args...) //nolint:forbidigo
}

// Errorf prints an error message with a colored marker. For standalone use.
func Errorf(format string, args ...any) {
	fmt.Printf(fail+" "+format+"\n", args...) //nolint:forbidigo
}

// Printf updates the spinner line or prints a log line.
func (p *Progress) Printf(format string, args ...any) {
	if p.s != nil && !p.v {
		p.s.Suffix = fmt.Sprintf(" "+format, args...)
	}
	if p.v {
		fmt.Printf(format+"\n", args...) //nolint:forbidigo
	}
}

// PersistentPrintf prints a persistent line that survives spinner updates.
func (p *Progress) PersistentPrintf(format string, args ...any) {
	if p.s != nil && !p.v {
		p.s.Stop()
		fmt.Printf("%s\n", fmt.Sprintf(format, args...)) //nolint:forbidigo
		p.s.Restart()
	}
	if p.v {
		fmt.Printf("%s\n", fmt.Sprintf(format, args...)) //nolint:forbidigo
	}
}

// Okf prints a success message with a colored marker.
func (p *Progress) Okf(format string, args ...any) {
	p.PersistentPrintf(ok+" "+format, args...)
}

// Errorf prints an error message with a colored marker.
func (p *Progress) Errorf(format string, args ...any) {
	p.PersistentPrintf(fail+" "+format, args...)
}

// Debugf prints a debug message when verbose mode is enabled.
func (p *Progress) Debugf(format string, args ...any) {
	if p.v {
		fmt.Printf("🚧 Debug: "+format+"\n", args...) //nolint:forbidigo
	}
}

// DebugSincef prints a debug message with timing info.
func (p *Progress) DebugSincef(start time.Time, format string, args ...any) {
	if p.v {
		fmt.Printf("⏱️ Debug Timing ("+time.Since(start).Round(time.Millisecond).String()+"): "+format+"\n", args...) //nolint:forbidigo
	}
}

// Write implements io.Writer for log output integration.
func (p *Progress) Write(payload []byte) (int, error) {
	message := strings.TrimRight(string(payload), "\n")
	if message == "" {
		return len(payload), nil
	}
	if p.s != nil && !p.v {
		p.s.Stop()
		fmt.Println(message) //nolint:forbidigo
		p.s.Restart()
		return len(payload), nil
	}
	if p.v {
		fmt.Println(message) //nolint:forbidigo
	}
	return len(payload), nil
}

// Close stops the spinner if it is running.
func (p *Progress) Close() {
	if p.s != nil {
		p.s.Stop()
	}
}
