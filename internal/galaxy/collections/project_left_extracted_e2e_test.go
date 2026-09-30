package collections_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/cleanup"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestCleanupFreesTheExtractedTreeOfAProjectThatLeft is a project deleted with
// the collections tree inside it: no scan finds acme.app again, so its extracted
// tree goes, while acme.lib, which a project still here installed, stays.
func TestCleanupFreesTheExtractedTreeOfAProjectThatLeft(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	root := filepath.Dir(f.cfg.RequirementsFile)
	gone, kept := filepath.Join(root, "gone"), filepath.Join(root, "kept")
	for _, project := range []struct{ dir, name string }{{gone, "acme.app"}, {kept, "acme.lib"}} {
		dir := project.dir
		if err := os.Mkdir(dir, helpers.DirMod); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		f.cfg.RequirementsFile = filepath.Join(dir, helpers.RequirementsYAMLName)
		f.cfg.DownloadPath = filepath.Join(dir, ".collections")
		writeRequirements(t, f.cfg.RequirementsFile, project.name)
		if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("install in %s: %v", dir, err)
		}
	}
	shas := loadStoreSnapshot(t, f.cfg, f.runtime).InstalledArtifactSHAByKey()
	appSHA, libSHA := shas["acme.app@"+testVersion100], shas["acme.lib@"+testVersion100]
	if appSHA == "" || libSHA == "" || appSHA == libSHA {
		t.Fatalf("install records = %v, want distinct shas for acme.app and acme.lib", shas)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("remove %s: %v", gone, err)
	}
	if err := cleanup.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("cleanup.Start: %v", err)
	}
	assertPathAbsent(t, filepath.Join(f.cfg.CacheDir, extracted.RootDirName, appSHA))
	assertExtractedStorePresent(t, f.cfg.CacheDir, libSHA)
	assertManifestInstalled(t, filepath.Join(kept, ".collections"), "lib")
}
