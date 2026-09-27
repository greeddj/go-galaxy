package helpers

import (
	"fmt"
	"os"
)

// CheckRegularRequirementsFile is the gate every reader passes before opening
// a requirements file: Stat, following a symlink, refuses what is no regular
// file, and a failed Stat passes, so the open reports absence as it always did.
func CheckRegularRequirementsFile(path string) error {
	info, err := os.Stat(path)
	if err != nil || info.Mode().IsRegular() {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrRequirementsNotRegular, path)
}
