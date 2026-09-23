package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestSanitizeArchivePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		wantErr error
		name    string
		input   string
		want    string
	}{
		{name: "empty", input: "", wantErr: helpers.ErrArchiveEntryHasEmptyName},
		{name: "abs", input: "/etc/passwd", wantErr: helpers.ErrArchiveEntryIsAbsolutePath},
		{name: "escape", input: "../evil", wantErr: helpers.ErrArchiveEntryEscapesDestination},
		{name: "dot", input: ".", want: ""},
		{name: "ok", input: "dir/file", want: filepath.FromSlash("dir/file")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := sanitizeArchivePath(tt.input)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error")
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

// testArchiveEntry describes one tar entry for buildTestArchive. mode is the
// tar header's permission bits; zero means "use the package's own default
// of 0o644", preserving every existing caller that never set it.
type testArchiveEntry struct {
	name     string
	linkname string
	content  []byte
	typeflag byte
	mode     int64
}

// buildTestArchive renders entries, in order, into an in-memory tar.gz,
// mirroring how fakegalaxy builds artifacts elsewhere in this repo.
func buildTestArchive(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()

	// testEntryModTime stamps every entry so these tests never depend on
	// wall-clock time.
	testEntryModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{
			Typeflag: e.typeflag,
			Name:     e.name,
			Linkname: e.linkname,
			Size:     int64(len(e.content)),
			Mode:     mode,
			ModTime:  testEntryModTime,
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("failed to write tar header for %s: %v", e.name, err)
		}
		if len(e.content) > 0 {
			if _, err := tw.Write(e.content); err != nil {
				t.Fatalf("failed to write tar content for %s: %v", e.name, err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// TestExtractMemoizedDeepTreeExtractsCorrectly pins that the
// ensureNoSymlinkParents memo is purely an optimization: many files sharing
// one deep parent chain all land at the right path with the right content.
func TestExtractMemoizedDeepTreeExtractsCorrectly(t *testing.T) {
	t.Parallel()

	const (
		depth = 6
		files = 50
	)
	dirParts := make([]string, depth)
	for i := range depth {
		dirParts[i] = fmt.Sprintf("d%d", i)
	}
	prefix := strings.Join(dirParts, "/")

	entries := make([]testArchiveEntry, files)
	for i := range files {
		entries[i] = testArchiveEntry{
			typeflag: tar.TypeReg,
			name:     fmt.Sprintf("%s/f%d.txt", prefix, i),
			content:  fmt.Appendf(nil, "content-%d", i),
		}
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	if err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, e := range entries {
		//nolint:gosec // dst is a t.TempDir() and e.name is a literal from the entries slice above, not external input.
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(e.name)))
		if err != nil {
			t.Fatalf("failed to read extracted file %s: %v", e.name, err)
		}
		if string(got) != string(e.content) {
			t.Fatalf("file %s: expected content %q, got %q", e.name, e.content, got)
		}
	}
}

// TestExtractSymlinkParentRejectedDespiteMemoizedAncestors pins that the memo
// holds only real directories: a symlink created under memoized parents is
// still Lstat'd fresh, so an entry beneath it is refused.
func TestExtractSymlinkParentRejectedDespiteMemoizedAncestors(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "p/m/sub/keep.txt", content: []byte("keep")},
		{typeflag: tar.TypeReg, name: "p/m/other.txt", content: []byte("other")},
		{typeflag: tar.TypeSymlink, name: "p/m/link", linkname: "sub"},
		{typeflag: tar.TypeReg, name: "p/m/link/escape.txt", content: []byte("escape")},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst)
	if err == nil {
		t.Fatalf("expected extraction to fail")
	}
	if !errors.Is(err, helpers.ErrArchivePathContainsSymlinkComponent) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchivePathContainsSymlinkComponent, err)
	}

	// The symlink itself must have been created (confirming the memo
	// really did let entry 3 through before entry 4 was rejected).
	linkInfo, statErr := os.Lstat(filepath.Join(dst, "p", "m", "link"))
	if statErr != nil {
		t.Fatalf("expected symlink p/m/link to exist: %v", statErr)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected p/m/link to be a symlink")
	}

	// No file must have been written under the symlink's resolved target.
	if _, statErr := os.Lstat(filepath.Join(dst, "p", "m", "sub", "escape.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected escape.txt to not exist under the symlink target, stat error: %v", statErr)
	}
}

// TestExtractSymlinkCannotReplaceMemoizedDir pins that a symlink entry named
// like an already extracted directory fails, since os.Symlink returns EEXIST,
// and leaves the directory in place, memoized or not.
func TestExtractSymlinkCannotReplaceMemoizedDir(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "p/m/a.txt", content: []byte("a")},
		{typeflag: tar.TypeReg, name: "p/m/b.txt", content: []byte("b")},
		{typeflag: tar.TypeSymlink, name: "p/m", linkname: "a.txt"},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst)
	if err == nil {
		t.Fatalf("expected extraction to fail")
	}

	info, statErr := os.Lstat(filepath.Join(dst, "p", "m"))
	if statErr != nil {
		t.Fatalf("expected p/m to still exist: %v", statErr)
	}
	if !info.IsDir() {
		t.Fatalf("expected p/m to still be a directory, got mode %v", info.Mode())
	}
}

// TestExtractHardlinkWithMemoizedParentChain pins that extractHardlink's own
// parent-chain check reuses the memo and still links the target correctly.
func TestExtractHardlinkWithMemoizedParentChain(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "p/q/a.txt", content: []byte("a")},
		{typeflag: tar.TypeReg, name: "p/q/target.txt", content: []byte("hello")},
		{typeflag: tar.TypeLink, name: "p/q/link.txt", linkname: "p/q/target.txt"},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	if err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	//nolint:gosec // dst is a t.TempDir() and the joined path is a literal, not external input.
	got, err := os.ReadFile(filepath.Join(dst, "p", "q", "link.txt"))
	if err != nil {
		t.Fatalf("failed to read linked file: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("expected linked content %q, got %q", "hello", got)
	}
}

