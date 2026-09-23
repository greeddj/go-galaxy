package collections

// This matrix pins that install --dry-run and a real install agree, per
// on-disk shape of ansible_collections and of a namespace directory, on
// success and on exitcode.FromError; accepted shapes are the positive controls.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// collectionsTreeShape is one on-disk arrangement to probe against Start,
// for both a real run and a dry run.
type collectionsTreeShape struct {
	// setup places the shape under test at downloadPath (an already-created
	// directory), given outside - a directory this process controls but that
	// sits entirely outside downloadPath, for the escaping-symlink shapes.
	setup func(t *testing.T, downloadPath, outside string)
	name  string
	// accepted is true for a shape a real, non-dry-run install completes
	// successfully against - this matrix's positive control for that shape.
	accepted bool
}

// ansibleCollectionsShapes lists the shapes ansible_collections itself can
// take, with the escaping symlink in both its relative form (the one a hostile
// checkout can plant) and its absolute form.
func ansibleCollectionsShapes() []collectionsTreeShape {
	return []collectionsTreeShape{
		{name: "absent", accepted: true, setup: setupAnsibleCollectionsAbsent},
		{name: "real directory", accepted: true, setup: setupAnsibleCollectionsRealDir},
		{name: "symlink, relative, in-root, to a directory", accepted: true, setup: setupAnsibleCollectionsInRootSymlink},
		{name: "symlink, escaping, relative", accepted: false, setup: setupAnsibleCollectionsEscapingRelative},
		{name: "symlink, escaping, absolute", accepted: false, setup: setupAnsibleCollectionsEscapingAbsolute},
		{name: "symlink, dangling", accepted: false, setup: setupAnsibleCollectionsDangling},
		{name: "regular file", accepted: false, setup: setupAnsibleCollectionsRegularFile},
	}
}

func setupAnsibleCollectionsAbsent(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, downloadPath)
}

func setupAnsibleCollectionsRealDir(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(downloadPath, "ansible_collections"))
}

func setupAnsibleCollectionsInRootSymlink(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(downloadPath, "real-target"))
	if err := os.Symlink("real-target", filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> real-target: %v", err)
	}
}

func setupAnsibleCollectionsEscapingRelative(t *testing.T, downloadPath, outside string) {
	t.Helper()
	mustMkdirAll(t, downloadPath)
	mustMkdirAll(t, outside)
	if err := os.Symlink(filepath.Join("..", "outside"), filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> ../outside: %v", err)
	}
}

func setupAnsibleCollectionsEscapingAbsolute(t *testing.T, downloadPath, outside string) {
	t.Helper()
	mustMkdirAll(t, downloadPath)
	mustMkdirAll(t, outside)
	if err := os.Symlink(outside, filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> outside: %v", err)
	}
}

func setupAnsibleCollectionsDangling(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, downloadPath)
	if err := os.Symlink("missing-target", filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> missing-target: %v", err)
	}
}

func setupAnsibleCollectionsRegularFile(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, downloadPath)
	mustWriteFile(t, filepath.Join(downloadPath, "ansible_collections"), []byte("not a directory"))
}

// namespaceShapes lists the shapes of ansible_collections/acme. A dangling
// symlink is accepted here, since extraction removes it and creates the
// directory fresh, while a dangling ansible_collections is refused.
func namespaceShapes() []collectionsTreeShape {
	return []collectionsTreeShape{
		{name: "absent", accepted: true, setup: setupNamespaceAbsent},
		{name: "real directory", accepted: true, setup: setupNamespaceRealDir},
		{name: "symlink, relative, in-root, to a directory", accepted: true, setup: setupNamespaceInRootSymlink},
		{name: "symlink, escaping, relative", accepted: false, setup: setupNamespaceEscapingRelative},
		{name: "symlink, dangling", accepted: true, setup: setupNamespaceDangling},
		{name: "regular file", accepted: false, setup: setupNamespaceRegularFile},
	}
}

func setupNamespaceAbsent(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(downloadPath, "ansible_collections"))
}

func setupNamespaceRealDir(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(downloadPath, "ansible_collections", "acme"))
}

func setupNamespaceInRootSymlink(t *testing.T, downloadPath, _ string) {
	t.Helper()
	acDir := filepath.Join(downloadPath, "ansible_collections")
	mustMkdirAll(t, filepath.Join(acDir, "real-ns-target"))
	if err := os.Symlink("real-ns-target", filepath.Join(acDir, "acme")); err != nil {
		t.Fatalf("symlink acme -> real-ns-target: %v", err)
	}
}

