package cleanup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// saveSeededStore saves one snapshot that seed fills, so collection and role
// records land in a single save, as one install writes them.
func saveSeededStore(t *testing.T, cfg *config.Config, seed func(st *store.Store)) {
	t.Helper()
	backend, err := cacheBackend.New(cfg, newTestRuntime())
	if err != nil {
		t.Fatalf("failed to build backend for seeding: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open backend for seeding: %v", err)
	}
	defer func() { _ = backend.Close(t.Context()) }()
	st := store.New()
	seed(st)
	if err := backend.SaveStore(t.Context(), st); err != nil {
		t.Fatalf("failed to save seeded store: %v", err)
	}
}

// writeRequirementsAt writes body as a requirements file in a new directory
// and returns its path, the anchor of a project that has not left.
func writeRequirementsAt(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), helpers.RequirementsYAMLName)
	if err := os.WriteFile(path, []byte(body), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// leftTreeCase is one install record no scan finds: where it says it was
// installed, whether a project still here has a tree no scan walked, and
// whether its extracted tree must stay.
type leftTreeCase struct {
	installPath func(unseen string) string
	name        string
	unseenLeft  bool
	unseen      bool
	wantKept    bool
}

func leftTreeCases() []leftTreeCase {
	relative := func(string) string { return ".collections/ansible_collections/gone/coll" }
	elsewhere := func(string) string { return "/nonexistent/left/.collections/ansible_collections/gone/coll" }
	inside := func(unseen string) string { return filepath.Join(unseen, "ansible_collections", "gone", "coll") }
	return []leftTreeCase{
		{name: "relative path, every tree walked", installPath: relative},
		{name: "relative path, a tree unseen", installPath: relative, unseen: true, wantKept: true},
		{name: "absolute path outside the unseen tree", installPath: elsewhere, unseen: true},
		{name: "absolute path inside the unseen tree", installPath: inside, unseen: true, wantKept: true},
		{name: "the unseen tree's project has left", installPath: relative, unseen: true, unseenLeft: true},
	}
}

// TestSweepFreesTheExtractedTreeNoProjectStillHereMayHold pins that a record
// no scan found keeps its extracted tree only while an unwalked tree of a
// project still here may hold it, and that a reached copy's tree always stays.
func TestSweepFreesTheExtractedTreeNoProjectStillHereMayHold(t *testing.T) {
	t.Parallel()
	for _, tc := range leftTreeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cacheDir := t.TempDir()
			cfg := &config.Config{CacheDir: cacheDir}
			walked := t.TempDir()
			seedInstallTree(t, walked)
			unseenTree := filepath.Join(t.TempDir(), ".collections")
			saveSeededStore(t, cfg, func(st *store.Store) {
				st.SetInstalled("ns.name@1.0.0", store.InstalledEntry{
					InstallPath: filepath.Join(walked, "ansible_collections", "ns", "name"), ArtifactSHA256: "sha-reached",
				})
				st.SetInstalled("gone.coll@1.0.0", store.InstalledEntry{InstallPath: tc.installPath(unseenTree), ArtifactSHA256: "sha-gone"})
			})
			seedExtractedDir(t, cacheDir, "sha-reached")
			seedExtractedDir(t, cacheDir, "sha-gone")
			projects := map[string]store.ProjectRecord{
				"walked": {RequirementsFile: writeRequirementsAt(t, "collections:\n  - ns.name\n"), CollectionsPath: walked, LastRun: time.Now().UTC()},
			}
			if tc.unseen {
				req := writeRequirementsAt(t, "collections: []\n")
				if tc.unseenLeft {
					req = filepath.Join(t.TempDir(), "gone", helpers.RequirementsYAMLName)
				}
				projects["unseen"] = store.ProjectRecord{RequirementsFile: req, CollectionsPath: unseenTree, LastRun: time.Now().UTC()}
			}
			writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{Projects: projects})

			if err := Start(t.Context(), cfg, newTestRuntime()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			assertExtractedDirsSurvive(t, cacheDir, "sha-reached")
			_, err := os.Stat(filepath.Join(cacheDir, extracted.RootDirName, "sha-gone"))
			if kept := err == nil; kept != tc.wantKept {
				t.Fatalf("extracted sha-gone kept = %v, want %v (stat error %v)", kept, tc.wantKept, err)
			}
		})
	}
}

// TestSweepFreesTheExtractedTreeOfARoleNoProjectStillHereMayHold pins the
// role half: a role record no scan found keeps its tree only while it lies
// in a recorded roles path, unopened, of a project still here.
func TestSweepFreesTheExtractedTreeOfARoleNoProjectStillHereMayHold(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	cfg := &config.Config{CacheDir: cacheDir}
	unopened := filepath.Join(t.TempDir(), ".roles")
	saveSeededStore(t, cfg, func(st *store.Store) {
		st.SetInstalledRole("left", store.InstalledRoleEntry{InstallPath: "/nonexistent/left/.roles/left", ArtifactSHA256: "sha-role-left"})
		st.SetInstalledRole("here", store.InstalledRoleEntry{InstallPath: filepath.Join(unopened, "here"), ArtifactSHA256: "sha-role-here"})
	})
	seedExtractedDir(t, cacheDir, "sha-role-left")
	seedExtractedDir(t, cacheDir, "sha-role-here")
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{Projects: map[string]store.ProjectRecord{
		"here": {RequirementsFile: writeRequirementsAt(t, "collections: []\n"), RolesPath: unopened, LastRun: time.Now().UTC()},
	}})

	if err := Start(t.Context(), cfg, newTestRuntime()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertExtractedDirsSurvive(t, cacheDir, "sha-role-here")
	if _, err := os.Stat(filepath.Join(cacheDir, extracted.RootDirName, "sha-role-left")); !os.IsNotExist(err) {
		t.Fatalf("expected the left role's extracted tree swept, stat error: %v", err)
	}
}
