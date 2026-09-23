package collections

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// downloadShapeFixture wires the download arm that has no extracted store (the
// prefetcher's) to serve body as the artifact with no declared sha256, so
// verifyDownloadSHA compares nothing and only the shape probe judges the bytes.
func downloadShapeFixture(t *testing.T, body []byte) (installDeps, *types.GalaxyCollectionVersionInfo, string) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	cacheDir := t.TempDir()
	cfg := &config.Config{CacheDir: cacheDir, Workers: 1, NoDeps: true}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	deps := installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, store.New()),
		artifacts:      local.NewArtifacts(cacheDir),
	}

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: server.URL}
	return deps, meta, artifactKey(col)
}

// TestDownloadWithoutExtractStoreRejectsNonArchiveBytes pins that the shape
// probe refuses an error page with no declared sha and, the point of the
// test, keeps it out of the shared artifact cache slot.
func TestDownloadWithoutExtractStoreRejectsNonArchiveBytes(t *testing.T) {
	t.Parallel()
	deps, meta, key := downloadShapeFixture(t, []byte("<html>404</html>"))

	ctx := context.Background()
	_, err := downloadCollectionToCache(ctx, deps, key, "", meta, true)

	// Errorf, not Fatalf, so the cache assertion below is always reached.
	if !errors.Is(err, helpers.ErrArtifactNotTarGz) {
		t.Errorf("downloadCollectionToCache = %v, want errors.Is helpers.ErrArtifactNotTarGz", err)
	}

	cached, hasErr := deps.artifacts.Has(ctx, key)
	if hasErr != nil {
		t.Fatalf("artifacts.Has: %v", hasErr)
	}
	if cached {
		t.Fatalf("key %q entered the artifact cache despite not being an archive", key)
	}
}

// TestDownloadWithoutExtractStoreCommitsAValidArchive is the positive control:
// the same fixture serving a real tarball commits it to the cache.
func TestDownloadWithoutExtractStoreCommitsAValidArchive(t *testing.T) {
	t.Parallel()
	deps, meta, key := downloadShapeFixture(t, buildMinimalTarGz(t))

	ctx := context.Background()
	result, err := downloadCollectionToCache(ctx, deps, key, "", meta, true)
	if err != nil {
		t.Fatalf("downloadCollectionToCache = %v, want nil", err)
	}
	defer cleanupIfNeeded(result.Cleanup)

	cached, hasErr := deps.artifacts.Has(ctx, key)
	if hasErr != nil {
		t.Fatalf("artifacts.Has: %v", hasErr)
	}
	if !cached {
		t.Fatalf("key %q did not enter the artifact cache", key)
	}
}
