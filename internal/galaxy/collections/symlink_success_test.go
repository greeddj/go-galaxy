package collections

// The symlink hardening is not a blanket refusal: a symlinked DownloadPath and
// an in-root ansible_collections symlink must install, since collections_path
// is routinely a symlink in CI (a cache mount, a workspace alias).

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

// TestDownloadPathSymlinkInstallSucceeds pins that a DownloadPath that is a
// symlink to a real directory installs under the symlink's target.
func TestDownloadPathSymlinkInstallSucceeds(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realDownloadPath := filepath.Join(root, "real-install")
	downloadPathLink := filepath.Join(root, "install-link")
	mustMkdirAll(t, realDownloadPath)
	if err := os.Symlink(realDownloadPath, downloadPathLink); err != nil {
		t.Fatalf("symlink downloadPath -> real: %v", err)
	}

	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPathLink,
		RequirementsFile: reqPath,
		Workers:          1,
	}
	runtime := infra.New(noopPrinter{}, srv.Client())

	if err := Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	manifest := filepath.Join(realDownloadPath, "ansible_collections", "acme", "app", "MANIFEST.json")
	assertExists(t, manifest)
}

// TestAnsibleCollectionsSymlinkToSiblingInsideDownloadPathSucceeds pins that an
// ansible_collections symlink resolving inside DownloadPath installs. The target
// must be relative: os.Root refuses every absolute symlink target.
func TestAnsibleCollectionsSymlinkToSiblingInsideDownloadPathSucceeds(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	mustMkdirAll(t, downloadPath)
	realCollectionsDir := filepath.Join(downloadPath, "real-ansible-collections")
	mustMkdirAll(t, realCollectionsDir)
	if err := os.Symlink("real-ansible-collections", filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> sibling inside DownloadPath: %v", err)
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

	if err := Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	manifest := filepath.Join(realCollectionsDir, "acme", "app", "MANIFEST.json")
	assertExists(t, manifest)
}

// TestNewInstallTargetRejectsUnsafeIdentityAndNilRoot pins that newInstallTarget
// refuses an unsafe namespace, name or version independently, and a nil root
// rather than panicking on first use.
func TestNewInstallTargetRejectsUnsafeIdentityAndNilRoot(t *testing.T) {
	t.Parallel()
	downloadPath := t.TempDir()
	validRoot := newTestCollectionsRoot(t, downloadPath)
	cfg := &config.Config{DownloadPath: downloadPath}

	base := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}

	tests := []struct {
		root *os.Root
		name string
		col  collection
	}{
		{name: "unsafe namespace", root: validRoot, col: collection{Namespace: "../escape", Name: base.Name, Version: base.Version}},
		{name: "unsafe name", root: validRoot, col: collection{Namespace: base.Namespace, Name: "..", Version: base.Version}},
		{name: "unsafe version", root: validRoot, col: collection{Namespace: base.Namespace, Name: base.Name, Version: "../../.."}},
		{name: "nil root", root: nil, col: base},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := newInstallTarget(tc.root, cfg, tc.col); ok {
				t.Fatalf("newInstallTarget(ns=%q name=%q version=%q, root=%v) ok = true, want false",
					tc.col.Namespace, tc.col.Name, tc.col.Version, tc.root)
			}
		})
	}
}

// TestBuildCollectionsMapRejectsUnsafeNamespace pins that buildCollectionsMap
// refuses "foo/../.." before any install work, since helpers.SplitFQDN does no
// path-safety validation.
func TestBuildCollectionsMapRejectsUnsafeNamespace(t *testing.T) {
	t.Parallel()
	resolved := map[string]collection{
		"x": {Namespace: "foo/../..", Name: "bar", Version: "1.0.0"},
	}
	_, err := buildCollectionsMap(resolved)
	if !errors.Is(err, helpers.ErrUnsafeCollectionIdentifier) {
		t.Fatalf("buildCollectionsMap error = %v, want errors.Is helpers.ErrUnsafeCollectionIdentifier", err)
	}
}

// TestBuildCollectionsMapRejectsInvalidVersion pins ErrInvalidCollectionVersion
// for "*": a safe path element but no installable version, the shape a poisoned
// snapshot or lockfile entry can carry.
func TestBuildCollectionsMapRejectsInvalidVersion(t *testing.T) {
	t.Parallel()
	resolved := map[string]collection{
		"x": {Namespace: "acme", Name: "widgets", Version: "*"},
	}
	_, err := buildCollectionsMap(resolved)
	if !errors.Is(err, helpers.ErrInvalidCollectionVersion) {
		t.Fatalf("buildCollectionsMap error = %v, want errors.Is helpers.ErrInvalidCollectionVersion", err)
	}
}
