//go:build unix

package store

import (
	"errors"
	"os"
	"sync"
	"syscall"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// flockFile takes a non-blocking exclusive flock(2) on lockPath, a content-free
// anchor, so a crashed run's leftovers never pose as a live lock; O_NOFOLLOW
// refuses a planted symlink. A repeat release returns the first call's result.
func flockFile(lockPath string) (func() error, error) {
	//nolint:gosec // G304: lockPath is derived from the configured cache dir and names the cache lock file.
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, helpers.FileMod)
	if err != nil {
		return nil, err
	}

	fd := fd(f)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, helpers.ErrAnotherInstanceIsRunning
		}
		return nil, err
	}

	release := sync.OnceValue(func() error {
		releaseErr := syscall.Flock(fd, syscall.LOCK_UN)
		if closeErr := f.Close(); releaseErr == nil {
			releaseErr = closeErr
		}
		return releaseErr
	})

	return release, nil
}

// fd narrows an os.File descriptor to the int type syscall.Flock expects.
// File descriptors are small non-negative values on unix, so this
// conversion never overflows in practice despite the uintptr source type.
func fd(f *os.File) int {
	return int(f.Fd())
}
