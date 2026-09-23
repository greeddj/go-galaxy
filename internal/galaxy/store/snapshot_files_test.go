package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

func TestOpenDBsReturnsCacheBusyOnTimeout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, helpers.StoreDBLocal)

	// Hold the consolidated DB's file lock externally to simulate a
	// concurrent process already occupying the cache.
	holder, err := bolt.Open(dbPath, helpers.FileMod, nil)
	if err != nil {
		t.Fatalf("failed to open holder DB: %v", err)
	}
	defer func() {
		_ = holder.Close()
	}()

	const shortTimeout = 200 * time.Millisecond
	start := time.Now()
	_, err = OpenDBs(dir, shortTimeout)
	elapsed := time.Since(start)

	if !errors.Is(err, helpers.ErrCacheBusy) {
		t.Fatalf("expected ErrCacheBusy, got %v", err)
	}
	if !errors.Is(err, bolterrors.ErrTimeout) {
		t.Fatalf("expected wrapped bolt.ErrTimeout, got %v", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("expected OpenDBs to fail fast, took %v", elapsed)
	}
}

// TestOpenBoltClassifiesGarbageAsCorruptButNotAPermissionFailure pins that
// openBolt's corruption arm is a closed set of bbolt sentinels: garbage bytes
// are ErrCorruptSnapshotStore, a permission failure on a valid file is not.
func TestOpenBoltClassifiesGarbageAsCorruptButNotAPermissionFailure(t *testing.T) {
	t.Parallel()

	t.Run("garbage bytes classify as corrupt", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		dbPath := filepath.Join(dir, helpers.StoreDBLocal)
		if err := os.WriteFile(dbPath, []byte("not a bolt database"), helpers.FileMod); err != nil {
			t.Fatalf("failed to write garbage db file: %v", err)
		}

		_, err := OpenDBs(dir, helpers.BoltOpenTimeout)
		if !errors.Is(err, helpers.ErrCorruptSnapshotStore) {
			t.Fatalf("expected ErrCorruptSnapshotStore, got %v", err)
		}
	})

	// Skipped when running as root, since root bypasses the permission bits
	// this subtest relies on to force the failure - the same root guard the
	// cleanup package's own permission-based tests use.
	t.Run("permission failure does not classify as corrupt", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root; permission-based open failure cannot be forced")
		}
		t.Parallel()
		dir := t.TempDir()
		dbPath := filepath.Join(dir, helpers.StoreDBLocal)

		// Seed a valid, empty Bolt database, then strip every permission bit:
		// garbage bytes would trip the corruption arm this subtest must avoid.
		seed, err := bolt.Open(dbPath, helpers.FileMod, nil)
		if err != nil {
			t.Fatalf("failed to seed a valid bolt database: %v", err)
		}
		if err := seed.Close(); err != nil {
			t.Fatalf("failed to close the seeded database: %v", err)
		}
		if err := os.Chmod(dbPath, 0o000); err != nil {
			t.Fatalf("failed to chmod db file unreadable: %v", err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(dbPath, helpers.FileMod); err != nil {
				t.Errorf("failed to restore db file perms: %v", err)
			}
		})

		_, err = OpenDBs(dir, helpers.BoltOpenTimeout)
		if err == nil {
			t.Fatalf("expected OpenDBs to fail against an unreadable db file")
		}
		if errors.Is(err, helpers.ErrCorruptSnapshotStore) {
			t.Fatalf("expected a permission failure not to classify as ErrCorruptSnapshotStore, got %v", err)
		}
	})
}
