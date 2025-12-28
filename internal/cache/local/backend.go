package local

import (
	"context"
	"os"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// Backend provides a filesystem-backed cache backend.
type Backend struct {
	cacheDir  string
	dbs       *store.DBs
	artifacts *Artifacts
}

// New creates a Backend rooted at cacheDir.
func New(cacheDir string) *Backend {
	return &Backend{
		cacheDir:  cacheDir,
		artifacts: NewArtifacts(cacheDir),
	}
}

// Open initializes local backend storage.
func (b *Backend) Open(_ context.Context) error {
	return b.ensureOpen()
}

// Close releases any open resources.
func (b *Backend) Close(_ context.Context) error {
	if b.dbs == nil {
		return nil
	}
	err := b.dbs.Close()
	b.dbs = nil
	return err
}

// Lock obtains an exclusive lock for the cache directory.
func (b *Backend) Lock(_ context.Context) (func() error, error) {
	if b.cacheDir == "" {
		return nil, errCacheDirEmpty
	}
	return store.AcquireLock(b.cacheDir)
}

// LoadStore loads the persistent snapshot store.
func (b *Backend) LoadStore(_ context.Context) (*store.Store, error) {
	if err := b.ensureOpen(); err != nil {
		return nil, err
	}
	return store.Load(b.dbs)
}

// SaveStore persists the snapshot store.
func (b *Backend) SaveStore(_ context.Context, st *store.Store) error {
	if err := b.ensureOpen(); err != nil {
		return err
	}
	return store.Save(b.dbs, st)
}

// ClearFiles removes cached artifact files from disk.
func (b *Backend) ClearFiles(_ context.Context) error {
	if b.cacheDir == "" {
		return errCacheDirEmpty
	}
	return store.ClearCacheFiles(b.cacheDir)
}

// RecordProject records the project in the local registry.
func (b *Backend) RecordProject(_ context.Context, requirementsFile, downloadPath string) error {
	if b.cacheDir == "" {
		return errCacheDirEmpty
	}
	return store.RecordProject(b.cacheDir, requirementsFile, downloadPath)
}

// LoadProjectRegistry loads the local project registry.
func (b *Backend) LoadProjectRegistry(_ context.Context) (*store.ProjectRegistry, error) {
	if b.cacheDir == "" {
		return nil, errCacheDirEmpty
	}
	return store.LoadProjectRegistry(b.cacheDir)
}

// Artifacts returns the artifact store for the backend.
func (b *Backend) Artifacts() cacheManager.ArtifactStore {
	return b.artifacts
}

// ensureOpen initializes storage if it is not yet opened.
func (b *Backend) ensureOpen() error {
	if b.dbs != nil {
		return nil
	}
	if b.cacheDir == "" {
		return errCacheDirEmpty
	}
	if err := os.MkdirAll(b.cacheDir, dirMod); err != nil {
		return err
	}
	dbs, err := store.OpenDBs(b.cacheDir)
	if err != nil {
		return err
	}
	b.dbs = dbs
	return nil
}
