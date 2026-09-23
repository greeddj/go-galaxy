package collections_test

// These e2e tests cover a non-pinned cache hit: a warm reinstall is served
// from the extracted tree and records the sidecar's digest, never a hash of
// the tarball bytes on disk.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// corruptedTarballContent overwrites a cached tarball. It is not a gzip
// stream, so extracting it fails and re-hashing it records a different digest.
const corruptedTarballContent = "this is not a valid tar.gz artifact - the cached tarball was corrupted on disk"

// acmeArtifactFilename returns the cache filename of an "acme" collection
// tarball, mirroring the unexported artifactKey so this external package can
// reach the cached file.
func acmeArtifactFilename(name, version string) string {
	return url.QueryEscape(fmt.Sprintf("acme-%s-%s.tar.gz", name, version))
}

// loadInstalledEntry returns the installed entry the persisted snapshot
// records for key. It takes and releases the backend lock, so call it only
// after every collections.Start on that cache has returned.
func loadInstalledEntry(t *testing.T, cfg *config.Config, runtime *infra.Infra, key string) store.InstalledEntry {
	t.Helper()
	ctx := context.Background()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("cacheBackend.New: %v", err)
	}
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() {
		_ = backend.Close(ctx)
	}()
	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	entry, ok := st.GetInstalled(key)
	if !ok {
		t.Fatalf("no installed entry recorded for %s", key)
	}
	return entry
}

// TestWarmInstallCacheHitDoesNotRehashCorruptedTarball proves an unpinned
// reinstall over a corrupted cached tarball succeeds from the warm extracted
// tree, with no network, and records the original sidecar sha256.
func TestWarmInstallCacheHitDoesNotRehashCorruptedTarball(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (populate the cache, sidecar, and extracted store): %v", err)
	}

	tarballPath := filepath.Join(f.cfg.CacheDir, acmeArtifactFilename("lib", "1.0.0"))
	if err := os.WriteFile(tarballPath, []byte(corruptedTarballContent), helpers.FileMod); err != nil {
		t.Fatalf("corrupt the cached tarball: %v", err)
	}

	f.server.ResetCounts()
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("wipe the install workspace (and its .extract-done marker): %v", err)
	}

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("reinstall from the corrupted-tarball cache hit: %v", err)
	}

	assertManifestInstalled(t, f.downloadPath, "lib")
	if got := f.server.Total(); got != 0 {
		t.Errorf("Total() after the reinstall = %d, want 0 (served from the warm cache/extracted tree, not the network)", got)
	}

	entry := loadInstalledEntry(t, f.cfg, f.runtime, "acme.lib@1.0.0")
	if entry.ArtifactSHA256 != f.libV1.SHA256 {
		t.Fatalf(
			"recorded ArtifactSHA256 = %q, want the original sidecar sha %q (a re-hash of the corrupted tarball would never produce this value)",
			entry.ArtifactSHA256, f.libV1.SHA256,
		)
	}
}
