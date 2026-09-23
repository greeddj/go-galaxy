package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// DBs holds the single BoltDB handle backing snapshot storage. All snapshot
// buckets live in one file so a save is one atomic Bolt transaction instead
// of one independent file write per bucket.
type DBs struct {
	db *bolt.DB
}

// OpenDBs opens the single consolidated snapshot BoltDB file under cacheDir.
// The file is opened with timeout bounding how long it waits on another
// process's flock, so a busy cache fails fast instead of hanging forever.
func OpenDBs(cacheDir string, timeout time.Duration) (*DBs, error) {
	db, err := openBolt(filepath.Join(cacheDir, helpers.StoreDBLocal), timeout)
	if err != nil {
		return nil, err
	}
	return &DBs{db: db}, nil
}

// Close closes the open BoltDB handle.
func (s *DBs) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// openBolt opens a Bolt database, reporting a flock timeout as ErrCacheBusy and
// only bbolt's ErrInvalid, ErrVersionMismatch and ErrChecksum as corrupt; an
// mmap or OS failure such as a permission error stays unclassified.
func openBolt(path string, timeout time.Duration) (*bolt.DB, error) {
	db, err := bolt.Open(path, helpers.FileMod, &bolt.Options{Timeout: timeout})
	if err != nil {
		if errors.Is(err, bolterrors.ErrTimeout) {
			return nil, fmt.Errorf("%w: %w", helpers.ErrCacheBusy, err)
		}
		if errors.Is(err, bolterrors.ErrInvalid) || errors.Is(err, bolterrors.ErrVersionMismatch) || errors.Is(err, bolterrors.ErrChecksum) {
			return nil, fmt.Errorf("%w: %w", helpers.ErrCorruptSnapshotStore, err)
		}
		return nil, err
	}
	return db, nil
}
