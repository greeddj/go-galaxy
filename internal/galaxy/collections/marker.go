package collections

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
)

// treeTally is a cheap fingerprint of an installed tree: its directory count,
// its non-directory entry count and their summed lstat sizes. It is not a hash;
// verifyExtractMarker states what it does and does not detect.
type treeTally struct {
	Entries int64 // non-directory entries (regular files, symlinks, anything else)
	Dirs    int64 // directories, excluding the root
	Bytes   int64 // sum of lstat sizes over non-directory entries
}

// extractMarkerVersion is the format tag every marker begins with. A format
// change must change the tag, so an older marker is rejected as unparseable
// and re-extracted rather than misread.
const extractMarkerVersion = "go-galaxy-extract-1"

// extractMarkerFieldCount is the exact number of space-separated fields in a
// well-formed marker: the version tag, then entries=, dirs= and bytes=.
const extractMarkerFieldCount = 4

// extractMarkerMaxReadSize caps a marker read. A well-formed marker is far
// shorter, so a longer file is rejected without learning its real length.
const extractMarkerMaxReadSize = 256

// scanTree tallies target.rel in one fs.WalkDir through target.root; a symlink
// is one entry and is never followed. Top-level ExtractMarkerPrefix entries are
// skipped so a role's marker, which lives in its own tree, never counts itself.
func scanTree(target installTarget) (treeTally, error) {
	var tally treeTally
	err := fs.WalkDir(target.root.FS(), target.rel, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == target.rel {
			return nil
		}
		if path.Dir(p) == target.rel && strings.HasPrefix(d.Name(), helpers.ExtractMarkerPrefix) {
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
		return treeTally{}, err
	}
	return tally, nil
}

// formatExtractMarker renders tally as the single-line marker
// "go-galaxy-extract-1 entries=<n> dirs=<n> bytes=<n>" and a newline.
func formatExtractMarker(tally treeTally) string {
	return fmt.Sprintf("%s entries=%d dirs=%d bytes=%d\n", extractMarkerVersion, tally.Entries, tally.Dirs, tally.Bytes)
}

// parseExtractMarker strictly parses formatExtractMarker's output; fmt.Sscanf
// is avoided because it ignores trailing garbage. Any mismatch reports false,
// which every caller treats as an absent marker.
func parseExtractMarker(content string) (treeTally, bool) {
	content = strings.TrimSuffix(content, "\n")
	fields := strings.Split(content, " ")
	if len(fields) != extractMarkerFieldCount || fields[0] != extractMarkerVersion {
		return treeTally{}, false
	}
	entries, ok := parseExtractMarkerField(fields[1], "entries=")
	if !ok {
		return treeTally{}, false
	}
	dirs, ok := parseExtractMarkerField(fields[2], "dirs=")
	if !ok {
		return treeTally{}, false
	}
	bytesCount, ok := parseExtractMarkerField(fields[3], "bytes=")
	if !ok {
		return treeTally{}, false
	}
	return treeTally{Entries: entries, Dirs: dirs, Bytes: bytesCount}, true
}

// parseExtractMarkerField requires field to be key followed by a non-negative
// base-10 int64 with nothing after the last digit.
func parseExtractMarkerField(field, key string) (int64, bool) {
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

// readExtractMarker reads the marker at rel through target.root, capped at
// extractMarkerMaxReadSize bytes, and reports false for a missing, unreadable
// or oversized file.
func readExtractMarker(target installTarget, rel string) (string, bool) {
	f, err := target.root.Open(rel)
	if err != nil {
		return "", false
	}
	defer func() {
		_ = f.Close()
	}()

	buf := make([]byte, extractMarkerMaxReadSize+1)
	n, err := io.ReadFull(f, buf)
	switch {
	case err == nil:
		// A full extractMarkerMaxReadSize+1 bytes were read, meaning the file
		// is at least one byte over the cap: reject as oversized.
		return "", false
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return string(buf[:n]), true
	default:
		return "", false
	}
}

// writeExtractMarker records target's current scanTree tally as sha's marker,
// removing whatever is at that name first so a hard link there is severed, not
// written through. extractTree creates and wipes the marker's directory.
func writeExtractMarker(target installTarget, sha string) error {
	rel, ok := markerRel(target, sha)
	if !ok {
		// A hard error, unlike the check paths: sha already passed validation,
		// and writing no marker would re-extract this tree on every future run.
		return fmt.Errorf("%w: %q", helpers.ErrMalformedArtifactSHA256, sha)
	}
	tally, err := scanTree(target)
	if err != nil {
		return err
	}
	if err := target.root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return target.root.WriteFile(rel, []byte(formatExtractMarker(tally)), helpers.FileMod)
}

// extractMarkerStatus discriminates why checkExtractMarker did or did not
// find target's extract-done marker still valid.
type extractMarkerStatus int

const (
	// extractMarkerUnknown is the zero value, never produced by
	// checkExtractMarker, so a zero extractMarkerOutcome fails closed.
	extractMarkerUnknown extractMarkerStatus = iota
	// extractMarkerMatches means the marker is present, well-formed, and its
	// recorded tally equals what scanTree observes right now.
	extractMarkerMatches
	// extractMarkerMissing means the marker file is absent or unreadable.
	extractMarkerMissing
	// extractMarkerMalformed means the marker exists but does not parse as
	// the current format (including the legacy "ok" sentinel it replaced).
	extractMarkerMalformed
	// extractMarkerScanFailed means scanTree itself returned an error.
	extractMarkerScanFailed
	// extractMarkerDrifted means the marker parsed fine, the scan succeeded,
	// but the two tallies disagree.
	extractMarkerDrifted
	// extractMarkerUnsafeSHA means sha is not helpers.IsSHA256Hex, so no
	// marker path was computed at all (see markerRel).
	extractMarkerUnsafeSHA
)

// extractMarkerOutcome is checkExtractMarker's result: want and got are set
// only for extractMarkerDrifted, scanErr only for extractMarkerScanFailed.
type extractMarkerOutcome struct {
	scanErr error
	want    treeTally
	got     treeTally
	status  extractMarkerStatus
}

// matches reports whether the marker was found valid - the single bit a
// caller that only cares about the yes/no answer (installDryRunProbe) needs,
// without inspecting status itself.
func (o extractMarkerOutcome) matches() bool {
	return o.status == extractMarkerMatches
}

// markerRel is the only builder of target's marker path for sha. sha is held
// to helpers.IsSHA256Hex before the join, since a sha carrying "/" and ".."
// would let path.Join climb out of target.marker.
func markerRel(target installTarget, sha string) (string, bool) {
	if !helpers.IsSHA256Hex(sha) {
		return "", false
	}
	return path.Join(target.marker, helpers.ExtractMarkerPrefix+sha), true
}

// checkExtractMarker reports whether target's marker for sha is present,
// well-formed and equal to what scanTree observes now. It is a pure read;
// verifyExtractMarker adds the install path's logging and cleanup.
func checkExtractMarker(target installTarget, sha string) extractMarkerOutcome {
	rel, ok := markerRel(target, sha)
	if !ok {
		return extractMarkerOutcome{status: extractMarkerUnsafeSHA}
	}

	content, ok := readExtractMarker(target, rel)
	if !ok {
		return extractMarkerOutcome{status: extractMarkerMissing}
	}
	want, ok := parseExtractMarker(content)
	if !ok {
		return extractMarkerOutcome{status: extractMarkerMalformed}
	}
	got, err := scanTree(target)
	if err != nil {
		return extractMarkerOutcome{status: extractMarkerScanFailed, scanErr: err}
	}
	if got != want {
		return extractMarkerOutcome{status: extractMarkerDrifted, want: want, got: got}
	}
	return extractMarkerOutcome{status: extractMarkerMatches}
}

// verifyExtractMarker is checkExtractMarker plus install's logging and cleanup:
// a rejected marker is removed so the caller re-extracts, and the run never
// fails. It misses an in-place edit that keeps a file's exact length.
func verifyExtractMarker(out output.Printer, target installTarget, sha string) bool {
	rel, ok := markerRel(target, sha)
	if !ok {
		// An unsafe sha means a corrupt snapshot or a lying server, so it warns
		// like a drift; %q keeps a control byte in sha from forging a log line.
		out.Warnf("refusing to use unsafe artifact sha256 %q as an extract marker for %s, will re-extract", sha, target.path)
		return false
	}
	// markerDisplay names the marker for log lines only; every filesystem
	// operation still goes through target.root and rel.
	markerDisplay := filepath.Join(target.root.Name(), filepath.FromSlash(rel))
	outcome := checkExtractMarker(target, sha)

	switch outcome.status {
	case extractMarkerUnknown:
		// Never produced by checkExtractMarker; listed so the zero value falls
		// through to the fail-closed removal below.
	case extractMarkerUnsafeSHA:
		// Unreachable: the markerRel check above already returned. Listed to
		// keep the switch exhaustive.
	case extractMarkerMatches:
		return true
	case extractMarkerMissing:
		out.Debugf("extract marker missing or unreadable at %s, will re-extract", markerDisplay)
	case extractMarkerMalformed:
		out.Debugf("extract marker at %s is not in the current format (legacy or corrupt), will re-extract", markerDisplay)
	case extractMarkerScanFailed:
		out.Debugf("failed to scan %s to verify its extract marker: %v, will re-extract", target.path, outcome.scanErr)
	case extractMarkerDrifted:
		out.Warnf(
			"installed tree %s no longer matches its extract marker (recorded entries=%d dirs=%d bytes=%d, "+
				"found entries=%d dirs=%d bytes=%d): the cache may have been modified after extraction; re-extracting",
			target.path, outcome.want.Entries, outcome.want.Dirs, outcome.want.Bytes, outcome.got.Entries, outcome.got.Dirs, outcome.got.Bytes,
		)
	}
	_ = target.root.Remove(rel)
	return false
}
