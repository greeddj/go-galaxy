package local

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// classifyCacheFailure maps an error alreadyClassified rejects onto the S3
// backend's classes, fs.ErrPermission to ErrCacheBackendUnusable and anything
// else to ErrCacheBackendUnavailable, so both backends exit alike.
func classifyCacheFailure(err error) error {
	if err == nil || alreadyClassified(err) {
		return err
	}
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: %w", helpers.ErrCacheBackendUnusable, err)
	}
	return fmt.Errorf("%w: %w", helpers.ErrCacheBackendUnavailable, err)
}

// alreadyClassified reports whether err already carries an exit-code verdict
// classifyCacheFailure must not move, fs.ErrNotExist included: absence is a
// normal outcome callers branch on and already exits as a usage error.
func alreadyClassified(err error) bool {
	return errors.Is(err, helpers.ErrCacheBackendUnavailable) ||
		errors.Is(err, helpers.ErrCacheBackendUnusable) ||
		errors.Is(err, helpers.ErrCacheBusy) ||
		errors.Is(err, helpers.ErrAnotherInstanceIsRunning) ||
		errors.Is(err, helpers.ErrCorruptSnapshotStore) ||
		errors.Is(err, helpers.ErrCorruptProjectRegistry) ||
		errors.Is(err, helpers.ErrUnsupportedSchemaVersion) ||
		errors.Is(err, helpers.ErrCacheDirEmpty) ||
		errors.Is(err, fs.ErrNotExist)
}
