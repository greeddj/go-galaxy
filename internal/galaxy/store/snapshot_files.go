package store

import (
	"path/filepath"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
)

// DBs holds BoltDB handles for snapshot storage buckets.
type DBs struct {
	meta         *bolt.DB
	apiCache     *bolt.DB
	depsCache    *bolt.DB
	installed    *bolt.DB
	graph        *bolt.DB
	requirements *bolt.DB
	roots        *bolt.DB
	resolved     *bolt.DB
	versions     *bolt.DB
}

// OpenDBs opens all snapshot BoltDB files under cacheDir.
func OpenDBs(cacheDir string) (*DBs, error) {
	dbs := &DBs{}
	var err error

	dbs.meta, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotMeta))
	if err != nil {
		return nil, err
	}
	dbs.apiCache, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotAPICache))
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.depsCache, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotDepsCache))
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.installed, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotInstalled))
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.graph, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotGraph))
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.requirements, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotRequirements))
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.roots, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotRoots))
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.resolved, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotResolved))
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.versions, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotVersions))
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}

	return dbs, nil
}

// Close closes all open BoltDB handles.
func (s *DBs) Close() error {
	if s == nil {
		return nil
	}
	var firstErr error
	closeDB := func(db *bolt.DB) {
		if db == nil {
			return
		}
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	closeDB(s.meta)
	closeDB(s.apiCache)
	closeDB(s.depsCache)
	closeDB(s.installed)
	closeDB(s.graph)
	closeDB(s.requirements)
	closeDB(s.roots)
	closeDB(s.resolved)
	closeDB(s.versions)
	return firstErr
}

// openBolt opens a Bolt database at the given path.
func openBolt(path string) (*bolt.DB, error) {
	return bolt.Open(path, helpers.FileMod, nil)
}
