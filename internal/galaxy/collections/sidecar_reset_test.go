package collections

// writeGalaxyInfo removes each name before writing it and replaces a .info
// that is not a real directory, since os.Root does not judge what already
// sits at a leaf. The tests plant each shape directly, bypassing extraction.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"go.yaml.in/yaml/v3"
)

// TestWriteGalaxyInfoOverwritesRelativeInRootSymlink pins that an in-root
// symlink at the GALAXY.yml leaf, which os.Root lets through, is severed
// rather than followed into the file it points at.
func TestWriteGalaxyInfoOverwritesRelativeInRootSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{Server: "https://galaxy.example.com", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)

	infoDir := filepath.Join(root, target.info)
	mustMkdirAll(t, infoDir)
	victim := filepath.Join(root, "victim.txt")
	const victimContent = "# real content elsewhere in the collections root\n"
	mustWriteFile(t, victim, []byte(victimContent))

	// Relative to infoDir: ".." reaches "ansible_collections", ".." again
	// reaches root - the symlink stays inside the collections root the whole
	// way, so os.Root permits traversing it.
	leaf := filepath.Join(infoDir, galaxyYAMLFileName)
	if err := os.Symlink(filepath.Join("..", "..", "victim.txt"), leaf); err != nil {
		t.Fatalf("symlink GALAXY.yml -> ../../victim.txt: %v", err)
	}

	if err := writeGalaxyInfo(target, cfg, col, nil); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	// The removal is what makes this assertion pass: without it, the WriteFile
	// that follows would have resolved the symlink and overwritten victim.txt
	// with GALAXY.yml's own content instead.
	assertFileContent(t, victim, victimContent)

	info, err := os.Lstat(leaf)
	if err != nil {
		t.Fatalf("lstat %s: %v", leaf, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected %s to be a regular file after the write, still a symlink", leaf)
	}
	assertGalaxyYAMLIdentity(t, leaf, col)
}

// TestWriteGalaxyInfoDoesNotCreateDanglingSymlinkTarget pins that a dangling
// in-root symlink at the GALAXY.yml leaf does not get its target created at
// whatever path the planter named.
func TestWriteGalaxyInfoDoesNotCreateDanglingSymlinkTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{Server: "https://galaxy.example.com", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)

	infoDir := filepath.Join(root, target.info)
	mustMkdirAll(t, infoDir)
	ghost := filepath.Join(root, "ghost.txt")

	leaf := filepath.Join(infoDir, galaxyYAMLFileName)
	if err := os.Symlink(filepath.Join("..", "..", "ghost.txt"), leaf); err != nil {
		t.Fatalf("symlink GALAXY.yml -> ../../ghost.txt (dangling): %v", err)
	}

	if err := writeGalaxyInfo(target, cfg, col, nil); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	assertPathAbsent(t, ghost)

	info, err := os.Lstat(leaf)
	if err != nil {
		t.Fatalf("lstat %s: %v", leaf, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected %s to be a regular file after the write, still a symlink", leaf)
	}
	assertGalaxyYAMLIdentity(t, leaf, col)
}

// TestWriteGalaxyInfoDoesNotCorruptHardlinkedContentOutsideRoot pins that a
// hardlink at the GALAXY.yml leaf, standing in for the shared extracted store,
// is unlinked rather than written through; os.Root cannot see a hardlink.
func TestWriteGalaxyInfoDoesNotCorruptHardlinkedContentOutsideRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{Server: "https://galaxy.example.com", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)

	infoDir := filepath.Join(root, target.info)
	mustMkdirAll(t, infoDir)

	// outside stands in for cfg.CacheDir's content-addressable extracted
	// store: a real file with content other projects depend on, entirely
	// outside the collections root.
	outsideDir := t.TempDir()
	shared := filepath.Join(outsideDir, "shared-content.bin")
	const sharedContent = "# content another project's hardlink still relies on\n"
	mustWriteFile(t, shared, []byte(sharedContent))

	leaf := filepath.Join(infoDir, galaxyYAMLFileName)
	if err := os.Link(shared, leaf); err != nil {
		t.Fatalf("hardlink GALAXY.yml -> %s: %v", shared, err)
	}

	if err := writeGalaxyInfo(target, cfg, col, nil); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	// The removal unlinks the in-root directory entry, which only
	// drops the shared inode's link count - it never touches the inode's
	// content, so the other name (shared) must read back unchanged.
	assertFileContent(t, shared, sharedContent)
	assertGalaxyYAMLIdentity(t, leaf, col)
}

