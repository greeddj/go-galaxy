package extractmarker

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// openTestRoot opens an os.Root at dir, closed when the test ends.
func openTestRoot(tb testing.TB, dir string) *os.Root {
	tb.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		tb.Fatalf("os.OpenRoot(%s): %v", dir, err)
	}
	tb.Cleanup(func() {
		_ = root.Close()
	})
	return root
}

// writeTestFile writes body at path, creating its parent directories.
func writeTestFile(tb testing.TB, path string, body []byte) {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(path), helpers.DirMod); err != nil {
		tb.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, body, helpers.FileMod); err != nil {
		tb.Fatalf("write %s: %v", path, err)
	}
}

// buildFixedTree creates a small deterministic tree and returns its directory
// plus the exact tally Scan must produce for it.
func buildFixedTree(t *testing.T) (string, Tally) {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "file0.txt"), []byte("root-file"))             // 9 bytes
	writeTestFile(t, filepath.Join(dir, "dirA", "file1.txt"), []byte("hello"))         // 5 bytes
	writeTestFile(t, filepath.Join(dir, "dirA", "file2.txt"), []byte("world!"))        // 6 bytes
	writeTestFile(t, filepath.Join(dir, "dirB", "nested", "file3.txt"), []byte("abc")) // 3 bytes
	// entries: file0, file1, file2, file3 = 4; dirs: dirA, dirB, dirB/nested = 3;
	// bytes: 9 + 5 + 6 + 3 = 23.
	return dir, Tally{Entries: 4, Dirs: 3, Bytes: 23}
}

// TestScanIgnoresSiblingMarkers pins that a top-level file carrying the marker
// prefix, such as another sha's marker, never changes the tally.
func TestScanIgnoresSiblingMarkers(t *testing.T) {
	t.Parallel()
	dir, want := buildFixedTree(t)
	root := openTestRoot(t, dir)

	before, err := Scan(root, ".")
	if err != nil {
		t.Fatalf("Scan before: %v", err)
	}
	if before != want {
		t.Fatalf("Scan before sibling = %+v, want %+v", before, want)
	}

	writeTestFile(t, filepath.Join(dir, helpers.ExtractMarkerPrefix+"deadbeef"), bytes.Repeat([]byte("x"), 128))

	after, err := Scan(root, ".")
	if err != nil {
		t.Fatalf("Scan after: %v", err)
	}
	if after != want {
		t.Fatalf("Scan after sibling marker = %+v, want unchanged %+v", after, want)
	}
}

// TestScanDoesNotFollowSymlinks pins that a symlink to an outside directory
// counts as one entry and is never descended into, so a loop cannot hang the
// walk and a link cannot inflate the tally.
func TestScanDoesNotFollowSymlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "real.txt"), []byte("data"))

	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "a.txt"), []byte("aaaa"))
	writeTestFile(t, filepath.Join(outside, "sub", "b.txt"), []byte("bbbbb"))

	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := Scan(openTestRoot(t, dir), ".")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	// real.txt plus the symlink itself: 2 non-directory entries. The
	// symlinked directory's own contents must never be counted.
	if got.Entries != 2 {
		t.Fatalf("Entries = %d, want 2 (real.txt + the symlink itself, not its target's contents)", got.Entries)
	}
	if got.Dirs != 0 {
		t.Fatalf("Dirs = %d, want 0 (a symlink to a directory must not be descended into)", got.Dirs)
	}
}

// TestOutcomeZeroValueFailsClosed pins that a zero Outcome never matches, so a
// path that forgets to set Status forces re-extraction.
func TestOutcomeZeroValueFailsClosed(t *testing.T) {
	t.Parallel()
	if (Outcome{}).Matches() {
		t.Fatal("expected a zero-value Outcome{} to never satisfy Matches()")
	}
}

// BenchmarkScan times one Scan pass over a collection-sized tree: 500 files of
// about 2000 bytes across 50 subdirectories.
func BenchmarkScan(b *testing.B) {
	dir := b.TempDir()
	const subdirs = 50
	const filesPerSubdir = 10
	const fileSize = 2000
	content := bytes.Repeat([]byte("x"), fileSize)
	for i := range subdirs {
		for j := range filesPerSubdir {
			writeTestFile(b, filepath.Join(dir, fmt.Sprintf("sub%d", i), fmt.Sprintf("file%d.dat", j)), content)
		}
	}
	root := openTestRoot(b, dir)

	for b.Loop() {
		if _, err := Scan(root, "."); err != nil {
			b.Fatalf("Scan: %v", err)
		}
	}
}

// TestSHAsListsOnlyRegularShaShapedMarkers pins what SHAs admits: a regular
// file with the marker prefix and a sha suffix, never a directory, a symlink
// or a non-sha suffix, and nothing for a directory that does not exist.
func TestSHAsListsOnlyRegularShaShapedMarkers(t *testing.T) {
	t.Parallel()
	const first = "1111111111111111111111111111111111111111111111111111111111111111"
	const second = "2222222222222222222222222222222222222222222222222222222222222222"
	const linked = "3333333333333333333333333333333333333333333333333333333333333333"
	const nested = "4444444444444444444444444444444444444444444444444444444444444444"
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, helpers.ExtractMarkerPrefix+second), nil)
	writeTestFile(t, filepath.Join(dir, helpers.ExtractMarkerPrefix+first), nil)
	writeTestFile(t, filepath.Join(dir, helpers.ExtractMarkerPrefix+"deadbeef"), nil)
	writeTestFile(t, filepath.Join(dir, "unrelated"), nil)
	writeTestFile(t, filepath.Join(dir, helpers.ExtractMarkerPrefix+nested, "inside"), nil)
	if err := os.Symlink("unrelated", filepath.Join(dir, helpers.ExtractMarkerPrefix+linked)); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	root := openTestRoot(t, dir)

	if got, want := SHAs(root, "."), []string{first, second}; !slices.Equal(got, want) {
		t.Fatalf("SHAs = %v, want %v", got, want)
	}
	if got := SHAs(root, "absent"); got != nil {
		t.Fatalf("SHAs of an absent directory = %v, want nil", got)
	}
}

// TestCheckReadsAMarkerInsideTheTreeItCounts covers a marker kept in the tree
// it tallies, where releases before the .info marker wrote a collection's: it
// matches, the marker left out of its own count, until a file is added.
func TestCheckReadsAMarkerInsideTheTreeItCounts(t *testing.T) {
	t.Parallel()
	const sha = "5555555555555555555555555555555555555555555555555555555555555555"
	dir, tally := buildFixedTree(t)
	writeTestFile(t, filepath.Join(dir, helpers.ExtractMarkerPrefix+sha), []byte(Format(tally)))
	root := openTestRoot(t, dir)

	if outcome := Check(root, ".", sha, "."); outcome.Status != StatusMatches {
		t.Fatalf("Check of an in-tree marker = %+v, want StatusMatches", outcome)
	}
	writeTestFile(t, filepath.Join(dir, "dirA", "added.txt"), []byte("new"))
	outcome := Check(root, ".", sha, ".")
	if outcome.Status != StatusDrifted || outcome.Want != tally {
		t.Fatalf("Check after an added file = %+v, want StatusDrifted with Want %+v", outcome, tally)
	}
}
