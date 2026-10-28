package archive

import (
	"archive/tar"
	"bytes"
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

// extractRefusalCase is one archive the extractor must refuse: its entries,
// the sentinel, and the path that must not exist under dst afterwards.
type extractRefusalCase struct {
	wantErr error
	name    string
	absent  string
	entries []testArchiveEntry
}

// assertExtractRefused extracts tc's archive into a fresh dst under its own
// parent and checks the sentinel, that tc.absent is not under dst, and that
// nothing landed beside dst.
func assertExtractRefused(t *testing.T, tc extractRefusalCase) {
	t.Helper()
	parent := t.TempDir()
	dst := filepath.Join(parent, "dst")
	if err := os.MkdirAll(dst, helpers.DirMod); err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	err := ExtractTarGzStream(context.Background(), bytes.NewReader(buildTestArchive(t, tc.entries)), dst)
	if !errors.Is(err, tc.wantErr) {
		t.Fatalf("ExtractTarGzStream error = %v, want %v", err, tc.wantErr)
	}
	if _, statErr := os.Lstat(filepath.Join(dst, filepath.FromSlash(tc.absent))); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("the refused entry %s exists, lstat error = %v", tc.absent, statErr)
	}
	siblings, readErr := os.ReadDir(parent)
	if readErr != nil {
		t.Fatalf("read parent: %v", readErr)
	}
	if len(siblings) != 1 {
		t.Fatalf("the extraction wrote beside dst: %v", siblings)
	}
}

func symlinkRefusalCases() []extractRefusalCase {
	link := func(name, entryName, target string, want error) extractRefusalCase {
		return extractRefusalCase{
			name: name, absent: entryName, wantErr: want,
			entries: []testArchiveEntry{
				{typeflag: tar.TypeDir, name: "sub/"},
				{typeflag: tar.TypeSymlink, name: entryName, linkname: target},
			},
		}
	}
	return []extractRefusalCase{
		link("empty target", "l", "", helpers.ErrSymlinkTargetIsEmpty),
		link("absolute target", "l", "/etc/passwd", helpers.ErrSymlinkTargetIsAbsolute),
		link("dot target", "l", ".", helpers.ErrSymlinkTarget),
		link("target cleaning to dot", "sub/l", "x/..", helpers.ErrSymlinkTarget),
		link("target resolving to the root", "sub/l", "..", helpers.ErrSymlinkTargetResolvesToRoot),
		link("target escaping the destination", "l", "../outside", helpers.ErrSymlinkTargetEscapesDestination),
		link("deep target escaping the destination", "sub/l", "../../outside", helpers.ErrSymlinkTargetEscapesDestination),
		link("target resolving to its own directory", "sub/l", "../sub", helpers.ErrSymlinkTargetResolvesToSelf),
	}
}

func hardlinkRefusalCases() []extractRefusalCase {
	return []extractRefusalCase{
		{
			name:    "target escaping the destination",
			entries: []testArchiveEntry{{typeflag: tar.TypeLink, name: "h", linkname: "../outside"}},
			absent:  "h", wantErr: helpers.ErrArchiveEntryEscapesDestination,
		},
		{
			name:    "absolute target",
			entries: []testArchiveEntry{{typeflag: tar.TypeLink, name: "h", linkname: "/etc/passwd"}},
			absent:  "h", wantErr: helpers.ErrArchiveEntryIsAbsolutePath,
		},
		{
			name:    "target naming the root",
			entries: []testArchiveEntry{{typeflag: tar.TypeLink, name: "h", linkname: "./"}},
			absent:  "h", wantErr: helpers.ErrHardlinkTargetIsEmpty,
		},
		{
			name:    "target with no name",
			entries: []testArchiveEntry{{typeflag: tar.TypeLink, name: "h", linkname: ""}},
			absent:  "h", wantErr: helpers.ErrArchiveEntryHasEmptyName,
		},
		{
			name: "target through a symlinked directory",
			entries: []testArchiveEntry{
				{typeflag: tar.TypeReg, name: "d/f", content: []byte("data")},
				{typeflag: tar.TypeSymlink, name: "s", linkname: "d"},
				{typeflag: tar.TypeLink, name: "h", linkname: "s/f"},
			},
			absent: "h", wantErr: helpers.ErrArchivePathContainsSymlinkComponent,
		},
		{
			name:    "target that does not exist",
			entries: []testArchiveEntry{{typeflag: tar.TypeLink, name: "h", linkname: "missing"}},
			absent:  "h", wantErr: fs.ErrNotExist,
		},
	}
}