// assertGalaxyYAMLIdentity checks that leaf unmarshals into a GalaxyYAML
// naming col, so each test also proves the real sidecar was written.
func assertGalaxyYAMLIdentity(t *testing.T, leaf string, col collection) {
	t.Helper()
	data, err := os.ReadFile(leaf) // #nosec G304 -- leaf is built from this test's own t.TempDir
	if err != nil {
		t.Fatalf("read %s: %v", leaf, err)
	}
	var g GalaxyYAML
	if err := yaml.Unmarshal(data, &g); err != nil {
		t.Fatalf("unmarshal %s: %v", leaf, err)
	}
	if g.Namespace != col.Namespace || g.Name != col.Name || g.Version != col.Version {
		t.Errorf("identity = %s.%s-%s, want %s.%s-%s", g.Namespace, g.Name, g.Version, col.Namespace, col.Name, col.Version)
	}
}

// TestWriteGalaxyInfoKeepsTheExtractMarker pins that the extract marker in
// .info survives the write; a reset would silently make every later run
// extract every collection again.
func TestWriteGalaxyInfoKeepsTheExtractMarker(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &config.Config{Server: "https://galaxy.example.com", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)
	mustMkdirAll(t, target.path)
	seedValidExtractMarker(t, target, validMarkerSHA)

	if err := writeGalaxyInfo(target, cfg, col, nil); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	if !verifyExtractMarker(&capturingPrinter{}, target, validMarkerSHA) {
		t.Fatal("the extract marker did not survive writing GALAXY.yml beside it")
	}
}

// TestWriteGalaxyInfoReplacesASymlinkedInfoDirectory pins that an in-root
// symlink at target.info itself is replaced by a real directory, leaving the
// directory it pointed at untouched.
func TestWriteGalaxyInfoReplacesASymlinkedInfoDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &config.Config{Server: "https://galaxy.example.com", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)

	elsewhere := filepath.Join(root, "ansible_collections", "elsewhere")
	mustMkdirAll(t, elsewhere)
	victim := filepath.Join(elsewhere, galaxyYAMLFileName)
	const victimContent = "# another directory's own document\n"
	mustWriteFile(t, victim, []byte(victimContent))
	infoDir := filepath.Join(root, target.info)
	if err := os.Symlink("elsewhere", infoDir); err != nil {
		t.Fatalf("symlink %s -> elsewhere: %v", infoDir, err)
	}

	if err := writeGalaxyInfo(target, cfg, col, nil); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	assertFileContent(t, victim, victimContent)
	info, err := os.Lstat(infoDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("%s is not a real directory after the write (%v)", infoDir, err)
	}
	assertGalaxyYAMLIdentity(t, filepath.Join(infoDir, galaxyYAMLFileName), col)
}

// TestWriteGalaxyInfoRemovesAnotherSourcesProvenance pins that a Galaxy
// install removes a url install's leftover provenance file for the same
// version, which would otherwise make outdated report it as a url install.
func TestWriteGalaxyInfoRemovesAnotherSourcesProvenance(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &config.Config{Server: "https://galaxy.example.com", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)
	stale := filepath.Join(root, target.info, provenanceFileName)
	mustMkdirAll(t, filepath.Dir(stale))
	mustWriteFile(t, stale, []byte("url_sha256: "+validMarkerSHA+"\n"))

	if err := writeGalaxyInfo(target, cfg, col, nil); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	assertPathAbsent(t, stale)
}
