package cleanup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// TestNoProjectsLineIsAResult pins that a cache with no recorded project says
// so on the tier --quiet keeps, since no summary line follows it.
func TestNoProjectsLineIsAResult(t *testing.T) {
	t.Parallel()
	printer := &recordingPrinter{}
	if err := Start(t.Context(), &config.Config{CacheDir: t.TempDir()}, newTestRuntimeWith(printer)); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	if !printer.hasResultContaining("No projects recorded for GC.") {
		t.Fatalf("expected the no-projects line as a result, got results %q, prints %q", printer.results, printer.prints)
	}
}

// TestDryRunCandidatesAreResults pins that a dry run names each collection and
// role it would remove on the tier --quiet keeps: they are its report.
func TestDryRunCandidatesAreResults(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	downloadPath := filepath.Join(base, "collections")
	seedInstallTree(t, downloadPath)
	rolesPath := filepath.Join(base, "roles")
	seedMarkedRole(t, rolesPath, "owner.stale")
	reqPath := filepath.Join(base, "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\nroles: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	cacheDir := t.TempDir()
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{Projects: map[string]store.ProjectRecord{
		base: {RequirementsFile: reqPath, CollectionsPath: downloadPath, RolesPath: rolesPath, LastRun: time.Now().UTC()},
	}})

	printer := &recordingPrinter{}
	if err := Start(t.Context(), &config.Config{CacheDir: cacheDir, DryRun: true}, newTestRuntimeWith(printer)); err != nil {
		t.Fatalf("expected dry-run Start to succeed, got %v", err)
	}
	for _, want := range []string{"Would remove ns.name@1.0.0", "Would remove role owner.stale"} {
		if !printer.hasResultContaining(want) {
			t.Errorf("expected %q as a result, got results %q, prints %q", want, printer.results, printer.prints)
		}
	}
}
