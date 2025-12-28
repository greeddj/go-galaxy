package store

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// ClearCacheFiles removes cache files that are safe to delete.
func ClearCacheFiles(cacheDir string) error {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !shouldDeleteCacheFile(name) {
			continue
		}
		if err := removeCacheFile(cacheDir, name); err != nil {
			return err
		}
	}
	return nil
}

func shouldDeleteCacheFile(name string) bool {
	if isDeleteCacheName(name) {
		return true
	}
	if isKeepCacheName(name) {
		return false
	}
	return strings.HasSuffix(name, ".tar.gz") || strings.HasPrefix(name, ".download-") || strings.HasSuffix(name, ".tmp")
}

func isDeleteCacheName(name string) bool {
	deleteList := []string{
		helpers.StoreSnapshotAPICache,
		helpers.StoreSnapshotDepsCache,
		helpers.StoreSnapshotVersions,
		helpers.StoreDBLock,
		helpers.StoreDBLocal,
	}
	return slices.Contains(deleteList, name)
}

func isKeepCacheName(name string) bool {
	keepList := []string{
		helpers.StoreSnapshotMeta,
		helpers.StoreSnapshotInstalled,
		helpers.StoreSnapshotGraph,
		helpers.StoreSnapshotRoots,
		helpers.StoreSnapshotResolved,
		helpers.StoreDBProjects,
	}

	return slices.Contains(keepList, name)
}

func removeCacheFile(cacheDir, name string) error {
	if err := os.Remove(filepath.Join(cacheDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
