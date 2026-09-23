package collections

// These tests pin warmOne's corruption recovery through prepareWithRecovery:
// a cached tarball not matching its sidecar sha is evicted and refetched once,
// and offline the mismatch surfaces without evicting the only local copy.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestWarmOneEvictsAndRefetchesCorruptCacheHit pins that a cache hit whose
// bytes do not hash to its sidecar sha is refetched exactly once, healing
// both the artifact cache and the extracted store.
func TestWarmOneEvictsAndRefetchesCorruptCacheHit(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	version := srv.AddVersion("acme", "widgets", "1.0.0", nil)

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	// Bytes that do not hash to version.SHA256, which the sidecar below still
	// claims.
	corruptBytes := []byte("not a gzip stream, despite what the sidecar claims")
	mustWriteFile(t, artifactPath, corruptBytes)
	sidecarPath := artifactPath + helpers.ArtifactSHASidecarSuffix
	mustWriteFile(t, sidecarPath, []byte(version.SHA256))

	cfg := &config.Config{
		Server:       srv.URL(),
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      false,
		NoCache:      false,
	}
	deps := newTestInstallDepsWithExtractStore(t, cfg)

	// No prefetch handoff: a cache hit is never a prefetch task.
	if err := warmOne(context.Background(), deps, col, nil, downloadResult{}); err != nil {
		t.Fatalf("expected the corrupt cache hit to recover via a single refetch, got %v", err)
	}
	if got := srv.Count(fakegalaxy.EndpointArtifact); got != 1 {
		t.Fatalf("EndpointArtifact count = %d, want 1 (a single bounded refetch)", got)
	}

	assertExists(t, filepath.Join(cacheDir, extracted.RootDirName, version.SHA256, extracted.ReadyMarker))
	assertFileSHA256(t, artifactPath, version.SHA256)
	assertFileContent(t, sidecarPath, version.SHA256)
}

// TestWarmOneOfflineCorruptSurfacesMismatch pins that offline, a corrupt
// cache hit fails with helpers.ErrSHA256Mismatch and leaves the tarball and
// its sidecar in place, since nothing could refetch them.
func TestWarmOneOfflineCorruptSurfacesMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "gremlins", Version: "1.0.0"}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	corruptBytes := []byte("still not a gzip stream, and the sidecar sha does not match either")
	mustWriteFile(t, artifactPath, corruptBytes)
	sidecarSHA := sha256Hex([]byte("bytes the cached tarball does not actually contain"))
	sidecarPath := artifactPath + helpers.ArtifactSHASidecarSuffix
	mustWriteFile(t, sidecarPath, []byte(sidecarSHA))

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      true,
		NoCache:      false,
	}
	deps := newTestInstallDepsWithExtractStore(t, cfg)

	err := warmOne(context.Background(), deps, col, nil, downloadResult{})
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is ErrSHA256Mismatch, got %v", err)
	}

	// No eviction: the offline no-refetch rule must leave the only local copy
	// (tarball and sidecar) untouched.
	assertExists(t, artifactPath)
	assertExists(t, sidecarPath)
}
