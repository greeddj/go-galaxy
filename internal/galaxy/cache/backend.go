// Package cache declares the persistence seam - Backend for state and locking,
// ArtifactStore for tarballs - plus its decorators, LockLostError and the
// policy-aware metadata fetch. A new backend is bound by what the interfaces state.
package cache

import (
	"context"
	"os"

	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// ArtifactFile describes a cached artifact file on disk.
type ArtifactFile struct {
	Cleanup func()
	Meta    map[string]string
	Path    string
	// SHA is the hex sha256 the backend computed over the bytes while producing
	// the file; install trusts it without re-hashing, so a sidecar's digest goes
	// in Meta instead (the local backend always leaves SHA empty).
	SHA string
}

// ArtifactStore provides access to cached collection artifacts. It is safe for
// concurrent use across distinct keys only: callers keep one goroutine per key,
// and a concurrent Commit and Delete of one key is undefined.
type ArtifactStore interface {
	Has(ctx context.Context, key string) (bool, error)
	// Meta reports key's metadata without reading the body: found=false with a
	// nil err means not cached, found with an empty map means cached with no
	// metadata, and found means nothing when err is non-nil.
	Meta(ctx context.Context, key string) (map[string]string, bool, error)
	Fetch(ctx context.Context, key string) (ArtifactFile, error)
	TempFile(ctx context.Context, prefix string) (*os.File, func(), error)
	Commit(ctx context.Context, key, tmpPath string, meta map[string]string) (ArtifactFile, error)
	Delete(ctx context.Context, key string) error
}

// Backend defines a cache backend for state and artifacts. It is not safe for
// concurrent use: both implementations initialize state lazily without locks,
// so the caller serializes every call, Artifacts included.
type Backend interface {
	Open(ctx context.Context) error
	Close(ctx context.Context) error
	// Lock takes the exclusive lock and returns the holder context, canceled
	// with a helpers.ErrCacheLockLost cause on loss (ctx itself where the lock
	// cannot be lost); only the release closure cancels it, never the caller.
	Lock(ctx context.Context) (context.Context, func() error, error)
	LoadStore(ctx context.Context) (*store.Store, error)
	SaveStore(ctx context.Context, st *store.Store) error
	ClearFiles(ctx context.Context) error
	// RecordProject enrolls the project behind requirementsFile in the cleanup
	// registry; an empty rolesPath records none, which cleanup reads as "do not
	// scan" (see store.ProjectRecord).
	RecordProject(ctx context.Context, requirementsFile, downloadPath, rolesPath string) error
	LoadProjectRegistry(ctx context.Context) (*store.ProjectRegistry, error)
	// Artifacts returns the backend's artifact store, which unlike the Backend
	// is safe for concurrent use. Call it only after a successful Open: the S3
	// backend builds its store there.
	Artifacts() ArtifactStore
	// SweepTemp deletes temporary artifact files left by a killed run. Call it
	// only under the exclusive lock, so no live writer's temp can be removed.
	SweepTemp(ctx context.Context) error
}
