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

// SweepDownloadTemps removes the download-temp files a hard-killed run left at
// the top level of cacheDir. It matches helpers.ArtifactDownloadTempPrefix only,
// so artifacts, sidecars, the Bolt database and the lock file are never touched.
func SweepDownloadTemps(cacheDir string) error {
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
		if !strings.HasPrefix(name, helpers.ArtifactDownloadTempPrefix) {
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
	return strings.HasSuffix(name, ".tar.gz") ||
		strings.HasPrefix(name, helpers.ArtifactDownloadTempPrefix) ||
		strings.HasSuffix(name, ".tmp") ||
		strings.HasSuffix(name, helpers.ArtifactSHASidecarSuffix)
}

// isDeleteCacheName reports whether name is one of the nine per-bucket snapshot
// files an older binary wrote, orphans that --clear-cache always reclaims. The
// lock file is not one: unlinking it while flocked lets two runs hold the lock.
func isDeleteCacheName(name string) bool {
	deleteList := []string{
		helpers.StoreSnapshotMeta,
		helpers.StoreSnapshotAPICache,
		helpers.StoreSnapshotDepsCache,
		helpers.StoreSnapshotInstalled,
		helpers.StoreSnapshotGraph,
		helpers.StoreSnapshotRequirements,
		helpers.StoreSnapshotRoots,
		helpers.StoreSnapshotResolved,
		helpers.StoreSnapshotVersions,
	}
	return slices.Contains(deleteList, name)
}

// isKeepCacheName reports whether name is a live file --clear-cache must never
// remove: the Bolt snapshot, the lock file (see isDeleteCacheName) and the
// project registry cleanup relies on.
func isKeepCacheName(name string) bool {
	keepList := []string{
		helpers.StoreDBLocal,
		helpers.StoreDBLock,
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
