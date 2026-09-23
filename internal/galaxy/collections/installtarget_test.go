package collections

// Shared scaffolding for tests that need a real os.Root or installTarget, so
// each opens, validates and closes them the way a real run does.

import (
	"os"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// newTestCollectionsRoot opens the collections root for downloadPath through
// openCollectionsRoot, creating it as installWithState does, and closes it on
// cleanup.
func newTestCollectionsRoot(t *testing.T, downloadPath string) *os.Root {
	t.Helper()
	root, err := openCollectionsRoot(downloadPath, true)
	if err != nil {
		t.Fatalf("openCollectionsRoot(%s): %v", downloadPath, err)
	}
	t.Cleanup(func() {
		_ = root.Close()
	})
	return root
}

// newTestInstallTarget builds col's installTarget through newInstallTarget on
// a fresh root, failing the test if col's identity is reported unsafe.
func newTestInstallTarget(t *testing.T, cfg *config.Config, col collection) installTarget {
	t.Helper()
	root := newTestCollectionsRoot(t, cfg.DownloadPath)
	target, ok := newInstallTarget(root, cfg, col)
	if !ok {
		t.Fatalf("newInstallTarget(%s.%s@%s): unsafe identity", col.Namespace, col.Name, col.Version)
	}
	return target
}

// newFlatInstallTarget builds an installTarget whose install directory is dir
// itself (rel "."), bypassing newInstallTarget's validation, for marker-level
// tests that work on a bare tree with their own, often unsafe, sha.
func newFlatInstallTarget(t *testing.T, dir string) installTarget {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("os.OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		_ = root.Close()
	})
	return installTarget{root: root, rel: ".", path: dir, marker: "."}
}