// buildEntriesArchive builds n flat entries of one typeflag named entry0 to
// entry<n-1>; a tar.TypeDir entry gets a trailing slash and zero size. It lets
// the entry-count tests use a tiny injected cap instead of 100,001 entries.
func buildEntriesArchive(tb testing.TB, n int, typeflag byte) []byte {
	tb.Helper()

	// entriesModTime stamps every generated entry so these tests never
	// depend on wall-clock time.
	entriesModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	content := []byte("x")
	for i := range n {
		name := fmt.Sprintf("entry%d", i)
		var size int64
		if typeflag == tar.TypeDir {
			name += "/"
		} else {
			size = int64(len(content))
		}
		header := &tar.Header{
			Typeflag: typeflag,
			Name:     name,
			Size:     size,
			Mode:     0o644,
			ModTime:  entriesModTime,
		}
		if err := tw.WriteHeader(header); err != nil {
			tb.Fatalf("failed to write tar header: %v", err)
		}
		if size > 0 {
			if _, err := tw.Write(content); err != nil {
				tb.Fatalf("failed to write tar content: %v", err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		tb.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		tb.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// tarReaderFrom decompresses gzip-encoded archive bytes into a *tar.Reader,
// for tests that call the unexported extractTarEntries directly with an
// injected cap rather than going through the public gzip-wrapping API.
func tarReaderFrom(t *testing.T, archiveBytes []byte) *tar.Reader {
	t.Helper()

	gzr, err := gzip.NewReader(bytes.NewReader(archiveBytes))
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	t.Cleanup(func() {
		_ = gzr.Close()
	})
	return tar.NewReader(gzr)
}

// TestExtractEntryCountUnderCap asserts that an archive with fewer entries
// than the injected cap extracts every entry normally.
func TestExtractEntryCountUnderCap(t *testing.T) {
	t.Parallel()

	const maxEntries = 5
	archiveBytes := buildEntriesArchive(t, 4, tar.TypeReg)

	dst := t.TempDir()
	if err := extractTarEntries(tarReaderFrom(t, archiveBytes), dst, maxEntries); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := range 4 {
		name := fmt.Sprintf("entry%d", i)
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}
}

// TestExtractEntryCountAtCap pins the boundary: exactly maxEntries entries
// must succeed, since the cap check is entries > maxEntries, not >=.
func TestExtractEntryCountAtCap(t *testing.T) {
	t.Parallel()

	const maxEntries = 5
	archiveBytes := buildEntriesArchive(t, maxEntries, tar.TypeReg)

	dst := t.TempDir()
	if err := extractTarEntries(tarReaderFrom(t, archiveBytes), dst, maxEntries); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := range maxEntries {
		name := fmt.Sprintf("entry%d", i)
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}
}

// TestExtractEntryCountOverCapFailsClosed asserts that one entry past the
// cap is rejected with ErrArchiveTooManyEntries, at the (maxEntries+1)th
// header.
func TestExtractEntryCountOverCapFailsClosed(t *testing.T) {
	t.Parallel()

	const maxEntries = 5
	archiveBytes := buildEntriesArchive(t, maxEntries+1, tar.TypeReg)

	dst := t.TempDir()
	err := extractTarEntries(tarReaderFrom(t, archiveBytes), dst, maxEntries)
	if !errors.Is(err, helpers.ErrArchiveTooManyEntries) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveTooManyEntries, err)
	}
}

// TestExtractEntryCountCountsNonRegularEntries pins that the entry-count cap
// counts every typeflag: zero-byte directories, which charge no byte budget,
// still trip it, the defense against an inode-exhaustion tarbomb.
func TestExtractEntryCountCountsNonRegularEntries(t *testing.T) {
	t.Parallel()

	const maxEntries = 3
	archiveBytes := buildEntriesArchive(t, maxEntries+1, tar.TypeDir)

	dst := t.TempDir()
	err := extractTarEntries(tarReaderFrom(t, archiveBytes), dst, maxEntries)
	if !errors.Is(err, helpers.ErrArchiveTooManyEntries) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveTooManyEntries, err)
	}
}

// TestExtractEntryCountCountsHardlinkEntries pins that hardlink entries count
// against the cap too: the first three entries extract and the fourth, a
// hardlink over a cap of three, is refused before it is linked.
func TestExtractEntryCountCountsHardlinkEntries(t *testing.T) {
	t.Parallel()

	const maxEntries = 3
	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "target.txt", content: []byte("hello")},
		{typeflag: tar.TypeLink, name: "link1.txt", linkname: "target.txt"},
		{typeflag: tar.TypeLink, name: "link2.txt", linkname: "target.txt"},
		{typeflag: tar.TypeLink, name: "link3.txt", linkname: "target.txt"},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	err := extractTarEntries(tarReaderFrom(t, archiveBytes), dst, maxEntries)
	if !errors.Is(err, helpers.ErrArchiveTooManyEntries) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveTooManyEntries, err)
	}

	// The first three entries (the target file and two hardlinks) must have
	// been extracted before the fourth tripped the cap.
	if _, statErr := os.Stat(filepath.Join(dst, "target.txt")); statErr != nil {
		t.Fatalf("expected target.txt to exist: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "link1.txt")); statErr != nil {
		t.Fatalf("expected link1.txt to exist: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "link2.txt")); statErr != nil {
		t.Fatalf("expected link2.txt to exist: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "link3.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected link3.txt to not exist, stat error: %v", statErr)
	}
}

// TestExtractLegitMultiFileArchiveStillExtracts pins that the real
// ArchiveMaxEntryCount raises no false positive on an ordinary 50-file archive
// extracted through ExtractTarGzStream.
func TestExtractLegitMultiFileArchiveStillExtracts(t *testing.T) {
	t.Parallel()

	const files = 50
	archiveBytes := buildEntriesArchive(t, files, tar.TypeReg)

	dst := t.TempDir()
	if err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := range files {
		name := fmt.Sprintf("entry%d", i)
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}
}

// TestExtractOverlongPathComponentFailsClosed pins that an Lstat failure other
// than ErrNotExist (here ENAMETOOLONG) in checkPathComponentNotSymlink fails
// extraction instead of treating the component as absent.
func TestExtractOverlongPathComponentFailsClosed(t *testing.T) {
	t.Parallel()

	// overlongComponent exceeds every common filesystem's per-component
	// name limit (typically 255 bytes on Linux and macOS), guaranteeing
	// os.Lstat rejects it with ENAMETOOLONG regardless of whether it exists.
	overlongComponent := strings.Repeat("a", 1000)
	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: overlongComponent + "/file.txt", content: []byte("x")},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst)
	if err == nil {
		t.Fatalf("expected extraction to fail")
	}
	if errors.Is(err, helpers.ErrArchivePathContainsSymlinkComponent) {
		t.Fatalf("expected a stat failure, not the symlink-component rejection: %v", err)
	}
	if !strings.Contains(err.Error(), "failed to stat path") {
		t.Fatalf("expected a wrapped stat error, got: %v", err)
	}
}

// assertExtractedFileModes asserts that each dst-relative path in want has
// exactly the given permission bits, via os.Lstat (never following a
// symlink, though none of these paths are expected to be one).
func assertExtractedFileModes(t *testing.T, dst string, want map[string]os.FileMode) {
	t.Helper()
	for name, wantMode := range want {
		info, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("lstat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Fatalf("%s: mode = %o, want %o", name, got, wantMode)
		}
	}
}

