// Package extractmarker is the extract-done marker: its path, its one-line
// format and the tree tally it records. Install writes and verifies it, and
// cleanup reads it as the evidence that a collection copy is this tool's.
package extractmarker

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Tally is a cheap fingerprint of an installed tree: its directory count, its
// non-directory entry count and their summed lstat sizes. It is not a hash;
// Check states what it does and does not detect.
type Tally struct {
	Entries int64 // non-directory entries (regular files, symlinks, anything else)
	Dirs    int64 // directories, excluding the root
	Bytes   int64 // sum of lstat sizes over non-directory entries
}

// formatVersion is the format tag every marker begins with. A format change
// must change the tag, so an older marker is rejected as unparseable and
// re-extracted rather than misread.
const formatVersion = "go-galaxy-extract-1"

// fieldCount is the exact number of space-separated fields in a well-formed
// marker: the version tag, then entries=, dirs= and bytes=.
const fieldCount = 4

// maxReadSize caps a marker read. A well-formed marker is far shorter, so a
// longer file is rejected without learning its real length.
const maxReadSize = 256

// Rel is the only builder of the path of dir's marker for sha, slash-separated
// for an os.Root. sha is held to helpers.IsSHA256Hex before the join, since a
// sha carrying "/" and ".." would let path.Join climb out of dir.
func Rel(dir, sha string) (string, bool) {
	if !helpers.IsSHA256Hex(sha) {
		return "", false
	}
	return path.Join(dir, helpers.ExtractMarkerPrefix+sha), true
}

// SHAs lists, in name order, the sha of every marker in dir under root. Only a
// regular file named ExtractMarkerPrefix plus a sha-shaped suffix counts, and
// a dir that cannot be listed has none.
func SHAs(root *os.Root, dir string) []string {
	entries, err := fs.ReadDir(root.FS(), dir)
	if err != nil {
		return nil
	}
	var shas []string
	for _, e := range entries {
		sha, ok := strings.CutPrefix(e.Name(), helpers.ExtractMarkerPrefix)
		if ok && e.Type().IsRegular() && helpers.IsSHA256Hex(sha) {
			shas = append(shas, sha)
		}
	}
	return shas
}

// Scan tallies tree in one fs.WalkDir through root; a symlink is one entry and
// is never followed. Top-level ExtractMarkerPrefix entries are skipped, so a
// marker kept inside the tree it counts never counts itself.
func Scan(root *os.Root, tree string) (Tally, error) {
	var tally Tally
	err := fs.WalkDir(root.FS(), tree, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == tree {
			return nil
		}
		if path.Dir(p) == tree && strings.HasPrefix(d.Name(), helpers.ExtractMarkerPrefix) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			tally.Dirs++
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		tally.Entries++
		tally.Bytes += info.Size()
		return nil
	})
	if err != nil {
		return Tally{}, err
	}
	return tally, nil
}

// Format renders tally as the single-line marker
// "go-galaxy-extract-1 entries=<n> dirs=<n> bytes=<n>" and a newline.
func Format(tally Tally) string {
	return fmt.Sprintf("%s entries=%d dirs=%d bytes=%d\n", formatVersion, tally.Entries, tally.Dirs, tally.Bytes)
}

// parse strictly parses Format's output; fmt.Sscanf is avoided because it
// ignores trailing garbage. Any mismatch reports false, which Check treats as
// a marker that proves nothing.
func parse(content string) (Tally, bool) {
	content = strings.TrimSuffix(content, "\n")
	fields := strings.Split(content, " ")
	if len(fields) != fieldCount || fields[0] != formatVersion {
		return Tally{}, false
	}
	entries, ok := parseField(fields[1], "entries=")
	if !ok {
		return Tally{}, false
	}
	dirs, ok := parseField(fields[2], "dirs=")
	if !ok {
		return Tally{}, false
	}
	bytesCount, ok := parseField(fields[3], "bytes=")
	if !ok {
		return Tally{}, false
	}
	return Tally{Entries: entries, Dirs: dirs, Bytes: bytesCount}, true
}

// parseField requires field to be key followed by a non-negative base-10
// int64 with nothing after the last digit.
func parseField(field, key string) (int64, bool) {
	rest, ok := strings.CutPrefix(field, key)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// read reads the marker at rel through root, capped at maxReadSize bytes, and
// reports false for a missing, unreadable or oversized file.
func read(root *os.Root, rel string) (string, bool) {
	f, err := root.Open(rel)
	if err != nil {
		return "", false
	}
	defer func() {
		_ = f.Close()
	}()

	buf := make([]byte, maxReadSize+1)
	n, err := io.ReadFull(f, buf)
	switch {
	case err == nil:
		// A full maxReadSize+1 bytes were read, meaning the file is at least
		// one byte over the cap: reject as oversized.
		return "", false
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return string(buf[:n]), true
	default:
		return "", false
	}
}

// Status discriminates why Check did or did not find a marker still valid.
type Status int

const (
	// StatusUnknown is the zero value, never produced by Check, so a zero
	// Outcome fails closed.
	StatusUnknown Status = iota
	// StatusMatches means the marker is present, well-formed, and its
	// recorded tally equals what Scan observes right now.
	StatusMatches
	// StatusMissing means the marker file is absent or unreadable.
	StatusMissing
	// StatusMalformed means the marker exists but does not parse as the
	// current format (including the legacy "ok" sentinel it replaced).
	StatusMalformed
	// StatusScanFailed means Scan itself returned an error.
	StatusScanFailed
	// StatusDrifted means the marker parsed fine, the scan succeeded, but
	// the two tallies disagree.
	StatusDrifted
	// StatusUnsafeSHA means sha is not helpers.IsSHA256Hex, so no marker
	// path was computed at all (see Rel).
	StatusUnsafeSHA
)

// Outcome is Check's result: Want and Got are set only for StatusDrifted,
// ScanErr only for StatusScanFailed.
type Outcome struct {
	ScanErr error
	Want    Tally
	Got     Tally
	Status  Status
}

// Matches reports whether the marker was found valid, the single bit a caller
// that does not tell the failures apart needs.
func (o Outcome) Matches() bool {
	return o.Status == StatusMatches
}

// Check reports whether dir's marker for sha under root is present, parses and
// equals what Scan observes in tree now. It is a pure read, and it misses an
// in-place edit that keeps a file's exact length.
func Check(root *os.Root, dir, sha, tree string) Outcome {
	rel, ok := Rel(dir, sha)
	if !ok {
		return Outcome{Status: StatusUnsafeSHA}
	}
	content, ok := read(root, rel)
	if !ok {
		return Outcome{Status: StatusMissing}
	}
	want, ok := parse(content)
	if !ok {
		return Outcome{Status: StatusMalformed}
	}
	got, err := Scan(root, tree)
	if err != nil {
		return Outcome{Status: StatusScanFailed, ScanErr: err}
	}
	if got != want {
		return Outcome{Status: StatusDrifted, Want: want, Got: got}
	}
	return Outcome{Status: StatusMatches}
}
