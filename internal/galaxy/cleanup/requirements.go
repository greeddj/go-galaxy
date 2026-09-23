package cleanup

import (
	"errors"
	"fmt"
	"os"

	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
)

// errRequirementsNotRegular names a recorded requirements path that resolves to
// a fifo, directory or other non-regular file. It is not fs.ErrNotExist, so it
// aborts the run as unreadable rather than passing as a stale registry entry.
var errRequirementsNotRegular = errors.New("requirements file is not a regular file")

// loadRequirements reads a recorded requirements file only after Stat finds a
// regular file: a fifo would block open() under the held cache lock. Stat, not
// Lstat, keeps a symlinked requirements.yml legal.
func loadRequirements(path, defaultSource string) (requirements.File, error) {
	// #nosec G703 -- path is a registry-recorded requirements file path (a
	// fixed set of candidates this program itself wrote via
	// store.RecordProject), not a value taken directly from an external
	// request; this is a read-only shape check before any read is attempted.
	info, err := os.Stat(path)
	if err != nil {
		return requirements.File{}, err
	}
	if !info.Mode().IsRegular() {
		return requirements.File{}, fmt.Errorf("%w: %q", errRequirementsNotRegular, path)
	}
	return requirements.Load(path, defaultSource)
}
