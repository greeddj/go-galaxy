package helpers

import (
	"os"
	"path/filepath"
)

// PhysicalAbs is path cleaned when absolute, else joined under the physical
// working directory, which the kernel and Python's os.getcwd() both use where
// os.Getwd may return a symlinked $PWD; symlinks inside path are kept.
func PhysicalAbs(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		wd = resolved
	}
	return filepath.Join(wd, path), nil
}
