package collections

// Tests for extractCollection's up-front artifactSHA guard and the .info
// sweep a collection extraction makes.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// TestExtractCollectionRefusesNonCanonicalSHABeforeDestroyingTree pins that a
// traversal artifactSHA is refused before the reset: the pre-existing tree
// survives, which writeExtractMarker's late guard alone would not ensure.
func TestExtractCollectionRefusesNonCanonicalSHABeforeDestroyingTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	installPath := filepath.Join(root, "ansible_collections", "acme", "widgets")
	preexisting := filepath.Join(installPath, "README.md")
	const preexistingContent = "# a real, previously installed tree\n"
	mustMkdirAll(t, installPath)
	mustWriteFile(t, preexisting, []byte(preexistingContent))

	tarPath := filepath.Join(root, "artifact.tar.gz")
	mustWriteFile(t, tarPath, buildMinimalTarGz(t))

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	target := newFlatInstallTarget(t, installPath)

	const traversalSHA = "../../../../../../home/ci/.ssh/authorized_keys"
	err := extractCollection(context.Background(), col, tarPath, target, runtime, nil, traversalSHA, false)
	if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
		t.Errorf("extractCollection error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
	}
	assertFileContent(t, preexisting, preexistingContent)
}

// TestResetCollectionInfoSweepsOnlyThisCollectionsVersions pins that the sweep
// removes only this collection's exact-version .info directories, keeps
// look-alikes and other collections', and recreates its own version's empty.
func TestResetCollectionInfoSweepsOnlyThisCollectionsVersions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &config.Config{DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)
	collectionsDir := filepath.Join(root, "ansible_collections")
	for _, dir := range []string{
		"acme.widgets-1.0.0.info", "acme.widgets-2.1.0.info", "acme.widgets-3.0.0-rc.1.info",
		"acme.widgets-notes.info", "acme.widgets_extra-1.0.0.info", "other.widgets-1.0.0.info",
	} {
		mustMkdirAll(t, filepath.Join(collectionsDir, dir))
		mustWriteFile(t, filepath.Join(collectionsDir, dir, galaxyYAMLFileName), []byte("seeded\n"))
	}

	if err := resetCollectionInfo(target); err != nil {
		t.Fatalf("resetCollectionInfo: %v", err)
	}

	for _, gone := range []string{"acme.widgets-2.1.0.info", "acme.widgets-3.0.0-rc.1.info"} {
		assertPathAbsent(t, filepath.Join(collectionsDir, gone))
	}
	for _, kept := range []string{"acme.widgets-notes.info", "acme.widgets_extra-1.0.0.info", "other.widgets-1.0.0.info"} {
		assertExists(t, filepath.Join(collectionsDir, kept, galaxyYAMLFileName))
	}
	entries, err := os.ReadDir(filepath.Join(collectionsDir, "acme.widgets-1.0.0.info"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("own .info directory = %v (%v), want it recreated empty", entries, err)
	}
}
