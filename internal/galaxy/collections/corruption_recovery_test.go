package collections

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/psvmcc/hub/pkg/types"
)

// newTestInstallDepsWithExtractStore is newTestInstallDeps plus an extracted
// store at cfg.CacheDir, for observing the recovery path's effect on it.
func newTestInstallDepsWithExtractStore(t *testing.T, cfg *config.Config) installDeps {
	t.Helper()
	deps := newTestInstallDeps(t, cfg)
	deps.extractStore = extracted.NewStore(cfg.CacheDir)
	return deps
}

// mustWriteFile writes data to path, failing the test on error. Used to seed
// fixture files (cached tarballs, sidecars) where any I/O failure means the
// test environment itself is broken, not the behavior under test.
func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// assertExists fails the test unless path exists.
func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

// assertFileContent fails the test unless the file at path has exactly want
// as its content.
func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	//nolint:gosec // path is built from this test's own temp dirs.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("unexpected content at %s: got %q, want %q", path, got, want)
	}
}

// assertFileSHA256 fails the test unless the file at path hashes to want.
func assertFileSHA256(t *testing.T, path, want string) {
	t.Helper()
	//nolint:gosec // path is built from this test's own temp dirs.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if got := sha256Hex(data); got != want {
		t.Fatalf("expected %s to hash to %s, got %s", path, want, got)
	}
}

// TestInstallCollectionCacheHitExtractFailureRefetchesOnce proves a cached
// tarball whose recorded sha is trusted but which fails to extract is evicted
// and refetched once, healing the artifact cache and the extracted store.
func TestInstallCollectionCacheHitExtractFailureRefetchesOnce(t *testing.T) {
	t.Parallel()
	validTarGz := buildMinimalTarGz(t)
	correctSHA := sha256Hex(validTarGz)

	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(validTarGz)
	}))
	defer server.Close()

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	// Not a valid gzip stream: this is what makes extraction fail even though
	// the sidecar and metadata both (falsely) claim correctSHA.
	corruptBytes := []byte("not a gzip stream, despite what the sidecar claims")
	mustWriteFile(t, artifactPath, corruptBytes)
	sidecarPath := artifactPath + helpers.ArtifactSHASidecarSuffix
	mustWriteFile(t, sidecarPath, []byte(correctSHA))

	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: server.URL}
	meta.Artifact.Sha256 = correctSHA

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      false,
		NoCache:      false,
	}
	deps := newTestInstallDepsWithExtractStore(t, cfg)

	if err := installCollection(context.Background(), col, deps, nil, meta, downloadResult{}); err != nil {
		t.Fatalf("expected the corrupt cache hit to recover via a single refetch, got %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected exactly 1 server hit, got %d", got)
	}

	installPath := filepath.Join(downloadPath, "ansible_collections", col.Namespace, col.Name)
	assertFileContent(t, filepath.Join(installPath, "README.md"), "# widgets\n")
	assertExists(t, filepath.Join(cacheDir, extracted.RootDirName, correctSHA, extracted.ReadyMarker))
	assertFileSHA256(t, artifactPath, correctSHA)
	assertFileContent(t, sidecarPath, correctSHA)
	assertExists(t, collectionMarkerPath(installPath, col, correctSHA))
}

// TestInstallCollectionCacheHitExtractFailureRefetchOnceThenFails proves the
// refetch is bounded to one when the new bytes, a well-formed archive whose
// entry escapes (shapeless bytes would fail earlier), fail extraction too.
func TestInstallCollectionCacheHitExtractFailureRefetchOnceThenFails(t *testing.T) {
	t.Parallel()
	corruptBytes := buildEscapingTarGz(t)
	corruptSHA := sha256Hex(corruptBytes)

	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(corruptBytes)
	}))
	defer server.Close()

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "gremlins", Version: "1.0.0"}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	mustWriteFile(t, artifactPath, corruptBytes)
	sidecarPath := artifactPath + helpers.ArtifactSHASidecarSuffix
	mustWriteFile(t, sidecarPath, []byte(corruptSHA))

	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: server.URL}
	meta.Artifact.Sha256 = corruptSHA

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      false,
		NoCache:      false,
	}
	// No extracted store here: the extraction failure and its cleanup must
	// come from the direct archive.ExtractTarGz path, not the CAS path.
	deps := newTestInstallDeps(t, cfg)

	err := installCollection(context.Background(), col, deps, nil, meta, downloadResult{})
	if err == nil {
		t.Fatalf("expected the refetch to also fail extraction, got nil")
	}
	if !strings.Contains(err.Error(), "failed to extract") {
		t.Fatalf("expected a wrapped extract failure, got %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected exactly 1 server hit (a single bounded refetch), got %d", got)
	}
}

