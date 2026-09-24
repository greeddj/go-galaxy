package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// ProjectRecord is one project's registry entry. The registry has no schema
// version and an older binary re-recording a project drops fields it does not
// know, so a field's reader must take its absence as the conservative answer.
type ProjectRecord struct {
	LastRun time.Time `json:"last_run"`
	// RequirementsFile is the absolute path of whichever file was read,
	// galaxy.toml included; kept in this one field so an older binary's
	// cleanup fails closed on it instead of reading the record as stale.
	RequirementsFile string `json:"requirements_file"`
	CollectionsPath  string `json:"collections_path"`
	// RolesPath is the absolute roles directory, or "" when none was
	// configured; omitempty keeps a collections-only record unchanged, and
	// cleanup reads an absent path as "do not scan", never as a guess.
	RolesPath string `json:"roles_path,omitempty"`
}

// ProjectRegistry stores known projects keyed by path.
type ProjectRegistry struct {
	Projects map[string]ProjectRecord `json:"projects"`
}

// RecordProject records or updates a project entry in the registry.
func RecordProject(cacheDir, requirementsFile, downloadPath, rolesPath string) error {
	if cacheDir == "" {
		return nil
	}
	projectPath, record := NewProjectRecord(requirementsFile, downloadPath, rolesPath)

	registry, err := LoadProjectRegistry(cacheDir)
	if err != nil {
		return err
	}
	// LoadProjectRegistry already returns a non-nil map; this keeps the write
	// safe without resting on that postcondition.
	registry.Projects = ensureMap(registry.Projects)
	registry.Projects[projectPath] = record
	return saveProjectRegistry(cacheDir, registry)
}

// NewProjectRecord builds a run's registry entry, keyed by the directory of the
// absolute requirements file, with both paths resolved against it by one rule.
// Both backends build records here, so their registries keep one shape.
func NewProjectRecord(requirementsFile, downloadPath, rolesPath string) (string, ProjectRecord) {
	absReq, err := filepath.Abs(requirementsFile)
	if err != nil {
		absReq = requirementsFile
	}
	projectPath := filepath.Dir(absReq)
	return projectPath, ProjectRecord{
		RequirementsFile: absReq,
		CollectionsPath:  resolveProjectPath(projectPath, downloadPath),
		RolesPath:        resolveProjectPath(projectPath, rolesPath),
		LastRun:          time.Now().UTC(),
	}
}

// LoadProjectRegistry loads cacheDir's registry with Projects never nil. A
// missing file is empty, but one that fails to decode is an error: cleanup
// computes reachability from it, and reading it as empty would delete everything.
func LoadProjectRegistry(cacheDir string) (*ProjectRegistry, error) {
	path := projectRegistryPath(cacheDir)
	//nolint:gosec // path is derived from cacheDir and is intended for project registry IO.
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ProjectRegistry{Projects: make(map[string]ProjectRecord)}, nil
		}
		return nil, err
	}
	var registry ProjectRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("%w at %s: %w (remove the file or clear the cache to rebuild the registry)",
			helpers.ErrCorruptProjectRegistry, path, err)
	}
	registry.Projects = ensureMap(registry.Projects)
	return &registry, nil
}

// saveProjectRegistry writes the registry atomically to disk.
func saveProjectRegistry(cacheDir string, registry *ProjectRegistry) error {
	if registry == nil {
		return nil
	}
	path := projectRegistryPath(cacheDir)
	if err := os.MkdirAll(filepath.Dir(path), helpers.DirMod); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".projects-")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(payload); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

// projectRegistryPath returns the registry path under cacheDir.
func projectRegistryPath(cacheDir string) string {
	return filepath.Join(cacheDir, helpers.StoreDBProjects)
}

// resolveProjectPath joins a relative p under projectPath; an empty p stays
// empty, so "not configured" survives rather than becoming the project directory.
func resolveProjectPath(projectPath, p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(projectPath, p)
}
