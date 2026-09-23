package collections_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// installedTally is the extract marker's tally (entries, directories, total
// size) measured from outside the package, so a test can show two versions
// are ones the tally cannot tell apart.
type installedTally struct {
	entries, dirs, bytes int64
}

func tallyInstalledTree(t *testing.T, dir string) installedTally {
	t.Helper()
	var tally installedTally
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		if d.IsDir() {
			tally.dirs++
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		tally.entries++
		tally.bytes += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return tally
}

// assertMarkerOnlyBesideVersion fails unless the one extract marker sits in the
// installed version's .info directory: none in the collection directory, which
// ansible-galaxy collection verify rejects, and no other version's .info left.
func assertMarkerOnlyBesideVersion(t *testing.T, downloadPath, version string) {
	t.Helper()
	entries, err := os.ReadDir(installPathFor(downloadPath, "lib"))
	if err != nil {
		t.Fatalf("read the installed collection: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), helpers.ExtractMarkerPrefix) {
			t.Errorf("the collection directory carries %s, which ansible-galaxy collection verify reports", entry.Name())
		}
	}
	infos, err := filepath.Glob(filepath.Join(downloadPath, "ansible_collections", "acme.lib-*.info"))
	if err != nil {
		t.Fatalf("glob .info directories: %v", err)
	}
	want := filepath.Join(downloadPath, "ansible_collections", "acme.lib-"+version+".info")
	if len(infos) != 1 || infos[0] != want {
		t.Fatalf(".info directories = %v, want only %s", infos, want)
	}
	markers, err := filepath.Glob(filepath.Join(want, helpers.ExtractMarkerPrefix+"*"))
	if err != nil || len(markers) != 1 {
		t.Fatalf("markers in %s = %v (%v), want exactly one", want, markers, err)
	}
}

// TestInstallBackToAnEarlierVersionReextractsIt pins that an install sweeps
// other versions' .info directories, so a downgrade between two versions of
// equal tally re-extracts rather than trusting a stale marker.
func TestInstallBackToAnEarlierVersionReextractsIt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	s := fakegalaxy.New(t)
	s.AddVersion("acme", "lib", "1.0.1", nil)
	s.AddVersion("acme", "lib", "1.0.2", nil)
	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
		Timeout:          e2eTimeout,
	}
	installVersion := func(version string) installedTally {
		t.Helper()
		content := "collections:\n  - name: acme.lib\n    version: \"==" + version + "\"\n"
		if err := os.WriteFile(reqPath, []byte(content), helpers.FileMod); err != nil {
			t.Fatalf("write requirements.yml: %v", err)
		}
		if err := collections.Start(context.Background(), cfg, infra.New(noopPrinter{}, s.Client())); err != nil {
			t.Fatalf("install acme.lib %s: %v", version, err)
		}
		if got := readManifestVersion(t, cfg.DownloadPath, "lib"); got != version {
			t.Fatalf("installed acme.lib version = %q, want %s", got, version)
		}
		assertMarkerOnlyBesideVersion(t, cfg.DownloadPath, version)
		return tallyInstalledTree(t, installPathFor(cfg.DownloadPath, "lib"))
	}

	first := installVersion("1.0.1")
	if second := installVersion("1.0.2"); second != first {
		t.Fatalf("fixture trees differ (%+v vs %+v); the test needs two versions a tally cannot tell apart", first, second)
	}
	installVersion("1.0.1")
}
