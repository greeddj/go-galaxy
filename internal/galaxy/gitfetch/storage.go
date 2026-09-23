package gitfetch

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// objectCacheSize bounds go-git's in-memory object cache for one fetch.
	objectCacheSize = 32 * cache.MiByte
	// largeObjectThreshold is the blob size above which go-git streams an
	// object from disk instead of buffering it: the builder reads every blob
	// exactly once per pass, so nothing is gained by holding a large one.
	largeObjectThreshold = int64(4 << 20)
	// storageDirPerm keeps the per-fetch object store private to this user.
	storageDirPerm = 0o700
	// storageDirPattern names the per-fetch directory under the run's temp
	// dir; the random suffix is os.MkdirTemp's.
	storageDirPattern = "go-galaxy-git-*"
)

// objectStore is one fetch's bare object storage on disk: the go-git storer,
// the byte counter behind it, and the directory to remove when done.
type objectStore struct {
	storer  *filesystem.Storage
	counter *byteCounter
	dir     string
}

// newObjectStore creates a private directory under tempDir and a go-git storage
// over it counting every write against maxBytes; the filesystem is bound, not
// merely chrooted, so no path go-git computes can leave it through a symlink.
func newObjectStore(tempDir string, maxBytes int64) (*objectStore, error) {
	dir, err := os.MkdirTemp(tempDir, storageDirPattern)
	if err != nil {
		return nil, fmt.Errorf("%w: creating git object storage: %w", helpers.ErrGitTransportFailed, err)
	}
	if err := os.Chmod(dir, storageDirPerm); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("%w: securing git object storage: %w", helpers.ErrGitTransportFailed, err)
	}
	counter := &byteCounter{max: maxBytes}
	fs := &countingFS{Filesystem: osfs.New(dir, osfs.WithBoundOS()), counter: counter}
	storer := filesystem.NewStorageWithOptions(fs, cache.NewObjectLRU(objectCacheSize), filesystem.Options{
		ExclusiveAccess:      true,
		LargeObjectThreshold: largeObjectThreshold,
	})
	return &objectStore{storer: storer, counter: counter, dir: dir}, nil
}

// Close releases the storer's descriptors and removes the directory. It is
// safe to call more than once.
func (s *objectStore) Close() {
	if s == nil || s.dir == "" {
		return
	}
	_ = s.storer.Close()
	_ = os.RemoveAll(s.dir)
	s.dir = ""
}

// bytes reports how many bytes the fetch wrote.
func (s *objectStore) bytes() int64 {
	return s.counter.written.Load()
}

// byteCounter is one storage tree's shared write tally; the first write past
// max fails. The cap sits on disk, not the wire, because the ssh transport
// exposes no reader to wrap and disk is where both transports converge.
type byteCounter struct {
	written atomic.Int64
	max     int64
}

func (c *byteCounter) add(n int) error {
	if c.written.Add(int64(n)) > c.max {
		return fmt.Errorf("%w: git fetch wrote more than %d bytes", helpers.ErrResponseTooLarge, c.max)
	}
	return nil
}

// countingFS is a billy.Filesystem whose writable files count their writes;
// Chroot returns a countingFS sharing the counter, because go-git's dotgit
// layer chroots into subdirectories for some of its files.
type countingFS struct {
	billy.Filesystem

	counter *byteCounter
}

func (fs *countingFS) Create(filename string) (billy.File, error) {
	f, err := fs.Filesystem.Create(filename)
	if err != nil {
		return nil, err
	}
	return &countingFile{File: f, counter: fs.counter}, nil
}

func (fs *countingFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	f, err := fs.Filesystem.OpenFile(filename, flag, perm)
	if err != nil {
		return nil, err
	}
	return &countingFile{File: f, counter: fs.counter}, nil
}

func (fs *countingFS) TempFile(dir, prefix string) (billy.File, error) {
	f, err := fs.Filesystem.TempFile(dir, prefix)
	if err != nil {
		return nil, err
	}
	return &countingFile{File: f, counter: fs.counter}, nil
}

func (fs *countingFS) Chroot(path string) (billy.Filesystem, error) {
	inner, err := fs.Filesystem.Chroot(path)
	if err != nil {
		return nil, err
	}
	return &countingFS{Filesystem: inner, counter: fs.counter}, nil
}

// countingFile is a billy.File whose Write charges the shared counter before
// the bytes land, so a refused write leaves the tally at the cap and nothing
// beyond it on disk.
type countingFile struct {
	billy.File

	counter *byteCounter
}

func (f *countingFile) Write(p []byte) (int, error) {
	if err := f.counter.add(len(p)); err != nil {
		return 0, err
	}
	return f.File.Write(p)
}

// storageDirFor returns the directory a Fetcher creates object stores under:
// the run's temp dir, or the OS default when the Fetcher was built without one.
func storageDirFor(tempDir func() string) string {
	if tempDir == nil {
		return os.TempDir()
	}
	if dir := tempDir(); dir != "" {
		return filepath.Clean(dir)
	}
	return os.TempDir()
}
