package cleanup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// tomlCleanupCase is one recorded galaxy.toml: its body (or absence when
// write is false) and what cleanup must do with ns.name's install tree.
type tomlCleanupCase struct {
	name        string
	body        string
	wantWarning string
	write       bool
	wantErr     bool
	wantKept    bool
}

// tomlCleanupCases pins that a recorded galaxy.toml is reloaded through the
// extension dispatch: its roots keep the collection, a roles refusal keeps
// the collections, a broken file aborts, and a missing one is stale.
func tomlCleanupCases() []tomlCleanupCase {
	return []tomlCleanupCase{
		{name: "valid galaxy.toml naming the collection", write: true,
			body: "[project]\ncollections = [\"ns.name\"]\n", wantKept: true},
		{name: "broken galaxy.toml aborts and removes nothing", write: true,
			body: "[project\ncollections = [\"ns.name\"]\n", wantErr: true, wantKept: true},
		{name: "roles refusal keeps the collections", write: true,
			body:        "[project]\ncollections = [\"ns.name\"]\nroles = [{ include = \"x\" }]\n",
			wantWarning: "cannot be read", wantKept: true},
		{name: "missing galaxy.toml is a stale entry", wantWarning: "no longer exists"},
	}
}

func TestStartReloadsARecordedGalaxyTOML(t *testing.T) {
	t.Parallel()
	for _, tc := range tomlCleanupCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cacheDir := t.TempDir()
			downloadPath := t.TempDir()
			installDir := seedInstallTree(t, downloadPath)
			reqPath := filepath.Join(t.TempDir(), helpers.RequirementsTOMLName)
			if tc.write {
				if err := os.WriteFile(reqPath, []byte(tc.body), helpers.FileMod); err != nil {
					t.Fatalf("write galaxy.toml: %v", err)
				}
			}
			registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)
			printer := &recordingPrinter{}
			cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

			err := Start(t.Context(), cfg, newTestRuntimeWith(printer))
			assertTOMLCleanupOutcome(t, tc, err, printer, installDir)
		})
	}
}

// assertTOMLCleanupOutcome checks one case's verdict: the run's error class,
// the warning it must have printed, and whether ns.name's tree survived.
func assertTOMLCleanupOutcome(t *testing.T, tc tomlCleanupCase, err error, printer *recordingPrinter, installDir string) {
	t.Helper()
	if tc.wantErr && !errors.Is(err, helpers.ErrProjectRequirementsUnreadable) {
		t.Fatalf("Start: %v, want ErrProjectRequirementsUnreadable", err)
	}
	if !tc.wantErr && err != nil {
		t.Fatalf("Start: %v, want success", err)
	}
	if tc.wantWarning != "" && !printer.hasWarningContaining(tc.wantWarning) {
		t.Fatalf("expected a warning containing %q, got %v", tc.wantWarning, printer.warnings)
	}
	assertInstallTreeKept(t, installDir, tc.wantKept)
}

// assertInstallTreeKept fails unless ns.name's MANIFEST.json under installDir
// survived when kept is true, or is gone when it is false.
func assertInstallTreeKept(t *testing.T, installDir string, kept bool) {
	t.Helper()
	_, statErr := os.Stat(filepath.Join(installDir, "MANIFEST.json"))
	if kept && statErr != nil {
		t.Fatalf("expected ns.name's install tree to survive, stat error: %v", statErr)
	}
	if !kept && !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced ns.name to be removed, stat error: %v", statErr)
	}
}
