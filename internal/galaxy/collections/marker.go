package collections

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/greeddj/go-galaxy/internal/galaxy/extractmarker"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
)

// writeExtractMarker records target's current tally as sha's marker, removing
// whatever is at that name first so a hard link there is severed, not written
// through. extractTree creates and wipes the marker's directory.
func writeExtractMarker(target installTarget, sha string) error {
	rel, ok := markerRel(target, sha)
	if !ok {
		// A hard error, unlike the check paths: sha already passed validation,
		// and writing no marker would re-extract this tree on every future run.
		return fmt.Errorf("%w: %q", helpers.ErrMalformedArtifactSHA256, sha)
	}
	tally, err := extractmarker.Scan(target.root, target.rel)
	if err != nil {
		return err
	}
	if err := target.root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return target.root.WriteFile(rel, []byte(extractmarker.Format(tally)), helpers.FileMod)
}

// markerRel is target's marker path for sha, in target.marker. The join and
// its sha check are extractmarker.Rel's alone, so a traversal sha is refused
// before any path exists.
func markerRel(target installTarget, sha string) (string, bool) {
	return extractmarker.Rel(target.marker, sha)
}

// checkExtractMarker reports whether target's marker for sha is present,
// well-formed and equal to target's tree now. It is a pure read;
// verifyExtractMarker adds the install path's logging and cleanup.
func checkExtractMarker(target installTarget, sha string) extractmarker.Outcome {
	return extractmarker.Check(target.root, target.marker, sha, target.rel)
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

	switch outcome.Status {
	case extractmarker.StatusUnknown:
		// Never produced by checkExtractMarker; listed so the zero value falls
		// through to the fail-closed removal below.
	case extractmarker.StatusUnsafeSHA:
		// Unreachable: the markerRel check above already returned. Listed to
		// keep the switch exhaustive.
	case extractmarker.StatusMatches:
		return true
	case extractmarker.StatusMissing:
		out.Debugf("extract marker missing or unreadable at %s, will re-extract", markerDisplay)
	case extractmarker.StatusMalformed:
		out.Debugf("extract marker at %s is not in the current format (legacy or corrupt), will re-extract", markerDisplay)
	case extractmarker.StatusScanFailed:
		out.Debugf("failed to scan %s to verify its extract marker: %v, will re-extract", target.path, outcome.ScanErr)
	case extractmarker.StatusDrifted:
		out.Warnf(
			"installed tree %s no longer matches its extract marker (recorded entries=%d dirs=%d bytes=%d, "+
				"found entries=%d dirs=%d bytes=%d): the cache may have been modified after extraction; re-extracting",
			target.path, outcome.Want.Entries, outcome.Want.Dirs, outcome.Want.Bytes, outcome.Got.Entries, outcome.Got.Dirs, outcome.Got.Bytes,
		)
	}
	_ = target.root.Remove(rel)
	return false
}