// TestExtractStripsWriteBits pins that a regular file loses every write bit at
// open time whatever its header mode, a directory stays at helpers.DirMod, and
// a symlink is never chmod'ed (os.Chmod would follow it to its target).
func TestExtractStripsWriteBits(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "rw.txt", content: []byte("a"), mode: 0o644},
		{typeflag: tar.TypeReg, name: "rwx.txt", content: []byte("b"), mode: 0o755},
		{typeflag: tar.TypeReg, name: "owner-rw.txt", content: []byte("c"), mode: 0o600},
		{typeflag: tar.TypeReg, name: "already-ro.txt", content: []byte("d"), mode: 0o400},
		{typeflag: tar.TypeDir, name: "sub/", mode: 0o755},
		{typeflag: tar.TypeReg, name: "sub/target.txt", content: []byte("e"), mode: 0o644},
		{typeflag: tar.TypeSymlink, name: "link.txt", linkname: "sub/target.txt"},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	if err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertExtractedFileModes(t, dst, map[string]os.FileMode{
		"rw.txt":         0o444,
		"rwx.txt":        0o555,
		"owner-rw.txt":   0o400,
		"already-ro.txt": 0o400,
		"sub/target.txt": 0o444,
	})

	dirInfo, err := os.Lstat(filepath.Join(dst, "sub"))
	if err != nil {
		t.Fatalf("lstat sub: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != helpers.DirMod {
		t.Fatalf("directory mode = %o, want %o (directories must never be hardened)", got, helpers.DirMod)
	}

	// The symlink entry itself must never be chmod'ed: os.Chmod follows a
	// symlink and would silently mutate whatever it points at.
	linkInfo, err := os.Lstat(filepath.Join(dst, "link.txt"))
	if err != nil {
		t.Fatalf("lstat link.txt: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected link.txt to be a symlink")
	}
	targetInfo, err := os.Lstat(filepath.Join(dst, "sub", "target.txt"))
	if err != nil {
		t.Fatalf("lstat symlink target: %v", err)
	}
	if got := targetInfo.Mode().Perm(); got != 0o444 {
		t.Fatalf("symlink target mode changed to %o, want unaffected 0444", got)
	}
}

// TestExtractRejectsDuplicateEntries pins that two regular-file entries at one
// path fail with helpers.ErrArchiveDuplicateEntry naming the path, rather than
// the bare permission error the read-only first copy would otherwise produce.
func TestExtractRejectsDuplicateEntries(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "dup.txt", content: []byte("first")},
		{typeflag: tar.TypeReg, name: "dup.txt", content: []byte("second")},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst)
	if !errors.Is(err, helpers.ErrArchiveDuplicateEntry) {
		t.Fatalf("expected ErrArchiveDuplicateEntry, got %v", err)
	}
	if !strings.Contains(err.Error(), "dup.txt") {
		t.Fatalf("expected the error to name the offending path, got: %v", err)
	}
}

// TestExtractAcceptsDotSlashPrefixedNames pins that entries named with the
// common "./" prefix are normalized by sanitizeArchivePath and extract
// normally, so neither the write-bit nor the duplicate check misfires on them.
func TestExtractAcceptsDotSlashPrefixedNames(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "./MANIFEST.json", content: []byte("{}")},
		{typeflag: tar.TypeReg, name: "./sub/file.txt", content: []byte("data")},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	if err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	//nolint:gosec // dst is a t.TempDir() and the joined path is a literal, not external input.
	got, err := os.ReadFile(filepath.Join(dst, "MANIFEST.json"))
	if err != nil {
		t.Fatalf("read MANIFEST.json: %v", err)
	}
	if string(got) != "{}" {
		t.Fatalf("MANIFEST.json = %q", got)
	}
	//nolint:gosec // dst is a t.TempDir() and the joined path is a literal, not external input.
	got, err = os.ReadFile(filepath.Join(dst, "sub", "file.txt"))
	if err != nil {
		t.Fatalf("read sub/file.txt: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("sub/file.txt = %q", got)
	}
}

// buildHeaderOnlyArchive renders one tar header declaring size bytes and writes
// no body, so a charge made before dispatch is the first thing that can fail.
// tw.Close's error is ignored: it reports the deliberately omitted body.
func buildHeaderOnlyArchive(t *testing.T, typeflag byte, name string, size int64) []byte {
	t.Helper()

	// headerOnlyModTime stamps the entry so these tests never depend on
	// wall-clock time.
	headerOnlyModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	header := &tar.Header{
		Typeflag: typeflag,
		Name:     name,
		Size:     size,
		Mode:     0o644,
		ModTime:  headerOnlyModTime,
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("failed to write tar header for %s: %v", name, err)
	}
	_ = tw.Close()
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// TestExtractUnknownTypeflagEntryIsSizeCapped pins that an entry of a typeflag
// the dispatch skips ('9') is still charged against the per-entry cap, before
// its body is read past.
func TestExtractUnknownTypeflagEntryIsSizeCapped(t *testing.T) {
	t.Parallel()

	archiveBytes := buildHeaderOnlyArchive(t, '9', "bomb", helpers.ArchiveMaxEntrySize+1)

	err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), t.TempDir())
	if !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveEntryIsTooLarge, err)
	}
}

// TestExtractUnknownTypeflagUnderCapIsSkipped is the positive control for
// TestExtractUnknownTypeflagEntryIsSizeCapped: a body-backed '9' entry under the
// cap is skipped and the rest extracts (a body-less one fails at any size > 0).
func TestExtractUnknownTypeflagUnderCapIsSkipped(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: '9', name: "bomb", content: []byte("body")},
		{typeflag: tar.TypeReg, name: "README.md", content: []byte("# ok\n")},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	if err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	//nolint:gosec // dst is a t.TempDir() and the joined path is a literal, not external input.
	got, err := os.ReadFile(filepath.Join(dst, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	if string(got) != "# ok\n" {
		t.Fatalf("README.md = %q", got)
	}
	if _, statErr := os.Lstat(filepath.Join(dst, "bomb")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected the skipped entry to leave nothing on disk, stat error: %v", statErr)
	}
}

// TestExtractDotNamedEntryIsSizeCapped pins that an entry named ".", which
// handleTarEntry returns on before any dispatch, is still charged against the
// per-entry cap.
func TestExtractDotNamedEntryIsSizeCapped(t *testing.T) {
	t.Parallel()

	archiveBytes := buildHeaderOnlyArchive(t, tar.TypeReg, ".", helpers.ArchiveMaxEntrySize+1)

	err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), t.TempDir())
	if !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveEntryIsTooLarge, err)
	}
}

// TestExtractDotEntryUnderCapIsSkipped is the body-backed positive control for
// TestExtractDotNamedEntryIsSizeCapped: an accepted "." entry is normalized
// away and skipped while the rest of the archive extracts.
func TestExtractDotEntryUnderCapIsSkipped(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: ".", content: []byte("x")},
		{typeflag: tar.TypeReg, name: "file.txt", content: []byte("data")},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	if err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	//nolint:gosec // dst is a t.TempDir() and the joined path is a literal, not external input.
	got, err := os.ReadFile(filepath.Join(dst, "file.txt"))
	if err != nil {
		t.Fatalf("read file.txt: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("file.txt = %q", got)
	}
}

// TestExtractHeaderOnlyEntryWithDeclaredSizeIsCharged pins that a header-only
// directory entry is charged its declared size like any other typeflag, so
// one declaring more than the per-entry cap is refused.
func TestExtractHeaderOnlyEntryWithDeclaredSizeIsCharged(t *testing.T) {
	t.Parallel()

	archiveBytes := buildHeaderOnlyArchive(t, tar.TypeDir, "d/", helpers.ArchiveMaxEntrySize+1)

	err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), t.TempDir())
	if !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveEntryIsTooLarge, err)
	}
}

