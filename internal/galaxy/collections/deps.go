package collections

import (
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	bolt "go.etcd.io/bbolt"
)

type collectionDeps struct {
	cfg     *config.Config
	runtime *infra.Infra
	st      *store.Store
}

type installDeps struct {
	collectionDeps

	artifacts cacheManager.ArtifactStore
	db        *bolt.DB
}

type prefetchDeps struct {
	collectionDeps

	artifacts cacheManager.ArtifactStore
}

func newCollectionDeps(cfg *config.Config, runtime *infra.Infra, st *store.Store) collectionDeps {
	return collectionDeps{cfg: cfg, runtime: runtime, st: st}
}

func newInstallDeps(
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	artifacts cacheManager.ArtifactStore,
	db *bolt.DB,
) installDeps {
	return installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
		db:             db,
	}
}

func newPrefetchDeps(
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	artifacts cacheManager.ArtifactStore,
) prefetchDeps {
	return prefetchDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
	}
}
