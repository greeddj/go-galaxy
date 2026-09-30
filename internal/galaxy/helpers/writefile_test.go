package helpers

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
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

// assertOnlyEntries fails unless dir holds exactly the named entries, so a
// leftover temp or a created symlink target shows up.
func assertOnlyEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entries = %v, want %v", got, want)
		}
	}
}

// assertExistsError fails unless err is fs.ErrExist as a *fs.PathError that
// names path, never the temp the link was made from.
func assertExistsError(t *testing.T, err error, path string) {
	t.Helper()
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want fs.ErrExist", err)
	}
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) || pathErr.Path != path {
		t.Fatalf("err = %#v, want a *fs.PathError naming %s", err, path)
	}
}

// TestWriteFileExclusiveCreates pins the content, FileMod past the umask and
// a directory holding the target alone afterwards.
func TestWriteFileExclusiveCreates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "galaxy.toml")
	want := []byte("[project]\n")
	if err := WriteFileExclusive(path, want); err != nil {
		t.Fatalf("WriteFileExclusive: %v", err)
	}
	if got := mustReadFile(t, path); !bytes.Equal(got, want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != FileMod {
		t.Fatalf("mode = %o, want %o", perm, FileMod)
	}
	assertOnlyEntries(t, dir, "galaxy.toml")
}

// plantedTargets puts a file, a directory or a symlink, dangling or not, at
// path; a symlink points at dir/nowhere, which holds "original" when it exists.
func plantedTargets() map[string]func(t *testing.T, dir, path string) {
	return map[string]func(t *testing.T, dir, path string){
		"file": func(t *testing.T, _, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte("original"), FileMod); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
		},
		"directory": func(t *testing.T, _, path string) {
			t.Helper()
			if err := os.Mkdir(path, DirMod); err != nil {
				t.Fatalf("Mkdir: %v", err)
			}
		},
		"dangling symlink": func(t *testing.T, dir, path string) {
			t.Helper()
			if err := os.Symlink(filepath.Join(dir, "nowhere"), path); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
		},
		"symlink to a file": func(t *testing.T, dir, path string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, "nowhere"), []byte("original"), FileMod); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if err := os.Symlink(filepath.Join(dir, "nowhere"), path); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
		},
	}
}

// assertTargetUnchanged fails unless path is still the entry before was, and
// a symlink's target, when it exists, still holds "original".
func assertTargetUnchanged(t *testing.T, dir, path string, before os.FileInfo) {
	t.Helper()
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() {
		t.Fatalf("target changed: %v -> %v", before, after)
	}
	nowhere := filepath.Join(dir, "nowhere")
	if _, err := os.Lstat(nowhere); err == nil {
		if got := mustReadFile(t, nowhere); string(got) != "original" {
			t.Fatalf("symlink target content = %q, want it untouched", got)
		}
	}
}

// TestWriteFileExclusiveNeverReplaces pins that a file, a directory and a
// symlink, dangling or not, stay as they were: nothing lands in them or
// through them, a dangling link's target is never created, no temp is left.
func TestWriteFileExclusiveNeverReplaces(t *testing.T) {
	t.Parallel()
	for name, plant := range plantedTargets() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "galaxy.toml")
			plant(t, dir, path)
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatalf("Lstat: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			assertExistsError(t, WriteFileExclusive(path, []byte("new")), path)
			assertTargetUnchanged(t, dir, path, before)
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			assertOnlyEntries(t, dir, names...)
		})
	}
}

// TestWriteFileExclusiveFallsBackWithoutLinks pins the O_EXCL path a file
// system without hard links takes: the target is written with FileMod, and
// anything already there is still fs.ErrExist.
func TestWriteFileExclusiveFallsBackWithoutLinks(t *testing.T) {
	t.Parallel()
	noLinks := func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "galaxy.toml")
	want := []byte("[project]\n")
	if err := writeFileExclusive(path, want, noLinks); err != nil {
		t.Fatalf("writeFileExclusive: %v", err)
	}
	if got := mustReadFile(t, path); !bytes.Equal(got, want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != FileMod {
		t.Fatalf("mode = %o, want %o", perm, FileMod)
	}
	assertOnlyEntries(t, dir, "galaxy.toml")

	assertExistsError(t, writeFileExclusive(path, []byte("new"), noLinks), path)
	if got := mustReadFile(t, path); !bytes.Equal(got, want) {
		t.Fatalf("content = %q after a refused write, want %q", got, want)
	}
	assertOnlyEntries(t, dir, "galaxy.toml")
}

// TestWriteFileExclusiveNeedsTheDirectory pins that a missing parent is
// fs.ErrNotExist and is not created.
func TestWriteFileExclusiveNeedsTheDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "missing", "galaxy.toml")
	if err := WriteFileExclusive(path, []byte("x")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Lstat(missing) = %v, want the directory never created", err)
	}
}
