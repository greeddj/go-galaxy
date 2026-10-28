package gitfetch

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestObjectStoreLifecycle pins the per-fetch store: a private directory under
// the temp dir, removed by Close, which is safe to repeat and on nil.
func TestObjectStoreLifecycle(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	store, err := newObjectStore(parent, 1<<20)
	if err != nil {
		t.Fatalf("newObjectStore: %v", err)
	}
	info, err := os.Stat(store.dir)
	if err != nil {
		t.Fatalf("stat store dir: %v", err)
	}
	if filepath.Dir(store.dir) != parent || info.Mode().Perm() != storageDirPerm {
		t.Fatalf("store dir %s mode %o, want under %s with mode %o", store.dir, info.Mode().Perm(), parent, storageDirPerm)
	}
	dir := store.dir
	store.Close()
	store.Close()
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Close left the store dir, stat error = %v", err)
	}
	var none *objectStore
	none.Close()

	_, err = newObjectStore(filepath.Join(parent, "absent"), 1)
	if !errors.Is(err, helpers.ErrGitTransportFailed) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("newObjectStore under a missing dir = %v, want ErrGitTransportFailed wrapping fs.ErrNotExist", err)
	}
}

// writeThrough writes data to f, which open just returned, and closes it,
// returning the write's error.
func writeThrough(t *testing.T, f billy.File, openErr error, data string) error {
	t.Helper()
	if openErr != nil {
		t.Fatalf("open: %v", openErr)
	}
	defer func() { _ = f.Close() }()
	_, err := io.WriteString(f, data)
	return err
}

// TestCountingFSChargesEveryWritableFile pins that files from Create,
// OpenFile, TempFile and a chrooted view all charge one shared counter.
func TestCountingFSChargesEveryWritableFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	counter := &byteCounter{max: 10}
	cfs := &countingFS{Filesystem: osfs.New(dir, osfs.WithBoundOS()), counter: counter}
	sub, err := cfs.Chroot("objects")
	if err != nil {
		t.Fatalf("Chroot: %v", err)
	}
	opens := map[string]func() (billy.File, error){
		"Create":   func() (billy.File, error) { return cfs.Create("created") },
		"OpenFile": func() (billy.File, error) { return cfs.OpenFile("opened", os.O_CREATE|os.O_WRONLY, 0o600) },
		"TempFile": func() (billy.File, error) { return cfs.TempFile("", "pack-") },
		"Chroot":   func() (billy.File, error) { return sub.Create("pack/inner") },
	}
	for name, open := range opens {
		f, err := open()
		if err := writeThrough(t, f, err, "ab"); err != nil {
			t.Fatalf("write through %s: %v", name, err)
		}
	}
	if got := counter.written.Load(); got != 8 {
		t.Fatalf("counter = %d after four 2-byte writes, want 8", got)
	}
	//nolint:gosec // path under t.TempDir().
	if inner, err := os.ReadFile(filepath.Join(dir, "objects", "pack", "inner")); err != nil || string(inner) != "ab" {
		t.Fatalf("chrooted write = %q (%v), want it inside the store", inner, err)
	}
}

// TestCountingFSRefusesTheCrossingWrite pins that the write taking the tally
// past the cap is refused before its bytes land.
func TestCountingFSRefusesTheCrossingWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfs := &countingFS{Filesystem: osfs.New(dir, osfs.WithBoundOS()), counter: &byteCounter{max: 4}}
	f, err := cfs.Create("first")
	if err := writeThrough(t, f, err, "abc"); err != nil {
		t.Fatalf("write under the cap: %v", err)
	}
	f, err = cfs.Create("crossing")
	if err := writeThrough(t, f, err, "de"); !errors.Is(err, helpers.ErrResponseTooLarge) {
		t.Fatalf("write past the cap = %v, want ErrResponseTooLarge", err)
	}
	//nolint:gosec // path under t.TempDir().
	if landed, err := os.ReadFile(filepath.Join(dir, "crossing")); err != nil || len(landed) != 0 {
		t.Fatalf("the refused write landed %q (%v), want an empty file", landed, err)
	}
}

// errFS is a billy.Filesystem whose opening methods all fail, standing in
// for a store directory that refuses go-git.
type errFS struct {
	billy.Filesystem
}

var errStoreRefused = errors.New("store refused")

func (errFS) Create(string) (billy.File, error)                     { return nil, errStoreRefused }
func (errFS) OpenFile(string, int, os.FileMode) (billy.File, error) { return nil, errStoreRefused }
func (errFS) TempFile(string, string) (billy.File, error)           { return nil, errStoreRefused }
func (errFS) Chroot(string) (billy.Filesystem, error)               { return nil, errStoreRefused }

// TestCountingFSPassesThroughOpenErrors pins that an inner failure reaches
// go-git unchanged with no file, so nothing is wrapped around a nil handle.
func TestCountingFSPassesThroughOpenErrors(t *testing.T) {
	t.Parallel()
	cfs := &countingFS{Filesystem: errFS{}, counter: &byteCounter{max: 1}}
	if f, err := cfs.Create("x"); f != nil || !errors.Is(err, errStoreRefused) {
		t.Errorf("Create = (%v, %v)", f, err)
	}
	if f, err := cfs.OpenFile("x", os.O_RDONLY, 0); f != nil || !errors.Is(err, errStoreRefused) {
		t.Errorf("OpenFile = (%v, %v)", f, err)
	}
	if f, err := cfs.TempFile("", "x"); f != nil || !errors.Is(err, errStoreRefused) {
		t.Errorf("TempFile = (%v, %v)", f, err)
	}
	if sub, err := cfs.Chroot("x"); sub != nil || !errors.Is(err, errStoreRefused) {
		t.Errorf("Chroot = (%v, %v)", sub, err)
	}
}

// TestStorageDirFor pins where object stores go: the run's temp dir, cleaned,
// or the OS default when there is no supplier or it names nothing.
func TestStorageDirFor(t *testing.T) {
	t.Parallel()
	if got := storageDirFor(nil); got != os.TempDir() {
		t.Errorf("storageDirFor(nil) = %q, want %q", got, os.TempDir())
	}
	if got := storageDirFor(func() string { return "" }); got != os.TempDir() {
		t.Errorf("storageDirFor(empty) = %q, want %q", got, os.TempDir())
	}
	raw := filepath.Join("run", "tmp", "..", "work") + string(os.PathSeparator)
	if got, want := storageDirFor(func() string { return raw }), filepath.Join("run", "work"); got != want {
		t.Errorf("storageDirFor(%q) = %q, want %q", raw, got, want)
	}
}
