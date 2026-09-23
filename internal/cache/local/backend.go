// Package local implements the filesystem cache backend: a BoltDB snapshot, a
// JSON project registry, flat single-element artifact files and an advisory
// flock; classifyCacheFailure maps its failures onto the S3 backend's classes.
package local

import (
	"context"
	"os"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// Backend provides a filesystem-backed cache backend.
type Backend struct {
	dbs       *store.DBs
	artifacts *Artifacts
	cacheDir  string
}

// New creates a Backend rooted at cacheDir.
func New(cacheDir string) *Backend {
	return &Backend{
		cacheDir:  cacheDir,
		artifacts: NewArtifacts(cacheDir),
	}
}

// Open ensures the cache directory exists. Bolt files are opened lazily by
// ensureOpen so the instance lock can be taken first, and a second process
// fails at once instead of waiting on Bolt's own file lock.
func (b *Backend) Open(_ context.Context) error {
	return classifyCacheFailure(b.ensureDir())
}

// Close releases any open resources.
func (b *Backend) Close(_ context.Context) error {
	if b.dbs == nil {
		return nil
	}
	err := b.dbs.Close()
	b.dbs = nil
	return classifyCacheFailure(err)
}

// Lock takes the cache directory's flock before any Bolt file is opened. The
// holder context is ctx unchanged: no process can take a held flock away, so
// ownership is never lost and a cancellation is reported as a cancellation.
func (b *Backend) Lock(ctx context.Context) (context.Context, func() error, error) {
	if err := b.ensureDir(); err != nil {
		return nil, nil, classifyCacheFailure(err)
	}
	release, err := store.AcquireLock(b.cacheDir)
	if err != nil {
		return nil, nil, classifyCacheFailure(err)
	}
	return ctx, release, nil
}

// LoadStore loads the persistent snapshot store.
func (b *Backend) LoadStore(_ context.Context) (*store.Store, error) {
	if err := b.ensureOpen(); err != nil {
		return nil, classifyCacheFailure(err)
	}
	st, err := store.Load(b.dbs)
	if err != nil {
		return nil, classifyCacheFailure(err)
	}
	return st, nil
}

// SaveStore persists the snapshot store.
func (b *Backend) SaveStore(_ context.Context, st *store.Store) error {
	if err := b.ensureOpen(); err != nil {
		return classifyCacheFailure(err)
	}
	return classifyCacheFailure(store.Save(b.dbs, st))
}

// ClearFiles removes cached artifact files from disk.
func (b *Backend) ClearFiles(_ context.Context) error {
	if b.cacheDir == "" {
		return helpers.ErrCacheDirEmpty
	}
	return classifyCacheFailure(store.ClearCacheFiles(b.cacheDir))
}

// RecordProject records the project in the local registry.
func (b *Backend) RecordProject(_ context.Context, requirementsFile, downloadPath, rolesPath string) error {
	if b.cacheDir == "" {
		return helpers.ErrCacheDirEmpty
	}
	return classifyCacheFailure(store.RecordProject(b.cacheDir, requirementsFile, downloadPath, rolesPath))
}

// LoadProjectRegistry loads the local project registry.
func (b *Backend) LoadProjectRegistry(_ context.Context) (*store.ProjectRegistry, error) {
	if b.cacheDir == "" {
		return nil, helpers.ErrCacheDirEmpty
	}
	registry, err := store.LoadProjectRegistry(b.cacheDir)
	if err != nil {
		return nil, classifyCacheFailure(err)
	}
	return registry, nil
}

// Artifacts returns the artifact store for the backend.
func (b *Backend) Artifacts() cacheManager.ArtifactStore {
	return b.artifacts
}

// SweepTemp removes download-temp files a killed run left in the cache
// directory. Call it only under the backend's exclusive lock, so no live
// writer's temp file can be removed.
func (b *Backend) SweepTemp(_ context.Context) error {
	if b.cacheDir == "" {
		return helpers.ErrCacheDirEmpty
	}
	return classifyCacheFailure(store.SweepDownloadTemps(b.cacheDir))
}

// ensureOpen lazily opens the Bolt snapshot, so callers can take the instance
// lock first; a Bolt open that still meets another holder gives up after
// helpers.BoltOpenTimeout instead of blocking forever.
func (b *Backend) ensureOpen() error {
	if b.dbs != nil {
		return nil
	}
	if err := b.ensureDir(); err != nil {
		return err
	}
	dbs, err := store.OpenDBs(b.cacheDir, helpers.BoltOpenTimeout)
	if err != nil {
		return err
	}
	b.dbs = dbs
	return nil
}

// ensureDir validates the cache directory is configured and creates it if
// missing, without touching any Bolt file.
func (b *Backend) ensureDir() error {
	if b.cacheDir == "" {
		return helpers.ErrCacheDirEmpty
	}
	return os.MkdirAll(b.cacheDir, helpers.DirMod)
}