// TestExtractHeaderOnlyEntryUnderCapIsAccepted is the positive control for
// TestExtractHeaderOnlyEntryWithDeclaredSizeIsCharged: the same fixture under
// the cap extracts, so that refusal is about the size and not the shape.
func TestExtractHeaderOnlyEntryUnderCapIsAccepted(t *testing.T) {
	t.Parallel()

	archiveBytes := buildHeaderOnlyArchive(t, tar.TypeDir, "d/", 4096)

	dst := t.TempDir()
	if err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	info, err := os.Lstat(filepath.Join(dst, "d"))
	if err != nil {
		t.Fatalf("lstat d: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected d to be a directory, got mode %v", info.Mode())
	}
}

// tarBlockSize is the size of one tar header block, and of one data block.
const tarBlockSize = 512

// sparseFixturePhysical and sparseFixtureLogical are the sparse fixture's bytes
// in the stream and its logical size, the only one chargeEntrySize sees; the
// ratio is wide enough that no nonzero declared-size budget refuses the entry.
const (
	sparseFixturePhysical = int64(1 << 20)
	sparseFixtureLogical  = int64(1)
)

// putTarOctal writes v into a tar header field as zero-padded octal with the
// customary NUL terminator, which is the encoding archive/tar's own parser
// reads back out of these fields.
func putTarOctal(field []byte, v int64) {
	copy(field, fmt.Sprintf("%0*o", len(field)-1, v))
	field[len(field)-1] = 0x00
}

// buildOldGNUSparseArchive hand-assembles one old-GNU sparse ('S') entry, as
// tar.Writer cannot encode realsize or a sparse map: physical zero bytes in the
// stream, and logical bytes in realsize, which Header.Size then reports.
func buildOldGNUSparseArchive(t *testing.T, name string, physical, logical int64) []byte {
	t.Helper()

	if physical%tarBlockSize != 0 {
		t.Fatalf("physical size %d is not a whole number of %d-byte blocks", physical, tarBlockSize)
	}

	// sparseModTime stamps the entry so this fixture never depends on
	// wall-clock time.
	sparseModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()

	blk := make([]byte, tarBlockSize)
	copy(blk[0:100], name)              // name
	putTarOctal(blk[100:108], 0o644)    // mode
	putTarOctal(blk[108:116], 0)        // uid
	putTarOctal(blk[116:124], 0)        // gid
	putTarOctal(blk[124:136], physical) // size: the bytes really in the stream
	putTarOctal(blk[136:148], sparseModTime)
	blk[156] = tar.TypeGNUSparse       // typeflag
	copy(blk[257:263], "ustar ")       // GNU magic
	copy(blk[263:265], " \x00")        // GNU version
	putTarOctal(blk[386:398], 0)       // sparse fragment 0: offset
	putTarOctal(blk[398:410], logical) // sparse fragment 0: length
	putTarOctal(blk[483:495], logical) // realsize: the logical file size

	// The checksum is computed over the block with its own field blanked to
	// spaces, as archive/tar verifies it; sealTarBlock below is its other copy.
	copy(blk[148:156], "        ")
	var sum int64
	for _, b := range blk {
		sum += int64(b)
	}
	copy(blk[148:156], fmt.Sprintf("%06o\x00 ", sum))

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	// The trailer is the two zero blocks that end every tar archive.
	for _, chunk := range [][]byte{blk, make([]byte, physical), make([]byte, 2*tarBlockSize)} {
		if _, err := gz.Write(chunk); err != nil {
			t.Fatalf("failed to write sparse fixture: %v", err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// TestExtractSparseEntryTripsDecompressedCap pins that only the decompressed
// stream cap refuses an entry declaring one byte and costing a megabyte. The
// cap is injected because the 4 GiB production ceiling is too slow under -race.
func TestExtractSparseEntryTripsDecompressedCap(t *testing.T) {
	t.Parallel()

	// refuseCap sits far above the entry's declared size and far below its
	// physical body, so only the stream cap can produce a refusal here.
	const refuseCap = int64(64 << 10)
	archiveBytes := buildOldGNUSparseArchive(t, "sparse.bin", sparseFixturePhysical, sparseFixtureLogical)

	err := extractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), t.TempDir(), refuseCap)
	if !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveDecompressedTooLarge, err)
	}
}

// TestExtractSparseEntryUnderDecompressedCapIsAccepted is the same-fixture
// positive control under a larger cap; the 'S' entry reaches handleTarEntry's
// default arm, so a megabyte of stream leaves nothing on disk.
func TestExtractSparseEntryUnderDecompressedCapIsAccepted(t *testing.T) {
	t.Parallel()

	// acceptCap is comfortably above the fixture's whole decompressed stream.
	const acceptCap = int64(8 << 20)
	archiveBytes := buildOldGNUSparseArchive(t, "sparse.bin", sparseFixturePhysical, sparseFixtureLogical)

	dst := t.TempDir()
	if err := extractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst, acceptCap); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, statErr := os.Lstat(filepath.Join(dst, "sparse.bin")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected the skipped sparse entry to leave nothing on disk, stat error: %v", statErr)
	}
}

// buildGNUHeaderOnlyArchive renders one body-less header-only entry in GNU
// format, whose base-256 size field carries a negative size in the entry's own
// header; the default format would move it into a PAX 'x' record instead.
func buildGNUHeaderOnlyArchive(t *testing.T, typeflag byte, name string, size int64) []byte {
	t.Helper()

	// gnuHeaderModTime stamps the entry so this fixture never depends on
	// wall-clock time.
	gnuHeaderModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	header := &tar.Header{
		Typeflag: typeflag,
		Name:     name,
		Size:     size,
		Mode:     0o755,
		ModTime:  gnuHeaderModTime,
		Format:   tar.FormatGNU,
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("failed to write GNU tar header for %s: %v", name, err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// negativeDeclaredSize is a size no legitimate archive declares, large enough
// that adding it would pull the running total far below the per-archive cap.
const negativeDeclaredSize = -(int64(1) << 62)

// TestExtractNegativeDeclaredSizeFailsClosed pins chargeEntrySize's negative
// branch: archive/tar delivers a GNU base-256 negative size intact on a
// header-only entry, and charging it would lower the per-archive running sum.
func TestExtractNegativeDeclaredSizeFailsClosed(t *testing.T) {
	t.Parallel()

	archiveBytes := buildGNUHeaderOnlyArchive(t, tar.TypeDir, "d/", negativeDeclaredSize)

	err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), t.TempDir())
	if !errors.Is(err, helpers.ErrArchiveEntryHasNegativeSize) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveEntryHasNegativeSize, err)
	}
}

// TestExtractGNUFormatHeaderOnlyEntryIsAccepted is the same-fixture positive
// control for TestExtractNegativeDeclaredSizeFailsClosed: the same entry with
// a positive declared size extracts.
func TestExtractGNUFormatHeaderOnlyEntryIsAccepted(t *testing.T) {
	t.Parallel()

	archiveBytes := buildGNUHeaderOnlyArchive(t, tar.TypeDir, "d/", 4096)

	dst := t.TempDir()
	if err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	info, err := os.Lstat(filepath.Join(dst, "d"))
	if err != nil {
		t.Fatalf("lstat d: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected d to be a directory, got mode %v", info.Mode())
	}
}

