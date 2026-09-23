package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Artifacts implements ArtifactStore for filesystem-backed artifacts.
type Artifacts struct {
	cacheDir string
}

// NewArtifacts returns a local artifact store rooted at cacheDir.
func NewArtifacts(cacheDir string) *Artifacts {
	return &Artifacts{cacheDir: cacheDir}
}

// Has reports whether the artifact exists in the local cache.
func (s *Artifacts) Has(_ context.Context, key string) (bool, error) {
	path, err := s.path(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, classifyCacheFailure(err)
}

// Fetch returns a cached artifact file by key. Meta carries the sidecar's
// sha256 only when it passes helpers.IsSHA256Hex; any other sidecar state
// yields nil Meta, so the caller hashes the file instead of trusting it.
func (s *Artifacts) Fetch(_ context.Context, key string) (cacheManager.ArtifactFile, error) {
	path, err := s.path(key)
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	if _, err := os.Stat(path); err != nil {
		return cacheManager.ArtifactFile{}, classifyCacheFailure(err)
	}
	return cacheManager.ArtifactFile{Path: path, Meta: s.sidecarMeta(path)}, nil
}

// Meta reports key's cached metadata without reading the artifact body.
// Presence comes from Has and the digest from sidecarMeta, so it reports
// exactly what Has and Fetch would, as cacheManager.ArtifactStore requires.
func (s *Artifacts) Meta(ctx context.Context, key string) (map[string]string, bool, error) {
	found, err := s.Has(ctx, key)
	if err != nil || !found {
		return nil, found, err
	}
	// The error arm is unreachable: Has already derived this path from the
	// same key without error. It stays rather than discarding an error.
	path, err := s.path(key)
	if err != nil {
		return nil, false, err
	}
	return s.sidecarMeta(path), true, nil
}

// TempFile creates a temporary file for staging an artifact.
func (s *Artifacts) TempFile(_ context.Context, prefix string) (*os.File, func(), error) {
	dir, err := s.dir()
	if err != nil {
		return nil, nil, err
	}
	file, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return nil, nil, classifyCacheFailure(err)
	}
	cleanup := func() {
		_ = os.Remove(file.Name())
	}
	return file, cleanup, nil
}

// Commit renames a temporary artifact into its cache slot and records a meta
// sha256 that passes helpers.IsSHA256Hex in a sidecar. A sidecar write failure
// is swallowed: the tarball is in place, and a missing sidecar costs a re-hash.
func (s *Artifacts) Commit(_ context.Context, key, tmpPath string, meta map[string]string) (cacheManager.ArtifactFile, error) {
	path, err := s.path(key)
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return cacheManager.ArtifactFile{}, classifyCacheFailure(err)
	}
	result := cacheManager.ArtifactFile{Path: path}
	if sha := strings.TrimSpace(meta["sha256"]); helpers.IsSHA256Hex(sha) {
		_ = os.WriteFile(path+helpers.ArtifactSHASidecarSuffix, []byte(sha), helpers.FileMod)
		result.Meta = map[string]string{"sha256": sha}
	}
	return result, nil
}

// Delete removes an artifact from the local cache, along with its sha256
// sidecar (if any), so eviction never leaves a sidecar orphaned next to a
// tarball that no longer exists.
func (s *Artifacts) Delete(_ context.Context, key string) error {
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return classifyCacheFailure(err)
	}
	if err := os.Remove(path + helpers.ArtifactSHASidecarSuffix); err != nil && !os.IsNotExist(err) {
		return classifyCacheFailure(err)
	}
	return nil
}

// sidecarMeta returns path's sidecar digest as a Meta map when it passes
// helpers.IsSHA256Hex, the gate Commit applies before writing it, and nil for
// a missing, torn or non-hex sidecar. Fetch and Meta share it so they agree.
func (s *Artifacts) sidecarMeta(path string) map[string]string {
	//nolint:gosec // path is derived from the process-controlled artifact key, not user input.
	data, err := os.ReadFile(path + helpers.ArtifactSHASidecarSuffix)
	if err != nil {
		return nil
	}
	sha := strings.TrimSpace(string(data))
	if !helpers.IsSHA256Hex(sha) {
		return nil
	}
	return map[string]string{"sha256": sha}
}

// dir returns the base cache directory for artifacts.
func (s *Artifacts) dir() (string, error) {
	trimmed := strings.TrimSpace(s.cacheDir)
	if trimmed == "" {
		return "", helpers.ErrCacheDirEmpty
	}
	return trimmed, nil
}

// path builds the full artifact path for a key.
func (s *Artifacts) path(key string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", errArtifactKeyEmpty
	}
	dir, err := s.dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, key), nil
}
