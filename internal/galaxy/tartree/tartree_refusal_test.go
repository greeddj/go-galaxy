package tartree

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestLoadReportsAnUnusableTempDir pins that a temp directory that does not
// exist fails Load by cause, before the tarball is read.
func TestLoadReportsAnUnusableTempDir(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "absent")
	_, err := Load(context.Background(), filepath.Join(t.TempDir(), "unread.tar.gz"), func() string { return missing })
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "creating role tarball extraction directory") {
		t.Fatalf("Load error = %v, want the extraction directory's fs.ErrNotExist", err)
	}
}

// TestTreeReadsStayInTheRoleRoot pins that a missing path, a file read as a
// directory and a path climbing out of the role root are all errors.
func TestTreeReadsStayInTheRoleRoot(t *testing.T) {
	t.Parallel()
	tree := loadFixture(t, prefixed("myrole-1.0", flatRole()))

	if _, err := tree.ReadDir("absent"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadDir(absent) = %v, want fs.ErrNotExist", err)
	}
	if _, err := tree.ReadDir("meta/main.yml"); err == nil {
		t.Error("ReadDir of a regular file returned no error")
	}
	if _, err := tree.Open("tasks/absent.yml"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open(tasks/absent.yml) = %v, want fs.ErrNotExist", err)
	}
	if _, err := tree.ReadDir(".."); err == nil {
		t.Error("ReadDir(..) listed the directory above the role root")
	}
	if _, err := tree.Open("../myrole-1.0/meta/main.yml"); err == nil {
		t.Error("Open climbed out of the role root")
	}
}

// TestReadDirRefusesASpecialFile pins that an entry that is neither a file, a
// directory nor a symlink is refused with ErrRoleTarballEntryInvalid rather
// than skipped or opened.
func TestReadDirRefusesASpecialFile(t *testing.T) {
	t.Parallel()
	tree := loadFixture(t, flatRole())
	if err := syscall.Mkfifo(filepath.Join(tree.dir, "tasks", "pipe"), 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	if _, err := tree.ReadDir("tasks"); !errors.Is(err, helpers.ErrRoleTarballEntryInvalid) {
		t.Fatalf("ReadDir error = %v, want ErrRoleTarballEntryInvalid", err)
	}
}

// TestCleanupIsIdempotent pins that Cleanup removes the extraction directory
// and that a second call is harmless.
func TestCleanupIsIdempotent(t *testing.T) {
	t.Parallel()
	tree, err := Load(context.Background(), writeTarGz(t, flatRole()), t.TempDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tree.Cleanup()
	tree.Cleanup()
	if _, err := os.Stat(tree.dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("extraction directory survived Cleanup, stat error = %v", err)
	}
}