// atCapDirCount is how many entries declaring helpers.ArchiveMaxEntrySize fit
// exactly in helpers.ArchiveMaxTotalSize, derived so the boundary tests follow
// either constant.
const atCapDirCount = int(helpers.ArchiveMaxTotalSize / helpers.ArchiveMaxEntrySize)

// buildDeclaredSizeDirArchive renders count directory entries d0/ onward, each
// declaring size bytes it does not carry: archive/tar reads no body for a
// header-only typeflag but reports Header.Size verbatim.
func buildDeclaredSizeDirArchive(t *testing.T, count int, size int64) []byte {
	t.Helper()

	// declaredSizeModTime stamps every entry so this fixture never depends on
	// wall-clock time.
	declaredSizeModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for i := range count {
		header := &tar.Header{
			Typeflag: tar.TypeDir,
			Name:     fmt.Sprintf("d%d/", i),
			Size:     size,
			Mode:     0o755,
			ModTime:  declaredSizeModTime,
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("failed to write tar header for d%d/: %v", i, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// TestExtractCumulativeDeclaredSizeOverCapFailsClosed pins chargeEntrySize's
// per-archive branch: entries each legal on their own are refused at the one
// that crosses helpers.ArchiveMaxTotalSize, and not earlier.
func TestExtractCumulativeDeclaredSizeOverCapFailsClosed(t *testing.T) {
	t.Parallel()

	archiveBytes := buildDeclaredSizeDirArchive(t, atCapDirCount+1, helpers.ArchiveMaxEntrySize)

	dst := t.TempDir()
	err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst)
	if !errors.Is(err, helpers.ErrArchiveExceedsMaxSize) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveExceedsMaxSize, err)
	}

	// The refusal lands on the crossing entry and not before it: every earlier
	// directory is already on disk, and the crossing one never was.
	for i := range atCapDirCount {
		name := fmt.Sprintf("d%d", i)
		if _, statErr := os.Stat(filepath.Join(dst, name)); statErr != nil {
			t.Fatalf("expected %s to exist: %v", name, statErr)
		}
	}
	crossing := fmt.Sprintf("d%d", atCapDirCount)
	if _, statErr := os.Lstat(filepath.Join(dst, crossing)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected %s to not exist, stat error: %v", crossing, statErr)
	}
}

// TestExtractCumulativeDeclaredSizeAtCapIsAccepted pins the boundary: a running
// total landing exactly on helpers.ArchiveMaxTotalSize is accepted, since the
// check is > rather than >=.
func TestExtractCumulativeDeclaredSizeAtCapIsAccepted(t *testing.T) {
	t.Parallel()

	archiveBytes := buildDeclaredSizeDirArchive(t, atCapDirCount, helpers.ArchiveMaxEntrySize)

	dst := t.TempDir()
	if err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := range atCapDirCount {
		name := fmt.Sprintf("d%d", i)
		if _, statErr := os.Stat(filepath.Join(dst, name)); statErr != nil {
			t.Fatalf("expected %s to exist: %v", name, statErr)
		}
	}
}

// sealTarBlock writes blk's own checksum into its checksum field, computed
// over the block with that field blanked to spaces, as archive/tar verifies
// it. buildOldGNUSparseArchive above holds this file's other copy of it.
func sealTarBlock(blk []byte) {
	copy(blk[148:156], "        ")
	var sum int64
	for _, b := range blk {
		sum += int64(b)
	}
	copy(blk[148:156], fmt.Sprintf("%06o\x00 ", sum))
}

// buildMetaHeaderChainArchive hand-assembles count zero-size GNU long-link
// ('K') headers, which tar.Writer refuses to encode, then the tar trailer,
// without which a short final read keeps the error and the test is vacuous.
func buildMetaHeaderChainArchive(t *testing.T, count int) []byte {
	t.Helper()

	// metaChainModTime stamps the header so this fixture never depends on
	// wall-clock time.
	metaChainModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()

	blk := make([]byte, tarBlockSize)
	copy(blk[0:100], "././@LongLink") // name, the one GNU tar itself writes
	putTarOctal(blk[100:108], 0o644)  // mode
	putTarOctal(blk[108:116], 0)      // uid
	putTarOctal(blk[116:124], 0)      // gid
	putTarOctal(blk[124:136], 0)      // size: no body at all
	putTarOctal(blk[136:148], metaChainModTime)
	blk[156] = tar.TypeGNULongLink // typeflag
	copy(blk[257:263], "ustar ")   // GNU magic
	copy(blk[263:265], " \x00")    // GNU version
	sealTarBlock(blk)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for range count {
		if _, err := gz.Write(blk); err != nil {
			t.Fatalf("failed to write meta header: %v", err)
		}
	}
	if _, err := gz.Write(make([]byte, 2*tarBlockSize)); err != nil {
		t.Fatalf("failed to write tar trailer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// TestExtractMetaHeaderChainTripsDecompressedCap pins the stream cap against
// 'K' headers, which Next consumes without returning, so neither the entry
// counter nor chargeEntrySize can see them. The cap is injected for speed.
func TestExtractMetaHeaderChainTripsDecompressedCap(t *testing.T) {
	t.Parallel()

	const (
		headers   = 200
		refuseCap = int64(4 << 10)
	)
	archiveBytes := buildMetaHeaderChainArchive(t, headers)

	err := extractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), t.TempDir(), refuseCap)
	if !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveDecompressedTooLarge, err)
	}
}

// TestExtractMetaHeaderChainUnderDecompressedCapIsAccepted is the same-fixture
// positive control under a larger cap: a chain of headers Next never returns
// extracts nothing, so the destination stays empty.
func TestExtractMetaHeaderChainUnderDecompressedCapIsAccepted(t *testing.T) {
	t.Parallel()

	const (
		headers   = 200
		acceptCap = int64(8 << 20)
	)
	archiveBytes := buildMetaHeaderChainArchive(t, headers)

	dst := t.TempDir()
	if err := extractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst, acceptCap); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	left, err := os.ReadDir(dst)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("expected the meta-header chain to leave nothing on disk, got %d entries", len(left))
	}
}

// TestExtractHeaderOnlyEntriesTripDecompressedCap pins the stream cap on
// counted size-0 directory entries, which break no other rule at the
// production entry cap: framing alone, 512 bytes per header, trips it.
func TestExtractHeaderOnlyEntriesTripDecompressedCap(t *testing.T) {
	t.Parallel()

	const (
		dirs      = 20
		refuseCap = int64(4 << 10)
	)
	archiveBytes := buildDeclaredSizeDirArchive(t, dirs, 0)

	err := extractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), t.TempDir(), refuseCap)
	if !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveDecompressedTooLarge, err)
	}
}

// TestExtractHeaderOnlyEntriesUnderDecompressedCapAreAccepted is the positive
// control for TestExtractHeaderOnlyEntriesTripDecompressedCap: the same
// fixture under a cap its stream fits in extracts every directory.
func TestExtractHeaderOnlyEntriesUnderDecompressedCapAreAccepted(t *testing.T) {
	t.Parallel()

	const (
		dirs      = 20
		acceptCap = int64(8 << 20)
	)
	archiveBytes := buildDeclaredSizeDirArchive(t, dirs, 0)

	dst := t.TempDir()
	if err := extractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst, acceptCap); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := range dirs {
		name := fmt.Sprintf("d%d", i)
		if _, statErr := os.Stat(filepath.Join(dst, name)); statErr != nil {
			t.Fatalf("expected %s to exist: %v", name, statErr)
		}
	}
}

