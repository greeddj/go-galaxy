package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
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
	return false, err
}

// Fetch returns a cached artifact file by key.
func (s *Artifacts) Fetch(_ context.Context, key string) (cacheManager.ArtifactFile, error) {
	path, err := s.path(key)
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	if _, err := os.Stat(path); err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	return cacheManager.ArtifactFile{Path: path}, nil
}

// TempFile creates a temporary file for staging an artifact.
func (s *Artifacts) TempFile(_ context.Context, prefix string) (*os.File, func(), error) {
	dir, err := s.dir()
	if err != nil {
		return nil, nil, err
	}
	file, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		_ = os.Remove(file.Name())
	}
	return file, cleanup, nil
}

// Commit moves a temporary artifact into its final cache location.
func (s *Artifacts) Commit(_ context.Context, key, tmpPath string, _ map[string]string) (cacheManager.ArtifactFile, error) {
	path, err := s.path(key)
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	return cacheManager.ArtifactFile{Path: path}, nil
}

// Delete removes an artifact from the local cache.
func (s *Artifacts) Delete(_ context.Context, key string) error {
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// dir returns the base cache directory for artifacts.
func (s *Artifacts) dir() (string, error) {
	trimmed := strings.TrimSpace(s.cacheDir)
	if trimmed == "" {
		return "", errCacheDirEmpty
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
