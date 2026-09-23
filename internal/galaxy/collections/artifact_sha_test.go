package collections

// These tests pin resolveArtifactSHA: a recorded sha must pass IsSHA256Hex, a
// computed one is never checked, and computed is true exactly on the arms
// that hashed the bytes, the bit that tells Ensure whether to re-hash.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/psvmcc/hub/pkg/types"
)

// traversalArtifactSHA is a path-traversal value resolveArtifactSHA must
// reject from a recorded source, the same one
// TestVerifyExtractMarkerRefusesTraversalSHA uses at the marker layer.
const traversalArtifactSHA = "../../../../../../home/ci/.ssh/authorized_keys"

// mustWriteTarball writes content to a fresh file and returns its path, a
// stand-in artifact for archive.FileHashSHA256; it need not be a tar.gz,
// since resolveArtifactSHA only hashes it.
func mustWriteTarball(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(path, content, helpers.FileMod); err != nil {
		t.Fatalf("write tarball: %v", err)
	}
	return path
}

// TestResolveArtifactSHARejectsMalformedMetaSha256 checks that a traversal,
// short or uppercase meta.Artifact.Sha256 is refused with
// ErrMalformedArtifactSHA256, never passed through or replaced by a file hash.
func TestResolveArtifactSHARejectsMalformedMetaSha256(t *testing.T) {
	t.Parallel()
	path := mustWriteTarball(t, []byte("irrelevant tarball bytes"))

	cases := []struct {
		name string
		sha  string
	}{
		{"traversal", traversalArtifactSHA},
		{"short but hex", "deadbeef"},
		{"uppercase", strings.ToUpper(validMarkerSHA)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			meta := &types.GalaxyCollectionVersionInfo{}
			meta.Artifact.Sha256 = tc.sha

			got, computed, err := resolveArtifactSHA(path, meta, nil, "", "")
			if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
				t.Fatalf("resolveArtifactSHA error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
			}
			if got != "" {
				t.Fatalf("resolveArtifactSHA sha = %q, want empty on rejection", got)
			}
			if computed {
				t.Fatalf("resolveArtifactSHA computed = true, want false on rejection")
			}
		})
	}
}

// TestResolveArtifactSHARejectsMalformedSidecarSha256 checks that the same three
// malformed shapes, this time arriving via artifactMeta["sha256"] - the
// cache-sidecar branch - are rejected identically.
func TestResolveArtifactSHARejectsMalformedSidecarSha256(t *testing.T) {
	t.Parallel()
	path := mustWriteTarball(t, []byte("irrelevant tarball bytes"))

	cases := []struct {
		name string
		sha  string
	}{
		{"traversal", traversalArtifactSHA},
		{"short but hex", "deadbeef"},
		{"uppercase", strings.ToUpper(validMarkerSHA)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			artifactMeta := map[string]string{"sha256": tc.sha}

			got, computed, err := resolveArtifactSHA(path, nil, artifactMeta, "", "")
			if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
				t.Fatalf("resolveArtifactSHA error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
			}
			if got != "" {
				t.Fatalf("resolveArtifactSHA sha = %q, want empty on rejection", got)
			}
			if computed {
				t.Fatalf("resolveArtifactSHA computed = true, want false on rejection")
			}
		})
	}
}

// TestResolveArtifactSHATrustsOwnComputationUnvalidated pins that a non-empty
// artifactSHA, this process's own digest, is returned as given, even when not
// hex, before any recorded value is looked at.
func TestResolveArtifactSHATrustsOwnComputationUnvalidated(t *testing.T) {
	t.Parallel()
	path := mustWriteTarball(t, []byte("irrelevant tarball bytes"))

	meta := &types.GalaxyCollectionVersionInfo{}
	meta.Artifact.Sha256 = traversalArtifactSHA // must never be reached: artifactSHA wins first.

	const notActuallyHex = "this-is-not-hex-at-all-but-must-pass-through-unvalidated"
	got, computed, err := resolveArtifactSHA(path, meta, nil, notActuallyHex, "")
	if err != nil {
		t.Fatalf("resolveArtifactSHA error = %v, want nil (artifactSHA is never validated)", err)
	}
	if got != notActuallyHex {
		t.Fatalf("resolveArtifactSHA sha = %q, want %q returned unvalidated", got, notActuallyHex)
	}
	if !computed {
		t.Fatalf("resolveArtifactSHA computed = false, want true (artifactSHA is this process's own digest)")
	}
}