// countingReader counts the bytes it hands out, so a test can tell a reader
// above it that stopped pulling from one that merely stopped reporting.
type countingReader struct {
	r    io.Reader
	read int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

// TestDecompressedLimitReaderRefusesThroughReadFull pins the crossing read's
// zero return, since io.ReadAtLeast drops an error once its request is filled;
// max is one below the request, as a lower one keeps the error regardless.
func TestDecompressedLimitReaderRefusesThroughReadFull(t *testing.T) {
	t.Parallel()

	const (
		request = 512
		limit   = int64(request - 1)
	)
	lim := &decompressedLimitReader{r: bytes.NewReader(make([]byte, 4<<10)), over: helpers.ErrArchiveDecompressedTooLarge, max: limit}

	_, err := io.ReadFull(lim, make([]byte, request))
	if !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveDecompressedTooLarge, err)
	}
}

// TestDecompressedLimitReaderRefusesThroughCopyN pins the same zero return
// against io.CopyN, which archive/tar's discard and extractRegularFile use; it
// requests exactly max+1, so returning the crossing bytes would be swallowed.
func TestDecompressedLimitReaderRefusesThroughCopyN(t *testing.T) {
	t.Parallel()

	const limit = int64(10)
	lim := &decompressedLimitReader{r: bytes.NewReader(make([]byte, 4<<10)), over: helpers.ErrArchiveDecompressedTooLarge, max: limit}

	_, err := io.CopyN(io.Discard, lim, limit+1)
	if !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveDecompressedTooLarge, err)
	}
}

// TestDecompressedLimitReaderRefusalIsSticky pins that reads after the refusal
// pull nothing more from the underlying reader; without the sticky error each
// still fails, but only after the clamp has pulled one more byte through.
func TestDecompressedLimitReaderRefusalIsSticky(t *testing.T) {
	t.Parallel()

	const (
		limit      = int64(8)
		afterReads = 5
	)
	counter := &countingReader{r: bytes.NewReader(make([]byte, 4<<10))}
	lim := &decompressedLimitReader{r: counter, over: helpers.ErrArchiveDecompressedTooLarge, max: limit}

	buf := make([]byte, 64)
	if _, err := lim.Read(buf); !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected the first read to trip the cap, got %v", err)
	}
	atRefusal := counter.read

	for i := range afterReads {
		n, err := lim.Read(buf)
		if n != 0 || !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
			t.Fatalf("read %d after the refusal returned (%d, %v)", i, n, err)
		}
	}

	if counter.read != atRefusal {
		t.Fatalf("underlying reader advanced by %d bytes after the refusal", counter.read-atRefusal)
	}
}

// TestDecompressedLimitReaderClampsOverrunToOneByte pins the clamp: a large
// request near the ceiling pulls one byte past it rather than a whole buffer,
// which keeps the byte count in the reported error exact.
func TestDecompressedLimitReaderClampsOverrunToOneByte(t *testing.T) {
	t.Parallel()

	const (
		limit   = int64(1024)
		request = 32 << 10
	)
	lim := &decompressedLimitReader{r: bytes.NewReader(make([]byte, 64<<10)), over: helpers.ErrArchiveDecompressedTooLarge, max: limit}

	if _, err := lim.Read(make([]byte, request)); !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveDecompressedTooLarge, err)
	}
	if lim.n != limit+1 {
		t.Fatalf("read %d bytes past a %d-byte ceiling, want exactly one", lim.n-limit, limit)
	}
}

// TestDecompressedLimitReaderClampSurvivesExtremeMax covers a max at which
// remaining+1 overflows and one negative enough to reslice below zero, both
// reachable through the injected-cap seam.
func TestDecompressedLimitReaderClampSurvivesExtremeMax(t *testing.T) {
	t.Parallel()

	t.Run("max at MaxInt64 passes the read through", func(t *testing.T) {
		t.Parallel()

		const payload = "hello"
		lim := &decompressedLimitReader{r: bytes.NewReader([]byte(payload)), over: helpers.ErrArchiveDecompressedTooLarge, max: math.MaxInt64}
		// remaining+1 overflows at this max, so a clamp compared against
		// remaining+1 rather than remaining would reslice below zero and panic.
		n, err := lim.Read(make([]byte, 16))
		if n != len(payload) || err != nil {
			t.Fatalf("read = (%d, %v), want (%d, <nil>)", n, err, len(payload))
		}
	})

	t.Run("negative max refuses without panicking", func(t *testing.T) {
		t.Parallel()

		const limit = int64(-1024)
		lim := &decompressedLimitReader{r: bytes.NewReader(make([]byte, 64)), over: helpers.ErrArchiveDecompressedTooLarge, max: limit}
		// Without clamping remaining at zero the reslice to remaining+1 panics; a
		// max of -1 would reslice to p[:0], so the case needs -2 or lower.
		n, err := lim.Read(make([]byte, 64))
		if n != 0 || !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
			t.Fatalf("read = (%d, %v), want (0, %v)", n, err, helpers.ErrArchiveDecompressedTooLarge)
		}
	})
}

// TestDecompressedLimitReaderClampAcceptsEmptyInputs pins that a zero-length
// buffer and a zero max over an empty stream pass through untouched: a reader
// that delivered nothing has exceeded nothing.
func TestDecompressedLimitReaderClampAcceptsEmptyInputs(t *testing.T) {
	t.Parallel()

	t.Run("zero-length buffer reads nothing", func(t *testing.T) {
		t.Parallel()

		lim := &decompressedLimitReader{r: bytes.NewReader([]byte("hello")), over: helpers.ErrArchiveDecompressedTooLarge, max: 1024}
		n, err := lim.Read(nil)
		if n != 0 || err != nil {
			t.Fatalf("read = (%d, %v), want (0, <nil>)", n, err)
		}
	})

	t.Run("zero max over an empty stream reports EOF", func(t *testing.T) {
		t.Parallel()

		lim := &decompressedLimitReader{r: bytes.NewReader(nil), over: helpers.ErrArchiveDecompressedTooLarge, max: 0}
		n, err := lim.Read(make([]byte, 16))
		if n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("read = (%d, %v), want (0, %v)", n, err, io.EOF)
		}
	})
}

// probeTarGzCase is one table entry for TestProbeTarGz.
type probeTarGzCase struct {
	// build returns the bytes to write at the probed path, or nil to skip
	// writing the file at all (the missing-path row).
	build   func(t *testing.T) []byte
	wantErr error
	name    string
	// wantAnyErr marks a row that must fail without carrying
	// helpers.ErrArtifactNotTarGz: an unreadable path is an environment
	// problem, not a statement about the artifact's shape.
	wantAnyErr bool
}

