package collections

// These tests pin the wiring of --clear-cache inside initInstall
// (clearCacheIfRequested, Store.ClearCaches, Backend.ClearFiles) and its
// failure arm; the file-selection policy is tested in the store package.

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// Fixture keys seedClearCacheStore writes and
// TestInitInstallClearCacheWipesArtifactsAndMetadataCaches asserts against.
const (
	clearCacheAPIKey       = "acme.api@1.0.0"
	clearCacheDepsKey      = "acme.deps@1.0.0"
	clearCacheVersionsKey  = "acme.versions"
	clearCacheInstalledKey = "acme.installed@1.0.0"
	clearCacheWarmedKey    = "acme.warmed@1.0.0"
)

// seedClearCacheStore persists a snapshot holding both sides of the
// --clear-cache boundary: metadata caches it empties and installed and warmed
// entries it keeps. It returns the fixed sha256 those entries share.
func seedClearCacheStore(t *testing.T, cacheDir string) string {
	t.Helper()
	ctx := context.Background()
	sha := strings.Repeat("a", 64)

	seed := local.New(cacheDir)
	if err := seed.Open(ctx); err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	seedStore, err := seed.LoadStore(ctx)
	if err != nil {
		t.Fatalf("seed LoadStore: %v", err)
	}
	// A zero FetchedAt would be age-evicted at save time, letting the later
	// GetAPICache miss pass without --clear-cache doing anything.
	seedStore.SetAPICache(clearCacheAPIKey, store.APICacheEntry{
		FetchedAt: time.Now().UTC(),
		URL:       "https://example.test/api",
		Body:      []byte("{}"),
	})
	seedStore.SetDepsCache(clearCacheDepsKey, map[string]string{"acme.dep": "*"})
	seedStore.SetVersionsCache(clearCacheVersionsKey, []string{"1.0.0"})
	seedStore.SetInstalled(clearCacheInstalledKey, store.InstalledEntry{
		InstallPath:    filepath.Join("ansible_collections", "acme", "installed"),
		Source:         "https://example.test",
		ArtifactSHA256: sha,
		InstalledAt:    time.Now().UTC(),
	})
	seedStore.SetWarmed(clearCacheWarmedKey, sha)
	if err := seed.SaveStore(ctx, seedStore); err != nil {
		t.Fatalf("seed SaveStore: %v", err)
	}
	if err := seed.Close(ctx); err != nil {
		t.Fatalf("seed Close: %v", err)
	}
	return sha
}

// assertFileRemoved reports a t.Errorf naming label unless path no longer
// exists.
func assertFileRemoved(t *testing.T, label, path string) {
	t.Helper()
	if _, statErr := os.Stat(path); statErr == nil || !os.IsNotExist(statErr) {
		t.Errorf("%s survived, stat err = %v", label, statErr)
	}
}

// assertFileKept reports a t.Errorf naming label unless path still exists.
func assertFileKept(t *testing.T, label, path string) {
	t.Helper()
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("%s removed: %v", label, statErr)
	}
}

// assertCacheEvicted reports a t.Errorf naming label when ok is true, i.e.
// when a Get* lookup on a bucket --clear-cache is supposed to empty still
// found an entry.
func assertCacheEvicted(t *testing.T, label string, ok bool) {
	t.Helper()
	if ok {
		t.Errorf("%s survived --clear-cache", label)
	}
}

// assertCacheKept reports a t.Errorf naming label when ok is false, i.e.
// when a Get* lookup on a bucket --clear-cache must never touch came back
// empty.
func assertCacheKept(t *testing.T, label string, ok bool) {
	t.Helper()
	if !ok {
		t.Errorf("%s was wiped by --clear-cache", label)
	}
}

// clearCacheWipeFixture bundles the config for a real --clear-cache run and
// the on-disk paths TestInitInstallClearCacheWipesArtifactsAndMetadataCaches
// checks afterwards.
type clearCacheWipeFixture struct {
	cfg          *config.Config
	cacheDir     string
	artifactPath string
	sidecarPath  string
	markerPath   string
}

// buildClearCacheWipeFixture seeds a persisted store, a cached artifact, its
// sha256 sidecar and a ready extracted tree under one cacheDir, and returns
// them with a real --clear-cache config.
func buildClearCacheWipeFixture(t *testing.T) clearCacheWipeFixture {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	sha := seedClearCacheStore(t, cacheDir)

	// A cached artifact and its sha256 sidecar, placed at the exact path the
	// real artifact store would use for this server/filename pair.
	artifactPath := filepath.Join(cacheDir, helpers.ArtifactKey("https://example.test", "acme-app-1.0.0.tar.gz"))
	mustWriteFile(t, artifactPath, []byte("tarball bytes"))
	sidecarPath := artifactPath + helpers.ArtifactSHASidecarSuffix
	mustWriteFile(t, sidecarPath, []byte(sha))

	// An extracted tree keyed by sha, the way extracted.Store.Ensure would
	// have left it, complete with its ready marker.
	extractedDir := filepath.Join(cacheDir, extracted.RootDirName, sha)
	if err := os.MkdirAll(extractedDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir extractedDir: %v", err)
	}
	markerPath := filepath.Join(extractedDir, extracted.ReadyMarker)
	mustWriteFile(t, markerPath, []byte(extracted.ReadyMarkerPayload))

	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	return clearCacheWipeFixture{
		cacheDir:     cacheDir,
		artifactPath: artifactPath,
		sidecarPath:  sidecarPath,
		markerPath:   markerPath,
		cfg: &config.Config{
			CacheDir:         cacheDir,
			RequirementsFile: reqPath,
			DownloadPath:     filepath.Join(root, "install"),
			ClearCache:       true,
			Workers:          1,
		},
	}
}

