package collections

// Tests for isDestinationSideFailure: a symlinked namespace escaping the
// download path must fail the install without evicting the good cached
// artifact, since no refetch can repair a destination-side refusal.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// deleteCountingArtifacts wraps local.Artifacts and counts Delete calls, so
// both tests here observe evictions through the same mechanism.
type deleteCountingArtifacts struct {
	*local.Artifacts

	deleteCalls atomic.Int32
}

// Delete counts the call, then delegates to the real local store.
func (a *deleteCountingArtifacts) Delete(ctx context.Context, key string) error {
	a.deleteCalls.Add(1)
	return a.Artifacts.Delete(ctx, key)
}

// newSymlinkedNamespaceInstallDeps builds installDeps whose
// ansible_collections/<namespace> links out of the download path. A real server
// serves a wrongful refetch, so a regression shows only in the Delete count.
func newSymlinkedNamespaceInstallDeps(t *testing.T, col collection, cacheDir, serverURL string, httpClient *http.Client) installDeps {
	t.Helper()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	collectionsDir := filepath.Join(downloadPath, "ansible_collections")
	mustMkdirAll(t, collectionsDir)
	outside := filepath.Join(root, "outside")
	mustMkdirAll(t, outside)
	if err := os.Symlink(outside, filepath.Join(collectionsDir, col.Namespace)); err != nil {
		t.Fatalf("symlink ansible_collections/%s -> outside: %v", col.Namespace, err)
	}

	osRoot, err := os.OpenRoot(downloadPath)
	if err != nil {
		t.Fatalf("os.OpenRoot(%s): %v", downloadPath, err)
	}
	t.Cleanup(func() {
		_ = osRoot.Close()
	})

	cfg := &config.Config{Server: serverURL, CacheDir: cacheDir, DownloadPath: downloadPath, Workers: 1, NoDeps: true}
	runtime := infra.New(noopPrinter{}, httpClient)
	artifacts := &deleteCountingArtifacts{Artifacts: local.NewArtifacts(cacheDir)}
	return installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, store.New()),
		artifacts:      artifacts,
		root:           osRoot,
	}
}

// TestInstallCollectionNamespaceEscapeEvictsNothing pins that a cache-hit
// install failing only on a symlinked namespace never calls Delete; the error
// alone cannot show it, since a refetch would fail the same way.
func TestInstallCollectionNamespaceEscapeEvictsNothing(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	cacheDir := t.TempDir()
	deps := newSymlinkedNamespaceInstallDeps(t, col, cacheDir, srv.URL(), srv.Client())

	// The seeded bytes are never read, since RemoveAll refuses first; they
	// only make the first attempt a cache hit.
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	mustWriteFile(t, artifactPath, []byte("stand-in for a perfectly good cached artifact"))

	err := installCollection(context.Background(), col, deps, nil, nil, downloadResult{})
	if !errors.Is(err, helpers.ErrCollectionsPathEscape) {
		t.Fatalf("installCollection error = %v, want errors.Is helpers.ErrCollectionsPathEscape", err)
	}

	artifacts, ok := deps.artifacts.(*deleteCountingArtifacts)
	if !ok {
		t.Fatalf("deps.artifacts = %T, want *deleteCountingArtifacts", deps.artifacts)
	}
	if got := artifacts.deleteCalls.Load(); got != 0 {
		t.Fatalf("Delete calls = %d, want 0: a destination-side failure must never evict the cache-hit artifact", got)
	}
	// The artifact itself must still be there, corroborating the counter: no
	// tarball or sidecar was removed from the shared cache.
	assertExists(t, artifactPath)
}

// TestInstallCollectionCacheHitExtractFailureEvictsOneWithCountingArtifacts is
// the positive control: an artifact-side failure (a corrupt cached tarball)
// must count exactly one Delete, so the zero in the escape test is not vacuous.
func TestInstallCollectionCacheHitExtractFailureEvictsOneWithCountingArtifacts(t *testing.T) {
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
	mustMkdirAll(t, cacheDir)

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	// Not a valid gzip stream, despite what the sidecar below claims - this is
	// what makes extraction (an artifact-side failure) fail.
	mustWriteFile(t, artifactPath, []byte("not a gzip stream, despite what the sidecar claims"))
	mustWriteFile(t, artifactPath+helpers.ArtifactSHASidecarSuffix, []byte(correctSHA))

	meta := newVersionInfo(server.URL, "")
	meta.Artifact.Sha256 = correctSHA

	cfg := &config.Config{CacheDir: cacheDir, DownloadPath: downloadPath, Workers: 1, NoDeps: true}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	artifacts := &deleteCountingArtifacts{Artifacts: local.NewArtifacts(cacheDir)}
	deps := installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, store.New()),
		artifacts:      artifacts,
		extractStore:   extracted.NewStore(cacheDir),
		root:           newTestCollectionsRoot(t, downloadPath),
	}

	if err := installCollection(context.Background(), col, deps, nil, meta, downloadResult{}); err != nil {
		t.Fatalf("expected the corrupt cache hit to recover via a single refetch, got %v", err)
	}
	if got := artifacts.deleteCalls.Load(); got != 1 {
		t.Fatalf("Delete calls = %d, want exactly 1 (an artifact-side failure must still evict-and-refetch)", got)
	}
}
