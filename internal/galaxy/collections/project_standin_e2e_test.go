package collections_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/cleanup"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestCleanupFollowsAProjectMovingToGalaxyTOML is requirements.yml deleted
// before any run recorded the galaxy.toml replacing it: the galaxy.toml stands
// in, and once it is gone too, cleanup refuses and removes nothing.
func TestCleanupFollowsAProjectMovingToGalaxyTOML(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	dir := filepath.Dir(f.cfg.RequirementsFile)
	toml := filepath.Join(dir, helpers.RequirementsTOMLName)
	if err := os.WriteFile(toml, []byte("[project]\ncollections = [\"acme.app\"]\n"), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", toml, err)
	}
	if err := os.Remove(f.cfg.RequirementsFile); err != nil {
		t.Fatalf("remove requirements.yml: %v", err)
	}
	if err := cleanup.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("cleanup.Start with galaxy.toml standing in: %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")

	if err := os.Remove(toml); err != nil {
		t.Fatalf("remove galaxy.toml: %v", err)
	}
	if err := cleanup.Start(context.Background(), f.cfg, f.runtime); !errors.Is(err, helpers.ErrProjectRequirementsMissing) {
		t.Fatalf("cleanup.Start with nothing left = %v, want ErrProjectRequirementsMissing", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
}

// TestCleanupPrunesAVanishedProjectFromASharedTree is the way a project leaves:
// its directory is deleted, and what it installed into a collections path
// outside it goes, since nothing is left to keep it.
func TestCleanupPrunesAVanishedProjectFromASharedTree(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	projectDir := filepath.Join(filepath.Dir(f.cfg.RequirementsFile), "project")
	if err := os.Mkdir(projectDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", projectDir, err)
	}
	f.cfg.RequirementsFile = filepath.Join(projectDir, helpers.RequirementsYAMLName)
	writeRequirements(t, f.cfg.RequirementsFile, "acme.app")
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := os.RemoveAll(projectDir); err != nil {
		t.Fatalf("remove %s: %v", projectDir, err)
	}
	if err := cleanup.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("cleanup.Start: %v", err)
	}
	assertPathAbsent(t, manifestPathFor(f.downloadPath, "app"))
	assertPathAbsent(t, manifestPathFor(f.downloadPath, "lib"))
}
