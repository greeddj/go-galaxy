package helpers

import (
	"os"
	"path/filepath"
)

// defaultCacheDir returns the default cache directory path.
func defaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(defaultHomeDir, dirSuffix)
	}
	return filepath.Join(home, dirSuffix)
}