func entryNameRefusalCases() []extractRefusalCase {
	return []extractRefusalCase{
		{
			name:    "name escaping the destination",
			entries: []testArchiveEntry{{typeflag: tar.TypeReg, name: "../evil.txt", content: []byte("x")}},
			absent:  "evil.txt", wantErr: helpers.ErrArchiveEntryEscapesDestination,
		},
		{
			name:    "absolute name",
			entries: []testArchiveEntry{{typeflag: tar.TypeReg, name: "/evil.txt", content: []byte("x")}},
			absent:  "evil.txt", wantErr: helpers.ErrArchiveEntryIsAbsolutePath,
		},
	}
}

// TestExtractRefusesUnsafeEntries pins every symlink, hard link and entry name
// refusal by sentinel through a real extraction: the entry is not created and
// nothing is written outside the destination.
func TestExtractRefusesUnsafeEntries(t *testing.T) {
	t.Parallel()
	groups := map[string][]extractRefusalCase{
		"symlink":  symlinkRefusalCases(),
		"hardlink": hardlinkRefusalCases(),
		"name":     entryNameRefusalCases(),
	}
	for group, cases := range groups {
		for _, tc := range cases {
			t.Run(group+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				assertExtractRefused(t, tc)
			})
		}
	}
}

// TestExtractNormalizesASymlinkTarget is the positive control for the symlink
// refusals: a target spelled with "." and ".." components that stays inside
// is written in its clean form.
func TestExtractNormalizesASymlinkTarget(t *testing.T) {
	t.Parallel()
	dst := t.TempDir()
	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "sub/y", content: []byte("y")},
		{typeflag: tar.TypeSymlink, name: "sub/l", linkname: "./x/../y"},
	}
	if err := ExtractTarGzStream(context.Background(), bytes.NewReader(buildTestArchive(t, entries)), dst); err != nil {
		t.Fatalf("ExtractTarGzStream: %v", err)
	}
	target, err := os.Readlink(filepath.Join(dst, "sub", "l"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "y" {
		t.Fatalf("link target = %q, want y", target)
	}
}

// TestExtractReportsAnUnwritableDestination pins that every entry kind fails
// with the permission error when its directory cannot be written, instead of
// being skipped or misreported as a duplicate.
func TestExtractReportsAnUnwritableDestination(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits do not block writes")
	}
	for _, tc := range []struct {
		name    string
		entries []testArchiveEntry
	}{
		{name: "file at the root", entries: []testArchiveEntry{{typeflag: tar.TypeReg, name: "f", content: []byte("x")}}},
		{name: "file in a new directory", entries: []testArchiveEntry{{typeflag: tar.TypeReg, name: "sub/f", content: []byte("x")}}},
		{name: "directory", entries: []testArchiveEntry{{typeflag: tar.TypeDir, name: "sub/"}}},
		{name: "symlink in a new directory", entries: []testArchiveEntry{{typeflag: tar.TypeSymlink, name: "sub/l", linkname: "x"}}},
		{name: "hard link in a new directory", entries: []testArchiveEntry{{typeflag: tar.TypeLink, name: "sub/h", linkname: "existing"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dst := t.TempDir()
			if err := os.WriteFile(filepath.Join(dst, "existing"), []byte("x"), helpers.FileMod); err != nil {
				t.Fatalf("write existing: %v", err)
			}
			//nolint:gosec // read-only on purpose: 0500 keeps dst listable but unwritable.
			if err := os.Chmod(dst, 0o500); err != nil {
				t.Fatalf("chmod dst: %v", err)
			}
			t.Cleanup(func() {
				//nolint:gosec // restoring the test directory's default mode before cleanup.
				_ = os.Chmod(dst, 0o755)
			})
			err := ExtractTarGzStream(context.Background(), bytes.NewReader(buildTestArchive(t, tc.entries)), dst)
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("ExtractTarGzStream error = %v, want fs.ErrPermission", err)
			}
			if errors.Is(err, helpers.ErrArchiveDuplicateEntry) {
				t.Fatalf("a first write was misreported as a duplicate: %v", err)
			}
		})
	}
}

