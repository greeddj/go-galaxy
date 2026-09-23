package collections

// Tests for classifyCollectionsRootError called directly: an end-to-end test
// only sees the already refused write, never whether an escape was told apart
// from an ordinary filesystem error.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestClassifyCollectionsRootErrorReturnsRawErrorForNonEscapeFailure pins that
// a failure with no symlink component (a regular file where a directory is
// expected, ENOTDIR) is returned unchanged, not as ErrCollectionsPathEscape.
func TestClassifyCollectionsRootErrorReturnsRawErrorForNonEscapeFailure(t *testing.T) {
	t.Parallel()
	downloadPath := t.TempDir()
	mustWriteFile(t, filepath.Join(downloadPath, "blocker"), []byte("not a directory"))

	root, err := os.OpenRoot(downloadPath)
	if err != nil {
		t.Fatalf("os.OpenRoot(%s): %v", downloadPath, err)
	}
	t.Cleanup(func() {
		_ = root.Close()
	})

	mkdirErr := root.MkdirAll("blocker/sub", helpers.DirMod)
	if mkdirErr == nil {
		t.Fatal("expected root.MkdirAll(\"blocker/sub\", ...) to fail against a regular file component")
	}

	got := classifyCollectionsRootError(root, "blocker/sub", mkdirErr)
	if errors.Is(got, helpers.ErrCollectionsPathEscape) {
		t.Fatalf("classifyCollectionsRootError(%v) = %v, want it NOT to report ErrCollectionsPathEscape for a non-escape failure",
			mkdirErr, got)
	}
	if !errors.Is(got, mkdirErr) {
		t.Fatalf("classifyCollectionsRootError(%v) = %v, want the raw error returned unchanged", mkdirErr, got)
	}
}

// TestClassifyCollectionsRootErrorReturnsEscapeForSymlinkComponent is the
// positive control: the same call with a symlinked component must report
// helpers.ErrCollectionsPathEscape.
func TestClassifyCollectionsRootErrorReturnsEscapeForSymlinkComponent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	mustMkdirAll(t, downloadPath)
	outside := filepath.Join(root, "outside")
	mustMkdirAll(t, outside)
	if err := os.Symlink(outside, filepath.Join(downloadPath, "escape")); err != nil {
		t.Fatalf("symlink escape -> outside: %v", err)
	}

	osRoot, err := os.OpenRoot(downloadPath)
	if err != nil {
		t.Fatalf("os.OpenRoot(%s): %v", downloadPath, err)
	}
	t.Cleanup(func() {
		_ = osRoot.Close()
	})

	mkdirErr := osRoot.MkdirAll("escape/sub", helpers.DirMod)
	if mkdirErr == nil {
		t.Fatal("expected root.MkdirAll(\"escape/sub\", ...) to fail against a symlinked component")
	}

	got := classifyCollectionsRootError(osRoot, "escape/sub", mkdirErr)
	if !errors.Is(got, helpers.ErrCollectionsPathEscape) {
		t.Fatalf("classifyCollectionsRootError(%v) = %v, want errors.Is helpers.ErrCollectionsPathEscape", mkdirErr, got)
	}
}
