package manifest

import (
	"fmt"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
)

// The synthetic artifact both benchmarks run over: about 50 MB of
// incompressible file bodies, so gzip cannot shape the measurement and a
// per-entry cost shows up.
const (
	benchChainFiles    = 2000
	benchChainFileSize = 25 << 10
	// benchChainSeed fixes the fixture's bytes, so two runs measure the same
	// artifact.
	benchChainSeed = 0x5a
)

// buildBenchChainArtifact writes the shared fixture per benchmark, so each temp
// directory reclaims its 50 MB, and returns its path with the manifest a
// signature would have covered.
func buildBenchChainArtifact(b *testing.B) (string, []byte) {
	b.Helper()

	source := rand.NewChaCha8([32]byte{benchChainSeed})
	entries := make([]chainEntry, 0, benchChainFiles)
	for i := range benchChainFiles {
		body := make([]byte, benchChainFileSize)
		if _, err := source.Read(body); err != nil {
			b.Fatalf("failed to fill the fixture's body: %v", err)
		}
		entries = append(entries, chainEntry{name: fmt.Sprintf("plugins/modules/m%d.bin", i), content: body})
	}
	return buildChainArtifact(b, chainSpec{entries: entries})
}

// BenchmarkVerifyChain measures one whole chain check: the gzip and tar walk,
// a sha256 over every entry body, and the two document decodes.
func BenchmarkVerifyChain(b *testing.B) {
	artifact, manifestJSON := buildBenchChainArtifact(b)

	b.ReportAllocs()
	for b.Loop() {
		if err := VerifyChain(b.Context(), artifact, manifestJSON); err != nil {
			b.Fatalf("VerifyChain failed: %v", err)
		}
	}
}

// BenchmarkExtractTarGzForComparison runs the extractor over the same artifact
// as a scale reference for the chain check. It also writes to disk, and only
// ns/op excludes the per-iteration directory setup.
func BenchmarkExtractTarGzForComparison(b *testing.B) {
	artifact, _ := buildBenchChainArtifact(b)

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		// os.MkdirTemp rather than b.TempDir, which defers every iteration's
		// cleanup to the end of the benchmark: at 50 MB an iteration that would
		// fill a disk before anything was reclaimed.
		//nolint:usetesting // per-iteration cleanup, see comment above.
		dst, err := os.MkdirTemp("", "gg-chain-bench-")
		if err != nil {
			b.Fatalf("failed to create temp dir: %v", err)
		}
		b.StartTimer()

		if err := archive.ExtractTarGz(b.Context(), artifact, dst); err != nil {
			b.Fatalf("extraction failed: %v", err)
		}

		b.StopTimer()
		if err := os.RemoveAll(dst); err != nil {
			b.Fatalf("failed to remove temp dir: %v", err)
		}
		b.StartTimer()
	}
}
