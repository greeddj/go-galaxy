package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// lockInfo is persisted inside the lock file.
type lockInfo struct {
	PID int `json:"pid"`
}

// AcquireLock creates a lock file in cacheDir to prevent concurrent writers.
func AcquireLock(cacheDir string) (func() error, error) {
	if cacheDir == "" {
		return nil, helpers.ErrCacheDirEmpty
	}

	lockPath := filepath.Join(cacheDir, helpers.StoreDBLock)
	payload, err := marshalLockPayload()
	if err != nil {
		return nil, err
	}

	for {
		release, ok, err := tryCreateLock(lockPath, payload)
		if ok || err != nil {
			return release, err
		}
		if err := handleExistingLock(lockPath); err != nil {
			return nil, err
		}
	}
}

func marshalLockPayload() ([]byte, error) {
	info := lockInfo{
		PID: os.Getpid(),
	}
	return json.Marshal(&info)
}

func tryCreateLock(lockPath string, payload []byte) (func() error, bool, error) {
	//nolint:gosec // lockPath is derived from cacheDir and is intended for lock file IO.
	f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, helpers.FileMod)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if err := writeLockFile(f, lockPath, payload); err != nil {
		return nil, false, err
	}
	return func() error { return releaseLock(lockPath, payload) }, true, nil
}

func writeLockFile(f *os.File, lockPath string, payload []byte) error {
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(lockPath)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(lockPath)
		return err
	}
	return nil
}

func handleExistingLock(lockPath string) error {
	current, ok, err := readLockInfo(lockPath)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if active := isLockActive(current.PID); active {
		return fmt.Errorf("%w (pid %d)", helpers.ErrAnotherInstanceIsRunning, current.PID)
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func readLockInfo(lockPath string) (lockInfo, bool, error) {
	//nolint:gosec // lockPath is derived from cacheDir and is intended for lock file IO.
	existing, err := os.ReadFile(lockPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return lockInfo{}, false, nil
		}
		return lockInfo{}, false, err
	}
	var current lockInfo
	if err := json.Unmarshal(existing, &current); err != nil {
		return lockInfo{}, false, fmt.Errorf("lock file exists but is invalid: %w", err)
	}
	return current, true, nil
}

// releaseLock removes the lock file if it matches payload.
func releaseLock(lockPath string, payload []byte) error {
	//nolint:gosec // lockPath is created by AcquireLock and is intended for lock file IO.
	existing, err := os.ReadFile(lockPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !bytes.Equal(existing, payload) {
		return nil
	}
	return os.Remove(lockPath)
}

// isLockActive reports whether a process PID is still running.
func isLockActive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
