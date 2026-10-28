package cleanup

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

const (
	recordedRoleSHA = "1111111111111111111111111111111111111111111111111111111111111111"
	keptRoleSHA     = "2222222222222222222222222222222222222222222222222222222222222222"
	recordedRoleSrc = "git+https://git.example/acme/web.git#@0123456789abcdef0123456789abcdef01234567"
)

// recordedRoleFixture is a project whose roles path holds web, which nothing
// reaches, and kept, which its requirements name, each with a snapshot record,
// an artifact and an extracted entry.
type recordedRoleFixture struct {
	cfg          *config.Config
	projectPath  string
	rolesPath    string
	artifact     string
	keptArtifact string
}

func seedRecordedRoleFixture(t *testing.T, dryRun bool) recordedRoleFixture {
	t.Helper()
	cacheDir := t.TempDir()
	f := recordedRoleFixture{cfg: &config.Config{CacheDir: cacheDir, DryRun: dryRun}, projectPath: t.TempDir()}
	f.rolesPath = filepath.Join(f.projectPath, "roles")
	seedMarkedRole(t, f.rolesPath, "web")
	seedMarkedRole(t, f.rolesPath, "kept")
	reqPath := filepath.Join(f.projectPath, "requirements.yml")
	reqBody := "roles:\n  - src: git+https://git.example/acme/kept.git\n    name: kept\n"
	if err := os.WriteFile(reqPath, []byte(reqBody), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements: %v", err)
	}
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{Projects: map[string]store.ProjectRecord{
		f.projectPath: {RequirementsFile: reqPath, RolesPath: f.rolesPath, LastRun: time.Now().UTC()},
	}})

	runtime := newTestRuntime()
	seedSnapshotInstalled(t, f.cfg, runtime, nil)
	resaveLoadedSnapshot(t, f.cfg, runtime, func(st *store.Store) {
		st.SetInstalledRole("web", store.InstalledRoleEntry{
			InstallPath: filepath.Join(f.rolesPath, "web"), Source: recordedRoleSrc, Version: "1.0.0", ArtifactSHA256: recordedRoleSHA,
		})
		st.SetInstalledRole("kept", store.InstalledRoleEntry{
			InstallPath: filepath.Join(f.rolesPath, "kept"), Source: recordedRoleSrc, Version: "2.0.0", ArtifactSHA256: keptRoleSHA,
		})
	})
	f.artifact = filepath.Join(cacheDir, helpers.ArtifactKey(recordedRoleSrc, helpers.RoleArtifactFilename("web", "1.0.0")))
	f.keptArtifact = filepath.Join(cacheDir, helpers.ArtifactKey(recordedRoleSrc, helpers.RoleArtifactFilename("kept", "2.0.0")))
	for _, p := range []string{f.artifact, f.keptArtifact} {
		if err := os.WriteFile(p, []byte("role-bytes"), helpers.FileMod); err != nil {
			t.Fatalf("failed to seed artifact %s: %v", p, err)
		}
	}
	seedExtractedDir(t, cacheDir, recordedRoleSHA)
	seedExtractedDir(t, cacheDir, keptRoleSHA)
	return f
}

// TestStartRemovesAnUnreachedRoleWithItsRecordAndArtifact pins that an
// unreached role with a snapshot record loses its directory, its artifact,
// its record and its extracted entry, while a reached role keeps all of them.
func TestStartRemovesAnUnreachedRoleWithItsRecordAndArtifact(t *testing.T) {
	t.Parallel()
	f := seedRecordedRoleFixture(t, false)
	printer := &recordingPrinter{}
	if err := Start(t.Context(), f.cfg, newTestRuntimeWith(printer)); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	assertRolesRemoved(t, f.projectPath, printer, []string{"roles/web"})
	assertPathsExist(t, filepath.Join(f.rolesPath, "kept"), f.keptArtifact, extractedEntry(f.cfg.CacheDir, keptRoleSHA))
	assertPathsGone(t, f.artifact, extractedEntry(f.cfg.CacheDir, recordedRoleSHA))
	st := reloadStoreThroughFreshBackend(t, f.cfg, newTestRuntime())
	if _, ok := st.GetInstalledRole("web"); ok {
		t.Error("the removed role's snapshot record survived")
	}
	if _, ok := st.GetInstalledRole("kept"); !ok {
		t.Error("the reached role's snapshot record was dropped")
	}
}

