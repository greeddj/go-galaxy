package helpers

import (
	"os"
	"path/filepath"
	"testing"
)

// enterLinkedDir makes real/x and the link repo/deploy to it under a resolved
// t.TempDir, and enters the link as a shell's cd does, leaving $PWD on it.
func enterLinkedDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	target := filepath.Join(root, "real", "x")
	link := filepath.Join(root, "repo", "deploy")
	for _, dir := range []string{target, filepath.Dir(link)} {
		if err := os.MkdirAll(dir, DirMod); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s: %v", link, err)
	}
	t.Chdir(link)
	return root
}

// TestPhysicalAbsResolvesAsTheKernelDoes enters real/x by the link repo/deploy:
// "../coll" names real/coll, the directory a relative mkdir creates, where
// filepath.Abs names repo/coll; an absolute path is only cleaned. Not parallel: t.Chdir.
func TestPhysicalAbsResolvesAsTheKernelDoes(t *testing.T) {
	root := enterLinkedDir(t)
	rel := filepath.Join("..", "coll")
	if err := os.Mkdir(rel, DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	want := filepath.Join(root, "real", "coll")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("the kernel did not create %s: %v", want, err)
	}
	if logical, err := filepath.Abs(rel); err == nil && logical == want {
		t.Fatalf("filepath.Abs(%q) = %q already: the working directory was not entered by its link", rel, logical)
	}
	cases := map[string]string{
		rel: want,
		filepath.Join(root, "a") + string(filepath.Separator) + ".." + string(filepath.Separator) + "b": filepath.Join(root, "b"),
	}
	for path, want := range cases {
		if got, err := PhysicalAbs(path); err != nil || got != want {
			t.Errorf("PhysicalAbs(%q) = %q, %v, want %q", path, got, err, want)
		}
	}
}
