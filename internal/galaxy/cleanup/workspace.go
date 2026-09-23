package cleanup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/greeddj/go-galaxy/internal/galaxy/store"
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

// maxCollectionsPathCandidates bounds collectionsPathCandidates: the recorded
// CollectionsPath plus the ".collections" and "collections" fallbacks.
const maxCollectionsPathCandidates = 3

// collectionsPathCandidates lists a project's collections-path candidates in
// order: its recorded CollectionsPath, then the project-relative fallbacks.
// openProjectWorkspace decides which one, if any, is usable.
func collectionsPathCandidates(projectPath string, project store.ProjectRecord) []string {
	candidates := make([]string, 0, maxCollectionsPathCandidates)
	if project.CollectionsPath != "" {
		candidates = append(candidates, project.CollectionsPath)
	}
	if projectPath != "" {
		candidates = append(candidates, filepath.Join(projectPath, ".collections"), filepath.Join(projectPath, "collections"))
	}
	return candidates
}

// openProjectWorkspace roots the first candidate holding ansible_collections.
// A non-ENOENT probe error (an escaping symlink, a loop) returns
// errWorkspaceUnrooted instead of falling through to another candidate tree.
func openProjectWorkspace(projectPath string, project store.ProjectRecord) (workspace, error) {
	for _, candidate := range collectionsPathCandidates(projectPath, project) {
		root, err := os.OpenRoot(candidate)
		if err != nil {
			continue
		}
		info, statErr := root.Stat("ansible_collections")
		switch {
		case statErr == nil && info.IsDir():
			return workspace{root: root, fsys: root.FS(), path: candidate}, nil
		case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
			_ = root.Close()
			return workspace{}, fmt.Errorf("%w: %q: %w", errWorkspaceUnrooted, filepath.Join(candidate, "ansible_collections"), statErr)
		}
		_ = root.Close()
	}
	return workspace{}, nil
}