// TestStartDryRunPlansTheUnreachedRoleAndItsExtractedEntry pins the dry run
// over the same fixture: it reports web and web's extracted entry alone, and
// removes nothing, the snapshot record included.
func TestStartDryRunPlansTheUnreachedRoleAndItsExtractedEntry(t *testing.T) {
	t.Parallel()
	f := seedRecordedRoleFixture(t, true)
	printer := &recordingPrinter{}
	if err := Start(t.Context(), f.cfg, newTestRuntimeWith(printer)); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	want := []string{"Would remove role web", `Would sweep extracted "` + recordedRoleSHA + `"`}
	if !slices.Equal(printer.results, want) {
		t.Errorf("results = %q, want %q", printer.results, want)
	}
	assertPathsExist(t, filepath.Join(f.rolesPath, "web"), filepath.Join(f.rolesPath, "kept"), f.artifact, f.keptArtifact,
		extractedEntry(f.cfg.CacheDir, recordedRoleSHA), extractedEntry(f.cfg.CacheDir, keptRoleSHA))
	if _, ok := reloadStoreThroughFreshBackend(t, f.cfg, newTestRuntime()).GetInstalledRole("web"); !ok {
		t.Error("a dry run dropped the role's snapshot record")
	}
}

// extractedEntry is the extracted-store directory of sha under cacheDir.
func extractedEntry(cacheDir, sha string) string {
	return filepath.Join(cacheDir, "extracted", sha)
}

func assertPathsExist(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to be kept: %v", p, err)
		}
	}
}

func assertPathsGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected %s to be removed, stat error: %v", p, err)
		}
	}
}

// TestScanProjectRolesIndexesOnlyMarkedRoleDirectories pins what the scan
// takes from a roles path: a marked directory with a role install name, its
// dependencies read from meta/requirements.yml too, and nothing else.
func TestScanProjectRolesIndexesOnlyMarkedRoleDirectories(t *testing.T) {
	t.Parallel()
	rolesPath := seedMixedRolesPath(t)

	byName := rolesByName{}
	unseen, err := scanProjectRoles(&recordingPrinter{}, "proj", store.ProjectRecord{RolesPath: rolesPath}, nil, byName)
	if err != nil || unseen {
		t.Fatalf("scanProjectRoles = (%t, %v), want (false, nil)", unseen, err)
	}
	if names := slices.Sorted(maps.Keys(byName)); !slices.Equal(names, []string{"web"}) {
		t.Fatalf("indexed roles = %q, want web alone", names)
	}
	web := byName["web"][0]
	if web.ArtifactSHA != testRoleMarkerSHA || web.Source != "" || web.InstallPath != filepath.Join(rolesPath, "web") {
		t.Errorf("web = %+v, want the marker's sha, its path and no recorded source", web)
	}
	if want := []string{"owner.common", "owner.extra"}; !slices.Equal(web.Deps, want) {
		t.Errorf("web.Deps = %q, want %q from both meta files, the local role skipped", web.Deps, want)
	}
}

// seedMixedRolesPath writes the marked role web, with a meta/requirements.yml
// dependency, beside what the scan skips: a marked bad name, an unmarked
// directory and a file.
func seedMixedRolesPath(t *testing.T) string {
	t.Helper()
	rolesPath := t.TempDir()
	seedMarkedRole(t, rolesPath, "web", "owner.common")
	reqMeta := filepath.Join(rolesPath, "web", "meta", "requirements.yml")
	if err := os.WriteFile(reqMeta, []byte("- role: owner.extra\n- role: localonly\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write %s: %v", reqMeta, err)
	}
	seedMarkedRole(t, rolesPath, "-dash")
	if err := os.MkdirAll(filepath.Join(rolesPath, "unmarked", "meta"), helpers.DirMod); err != nil {
		t.Fatalf("failed to create the unmarked role: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rolesPath, "notes.txt"), []byte("not a role"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write notes.txt: %v", err)
	}
	return rolesPath
}

