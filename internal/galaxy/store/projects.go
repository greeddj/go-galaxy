package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// ProjectRecord describes a project and its last run metadata.
type ProjectRecord struct {
	RequirementsFile string    `json:"requirements_file"`
	CollectionsPath  string    `json:"collections_path"`
	LastRun          time.Time `json:"last_run"`
}

// ProjectRegistry stores known projects keyed by path.
type ProjectRegistry struct {
	Projects map[string]ProjectRecord `json:"projects"`
}

// RecordProject records or updates a project entry in the registry.
func RecordProject(cacheDir, requirementsFile, downloadPath string) error {
	if cacheDir == "" {
		return nil
	}
	absReq, err := filepath.Abs(requirementsFile)
	if err != nil {
		absReq = requirementsFile
	}
	projectPath := filepath.Dir(absReq)
	collectionsPath := resolveCollectionsPath(projectPath, downloadPath)

	registry, err := LoadProjectRegistry(cacheDir)
	if err != nil {
		return err
	}
	if registry.Projects == nil {
		registry.Projects = make(map[string]ProjectRecord)
	}
	registry.Projects[projectPath] = ProjectRecord{
		RequirementsFile: absReq,
		CollectionsPath:  collectionsPath,
		LastRun:          time.Now().UTC(),
	}
	return saveProjectRegistry(cacheDir, registry)
}

// LoadProjectRegistry loads the project registry from cacheDir.
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
		return &ProjectRegistry{Projects: make(map[string]ProjectRecord)}, nil
	}
	if registry.Projects == nil {
		registry.Projects = make(map[string]ProjectRecord)
	}
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

// resolveCollectionsPath returns an absolute collections path for a project.
func resolveCollectionsPath(projectPath, downloadPath string) string {
	if downloadPath == "" {
		return ""
	}
	if filepath.IsAbs(downloadPath) {
		return downloadPath
	}
	return filepath.Join(projectPath, downloadPath)
}
