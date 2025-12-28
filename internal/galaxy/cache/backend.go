package cache

import (
	"context"
	"os"

	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// ArtifactFile describes a cached artifact file on disk.
type ArtifactFile struct {
	Path    string
	Cleanup func()
	Meta    map[string]string
}

// ArtifactStore provides access to cached collection artifacts.
type ArtifactStore interface {
	Has(ctx context.Context, key string) (bool, error)
	Fetch(ctx context.Context, key string) (ArtifactFile, error)
	TempFile(ctx context.Context, prefix string) (*os.File, func(), error)
	Commit(ctx context.Context, key, tmpPath string, meta map[string]string) (ArtifactFile, error)
	Delete(ctx context.Context, key string) error
}

// Backend defines a cache backend for state and artifacts.
type Backend interface {
	Open(ctx context.Context) error
	Close(ctx context.Context) error
	Lock(ctx context.Context) (func() error, error)
	LoadStore(ctx context.Context) (*store.Store, error)
	SaveStore(ctx context.Context, st *store.Store) error
	ClearFiles(ctx context.Context) error
	RecordProject(ctx context.Context, requirementsFile, downloadPath string) error
	LoadProjectRegistry(ctx context.Context) (*store.ProjectRegistry, error)
	Artifacts() ArtifactStore
}
