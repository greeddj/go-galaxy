package helpers

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// mustReadFile reads path and fails the test on error, giving gosec's G304 a
// single call site to annotate.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	//nolint:gosec // path is built from this test's own t.TempDir fixture, never external input.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return data
}

// TestWriteFileAtomicWritesContentAndMode asserts a successful write round
// trips the content, stamps FileMod exactly (no umask filtering), and leaves
// no temp file behind in the target directory.
func TestWriteFileAtomicWritesContentAndMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	want := []byte(`{"ok":true}`)

	if err := WriteFileAtomic(path, want); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got := mustReadFile(t, path)
	if !bytes.Equal(got, want) {
		t.Fatalf("content = %q, want %q", got, want)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != FileMod {
		t.Fatalf("mode = %o, want %o", perm, FileMod)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no leftover temp files, found %v", matches)
	}
}

// TestWriteFileAtomicCreatesMissingParents pins that a missing parent tree is
// created. The directory mode is not asserted: os.MkdirAll is umask-filtered.
func TestWriteFileAtomicCreatesMissingParents(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "report.json")
	want := []byte("nested")

	if err := WriteFileAtomic(path, want); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got := mustReadFile(t, path)
	if !bytes.Equal(got, want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestWriteFileAtomicReplacesSymlinkWithoutFollowing pins that a symlink
// planted at the target is replaced by the rename, never followed and
// truncated in place as a plain os.WriteFile would.
func TestWriteFileAtomicReplacesSymlinkWithoutFollowing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	victimDir := filepath.Join(dir, "victim-dir")
	if err := os.Mkdir(victimDir, DirMod); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	victim := filepath.Join(victimDir, "victim.txt")
	sentinel := []byte("do-not-touch")
	if err := os.WriteFile(victim, sentinel, FileMod); err != nil {
		t.Fatalf("WriteFile victim: %v", err)
	}

	target := filepath.Join(dir, "report.json")
	if err := os.Symlink(victim, target); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	want := []byte("new-content")
	if err := WriteFileAtomic(target, want); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	gotVictim := mustReadFile(t, victim)
	if !bytes.Equal(gotVictim, sentinel) {
		t.Fatalf("victim content = %q, want unchanged %q", gotVictim, sentinel)
	}

	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("Lstat target: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("target is still a symlink after WriteFileAtomic")
	}

	gotTarget := mustReadFile(t, target)
	if !bytes.Equal(gotTarget, want) {
		t.Fatalf("target content = %q, want %q", gotTarget, want)
	}
}

// TestWriteFileAtomicOverwritesExistingFile pins that a rerun replaces the
// file, which is why the target is not opened with O_EXCL or O_NOFOLLOW.
func TestWriteFileAtomicOverwritesExistingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")

	if err := WriteFileAtomic(path, []byte("first")); err != nil {
		t.Fatalf("WriteFileAtomic (first): %v", err)
	}
	if err := WriteFileAtomic(path, []byte("second")); err != nil {
		t.Fatalf("WriteFileAtomic (second): %v", err)
	}

	got := mustReadFile(t, path)
	if string(got) != "second" {
		t.Fatalf("content = %q, want %q", got, "second")
	}
}

// TestWriteFileAtomicLeavesNoTempOnFailure pins that a failed rename (the
// target is a directory) leaves no temp file; the error is checked only for
// non-nil because its errno differs by platform.
func TestWriteFileAtomicLeavesNoTempOnFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "report.json")
	if err := os.Mkdir(target, DirMod); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	if err := WriteFileAtomic(target, []byte("data")); err == nil {
		t.Fatalf("expected WriteFileAtomic to fail when target is an existing directory")
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no leftover temp files, found %v", matches)
	}
}
