package collections

// Tests that a snapshot whose Resolved bucket carries "*" installs nothing:
// snapshot replay refuses only an empty version, so buildCollectionsMap's
// helpers.IsExactVersion guard is what stops it.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// poisonedVersionFixture builds a cold cache directory, a fakegalaxy server
// with acme.widgets@1.0.0 registered, and a requirements.yml requiring
// acme.widgets unpinned.
func poisonedVersionFixture(t *testing.T) (*config.Config, *fakegalaxy.Server) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
	}
	return cfg, srv
}

// seedResolvedSnapshot persists a store pinning acme.widgets at the
// unvalidated version, with the requirements hash cfg would compute, so a
// later Start against cfg takes the snapshot-reuse path.
func seedResolvedSnapshot(t *testing.T, runtime *infra.Infra, cfg *config.Config, version string) {
	t.Helper()
	roots, _, err := loadRoots(cfg, runtime)
	if err != nil {
		t.Fatalf("loadRoots: %v", err)
	}
	reqSpec := buildRequirementsSpec(roots)
	reqHash := requirementsSignatureFromSpec(reqSpec, cfg.NoDeps, serversSignature(cfg))

	st := store.New()
	st.SetResolvedAll(map[string]store.ResolvedEntry{
		"acme.widgets": {Version: version, Source: cfg.Server},
	})
	st.SetGraphSnapshot(map[string][]string{"acme.widgets@" + version: {}})
	st.SetMetaRequirements(reqHash, cfg.Server)
	st.SetRequirements(reqSpec)

	ctx := context.Background()
	backend := local.New(cfg.CacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()
	if err := backend.SaveStore(ctx, st); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}
}

// widgetsManifestPath returns where a real acme.widgets install would land
// its MANIFEST.json under cfg.DownloadPath.
func widgetsManifestPath(cfg *config.Config) string {
	return filepath.Join(cfg.DownloadPath, "ansible_collections", "acme", "widgets", "MANIFEST.json")
}

// TestPoisonedResolvedSnapshotVersionRejectsInstall pins that Start fails a
// "*" snapshot version with helpers.ErrInvalidCollectionVersion before any
// worker runs, not joined behind ErrInstallationFailed, and installs nothing.
func TestPoisonedResolvedSnapshotVersionRejectsInstall(t *testing.T) {
	t.Parallel()
	cfg, srv := poisonedVersionFixture(t)
	runtime := infra.New(noopPrinter{}, srv.Client())
	seedResolvedSnapshot(t, runtime, cfg, "*")

	err := Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a poisoned resolved snapshot version, got nil")
	}
	if !errors.Is(err, helpers.ErrInvalidCollectionVersion) {
		t.Fatalf("Start error = %v, want errors.Is helpers.ErrInvalidCollectionVersion", err)
	}
	if errors.Is(err, helpers.ErrInstallationFailed) {
		t.Errorf("Start error = %v, unexpectedly joined behind helpers.ErrInstallationFailed: "+
			"this sentinel is raised while building the resolved identity set, before any "+
			"per-collection worker starts", err)
	}
	if _, statErr := os.Stat(widgetsManifestPath(cfg)); !os.IsNotExist(statErr) {
		t.Fatalf("expected nothing installed, manifest stat err = %v", statErr)
	}
}

// TestPoisonedResolvedSnapshotVersionAcceptsInstall is the positive control:
// the same seeding with an exact version installs through snapshot reuse.
func TestPoisonedResolvedSnapshotVersionAcceptsInstall(t *testing.T) {
	t.Parallel()
	cfg, srv := poisonedVersionFixture(t)
	runtime := infra.New(noopPrinter{}, srv.Client())
	seedResolvedSnapshot(t, runtime, cfg, testVersion100)

	if err := Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, statErr := os.Stat(widgetsManifestPath(cfg)); statErr != nil {
		t.Fatalf("expected acme.widgets installed, manifest stat err = %v", statErr)
	}
}