func setupNamespaceEscapingRelative(t *testing.T, downloadPath, outside string) {
	t.Helper()
	acDir := filepath.Join(downloadPath, "ansible_collections")
	mustMkdirAll(t, acDir)
	mustMkdirAll(t, outside)
	if err := os.Symlink(filepath.Join("..", "..", "outside"), filepath.Join(acDir, "acme")); err != nil {
		t.Fatalf("symlink acme -> ../../outside: %v", err)
	}
}

func setupNamespaceDangling(t *testing.T, downloadPath, _ string) {
	t.Helper()
	acDir := filepath.Join(downloadPath, "ansible_collections")
	mustMkdirAll(t, acDir)
	if err := os.Symlink("missing-ns-target", filepath.Join(acDir, "acme")); err != nil {
		t.Fatalf("symlink acme -> missing-ns-target: %v", err)
	}
}

func setupNamespaceRegularFile(t *testing.T, downloadPath, _ string) {
	t.Helper()
	acDir := filepath.Join(downloadPath, "ansible_collections")
	mustMkdirAll(t, acDir)
	mustWriteFile(t, filepath.Join(acDir, "acme"), []byte("not a directory"))
}

// runCollectionsTreeShape runs Start over shape, under dryRun, with a fresh
// temp tree and fake server requiring acme.app, and returns the server's
// request count and the run's error.
func runCollectionsTreeShape(t *testing.T, shape collectionsTreeShape, dryRun bool) (int, error) {
	t.Helper()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	outside := filepath.Join(root, "outside")
	shape.setup(t, downloadPath, outside)

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
		DryRun:           dryRun,
	}
	runtime := infra.New(noopPrinter{}, srv.Client())

	err := Start(context.Background(), cfg, runtime)
	return srv.Total(), err
}

// assertPreviewAgreesWithRealRun asserts the real run matches shape.accepted
// and the dry run matches it in error-ness and exit code; checkZeroRequests
// also requires a refused shape to stop both runs before any request.
func assertPreviewAgreesWithRealRun(t *testing.T, shape collectionsTreeShape, checkZeroRequests bool) {
	t.Helper()
	realRequests, realErr := runCollectionsTreeShape(t, shape, false)
	dryRequests, dryErr := runCollectionsTreeShape(t, shape, true)

	if (realErr == nil) != shape.accepted {
		t.Fatalf("fixture sanity: real run error-ness = %v, want accepted=%v; err=%v", realErr == nil, shape.accepted, realErr)
	}
	if (dryErr == nil) != (realErr == nil) {
		t.Errorf("dry run error-ness = %v, want it to match the real run's %v; dryErr=%v realErr=%v",
			dryErr == nil, realErr == nil, dryErr, realErr)
	}
	if gotDry, gotReal := exitcode.FromError(dryErr), exitcode.FromError(realErr); gotDry != gotReal {
		t.Errorf("exitcode.FromError(dry) = %d, exitcode.FromError(real) = %d, want equal; dryErr=%v realErr=%v",
			gotDry, gotReal, dryErr, realErr)
	}
	if checkZeroRequests && !shape.accepted {
		if realRequests != 0 {
			t.Errorf("real run server request count = %d, want 0 (the escape must be caught before resolution starts)", realRequests)
		}
		if dryRequests != 0 {
			t.Errorf("dry run server request count = %d, want 0 (the preview must abort at the same point in the pipeline)", dryRequests)
		}
	}
}

// TestCollectionsTreeMatrixAnsibleCollectionsAgreesBetweenPreviewAndRealRun
// is the seven-shape ansible_collections half of the matrix.
func TestCollectionsTreeMatrixAnsibleCollectionsAgreesBetweenPreviewAndRealRun(t *testing.T) {
	t.Parallel()
	for _, shape := range ansibleCollectionsShapes() {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			assertPreviewAgreesWithRealRun(t, shape, true)
		})
	}
}

// TestCollectionsTreeMatrixNamespaceAgreesBetweenPreviewAndRealRun is the
// six-shape namespace half of the matrix.
func TestCollectionsTreeMatrixNamespaceAgreesBetweenPreviewAndRealRun(t *testing.T) {
	t.Parallel()
	for _, shape := range namespaceShapes() {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			assertPreviewAgreesWithRealRun(t, shape, false)
		})
	}
}
