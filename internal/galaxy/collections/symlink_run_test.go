package collections

// Whole-run symlink hardening through Start: an escaping ansible_collections
// fails the run once, before resolution, and a dry run never creates an absent
// DownloadPath.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestSymlinkedAnsibleCollectionsFailsWholeRunWithoutDestroyingOutsideTree pins
// that an escaping ansible_collections symlink, absolute or relative, fails
// Start with ErrCollectionsPathEscape, no request, and the outside tree intact.
func TestSymlinkedAnsibleCollectionsFailsWholeRunWithoutDestroyingOutsideTree(t *testing.T) {
	t.Parallel()
	for _, form := range []symlinkForm{symlinkAbsolute, symlinkRelative} {
		t.Run(symlinkFormName(form), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			downloadPath := filepath.Join(root, "install")
			mustMkdirAll(t, downloadPath)
			outside := filepath.Join(root, "outside")
			mustMkdirAll(t, outside)
			victim := filepath.Join(outside, "victim.txt")
			const victimContent = "# a real, pre-existing outside tree\n"
			mustWriteFile(t, victim, []byte(victimContent))
			linkTarget := outside
			if form == symlinkRelative {
				linkTarget = filepath.Join("..", "outside")
			}
			if err := os.Symlink(linkTarget, filepath.Join(downloadPath, "ansible_collections")); err != nil {
				t.Fatalf("symlink ansible_collections -> %s: %v", linkTarget, err)
			}

			cacheDir := filepath.Join(root, "cache")
			reqPath := filepath.Join(root, "requirements.yml")
			mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

			srv := fakegalaxy.New(t)
			srv.AddVersion("acme", "app", "1.0.0", nil)

			cfg := &config.Config{
				Server:           srv.URL(),
				CacheDir:         cacheDir,
				DownloadPath:     downloadPath,
				RequirementsFile: reqPath,
				Workers:          1,
			}
			runtime := infra.New(noopPrinter{}, srv.Client())

			err := Start(context.Background(), cfg, runtime)
			if !errors.Is(err, helpers.ErrCollectionsPathEscape) {
				t.Errorf("Start error = %v, want errors.Is helpers.ErrCollectionsPathEscape", err)
			}
			assertFileContent(t, victim, victimContent)
			entries, readErr := os.ReadDir(outside)
			if readErr != nil {
				t.Fatalf("read outside dir: %v", readErr)
			}
			if len(entries) != 1 || entries[0].Name() != "victim.txt" {
				t.Errorf("expected outside dir to contain only the pre-existing victim.txt, got %v", entries)
			}
			if got := srv.Total(); got != 0 {
				t.Errorf("fake server request count = %d, want 0 (the escape must be caught before resolution ever starts)", got)
			}
		})
	}
}

// TestSymlinkedAnsibleCollectionsProducesOneFailureNotOnePerCollection pins
// that a symlinked ansible_collections fails a two-collection run once, before
// resolution, with no per-collection "Failed:" line.
func TestSymlinkedAnsibleCollectionsProducesOneFailureNotOnePerCollection(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	mustMkdirAll(t, downloadPath)
	outside := filepath.Join(root, "outside")
	mustMkdirAll(t, outside)
	if err := os.Symlink(outside, filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> outside: %v", err)
	}

	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", map[string]string{"acme.lib": ">=1.0.0"})
	srv.AddVersion("acme", "lib", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          4,
	}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())

	err := Start(context.Background(), cfg, runtime)
	if !errors.Is(err, helpers.ErrCollectionsPathEscape) {
		t.Fatalf("Start error = %v, want errors.Is helpers.ErrCollectionsPathEscape", err)
	}
	// main prints the run's terminal error, so any Errorf line here would be
	// a per-collection failure.
	if len(printer.errs) != 0 {
		t.Errorf("printer recorded %d Errorf lines, want none (the run failure is printed by main), got %v", len(printer.errs), printer.errs)
	}
	if printer.hasErrContaining("Failed: acme.") {
		t.Errorf("expected no per-collection \"Failed: acme.*\" line, got %v", printer.errs)
	}
	if got := srv.Total(); got != 0 {
		t.Errorf("fake server request count = %d, want 0 (neither collection was ever individually considered)", got)
	}
}

// TestDryRunInstallLeavesAbsentDownloadPathAbsentAndStillClassifies pins that
// install --dry-run never creates an absent DownloadPath and still reports each
// collection as would-install.
func TestDryRunInstallLeavesAbsentDownloadPathAbsentAndStillClassifies(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install") // deliberately never created
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          1,
		DryRun:           true,
	}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())

	if err := Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	assertPathAbsent(t, downloadPath)
	if !printer.hasOkContaining("Would install: acme.app@1.0.0") {
		t.Errorf("expected acme.app classified as would-install, got okLines %v", printer.okLines())
	}
	if !printer.hasPersistentPrintContaining("1 would install, 0 already up to date, 0 would fail") {
		t.Errorf("expected the dry-run summary line to count the collection, got %v", printer.persists)
	}
}
