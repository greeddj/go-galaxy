// Package output declares Printer, the operator-output interface every
// subsystem takes; internal/progress implements it. It imports only time, so a
// test can substitute a recorder without pulling the renderer in.
package output

import "time"

// Printer is the operator-output interface: Printf is progress that --quiet
// drops, Debugf/DebugSincef are verbose-only, and the rest always emit. A
// version is its own parameter since the renderer colors it after sanitizing.
type Printer interface {
	Printf(format string, args ...any)
	PersistentPrintf(format string, args ...any)
	Okf(format string, args ...any)
	OkVersionf(version, format string, args ...any)
	// Updatef is the verdict beside Okf and Errorf for a subject that is intact
	// but superseded; it lands on stdout with the rest of the report.
	Updatef(format string, args ...any)
	Errorf(format string, args ...any)
	ErrorVersionf(version, cause, format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
	DebugSincef(startTime time.Time, format string, args ...any)
}
