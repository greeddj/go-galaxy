package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// seedAnsibleGalaxyInstall writes community.general@9.0.0 under collectionsPath
// as ansible-galaxy leaves it: MANIFEST.json and a GALAXY.yml in its .info
// directory, with no go-galaxy extract marker. It returns the manifest path.
func seedAnsibleGalaxyInstall(t *testing.T, collectionsPath string) string {
	t.Helper()
	root := filepath.Join(collectionsPath, "ansible_collections")
	files := map[string]string{
		filepath.Join(root, "community", "general", "MANIFEST.json"): `{"collection_info": ` +
			`{"namespace": "community", "name": "general", "version": "9.0.0", "dependencies": {}}}`,
		filepath.Join(root, "community.general-9.0.0.info", "GALAXY.yml"): "download_url: " +
			"https://galaxy.ansible.com/download/community-general-9.0.0.tar.gz\nformat_version: 1.0.0\n" +
			"name: general\nnamespace: community\nsignatures: []\nversion: 9.0.0\n",
	}
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), helpers.DirMod); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(body), helpers.FileMod); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return filepath.Join(root, "community", "general", "MANIFEST.json")
}

// TestCleanupLeavesAnsibleGalaxyInstallsInTheSharedTree records ansible's own
// ~/.ansible/collections through ansible.cfg, then drops what go-galaxy
// installed there: cleanup removes that alone. Not parallel: t.Setenv, t.Chdir.
func TestCleanupLeavesAnsibleGalaxyInstallsInTheSharedTree(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	cache := filepath.Join(t.TempDir(), "cache")
	cfgPath := filepath.Join(project, "ansible.cfg")
	if err := os.WriteFile(cfgPath, []byte("[defaults]\ncollections_path = ~/.ansible/collections\n"), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", cfgPath, err)
	}
	for _, key := range []string{"ANSIBLE_COLLECTIONS_PATH", "GO_GALAXY_DOWNLOAD_PATH", "GO_GALAXY_REQUIREMENTS_FILE",
		"ANSIBLE_GALAXY_REQUIREMENTS_FILE", "GO_GALAXY_ANSIBLE_CONFIG", "GO_GALAXY_CACHE_DIR"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("os.Unsetenv(%q) error = %v, want nil", key, err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("ANSIBLE_CONFIG", cfgPath)
	t.Chdir(project)

	shared := filepath.Join(home, ".ansible", "collections")
	foreign := seedAnsibleGalaxyInstall(t, shared)
	galaxy := fakegalaxy.New(t)
	galaxy.AddVersion("acme", "app", "1.0.0", nil)
	reqPath := filepath.Join(project, "requirements.yml")
	writeReq := func(body string) {
		t.Helper()
		if err := os.WriteFile(reqPath, []byte(body), helpers.FileMod); err != nil {
			t.Fatalf("write %s: %v", reqPath, err)
		}
	}
	writeReq("collections:\n  - name: acme.app\n")

	args := []string{"install", "--quiet", "--cache-dir", cache, "--server", galaxy.URL(), "-r", reqPath}
	if err := runRootCommand(t, args); err != nil {
		t.Fatalf("install: %v", err)
	}
	installed := filepath.Join(shared, "ansible_collections", "acme", "app", "MANIFEST.json")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("install did not write acme.app under ~/.ansible/collections: %v", err)
	}

	writeReq("collections: []\n")
	if err := runRootCommand(t, []string{"cleanup", "--quiet", "--cache-dir", cache}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(installed); !os.IsNotExist(err) {
		t.Fatalf("expected cleanup to remove the acme.app go-galaxy installed, stat error: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("expected community.general, which ansible-galaxy installed, to survive cleanup: %v", err)
	}
}
