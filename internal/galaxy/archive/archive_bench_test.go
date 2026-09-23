package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// benchFileMode is the fixed permission bits stamped on every generated
// benchmark tar entry, matching a typical extracted Ansible collection file.
const benchFileMode = 0o644

// buildBenchmarkArchive builds a deterministic tar.gz of the given number of
// one-byte files under one depth-deep directory chain, the shared-parent shape
// the ensureNoSymlinkParents memo targets.
func buildBenchmarkArchive(tb testing.TB, files, depth int) []byte {
	tb.Helper()

	// benchModTime stamps every generated tar entry so the archive bytes -
	// and therefore the benchmark's decompression cost - do not depend on
	// wall-clock time.
	benchModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	dirParts := make([]string, depth)
	for i := range depth {
		dirParts[i] = fmt.Sprintf("d%d", i)
	}
	prefix := strings.Join(dirParts, "/")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	content := []byte("x")
	for i := range files {
		header := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     prefix + "/" + fmt.Sprintf("f%d.txt", i),
			Size:     int64(len(content)),
			Mode:     benchFileMode,
			ModTime:  benchModTime,
		}
		if err := tw.WriteHeader(header); err != nil {
			tb.Fatalf("failed to write tar header: %v", err)
		}
		if _, err := tw.Write(content); err != nil {
			tb.Fatalf("failed to write tar content: %v", err)
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

// BenchmarkExtractTarGzStream measures extracting a deep, shared directory
// chain. Read ns/op: allocs/op also counts the per-iteration temp-dir setup and
// teardown, which the timer excludes but ReportAllocs does not.
func BenchmarkExtractTarGzStream(b *testing.B) {
	sizes := []struct {
		files int
		depth int
	}{
		{files: 1000, depth: 8},
		{files: 5000, depth: 8},
	}

	for _, sz := range sizes {
		archiveBytes := buildBenchmarkArchive(b, sz.files, sz.depth)
		b.Run(fmt.Sprintf("files=%d/depth=%d", sz.files, sz.depth), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				b.StopTimer()
				// os.MkdirTemp and os.RemoveAll rather than b.TempDir, so a
				// large b.N does not pile extracted trees up on disk.
				//nolint:usetesting // per-iteration cleanup, see comment above.
				dst, err := os.MkdirTemp("", "gg-extract-bench-")
				if err != nil {
					b.Fatalf("failed to create temp dir: %v", err)
				}
				b.StartTimer()

				if err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst); err != nil {
					b.Fatalf("extraction failed: %v", err)
				}

				b.StopTimer()
				if err := os.RemoveAll(dst); err != nil {
					b.Fatalf("failed to remove temp dir: %v", err)
				}
			}
		})
	}
}
