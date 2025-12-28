package output

import "time"

// Printer defines the progress output interface.
type Printer interface {
	Printf(format string, args ...any)
	PersistentPrintf(format string, args ...any)
	Debugf(format string, args ...any)
	DebugSincef(startTime time.Time, format string, args ...any)
}

// Printf proxies formatted output to the printer.
func Printf(printer Printer, format string, args ...any) {
	printer.Printf(format, args...)
}

// PersistentPrintf proxies persistent output to the printer.
func PersistentPrintf(printer Printer, format string, args ...any) {
	printer.PersistentPrintf(format, args...)
}
