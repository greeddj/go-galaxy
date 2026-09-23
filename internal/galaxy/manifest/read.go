package manifest

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/gzipstream"
)

// errScanLimitReached is the verdict limitReader carries for the manifest
// scan; walkError is the one place it becomes helpers.ErrManifestNotFound.
var errScanLimitReached = errors.New("manifest scan limit reached")

// ReadFromTarGz returns the artifact's top-level MANIFEST.json, reading at
// most helpers.ManifestScanMaxBytes under the entry caps. It never returns zero
// bytes, since a detached signature over an empty document would still verify.
func ReadFromTarGz(ctx context.Context, artifactPath string) ([]byte, error) {
	//nolint:gosec // artifactPath names an artifact this run downloaded or produced.
	file, err := os.Open(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open artifact to read its manifest: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	data, err := readFromTarGzStream(ctx, file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", artifactPath, err)
	}
	return data, nil
}

// readFromTarGzStream walks r as a gzipped tar under ReadFromTarGz's bounds and
// does not close r. It opens through internal/gzipstream, so it accepts exactly
// the streams the extractor would.
func readFromTarGzStream(ctx context.Context, r io.Reader) ([]byte, error) {
	uncompressed, err := gzipstream.NewReader(ctx, r)
	if err != nil {
		return nil, decompressorOpenError(err)
	}
	defer func() {
		_ = uncompressed.Close()
	}()

	limited := &limitReader{r: uncompressed, over: errScanLimitReached, max: helpers.ManifestScanMaxBytes}
	tarReader := tar.NewReader(limited)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: the tar stream ended after %d bytes", helpers.ErrManifestNotFound, limited.n)
		}
		if err != nil {
			return nil, walkError(err)
		}
		// Measured ahead of the refusal below, the one message this walk
		// renders an archive-chosen name in.
		if err := checkEntryNameLength(header.Name); err != nil {
			return nil, err
		}
		// Charged whether or not this is the entry being searched for, since
		// the walk has to read past every other one to reach it.
		if header.Size > helpers.ArchiveMaxEntrySize {
			return nil, fmt.Errorf("%w %q: %d bytes", helpers.ErrArchiveEntryIsTooLarge, header.Name, header.Size)
		}
		if !topLevelManifest(header) {
			continue
		}
		return readManifestBody(tarReader, header.Size)
	}
}

// topLevelManifest reports whether header names the artifact's own manifest.
// It cleans with path, not filepath: a tar name is slash-separated everywhere,
// so a backslash can never turn a nested entry into a top-level one.
func topLevelManifest(header *tar.Header) bool {
	return header.Typeflag == tar.TypeReg && path.Clean(header.Name) == helpers.ManifestFileName
}

// readManifestBody reads the size-byte entry r is positioned on, refusing an
// empty one. The buffer is clamped to the scan bound so an over-declaring
// header cannot force a large allocation; archive/tar yields size bytes or fails.
func readManifestBody(r io.Reader, size int64) ([]byte, error) {
	if size <= 0 {
		return nil, fmt.Errorf("%w: the entry named %s is empty", helpers.ErrManifestNotFound, helpers.ManifestFileName)
	}

	// The extra bytes.MinRead is the headroom ReadFrom wants available before
	// it stops, so the buffer is sized once and never grown.
	buf := bytes.NewBuffer(make([]byte, 0, min(size, helpers.ManifestScanMaxBytes)+bytes.MinRead))
	if _, err := buf.ReadFrom(r); err != nil {
		return nil, walkError(err)
	}
	return buf.Bytes(), nil
}

// decompressorOpenError wraps a decompressor that refused to open in
// helpers.ErrArtifactNotTarGz, except the caller's own cancellation, returned
// unchanged so a Ctrl-C is never reported as a malformed artifact.
func decompressorOpenError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %w", helpers.ErrArtifactNotTarGz, err)
}

// walkError renders one tar-walk failure. Crossing the scan bound, whether
// hunting for the entry or reading it, is helpers.ErrManifestNotFound: a
// manifest not presented within the window was not presented.
func walkError(err error) error {
	if errors.Is(err, errScanLimitReached) {
		return fmt.Errorf("%w within the first %d bytes of the tar stream",
			helpers.ErrManifestNotFound, helpers.ManifestScanMaxBytes)
	}
	return fmt.Errorf("failed to read the artifact's tar stream: %w", err)
}

// limitReader fails with over once more than max bytes come out of a
// decompressor, bounding a pass by what it reads rather than by declared or
// compressed sizes; over is a field so each caller names its own verdict.
type limitReader struct {
	r    io.Reader
	err  error
	over error
	max  int64
	n    int64
}

// Read returns zero bytes and over on the crossing call, since io.ReadAtLeast
// and io.CopyN inside archive/tar drop an error that comes with a satisfied
// read. The refusal is sticky and never reads r again.
func (r *limitReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	// Keeps p[:remaining+1] in range: a max below zero drives remaining
	// negative, and at -2 or lower that reslice panics.
	remaining := max(r.max-r.n, 0)
	// Taking this branch means remaining < len(p), hence remaining+1 <= len(p),
	// so the reslice is always within p.
	if remaining < int64(len(p)) {
		p = p[:remaining+1]
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	if r.n > r.max {
		r.err = fmt.Errorf("%w: read %d bytes, limit is %d bytes", r.over, r.n, r.max)
		return 0, r.err
	}
	return n, err
}