// TestScanProjectRolesReportsARolesPathItCannotOpen pins that a recorded
// roles path that is not a directory is warned about and counted unseen,
// while one that does not exist is unseen silently.
func TestScanProjectRolesReportsARolesPathItCannotOpen(t *testing.T) {
	t.Parallel()
	notDir := filepath.Join(t.TempDir(), "roles")
	if err := os.WriteFile(notDir, []byte("a file"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write %s: %v", notDir, err)
	}
	printer := &recordingPrinter{}
	unseen, err := scanProjectRoles(printer, "proj", store.ProjectRecord{RolesPath: notDir}, nil, rolesByName{})
	if err != nil || !unseen {
		t.Fatalf("scanProjectRoles over a file = (%t, %v), want (true, nil)", unseen, err)
	}
	if !printer.hasWarningContaining(`skipping roles of project "proj"`) {
		t.Errorf("warnings = %q, want one naming the project", printer.warnings)
	}

	quiet := &recordingPrinter{}
	missing := store.ProjectRecord{RolesPath: filepath.Join(t.TempDir(), "absent")}
	if unseen, err := scanProjectRoles(quiet, "proj", missing, nil, rolesByName{}); err != nil || !unseen {
		t.Fatalf("scanProjectRoles over a missing path = (%t, %v), want (true, nil)", unseen, err)
	}
	if len(quiet.warnings) != 0 {
		t.Errorf("warnings = %q, want none for a roles path that does not exist", quiet.warnings)
	}
}

// TestRemoveUnusedRolesStopsAtTheFirstRefusal pins removeUnusedRoles' stops:
// a canceled context before any removal, and a name that is no role install
// name, both leaving every directory in place.
func TestRemoveUnusedRolesStopsAtTheFirstRefusal(t *testing.T) {
	t.Parallel()
	rolesPath := t.TempDir()
	seedMarkedRole(t, rolesPath, "web")
	cfg := &config.Config{CacheDir: t.TempDir()}
	backend := openTestBackend(t, cfg, newTestRuntime())
	byName := rolesByName{"web": {{Name: "web", InstallPath: filepath.Join(rolesPath, "web"), RolesDir: rolesPath}}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	removed, err := removeUnusedRoles(ctx, cfg, newTestRuntime(), backend, nil, map[string]bool{}, byName)
	if !errors.Is(err, context.Canceled) || removed != 0 || !strings.Contains(err.Error(), "cleanup stopped at role web") {
		t.Errorf("removeUnusedRoles(canceled) = (%d, %v), want (0, context.Canceled naming web)", removed, err)
	}

	byName["../escape"] = []installedRole{{Name: "../escape", RolesDir: rolesPath}}
	_, err = removeUnusedRoles(t.Context(), cfg, newTestRuntime(), backend, nil, map[string]bool{"web": true}, byName)
	if !errors.Is(err, helpers.ErrUnsafeRemovalPath) {
		t.Errorf("removeUnusedRoles(../escape) = %v, want %v", err, helpers.ErrUnsafeRemovalPath)
	}
	assertPathsExist(t, filepath.Join(rolesPath, "web"))
}

// TestRemoveRoleToleratesAVanishedRolesPath pins that a roles path gone since
// the scan is nothing to remove, and a refused removal is reported naming the
// role's path. Skipped as root, which bypasses permissions.
func TestRemoveRoleToleratesAVanishedRolesPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; a read-only roles path cannot refuse the removal")
	}
	t.Parallel()
	gone := installedRole{Name: "web", RolesDir: filepath.Join(t.TempDir(), "absent")}
	if err := removeRole(t.Context(), gone, nil); err != nil {
		t.Errorf("removeRole over a vanished roles path = %v, want nil", err)
	}

	rolesPath := t.TempDir()
	seedMarkedRole(t, rolesPath, "web")
	//nolint:gosec // G302: read-only on purpose, so the final rmdir of the role fails.
	if err := os.Chmod(rolesPath, 0o555); err != nil {
		t.Fatalf("chmod %s: %v", rolesPath, err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(rolesPath, helpers.DirMod); err != nil {
			t.Errorf("failed to restore %s perms: %v", rolesPath, err)
		}
	})
	locked := installedRole{Name: "web", InstallPath: filepath.Join(rolesPath, "web"), RolesDir: rolesPath}
	err := removeRole(t.Context(), locked, nil)
	if err == nil || !strings.Contains(err.Error(), "failed to remove role "+locked.InstallPath) {
		t.Errorf("removeRole under a read-only roles path = %v, want a failure naming %s", err, locked.InstallPath)
	}
}

// TestRoleKeepSHAsWithoutASnapshotKeepsNothing pins that with no snapshot no
// role sha is kept, since the sweep runs only over recorded content.
func TestRoleKeepSHAsWithoutASnapshotKeepsNothing(t *testing.T) {
	t.Parallel()
	if keep := roleKeepSHAs(nil, map[string]bool{"web": true}, rolesByName{}, nil); len(keep) != 0 {
		t.Errorf("roleKeepSHAs(nil store) = %v, want empty", keep)
	}
	if records := recordsByInstallPath(nil); len(records) != 0 {
		t.Errorf("recordsByInstallPath(nil store) = %v, want empty", records)
	}
}