// TestInstallCollectionCacheHitExtractFailureCountsOneHitAndOneMiss pins that
// an evict-and-refetch recovery counts exactly one cache hit, the corrupt
// cached tarball, and one cache miss, the replacement download.
func TestInstallCollectionCacheHitExtractFailureCountsOneHitAndOneMiss(t *testing.T) {
	t.Parallel()
	validTarGz := buildMinimalTarGz(t)
	correctSHA := sha256Hex(validTarGz)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(validTarGz)
	}))
	defer server.Close()

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "gizmos", Version: "1.0.0"}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	// Not a valid gzip stream, despite what the sidecar claims: the cache-hit
	// extraction fails and triggers the evict-and-refetch recovery path.
	corruptBytes := []byte("not a gzip stream, despite what the sidecar claims")
	mustWriteFile(t, artifactPath, corruptBytes)
	sidecarPath := artifactPath + helpers.ArtifactSHASidecarSuffix
	mustWriteFile(t, sidecarPath, []byte(correctSHA))

	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: server.URL}
	meta.Artifact.Sha256 = correctSHA

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      false,
		NoCache:      false,
	}
	deps := newTestInstallDepsWithExtractStore(t, cfg)

	if err := installCollection(context.Background(), col, deps, nil, meta, downloadResult{}); err != nil {
		t.Fatalf("expected the corrupt cache hit to recover via a single refetch, got %v", err)
	}

	totals := deps.runtime.Metrics.Totals()
	if totals.CacheHits != 1 {
		t.Errorf("CacheHits = %d, want 1 (the corrupt cache-resident artifact was served once before eviction)", totals.CacheHits)
	}
	if totals.CacheMisses != 1 {
		t.Errorf("CacheMisses = %d, want 1 (the forced refetch after eviction)", totals.CacheMisses)
	}
}

// TestInstallCollectionCacheHitPinMismatchRefetchesOnce proves a cached
// tarball that drifted from its lockfile pin is evicted and refetched once,
// and the matching refetched bytes install.
func TestInstallCollectionCacheHitPinMismatchRefetchesOnce(t *testing.T) {
	t.Parallel()
	validTarGz := buildMinimalTarGz(t)
	correctSHA := sha256Hex(validTarGz)

	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(validTarGz)
	}))
	defer server.Close()

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "sprockets", Version: "1.0.0", SHA256: correctSHA}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	drifted := []byte("drifted cached bytes that do not hash to the pin")
	mustWriteFile(t, artifactPath, drifted)
	if driftedSHA := sha256Hex(drifted); driftedSHA == correctSHA {
		t.Fatalf("test setup bug: drifted bytes accidentally match the pin")
	}

	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: server.URL}
	meta.Artifact.Sha256 = correctSHA

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      false,
		NoCache:      false,
	}
	deps := newTestInstallDeps(t, cfg)

	if err := installCollection(context.Background(), col, deps, nil, meta, downloadResult{}); err != nil {
		t.Fatalf("expected the pin-mismatch cache hit to recover via a single refetch, got %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected exactly 1 server hit, got %d", got)
	}

	installPath := filepath.Join(downloadPath, "ansible_collections", col.Namespace, col.Name)
	assertFileContent(t, filepath.Join(installPath, "README.md"), "# widgets\n")
	assertFileSHA256(t, artifactPath, correctSHA)
	assertFileContent(t, artifactPath+helpers.ArtifactSHASidecarSuffix, correctSHA)
}
