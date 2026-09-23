package collections

// Each install write site, driven directly, must refuse a symlinked
// ansible_collections escaping DownloadPath and leave a pre-existing outside
// tree intact; every write goes through one os.Root.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// symlinkForm selects an absolute or relative escaping symlink target. os.Root
// refuses every absolute target, so only the relative form, the one a hostile
// checkout ships, proves the refusal is about escaping.
type symlinkForm int

const (
	// symlinkAbsolute points ansible_collections at outside's absolute path.
	symlinkAbsolute symlinkForm = iota
	// symlinkRelative points ansible_collections at the same outside
	// directory through a relative ".." target.
	symlinkRelative
)

// symlinkedEscapeFixture builds a downloadPath whose ansible_collections is a
// symlink (per form) to a sibling outside directory, returning col's
// installTarget and the outside path the symlink would redirect writes to.
func symlinkedEscapeFixture(t *testing.T, col collection, form symlinkForm) (installTarget, string) {
	t.Helper()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	mustMkdirAll(t, downloadPath)
	outside := filepath.Join(root, "outside")
	mustMkdirAll(t, outside)

	linkTarget := outside
	if form == symlinkRelative {
		linkTarget = filepath.Join("..", "outside")
	}
	if err := os.Symlink(linkTarget, filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> %s: %v", linkTarget, err)
	}

	osRoot, err := os.OpenRoot(downloadPath)
	if err != nil {
		t.Fatalf("os.OpenRoot(%s): %v", downloadPath, err)
	}
	t.Cleanup(func() {
		_ = osRoot.Close()
	})

	cfg := &config.Config{DownloadPath: downloadPath, Server: "https://galaxy.example.com"}
	target, ok := newInstallTarget(osRoot, cfg, col)
	if !ok {
		t.Fatalf("newInstallTarget(%s.%s@%s): unsafe identity", col.Namespace, col.Name, col.Version)
	}
	return target, filepath.Join(outside, col.Namespace, col.Name)
}

// realInstallFixture builds col's installTarget like symlinkedEscapeFixture but
// over a real ansible_collections directory: the positive control that keeps
// each escape test's "false" from being vacuous.
func realInstallFixture(t *testing.T, col collection) installTarget {
	t.Helper()
	downloadPath := t.TempDir()
	cfg := &config.Config{DownloadPath: downloadPath, Server: "https://galaxy.example.com"}
	target := newTestInstallTarget(t, cfg, col)
	mustMkdirAll(t, target.path)
	return target
}

// TestExtractCollectionSymlinkedPrefixLeavesOutsideTreeIntact pins that
// extractCollection's reset never follows an escaping ansible_collections
// symlink, absolute or relative, to delete the tree at its target.
func TestExtractCollectionSymlinkedPrefixLeavesOutsideTreeIntact(t *testing.T) {
	t.Parallel()
	for _, form := range []symlinkForm{symlinkAbsolute, symlinkRelative} {
		t.Run(symlinkFormName(form), func(t *testing.T) {
			t.Parallel()
			col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
			target, outsideInstallDir := symlinkedEscapeFixture(t, col, form)

			preexisting := filepath.Join(outsideInstallDir, "README.md")
			const preexistingContent = "# a real, previously installed tree\n"
			mustMkdirAll(t, outsideInstallDir)
			mustWriteFile(t, preexisting, []byte(preexistingContent))

			tarRoot := t.TempDir()
			tarPath := filepath.Join(tarRoot, "artifact.tar.gz")
			mustWriteFile(t, tarPath, buildMinimalTarGz(t))

			runtime := infra.New(noopPrinter{}, http.DefaultClient)
			err := extractCollection(context.Background(), col, tarPath, target, runtime, nil, "", false)
			if !errors.Is(err, helpers.ErrCollectionsPathEscape) {
				t.Errorf("extractCollection error = %v, want errors.Is helpers.ErrCollectionsPathEscape", err)
			}
			assertFileContent(t, preexisting, preexistingContent)
		})
	}
}

// symlinkFormName renders form as a subtest name.
func symlinkFormName(form symlinkForm) string {
	if form == symlinkRelative {
		return "relative"
	}
	return "absolute"
}

// TestWriteGalaxyInfoSymlinkedPrefixWritesNothingOutside pins that
// writeGalaxyInfo refuses the symlinked prefix before creating the .info
// sidecar directory anywhere.
func TestWriteGalaxyInfoSymlinkedPrefixWritesNothingOutside(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target, outsideInstallDir := symlinkedEscapeFixture(t, col, symlinkAbsolute)
	// The sidecar directory ("<ns>.<name>-<version>.info") is a sibling of
	// col's install directory under the symlinked prefix, not underneath it.
	outsideInfoDir := filepath.Join(filepath.Dir(filepath.Dir(outsideInstallDir)), col.Namespace+"."+col.Name+"-"+col.Version+".info")

	// writeGalaxyInfo only reads cfg.Server (for the sidecar body); it never
	// touches cfg.DownloadPath, so a bare cfg is enough here.
	cfg := &config.Config{Server: "https://galaxy.example.com"}
	err := writeGalaxyInfo(target, cfg, col, nil)
	if !errors.Is(err, helpers.ErrCollectionsPathEscape) {
		t.Errorf("writeGalaxyInfo error = %v, want errors.Is helpers.ErrCollectionsPathEscape", err)
	}
	assertPathAbsent(t, outsideInfoDir)
	assertPathAbsent(t, filepath.Join(outsideInfoDir, galaxyYAMLFileName))
}

