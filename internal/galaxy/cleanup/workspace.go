package cleanup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// errWorkspaceUnrooted marks an ansible_collections entry whose probe failed
// with anything but fs.ErrNotExist. It only feeds a skip warning, so it is not
// helpers.ErrCollectionsPathEscape, which maps to an install exit code.
var errWorkspaceUnrooted = errors.New("ansible_collections does not resolve inside the project collections path")

// workspace is one project's rooted collections workspace. root is anchored at
// the collections path, never at ansible_collections, because os.OpenRoot
// follows a symlink when establishing a root; paths under root use slashes.
type workspace struct {
	root *os.Root
	fsys fs.FS
	path string
}

// openProjectWorkspace roots the recorded collectionsPath when it holds
// ansible_collections, and no other tree: one nobody recorded is not scanned.
// A non-ENOENT probe error (an escaping symlink, a loop) is errWorkspaceUnrooted.
func openProjectWorkspace(collectionsPath string) (workspace, error) {
	root, err := os.OpenRoot(collectionsPath)
	if err != nil {
		return workspace{}, nil
	}
	info, statErr := root.Stat("ansible_collections")
	switch {
	case statErr == nil && info.IsDir():
		return workspace{root: root, fsys: root.FS(), path: collectionsPath}, nil
	case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
		_ = root.Close()
		return workspace{}, fmt.Errorf("%w: %q: %w", errWorkspaceUnrooted, filepath.Join(collectionsPath, "ansible_collections"), statErr)
	}
	_ = root.Close()
	return workspace{}, nil
}
