package collections

// The S3 store reports a corrupt object as a sha256 mismatch from Fetch, which
// prepareWithRecovery evicts and refetches once. The S3 fake is unexported in
// internal/cache/s3, so a stub store stands in for it here.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// fetchOnceMismatchArtifacts wraps a real local.Artifacts as a stand-in for
// the S3 store: its first Fetch fails with helpers.ErrSHA256Mismatch like a
// corrupt read, and Delete is counted so a test can assert a single eviction.
type fetchOnceMismatchArtifacts struct {
	*local.Artifacts

	fetchCalls  atomic.Int32
	deleteCalls atomic.Int32
}

// Fetch fails with a wrapped helpers.ErrSHA256Mismatch on its first call,
// simulating a corrupt cache-resident object; every later call delegates to
// the real local store.
func (a *fetchOnceMismatchArtifacts) Fetch(ctx context.Context, key string) (cacheManager.ArtifactFile, error) {
	if a.fetchCalls.Add(1) == 1 {
		return cacheManager.ArtifactFile{}, fmt.Errorf("simulated corrupt s3 object: %w", helpers.ErrSHA256Mismatch)
	}
	return a.Artifacts.Fetch(ctx, key)
}

// Delete counts every call before delegating to the real local store, so a
// test can assert the recovery arm evicted exactly once.
func (a *fetchOnceMismatchArtifacts) Delete(ctx context.Context, key string) error {
	a.deleteCalls.Add(1)
	return a.Artifacts.Delete(ctx, key)
}

// TestInstallCollectionS3CacheFetchMismatchEvictsAndRefetches pins that a
// cache-resident artifact failing its sha256 check on Fetch is evicted and
// refetched from the origin exactly once, healing the cache and the install.
func TestInstallCollectionS3CacheFetchMismatchEvictsAndRefetches(t *testing.T) {
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
	// The seeded content is irrelevant: Fetch is stubbed to fail on its first
	// call regardless of what is on disk. Only Has() needs to report the key
	// present, so prepareInstall takes the cache-hit path in the first place.
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	mustWriteFile(t, artifactPath, []byte("stand-in for a corrupt cache-resident object"))

	cfg := &config.Config{
		Server:       srv.URL(),
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      false,
		NoCache:      false,
	}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	st := store.New()
	artifacts := &fetchOnceMismatchArtifacts{Artifacts: local.NewArtifacts(cacheDir)}
	deps := installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
		root:           newTestCollectionsRoot(t, downloadPath),
	}

	if err := installCollection(context.Background(), col, deps, nil, nil, downloadResult{}); err != nil {
		t.Fatalf("expected the corrupt cache-resident artifact to recover via a single refetch, got %v", err)
	}
	if got := srv.Count(fakegalaxy.EndpointArtifact); got != 1 {
		t.Fatalf("EndpointArtifact count = %d, want 1 (a single bounded refetch)", got)
	}
	if got := artifacts.deleteCalls.Load(); got != 1 {
		t.Fatalf("Delete calls = %d, want exactly 1", got)
	}

	installPathDir := filepath.Join(downloadPath, "ansible_collections", col.Namespace, col.Name)
	assertFileContent(t, filepath.Join(installPathDir, "README.md"), "# acme.widgets\n")
	assertFileSHA256(t, artifactPath, version.SHA256)

	// A hit counts only when Fetch returns nil, so a read-time failure is 0 hits
	// and 1 miss, unlike the extraction failure pinned by
	// TestInstallCollectionCacheHitExtractFailureCountsOneHitAndOneMiss.
	totals := runtime.Metrics.Totals()
	if totals.CacheHits != 0 {
		t.Errorf("CacheHits = %d, want 0 (Fetch failed before serving any bytes, so it can never register a hit)", totals.CacheHits)
	}
	if totals.CacheMisses != 1 {
		t.Errorf("CacheMisses = %d, want 1 (the forced refetch after the read-time integrity failure)", totals.CacheMisses)
	}
}