// TestVerifyExtractMarkerSymlinkedPrefixLeavesOutsideMarkerIntact pins that a
// marker reachable only through a symlinked prefix is reported unverified and
// never unlinked by verifyExtractMarker's cleanup.
func TestVerifyExtractMarkerSymlinkedPrefixLeavesOutsideMarkerIntact(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target, outsideInstallDir := symlinkedEscapeFixture(t, col, symlinkAbsolute)
	mustMkdirAll(t, outsideInstallDir)

	const sha = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcd12"
	const markerContent = "go-galaxy-extract-1 entries=0 dirs=0 bytes=0\n"
	markerPath := collectionMarkerPath(outsideInstallDir, col, sha)
	mustMkdirAll(t, filepath.Dir(markerPath))
	mustWriteFile(t, markerPath, []byte(markerContent))

	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, target, sha) {
		t.Fatal("expected verifyExtractMarker to reject a marker reachable only through a symlinked prefix")
	}
	assertFileContent(t, markerPath, markerContent)
}

// TestVerifyExtractMarkerRealInstallReturnsTrue pins that a valid marker at a
// real install directory is accepted: the positive control for
// TestVerifyExtractMarkerSymlinkedPrefixLeavesOutsideMarkerIntact.
func TestVerifyExtractMarkerRealInstallReturnsTrue(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target := realInstallFixture(t, col)

	const sha = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	seedValidExtractMarker(t, target, sha)

	printer := &capturingPrinter{}
	if !verifyExtractMarker(printer, target, sha) {
		t.Fatal("expected verifyExtractMarker to accept a valid marker at a real, non-symlinked install directory")
	}
}

// TestInstallRecordMatchesSymlinkedPrefixReturnsFalse pins that
// installRecordMatches, on its own, rejects a record, marker and sidecar
// reachable only through a symlinked prefix.
func TestInstallRecordMatchesSymlinkedPrefixReturnsFalse(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target, outsideInstallDir := symlinkedEscapeFixture(t, col, symlinkAbsolute)
	mustMkdirAll(t, outsideInstallDir)

	const sha = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	outsideInfoDir := filepath.Join(filepath.Dir(filepath.Dir(outsideInstallDir)), col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, outsideInfoDir)
	mustWriteFile(t, collectionMarkerPath(outsideInstallDir, col, sha), []byte("go-galaxy-extract-1 entries=0 dirs=0 bytes=0\n"))
	mustWriteFile(t, filepath.Join(outsideInfoDir, galaxyYAMLFileName), sidecarFor(col))

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: sha,
		InstalledAt:    time.Now().UTC(),
	})

	if installRecordMatches(target, col, st) {
		t.Fatal("expected installRecordMatches to return false when the record, marker, and " +
			"sidecar are all reachable only through a symlinked prefix")
	}
}

// TestInstallRecordMatchesRealInstallReturnsTrue pins that the same seed at a
// real install directory is accepted: the positive control for
// TestInstallRecordMatchesSymlinkedPrefixReturnsFalse.
func TestInstallRecordMatchesRealInstallReturnsTrue(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target := realInstallFixture(t, col)

	const sha = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	seedValidExtractMarker(t, target, sha)
	infoDir := filepath.Join(filepath.Dir(filepath.Dir(target.path)), col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, infoDir)
	mustWriteFile(t, filepath.Join(infoDir, galaxyYAMLFileName), sidecarFor(col))

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: sha,
		InstalledAt:    time.Now().UTC(),
	})

	if !installRecordMatches(target, col, st) {
		t.Fatal("expected installRecordMatches to return true against a real, non-symlinked install " +
			"directory holding a matching record, marker, and sidecar")
	}
}

// TestCanSkipInstallSymlinkedPrefixReturnsFalse pins that canSkipInstall never
// takes a symlink-only fake install as installed. Its two rooted gates each
// suffice alone, so each has its own single-gate test.
func TestCanSkipInstallSymlinkedPrefixReturnsFalse(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target, outsideInstallDir := symlinkedEscapeFixture(t, col, symlinkAbsolute)
	mustMkdirAll(t, outsideInstallDir)

	const sha = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	outsideInfoDir := filepath.Join(filepath.Dir(filepath.Dir(outsideInstallDir)), col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, outsideInfoDir)
	mustWriteFile(t, collectionMarkerPath(outsideInstallDir, col, sha), []byte("go-galaxy-extract-1 entries=0 dirs=0 bytes=0\n"))
	mustWriteFile(t, filepath.Join(outsideInfoDir, galaxyYAMLFileName), sidecarFor(col))

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: sha,
		InstalledAt:    time.Now().UTC(),
	})

	if _, ok := canSkipInstall(target, col, st, noopPrinter{}); ok {
		t.Fatal("expected canSkipInstall to return false when the record, marker, and sidecar are all reachable only through a symlinked prefix")
	}
}

// TestCanSkipInstallRealInstallReturnsTrue pins that the same seed at a real
// install directory is accepted: the positive control for
// TestCanSkipInstallSymlinkedPrefixReturnsFalse.
func TestCanSkipInstallRealInstallReturnsTrue(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target := realInstallFixture(t, col)

	const sha = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	seedValidExtractMarker(t, target, sha)
	infoDir := filepath.Join(filepath.Dir(filepath.Dir(target.path)), col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, infoDir)
	mustWriteFile(t, filepath.Join(infoDir, galaxyYAMLFileName), sidecarFor(col))

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: sha,
		InstalledAt:    time.Now().UTC(),
	})

	if _, ok := canSkipInstall(target, col, st, noopPrinter{}); !ok {
		t.Fatal("expected canSkipInstall to return true against a real, non-symlinked install " +
			"directory holding a matching record, marker, and sidecar")
	}
}