// requireResolvedSHA fails unless resolveArtifactSHA returned exactly the
// expected digest and provenance. arm names the precedence arm the
// expectation pins, so a failure identifies the arm rather than a bare bool.
func requireResolvedSHA(t *testing.T, got string, computed bool, err error, wantSHA string, wantComputed bool, arm string) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveArtifactSHA error = %v (%s)", err, arm)
	}
	if got != wantSHA {
		t.Fatalf("resolveArtifactSHA sha = %q, want %q (%s)", got, wantSHA, arm)
	}
	if computed != wantComputed {
		t.Fatalf("resolveArtifactSHA computed = %v, want %v (%s)", computed, wantComputed, arm)
	}
}

// TestResolveArtifactSHAPrecedence checks the arms with well-formed values:
// artifactSHA wins without reading the file, a pin forces a file hash, then
// meta, then the sidecar, and the fallback hashes the file.
func TestResolveArtifactSHAPrecedence(t *testing.T) {
	t.Parallel()
	content := []byte("real tarball bytes resolveArtifactSHA must hash on the pin and fallback arms")
	path := mustWriteTarball(t, content)
	realHash := sha256Hex(content)

	t.Run("pin forces a real file hash", func(t *testing.T) {
		t.Parallel()
		meta := &types.GalaxyCollectionVersionInfo{}
		meta.Artifact.Sha256 = validMarkerSHA // must be ignored: a pin always re-hashes the file.
		got, computed, err := resolveArtifactSHA(path, meta, map[string]string{"sha256": validMarkerSHA}, "", realHash)
		requireResolvedSHA(t, got, computed, err, realHash, true, "the pin arm hashes the file itself")
	})

	t.Run("process-computed sha wins over pin without re-reading the file", func(t *testing.T) {
		t.Parallel()
		// The path names no file, so a pin-arm file hash would fail the call:
		// a non-empty artifactSHA must win before the file is read.
		missing := filepath.Join(t.TempDir(), "never-written.tar.gz")
		got, computed, err := resolveArtifactSHA(missing, nil, nil, validMarkerSHA, realHash)
		requireResolvedSHA(t, got, computed, err, validMarkerSHA, true,
			"a process-computed sha must win without the pin arm re-reading the file")
	})

	t.Run("well-formed meta hit is used directly", func(t *testing.T) {
		t.Parallel()
		meta := &types.GalaxyCollectionVersionInfo{}
		meta.Artifact.Sha256 = validMarkerSHA
		got, computed, err := resolveArtifactSHA(path, meta, nil, "", "")
		requireResolvedSHA(t, got, computed, err, validMarkerSHA, false,
			"a meta hit is a recorded value, not our hash")
	})

	t.Run("well-formed sidecar hit is used when meta is absent", func(t *testing.T) {
		t.Parallel()
		got, computed, err := resolveArtifactSHA(path, nil, map[string]string{"sha256": validMarkerSHA}, "", "")
		requireResolvedSHA(t, got, computed, err, validMarkerSHA, false,
			"a sidecar hit is a recorded value, not our hash")
	})

	t.Run("fallback hashes the file when nothing else is available", func(t *testing.T) {
		t.Parallel()
		got, computed, err := resolveArtifactSHA(path, nil, nil, "", "")
		requireResolvedSHA(t, got, computed, err, realHash, true, "the fallback arm hashes the file itself")
	})
}

// TestPayloadFromPrefetchedMarksSHAComputed proves a prefetch handoff's
// streamed hash keeps artifactSHAComputed, so ingest skips re-hashing; the
// temp path names no file, so any re-read fails loudly.
func TestPayloadFromPrefetchedMarksSHAComputed(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	prefetched := downloadResult{
		Path: filepath.Join(t.TempDir(), "never-written.tar.gz"),
		SHA:  validMarkerSHA,
	}

	payload, servedFromCache, err := payloadFromPrefetched(col, nil, prefetched)
	if err != nil {
		t.Fatalf("payloadFromPrefetched error = %v", err)
	}
	if servedFromCache {
		t.Fatalf("servedFromCache = true, want false (fresh origin bytes, not a cache hit)")
	}
	if payload.artifactSHA != validMarkerSHA {
		t.Fatalf("payload.artifactSHA = %q, want %q", payload.artifactSHA, validMarkerSHA)
	}
	if !payload.artifactSHAComputed {
		t.Fatalf("payload.artifactSHAComputed = false, want true (the handoff sha was hashed from the downloaded stream)")
	}
}