// probeTarGzCases enumerates what ProbeTarGz accepts (a real archive, one
// larger than its buffer, an empty one), the outer shapes it refuses (not gzip,
// gzip over non-tar, cut before the first header) and an unreadable path.
func probeTarGzCases() []probeTarGzCase {
	notAnArchive := []byte("<html>404</html>")
	return []probeTarGzCase{
		{
			name: "real archive accepted",
			build: func(t *testing.T) []byte {
				t.Helper()
				return buildTestArchive(t, []testArchiveEntry{{name: "README.md", content: []byte("x")}})
			},
		},
		{
			// Accepted though its first entry alone is four times the probe's
			// buffer; hand-spelled so it cannot shrink with the sizing constants,
			// which TestProbeGzipSizingStaysWithinItsBudget bounds instead.
			name: "archive larger than the probe's whole buffer accepted",
			build: func(t *testing.T) []byte {
				t.Helper()
				return buildTestArchive(t, []testArchiveEntry{
					{name: "big.bin", content: incompressibleBytes(262144)},
					{name: "README.md", content: []byte("x")},
				})
			},
		},
		{
			// The row above is its positive control. 20 bytes cannot yield the
			// first tar header; a longer truncated copy that does is not refused,
			// since pgzip returns a truncated read as a short block with no error.
			name: "download truncated before its first tar header refused",
			build: func(t *testing.T) []byte {
				t.Helper()
				return buildTestArchive(t, []testArchiveEntry{
					{name: "big.bin", content: incompressibleBytes(262144)},
					{name: "README.md", content: []byte("x")},
				})[:20]
			},
			wantErr: helpers.ErrArtifactNotTarGz,
		},
		{
			// A tar with no entries is well-formed; Next reports io.EOF on the
			// first call and the probe must read that as "empty", not "broken".
			name:  "empty archive accepted",
			build: func(t *testing.T) []byte { t.Helper(); return buildTestArchive(t, nil) },
		},
		{
			name:    "non-gzip bytes refused",
			build:   func(t *testing.T) []byte { t.Helper(); return notAnArchive },
			wantErr: helpers.ErrArtifactNotTarGz,
		},
		{
			name:    "gzip wrapping something that is not a tar refused",
			build:   func(t *testing.T) []byte { t.Helper(); return gzipBytes(t, notAnArchive) },
			wantErr: helpers.ErrArtifactNotTarGz,
		},
		{
			name:       "missing path fails without claiming a shape",
			build:      nil,
			wantAnyErr: true,
		},
	}
}

// gzipBytes compresses data with gzip and returns the compressed bytes, so a
// test can build a stream that is valid gzip and nothing else.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// assertProbeOutcome checks err against what tt expects. Split out of the
// test body so the loop stays within the cyclomatic-complexity budget the
// three-way outcome would otherwise push it past.
func assertProbeOutcome(t *testing.T, tt probeTarGzCase, path string, err error) {
	t.Helper()
	switch {
	case tt.wantAnyErr:
		if err == nil {
			t.Fatalf("ProbeTarGz(%q) = nil, want a non-nil error", path)
		}
		if errors.Is(err, helpers.ErrArtifactNotTarGz) {
			t.Fatalf("ProbeTarGz(%q) = %v, want an error that does not claim the artifact's shape", path, err)
		}
	case tt.wantErr != nil:
		if !errors.Is(err, tt.wantErr) {
			t.Fatalf("ProbeTarGz(%q) = %v, want errors.Is %v", path, err, tt.wantErr)
		}
	default:
		if err != nil {
			t.Fatalf("ProbeTarGz(%q) = %v, want nil", path, err)
		}
	}
}

// TestProbeTarGz drives ProbeTarGz over every row in probeTarGzCases against
// real files on disk, since the probe takes a path rather than a reader.
func TestProbeTarGz(t *testing.T) {
	t.Parallel()

	for _, tt := range probeTarGzCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "artifact.tar.gz")
			if tt.build != nil {
				if err := os.WriteFile(path, tt.build(t), 0o600); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}

			assertProbeOutcome(t, tt, path, ProbeTarGz(t.Context(), path))
		})
	}
}

// cancelAfterReads cancels ctx once the extraction has taken n reads out of
// it, so a test can stop an unpack partway through deterministically instead
// of racing a timer against it.
type cancelAfterReads struct {
	r      io.Reader
	cancel context.CancelFunc
	after  int
	reads  int
}

// Read caps each call at a small chunk so the number of reads is a property
// of the archive's size rather than of the caller's buffer, which is what
// makes "cancel on read N" land in the middle of the unpack deterministically.
func (c *cancelAfterReads) Read(p []byte) (int, error) {
	c.reads++
	if c.reads == c.after {
		c.cancel()
	}
	if len(p) > cancelReadChunk {
		p = p[:cancelReadChunk]
	}
	return c.r.Read(p)
}

// cancelReadChunk is the per-read cap cancelAfterReads applies.
const cancelReadChunk = 512

// incompressibleBytes fills n bytes from a fixed-seed xorshift, so the archive
// does not deflate away and the decompressor has to make many reads; math/rand
// is avoided because the weak-randomness lint flags it.
func incompressibleBytes(n int) []byte {
	out := make([]byte, n)
	state := uint32(0x9E3779B9)
	for i := range out {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		out[i] = byte(state & 0xff)
	}
	return out
}

// multiEntryArchive builds an archive with enough separate entries that an
// extraction stopped partway leaves some of them unwritten.
func multiEntryArchive(t *testing.T) []byte {
	t.Helper()
	entries := make([]testArchiveEntry, 0, 32)
	for i := range 32 {
		entries = append(entries, testArchiveEntry{
			name:    fmt.Sprintf("file-%02d.txt", i),
			content: incompressibleBytes(4096),
		})
	}
	return buildTestArchive(t, entries)
}

// TestExtractTarGzStreamHonorsCancellation pins that a canceled unpack stops
// short of the archive's end with context.Canceled reachable, so the run exits
// as interrupted. Either side's context check may be the one that stops it.
func TestExtractTarGzStreamHonorsCancellation(t *testing.T) {
	t.Parallel()

	archiveBytes := multiEntryArchive(t)
	dst := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Each read is capped at 512 bytes and the archive is tens of kilobytes,
	// so canceling on the eighth lands well inside it rather than at either
	// end.
	src := &cancelAfterReads{r: bytes.NewReader(archiveBytes), cancel: cancel, after: 8}

	err := ExtractTarGzStream(ctx, src, dst)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ExtractTarGzStream under a canceled context = %v, want errors.Is context.Canceled", err)
	}
	written, readErr := os.ReadDir(dst)
	if readErr != nil {
		t.Fatalf("os.ReadDir(%q): %v", dst, readErr)
	}
	if len(written) >= 32 {
		t.Fatalf("extraction wrote %d entries despite cancellation, want fewer than the archive's 32", len(written))
	}
}

// cancelOnSourceDrained cancels ctx on the read that hands back the archive's
// last byte, so the unpack is canceled with the compressed source already
// exhausted and no later read ever taken off it.
type cancelOnSourceDrained struct {
	r      *bytes.Reader
	cancel context.CancelFunc
}

func (c *cancelOnSourceDrained) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.r.Len() == 0 {
		c.cancel()
	}
	return n, err
}