// TestInitInstallClearCacheWipesArtifactsAndMetadataCaches proves a real
// --clear-cache empties the metadata caches and deletes cached artifacts and
// sidecars, keeping installed and warmed records, extracted trees and the db.
func TestInitInstallClearCacheWipesArtifactsAndMetadataCaches(t *testing.T) {
	t.Parallel()
	fx := buildClearCacheWipeFixture(t)

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	_, state, err := initInstall(context.Background(), fx.cfg, runtime)
	if err != nil {
		t.Fatalf("initInstall: %v", err)
	}
	t.Cleanup(func() {
		if state.release != nil {
			_ = state.release()
		}
		_ = state.backend.Close(context.Background())
	})

	// The assertions are independent, so each uses t.Errorf and one run
	// reports every broken one.
	assertFileRemoved(t, "artifact", fx.artifactPath)
	assertFileRemoved(t, "sidecar", fx.sidecarPath)
	assertFileKept(t, "bolt db", filepath.Join(fx.cacheDir, helpers.StoreDBLocal))
	assertFileKept(t, "extracted tree", fx.markerPath)

	_, apiOK := state.store.GetAPICache(clearCacheAPIKey)
	_, depsOK := state.store.GetDepsCache(clearCacheDepsKey)
	_, versionsOK := state.store.GetVersionsCache(clearCacheVersionsKey)
	_, installedOK := state.store.GetInstalled(clearCacheInstalledKey)
	assertCacheEvicted(t, "APICache", apiOK)
	assertCacheEvicted(t, "DepsCache", depsOK)
	assertCacheEvicted(t, "Versions", versionsOK)
	assertCacheKept(t, "Installed entry", installedOK)

	if warmed := state.store.WarmedArtifactSHAByKey(); len(warmed) != 1 {
		t.Errorf("Warmed set = %v, want 1 entry", warmed)
	}
	if printer.hasWarnContaining("--clear-cache") {
		t.Errorf("unexpected --clear-cache warning in a real (non-dry-run) run: %v", printer.warns)
	}
}

// clearCacheFixture seeds a store and one cached tarball for a real
// --clear-cache run. With readOnly, cacheDir is made read-only after the lock
// file exists, so Lock and LoadStore succeed and only ClearFiles fails.
func clearCacheFixture(t *testing.T, readOnly bool) (string, *config.Config) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	seedClearCacheStore(t, cacheDir)

	// Create the lock file before the chmod, or the read-only variant would
	// fail in Lock's O_CREATE instead of in ClearFiles.
	rel, err := store.AcquireLock(cacheDir)
	if err != nil {
		t.Fatalf("materialize lock file: %v", err)
	}
	if err := rel(); err != nil {
		t.Fatalf("release materializing lock: %v", err)
	}

	mustWriteFile(t, filepath.Join(cacheDir, "sentinel.acme-app-1.0.0.tar.gz"), []byte("cached bytes"))

	if readOnly {
		//nolint:gosec // G302: intentionally read-only (no write bit) to force ClearFiles's os.Remove to fail with a permission error.
		if err := os.Chmod(cacheDir, 0o555); err != nil {
			t.Fatalf("chmod cacheDir read-only: %v", err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(cacheDir, helpers.DirMod); err != nil {
				t.Errorf("restore cacheDir perms: %v", err)
			}
		})
	}

	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	cfg := &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		ClearCache:       true,
		Workers:          1,
	}
	return cacheDir, cfg
}

// TestInitInstallClearCacheFailureReleasesLock proves a ClearFiles failure
// in initInstall releases the cache lock and closes the backend, so it does
// not block later runs; the writable subtest is the positive control.
func TestInitInstallClearCacheFailureReleasesLock(t *testing.T) {
	t.Parallel()

	// The positive control: the shared fixture reaches and passes ClearFiles.
	t.Run("writable cache dir", func(t *testing.T) {
		t.Parallel()
		cacheDir, cfg := clearCacheFixture(t, false)
		printer := &capturingPrinter{}
		runtime := infra.New(printer, http.DefaultClient)

		_, state, err := initInstall(context.Background(), cfg, runtime)
		if err != nil {
			t.Fatalf("initInstall: %v", err)
		}
		t.Cleanup(func() {
			if state.release != nil {
				_ = state.release()
			}
			_ = state.backend.Close(context.Background())
		})

		tarballPath := filepath.Join(cacheDir, "sentinel.acme-app-1.0.0.tar.gz")
		if _, statErr := os.Stat(tarballPath); statErr == nil || !os.IsNotExist(statErr) {
			t.Errorf("expected the tarball to be removed by a real --clear-cache run, stat err = %v", statErr)
		}
	})

	t.Run("read-only cache dir", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root; permission-based removal guard cannot be tested")
		}
		t.Parallel()
		cacheDir, cfg := clearCacheFixture(t, true)
		runtime := infra.New(&capturingPrinter{}, http.DefaultClient)

		_, _, err := initInstall(context.Background(), cfg, runtime)
		if err == nil {
			t.Fatalf("expected initInstall to fail against a read-only cache dir")
		}
		// The failure arm must propagate the underlying cause; checked with
		// errors.Is so a later %w wrap still passes.
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("expected errors.Is(err, fs.ErrPermission), got %v", err)
		}

		// A fresh acquisition on the still read-only cacheDir must succeed,
		// proving the release itself, not the later chmod restore, freed it.
		rel, lockErr := store.AcquireLock(cacheDir)
		if lockErr != nil {
			t.Errorf("lock not released by initInstall: %v", lockErr)
			return
		}
		if err := rel(); err != nil {
			t.Errorf("release re-acquired lock: %v", err)
		}
	})
}
