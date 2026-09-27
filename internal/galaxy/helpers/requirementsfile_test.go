package helpers

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCheckRegularRequirementsFile pins the gate row by row: a regular file,
// one reached through a symlink and an absent path pass, so the open reports
// absence itself; a directory, directly or through a symlink, is refused.
func TestCheckRegularRequirementsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	regular := filepath.Join(dir, "requirements.yml")
	if err := os.WriteFile(regular, []byte("collections: []\n"), FileMod); err != nil {
		t.Fatalf("write %s: %v", regular, err)
	}
	linkToRegular := filepath.Join(dir, "linked.yml")
	if err := os.Symlink(regular, linkToRegular); err != nil {
		t.Fatalf("symlink %s: %v", linkToRegular, err)
	}
	directory := filepath.Join(dir, "dir.yml")
	if err := os.Mkdir(directory, DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", directory, err)
	}
	linkToDirectory := filepath.Join(dir, "linked-dir.yml")
	if err := os.Symlink(directory, linkToDirectory); err != nil {
		t.Fatalf("symlink %s: %v", linkToDirectory, err)
	}
	rows := []struct {
		want error
		name string
		path string
	}{
		{name: "a regular file", path: regular},
		{name: "a symlink to a regular file", path: linkToRegular},
		{name: "an absent path", path: filepath.Join(dir, "absent.yml")},
		{name: "a directory", path: directory, want: ErrRequirementsNotRegular},
		{name: "a symlink to a directory", path: linkToDirectory, want: ErrRequirementsNotRegular},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if err := CheckRegularRequirementsFile(row.path); !errors.Is(err, row.want) {
				t.Fatalf("CheckRegularRequirementsFile(%q) = %v, want errors.Is %v", row.path, err, row.want)
			}
		})
	}
}