// TestExtractTarGzStreamStopsOnTheDecompressedSideAlone pins the contextReader
// on the decompressed side: it cancels on the source's last read, when pgzip's
// 1 MiB block already holds the whole archive and no compressed read remains.
func TestExtractTarGzStreamStopsOnTheDecompressedSideAlone(t *testing.T) {
	t.Parallel()

	archiveBytes := multiEntryArchive(t)
	dst := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &cancelOnSourceDrained{r: bytes.NewReader(archiveBytes), cancel: cancel}

	err := ExtractTarGzStream(ctx, src, dst)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ExtractTarGzStream canceled on the source's final read = %v, want errors.Is context.Canceled", err)
	}
}

// TestExtractTarGzStreamExtractsFullyWithoutCancellation is the positive
// control described on TestExtractTarGzStreamHonorsCancellation: the same
// archive, extracted under a live context, must produce every entry.
func TestExtractTarGzStreamExtractsFullyWithoutCancellation(t *testing.T) {
	t.Parallel()

	archiveBytes := multiEntryArchive(t)
	dst := t.TempDir()

	if err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("ExtractTarGzStream = %v, want nil", err)
	}
	written, err := os.ReadDir(dst)
	if err != nil {
		t.Fatalf("os.ReadDir(%q): %v", dst, err)
	}
	if len(written) != 32 {
		t.Fatalf("extraction wrote %d entries, want all 32", len(written))
	}
}

// buildProbePaxPrologueArchive hand-assembles count PAX 'x' headers carrying
// record (nil for zero-body headers), which tar.Writer refuses to encode, then
// one ordinary entry through tar.Writer and the tar trailer.
func buildProbePaxPrologueArchive(t *testing.T, count int, record []byte) []byte {
	t.Helper()

	// paxPrologueModTime stamps the fixture so it never depends on wall-clock
	// time.
	paxPrologueModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	blk := make([]byte, tarBlockSize)
	copy(blk[0:100], "PaxHeaders.0/README.md")           // name, as a real writer spells it
	putTarOctal(blk[100:108], 0o644)                     // mode
	putTarOctal(blk[108:116], 0)                         // uid
	putTarOctal(blk[116:124], 0)                         // gid
	putTarOctal(blk[124:136], int64(len(record)))        // size: the record body, if any
	putTarOctal(blk[136:148], paxPrologueModTime.Unix()) // mtime
	blk[156] = tar.TypeXHeader                           // typeflag
	copy(blk[257:263], "ustar\x00")                      // POSIX magic
	copy(blk[263:265], "00")                             // POSIX version
	sealTarBlock(blk)

	// A tar body is padded out to a whole block; a zero-length one needs none.
	padding := make([]byte, (tarBlockSize-len(record)%tarBlockSize)%tarBlockSize)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for range count {
		for _, chunk := range [][]byte{blk, record, padding} {
			if _, err := gz.Write(chunk); err != nil {
				t.Fatalf("failed to write pax meta header: %v", err)
			}
		}
	}
	tw := tar.NewWriter(gz)
	header := &tar.Header{Name: "README.md", Size: 1, Mode: 0o644, ModTime: paxPrologueModTime}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("failed to write the trailing entry's header: %v", err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatalf("failed to write the trailing entry: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// probeArchiveBytes writes archiveBytes to a file of its own and returns what
// ProbeTarGz makes of it, since the probe takes a path rather than a reader.
func probeArchiveBytes(t *testing.T, archiveBytes []byte) error {
	t.Helper()

	path := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(path, archiveBytes, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return ProbeTarGz(t.Context(), path)
}

// TestProbeTarGzRefusesAnUnboundedMetaHeaderChain pins the probe's scan bound
// against 'x' headers Next never returns. 10,240 is the shortest chain refused,
// hand-spelled so moving helpers.ArchiveProbeMaxBytes fails this or its control.
func TestProbeTarGzRefusesAnUnboundedMetaHeaderChain(t *testing.T) {
	t.Parallel()

	const chainHeaders = 10240

	err := probeArchiveBytes(t, buildProbePaxPrologueArchive(t, chainHeaders, nil))
	if !errors.Is(err, helpers.ErrArtifactTarHeaderNotFound) {
		t.Fatalf("%d-header chain: ProbeTarGz = %v, want %v", chainHeaders, err, helpers.ErrArtifactTarHeaderNotFound)
	}
	// The crossing must not also read as ErrArtifactNotTarGz: the stream is a
	// tar, and that headline would send an operator looking for an error page.
	if errors.Is(err, helpers.ErrArtifactNotTarGz) {
		t.Fatalf("%d-header chain: the refusal also reads as %v", chainHeaders, helpers.ErrArtifactNotTarGz)
	}
}

// TestProbeTarGzAcceptsAChainInsideTheBound is the positive control: 10,239
// headers plus the ordinary one pull exactly helpers.ArchiveProbeMaxBytes, so
// it sits at the edge and also pins the off-by-one.
func TestProbeTarGzAcceptsAChainInsideTheBound(t *testing.T) {
	t.Parallel()

	const chainHeaders = 10239

	if err := probeArchiveBytes(t, buildProbePaxPrologueArchive(t, chainHeaders, nil)); err != nil {
		t.Fatalf("ProbeTarGz over a %d-header chain = %v, want nil", chainHeaders, err)
	}
}

// TestProbeTarGzAcceptsARealPaxPrologue pins that an ordinary PAX artifact
// still passes the scan bound: one 'x' header with a 1,024-byte path record,
// spelled as a writer emits it, then the entry it renames.
func TestProbeTarGzAcceptsARealPaxPrologue(t *testing.T) {
	t.Parallel()

	const (
		recordLen = 1024
		nameLen   = recordLen - len("1024 path=\n")
	)
	record := fmt.Sprintf("%d path=%s\n", recordLen, strings.Repeat("n", nameLen))
	// A fixture guard rather than a pin: it catches a later edit of recordLen
	// whose digit count no longer matches the literal measured above.
	if len(record) != recordLen {
		t.Fatalf("fixture record is %d bytes, want %d", len(record), recordLen)
	}

	if err := probeArchiveBytes(t, buildProbePaxPrologueArchive(t, 1, []byte(record))); err != nil {
		t.Fatalf("ProbeTarGz over a real PAX prologue = %v, want nil", err)
	}
}

// TestProbeTarGzReportsCancellationAsItself pins notTarGzError's exception: a
// probe stopped by the caller's cancellation reports context.Canceled, not
// ErrArtifactNotTarGz. The first probe is the same-file positive control.
func TestProbeTarGzReportsCancellationAsItself(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(path, buildTestArchive(t, nil), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if err := ProbeTarGz(t.Context(), path); err != nil {
		t.Fatalf("ProbeTarGz over an empty archive = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ProbeTarGz(ctx, path)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ProbeTarGz under a canceled context = %v, want errors.Is context.Canceled", err)
	}
	if errors.Is(err, helpers.ErrArtifactNotTarGz) {
		t.Fatalf("ProbeTarGz under a canceled context also reads as %v", helpers.ErrArtifactNotTarGz)
	}
}