// TestExtractRefusesANonGzipStream pins that bytes that are not gzip, such as
// an error page, are refused before anything is written.
func TestExtractRefusesANonGzipStream(t *testing.T) {
	t.Parallel()
	dst := t.TempDir()
	err := ExtractTarGzStream(context.Background(), strings.NewReader("<html>not found</html>"), dst)
	if err == nil || !strings.Contains(err.Error(), "failed to create gzip reader") {
		t.Fatalf("ExtractTarGzStream error = %v, want the gzip reader refusal", err)
	}
	entries, readErr := os.ReadDir(dst)
	if readErr != nil {
		t.Fatalf("read dst: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused stream wrote %v", entries)
	}
}

// TestExtractTarGzReadsAFile pins the file entry point on a real archive.
func TestExtractTarGzReadsAFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "a.tar.gz")
	entries := []testArchiveEntry{{typeflag: tar.TypeReg, name: "f.txt", content: []byte("payload")}}
	if err := os.WriteFile(archivePath, buildTestArchive(t, entries), helpers.FileMod); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	dst := filepath.Join(dir, "dst")
	if err := ExtractTarGz(context.Background(), archivePath, dst); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	//nolint:gosec // path under t.TempDir().
	got, err := os.ReadFile(filepath.Join(dst, "f.txt"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("extracted f.txt = %q, %v", got, err)
	}
}

// TestExtractTarGzRefusesAFileByCause pins that a missing, an empty and an
// unreadable archive file are refused each by its own cause.
func TestExtractTarGzRefusesAFileByCause(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst")
	if err := ExtractTarGz(context.Background(), filepath.Join(dir, "absent.tar.gz"), dst); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ExtractTarGz of a missing file = %v, want fs.ErrNotExist", err)
	}
	empty := filepath.Join(dir, "empty.tar.gz")
	if err := os.WriteFile(empty, nil, helpers.FileMod); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	if err := ExtractTarGz(context.Background(), empty, dst); !errors.Is(err, helpers.ErrFileIsEmpty) {
		t.Errorf("ExtractTarGz of an empty file = %v, want ErrFileIsEmpty", err)
	}

	if os.Geteuid() == 0 {
		return
	}
	unreadable := filepath.Join(dir, "unreadable.tar.gz")
	if err := os.WriteFile(unreadable, []byte("x"), 0o000); err != nil {
		t.Fatalf("write unreadable: %v", err)
	}
	if err := ExtractTarGz(context.Background(), unreadable, dst); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("ExtractTarGz of an unreadable file = %v, want fs.ErrPermission", err)
	}
}

// TestFileHashSHA256 pins the digest of known bytes and that a missing file
// and a directory are errors rather than the digest of nothing.
func TestFileHashSHA256(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("hello"), helpers.FileMod); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := FileHashSHA256(path)
	if err != nil {
		t.Fatalf("FileHashSHA256: %v", err)
	}
	if want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"; got != want {
		t.Fatalf("FileHashSHA256 = %s, want %s", got, want)
	}
	if _, err := FileHashSHA256(filepath.Join(dir, "absent")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("FileHashSHA256 of a missing file = %v, want fs.ErrNotExist", err)
	}
	if _, err := FileHashSHA256(dir); !errors.Is(err, syscall.EISDIR) {
		t.Errorf("FileHashSHA256 of a directory = %v, want EISDIR", err)
	}
}

// TestEnsureNoSymlinkParentsForAnUnsanitizedCaller pins the guard's own input
// handling: an empty or "." path is nothing to check, and "." and empty
// components are skipped without hiding a symlink behind them.
func TestEnsureNoSymlinkParentsForAnUnsanitizedCaller(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "real"), helpers.DirMod); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "s")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	for _, rel := range []string{"", ".", "real//x", "./real/x"} {
		if err := ensureNoSymlinkParents(dir, filepath.FromSlash(rel), map[string]struct{}{}); err != nil {
			t.Errorf("ensureNoSymlinkParents(%q) = %v, want nil", rel, err)
		}
	}
	for _, rel := range []string{"./s/x", "real//../s/x"} {
		err := ensureNoSymlinkParents(dir, filepath.FromSlash(rel), map[string]struct{}{})
		if !errors.Is(err, helpers.ErrArchivePathContainsSymlinkComponent) {
			t.Errorf("ensureNoSymlinkParents(%q) = %v, want ErrArchivePathContainsSymlinkComponent", rel, err)
		}
	}
}
