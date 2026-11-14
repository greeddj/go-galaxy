package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// writeTimedArchive writes a tar.gz whose entries carry the given times in
// order, so a test can tell the first entry's time from a later one's.
func writeTimedArchive(t *testing.T, times ...time.Time) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i, when := range times {
		header := &tar.Header{Typeflag: tar.TypeReg, Name: string(rune('a'+i)) + ".txt", Mode: 0o644, ModTime: when}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("write header %d: %v", i, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	path := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// TestFirstEntryModTimeReadsTheFirstEntryInUTC pins that the first entry's
// time comes back, not a later one's, and in UTC whatever zone wrote it.
func TestFirstEntryModTimeReadsTheFirstEntryInUTC(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, time.October, 29, 18, 55, 0, 0, time.FixedZone("", 5*60*60))
	path := writeTimedArchive(t, first, first.Add(time.Hour))

	got, err := FirstEntryModTime(t.Context(), path)
	if err != nil {
		t.Fatalf("FirstEntryModTime = %v, want nil", err)
	}
	if !got.Equal(first) || got.Location() != time.UTC {
		t.Fatalf("FirstEntryModTime = %v (%v), want %v in UTC", got, got.Location(), first.UTC())
	}
}

// TestFirstEntryModTimeRefusesWhatHoldsNoEntry pins the two failures a caller
// reads as "no time": an empty archive and bytes that are no archive at all.
func TestFirstEntryModTimeRefusesWhatHoldsNoEntry(t *testing.T) {
	t.Parallel()

	empty := writeTimedArchive(t)
	if _, err := FirstEntryModTime(t.Context(), empty); !errors.Is(err, helpers.ErrArtifactTarHeaderNotFound) {
		t.Fatalf("FirstEntryModTime over an empty archive = %v, want errors.Is %v", err, helpers.ErrArtifactTarHeaderNotFound)
	}

	page := filepath.Join(t.TempDir(), "page.tar.gz")
	if err := os.WriteFile(page, []byte("<html>404</html>"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := FirstEntryModTime(t.Context(), page); !errors.Is(err, helpers.ErrArtifactNotTarGz) {
		t.Fatalf("FirstEntryModTime over non-gzip bytes = %v, want errors.Is %v", err, helpers.ErrArtifactNotTarGz)
	}
}
