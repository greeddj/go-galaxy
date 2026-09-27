package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestLoadProjectRegistryRejectsCorruptFile pins that an undecodable registry is
// ErrCorruptProjectRegistry rather than an empty registry, which would make
// cleanup see nothing as reachable and delete everything.
func TestLoadProjectRegistryRejectsCorruptFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeRegistryFile(t, dir, []byte("{invalid"))

	_, err := LoadProjectRegistry(dir)
	if !errors.Is(err, helpers.ErrCorruptProjectRegistry) {
		t.Fatalf("expected ErrCorruptProjectRegistry, got %v", err)
	}
}

// TestLoadProjectRegistryRejectsTruncatedFile covers a non-empty but
// truncated JSON payload, distinct from the wholly-invalid case above.
func TestLoadProjectRegistryRejectsTruncatedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeRegistryFile(t, dir, []byte(`{"projects": {"foo": {"last_run": "2024-01-02T03:04:05Z"`))

	_, err := LoadProjectRegistry(dir)
	if !errors.Is(err, helpers.ErrCorruptProjectRegistry) {
		t.Fatalf("expected ErrCorruptProjectRegistry, got %v", err)
	}
}

// TestLoadProjectRegistryMissingFileReturnsEmpty is a regression guard: a
// cache directory that has never recorded a project must still return an
// empty, initialized registry with a nil error.
func TestLoadProjectRegistryMissingFileReturnsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	registry, err := LoadProjectRegistry(dir)
	if err != nil {
		t.Fatalf("expected nil error for a missing registry file, got %v", err)
	}
	if registry == nil || registry.Projects == nil {
		t.Fatalf("expected an initialized empty registry, got %#v", registry)
	}
	if len(registry.Projects) != 0 {
		t.Fatalf("expected no projects, got %#v", registry.Projects)
	}
}

// TestLoadProjectRegistryRestoresANullProjectsMap pins that a registry holding
// an explicit JSON null under projects still loads with a non-nil map; the
// populated subtest is the control that the fixture reaches the decode at all.
func TestLoadProjectRegistryRestoresANullProjectsMap(t *testing.T) {
	t.Parallel()

	t.Run("explicit null projects map", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeRegistryFile(t, dir, []byte(`{"projects": null}`))

		registry, err := LoadProjectRegistry(dir)
		if err != nil {
			t.Fatalf("expected nil error for a null projects map, got %v", err)
		}
		if registry == nil || registry.Projects == nil {
			t.Fatalf("expected an initialized registry, got %#v", registry)
		}
		if len(registry.Projects) != 0 {
			t.Fatalf("expected no projects, got %#v", registry.Projects)
		}
	})

	t.Run("populated projects map", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeRegistryFile(t, dir, []byte(`{"projects": {"/p": {"requirements_file": "/p/requirements.yml", `+
			`"collections_path": "/p/collections", "last_run": "2024-01-02T03:04:05Z"}}}`))

		registry, err := LoadProjectRegistry(dir)
		if err != nil {
			t.Fatalf("expected nil error for a populated registry, got %v", err)
		}
		record, ok := registry.Projects["/p"]
		if !ok {
			t.Fatalf("expected an entry under /p, got %#v", registry.Projects)
		}
		if record.RequirementsFile != "/p/requirements.yml" {
			t.Fatalf("expected the decoded requirements file, got %q", record.RequirementsFile)
		}
	})
}

// writeRegistryFile writes raw bytes at the project registry path under
// dir, failing the test on error.
func writeRegistryFile(t *testing.T, dir string, data []byte) {
	t.Helper()
	path := filepath.Join(dir, helpers.StoreDBProjects)
	if err := os.WriteFile(path, data, helpers.FileMod); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

// TestLoadProjectRegistryWithoutRolesPathDecodesEmpty pins that a record with
// no roles_path key decodes with RolesPath "", which cleanup reads as "do not
// scan": the unversioned registry's only migration path, and a conservative one.
func TestLoadProjectRegistryWithoutRolesPathDecodesEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeRegistryFile(t, dir, []byte(`{"projects": {"/p": {"requirements_file": "/p/requirements.yml", `+
		`"collections_path": "/p/collections", "last_run": "2024-01-02T03:04:05Z"}}}`))

	registry, err := LoadProjectRegistry(dir)
	if err != nil {
		t.Fatalf("LoadProjectRegistry: %v", err)
	}
	record, ok := registry.Projects["/p"]
	if !ok {
		t.Fatalf("expected an entry under /p, got %#v", registry.Projects)
	}
	if record.RolesPath != "" {
		t.Fatalf("RolesPath = %q, want empty for a record written without one", record.RolesPath)
	}
	if record.CollectionsPath != "/p/collections" {
		t.Fatalf("CollectionsPath = %q, want /p/collections", record.CollectionsPath)
	}
}

// TestRecordProjectWritesRolesPath proves RecordProject resolves the roles
// path against the project directory by the same rule as the collections
// path - relative joins, absolute stays - and that it survives a reload.
func TestRecordProjectWritesRolesPath(t *testing.T) {
	t.Parallel()

	t.Run("relative", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		projectDir := t.TempDir()
		reqPath := filepath.Join(projectDir, "requirements.yml")

		if err := RecordProject(cacheDir, reqPath, "collections", "roles"); err != nil {
			t.Fatalf("RecordProject: %v", err)
		}
		record := mustLoadProjectRecord(t, cacheDir, projectDir)
		if want := filepath.Join(projectDir, "roles"); record.RolesPath != want {
			t.Fatalf("RolesPath = %q, want %q", record.RolesPath, want)
		}
		if want := filepath.Join(projectDir, "collections"); record.CollectionsPath != want {
			t.Fatalf("CollectionsPath = %q, want %q", record.CollectionsPath, want)
		}
	})

	t.Run("absolute", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		projectDir := t.TempDir()
		rolesDir := t.TempDir()
		reqPath := filepath.Join(projectDir, "requirements.yml")

		if err := RecordProject(cacheDir, reqPath, "collections", rolesDir); err != nil {
			t.Fatalf("RecordProject: %v", err)
		}
		if record := mustLoadProjectRecord(t, cacheDir, projectDir); record.RolesPath != rolesDir {
			t.Fatalf("RolesPath = %q, want %q", record.RolesPath, rolesDir)
		}
	})
}

// TestRecordProjectWithoutRolesPathStaysLegacyShape pins that omitempty keeps a
// collections-only record, requirements_files aside, byte-identical to the
// shape older binaries wrote, which they still decode.
func TestRecordProjectWithoutRolesPathStaysLegacyShape(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	projectDir := t.TempDir()
	reqPath := filepath.Join(projectDir, "requirements.yml")

	if err := RecordProject(cacheDir, reqPath, "collections", ""); err != nil {
		t.Fatalf("RecordProject: %v", err)
	}
	record := mustLoadProjectRecord(t, cacheDir, projectDir)
	if record.RolesPath != "" {
		t.Fatalf("RolesPath = %q, want empty", record.RolesPath)
	}

	if want := []string{reqPath}; !slices.Equal(record.RequirementsFiles, want) {
		t.Fatalf("RequirementsFiles = %q, want %q", record.RequirementsFiles, want)
	}

	// legacyRecord is the record's shape before RolesPath existed, tag for
	// tag; the two encodings must agree byte for byte once the list is set aside.
	type legacyRecord struct {
		LastRun          time.Time `json:"last_run"`
		RequirementsFile string    `json:"requirements_file"`
		CollectionsPath  string    `json:"collections_path"`
	}
	withoutList := record
	withoutList.RequirementsFiles = nil
	got, err := json.Marshal(withoutList)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	want, err := json.Marshal(legacyRecord{
		LastRun:          record.LastRun,
		RequirementsFile: record.RequirementsFile,
		CollectionsPath:  record.CollectionsPath,
	})
	if err != nil {
		t.Fatalf("marshal legacy record: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("a collections-only record changed shape:\n got %s\nwant %s", got, want)
	}
	if bytes.Contains(got, []byte("roles_path")) {
		t.Fatalf("a collections-only record carries roles_path: %s", got)
	}
}

// mustLoadProjectRecord reloads the registry under cacheDir and returns the
// record keyed by projectDir, failing the test when it is absent.
func mustLoadProjectRecord(t *testing.T, cacheDir, projectDir string) ProjectRecord {
	t.Helper()
	registry, err := LoadProjectRegistry(cacheDir)
	if err != nil {
		t.Fatalf("LoadProjectRegistry: %v", err)
	}
	record, ok := registry.Projects[projectDir]
	if !ok {
		t.Fatalf("no record under %q, got %#v", projectDir, registry.Projects)
	}
	return record
}

// TestNewProjectRecordKeysGalaxyTOMLByItsDirectory pins that a galaxy.toml
// project is keyed by its directory like a requirements.yml one and records
// the TOML file's own path in requirements_file, the one field cleanup reads.
func TestNewProjectRecordKeysGalaxyTOMLByItsDirectory(t *testing.T) {
	t.Parallel()
	key, record := NewProjectRecord("/p/galaxy.toml", "collections", "")
	if key != "/p" {
		t.Fatalf("key = %q, want /p", key)
	}
	if record.RequirementsFile != "/p/galaxy.toml" {
		t.Fatalf("RequirementsFile = %q, want /p/galaxy.toml", record.RequirementsFile)
	}
	if record.CollectionsPath != "/p/collections" {
		t.Fatalf("CollectionsPath = %q, want /p/collections", record.CollectionsPath)
	}
}

// TestRecordProjectOneRecordPerDirectory pins that galaxy.toml and
// requirements.yml in one directory share one registry entry: the later run
// names the latest file, and both are remembered while both exist.
func TestRecordProjectOneRecordPerDirectory(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	projectDir := t.TempDir()
	tomlPath := filepath.Join(projectDir, "galaxy.toml")
	yamlPath := filepath.Join(projectDir, "requirements.yml")
	for _, path := range []string{tomlPath, yamlPath} {
		if err := os.WriteFile(path, []byte("collections: []\n"), helpers.FileMod); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	if err := RecordProject(cacheDir, tomlPath, "collections", ""); err != nil {
		t.Fatalf("RecordProject (galaxy.toml): %v", err)
	}
	if got := mustLoadProjectRecord(t, cacheDir, projectDir).RequirementsFile; got != tomlPath {
		t.Fatalf("RequirementsFile = %q, want %q", got, tomlPath)
	}
	if err := RecordProject(cacheDir, yamlPath, "collections", ""); err != nil {
		t.Fatalf("RecordProject (requirements.yml): %v", err)
	}
	registry, err := LoadProjectRegistry(cacheDir)
	if err != nil {
		t.Fatalf("LoadProjectRegistry: %v", err)
	}
	if len(registry.Projects) != 1 {
		t.Fatalf("registry holds %d projects, want 1: %#v", len(registry.Projects), registry.Projects)
	}
	if got := registry.Projects[projectDir].RequirementsFile; got != yamlPath {
		t.Fatalf("RequirementsFile = %q, want the later %q", got, yamlPath)
	}
	if got, want := registry.Projects[projectDir].RequirementsFiles, []string{tomlPath, yamlPath}; !slices.Equal(got, want) {
		t.Fatalf("RequirementsFiles = %q, want both files, sorted: %q", got, want)
	}
}

// TestLoadProjectRegistryNamesItsFile pins Location, which cleanup's hints
// name: the registry path under cacheDir whether or not the file exists yet,
// and never a key in the bytes RecordProject writes.
func TestLoadProjectRegistryNamesItsFile(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	want := filepath.Join(cacheDir, helpers.StoreDBProjects)

	registry, err := LoadProjectRegistry(cacheDir)
	if err != nil {
		t.Fatalf("LoadProjectRegistry (absent): %v", err)
	}
	if registry.Location != want {
		t.Fatalf("Location (absent) = %q, want %q", registry.Location, want)
	}
	if err := RecordProject(cacheDir, filepath.Join(t.TempDir(), "requirements.yml"), "collections", ""); err != nil {
		t.Fatalf("RecordProject: %v", err)
	}
	registry, err = LoadProjectRegistry(cacheDir)
	if err != nil {
		t.Fatalf("LoadProjectRegistry (written): %v", err)
	}
	if registry.Location != want {
		t.Fatalf("Location (written) = %q, want %q", registry.Location, want)
	}
	data, err := os.ReadFile(want) // #nosec G304 -- want is built from this test's own t.TempDir
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if bytes.Contains(bytes.ToLower(data), []byte("location")) {
		t.Fatalf("the registry file carries its own location: %s", data)
	}
}

// TestRecordKeepsOnlyEarlierFilesThatStillExist pins the merge rule row by
// row: an earlier regular file is kept, a vanished one or one that is no
// longer a regular file is dropped, and a repeat of the latest is one entry.
func TestRecordKeepsOnlyEarlierFilesThatStillExist(t *testing.T) {
	t.Parallel()
	projectDir := t.TempDir()
	latest := filepath.Join(projectDir, "requirements.yml")
	kept := filepath.Join(projectDir, "requirements-dev.yml")
	for _, path := range []string{latest, kept} {
		if err := os.WriteFile(path, []byte("collections: []\n"), helpers.FileMod); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	vanished := filepath.Join(projectDir, "requirements-old.yml")
	directory := filepath.Join(projectDir, "dir.yml")
	if err := os.Mkdir(directory, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", directory, err)
	}

	registry := &ProjectRegistry{Projects: map[string]ProjectRecord{
		projectDir: {RequirementsFile: vanished, RequirementsFiles: []string{directory, kept, latest, vanished}},
	}}
	registry.Record(latest, "collections", "")

	want := []string{kept, latest}
	if got := registry.Projects[projectDir].RequirementsFiles; !slices.Equal(got, want) {
		t.Fatalf("RequirementsFiles = %q, want %q", got, want)
	}
	if got := registry.Projects[projectDir].RequirementsFile; got != latest {
		t.Fatalf("RequirementsFile = %q, want the latest %q", got, latest)
	}
}

// TestRecordKeepsAnEarlierFileStatCannotJudge pins the conservative side of
// the merge: a file under a directory this process may not search fails Stat
// with permission denied, not absence, and stays. Root searches it, so skip.
func TestRecordKeepsAnEarlierFileStatCannotJudge(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root searches a mode-000 directory")
	}
	projectDir := t.TempDir()
	latest := filepath.Join(projectDir, "requirements.yml")
	sealed := filepath.Join(projectDir, "sealed")
	if err := os.Mkdir(sealed, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", sealed, err)
	}
	hidden := filepath.Join(sealed, "requirements.yml")
	if err := os.WriteFile(hidden, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", hidden, err)
	}
	if err := os.Chmod(sealed, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", sealed, err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, helpers.DirMod) })
	if _, err := os.Stat(hidden); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Stat(%s) = %v, want a permission error for the fixture to mean anything", hidden, err)
	}

	registry := &ProjectRegistry{Projects: map[string]ProjectRecord{
		projectDir: {RequirementsFile: latest, RequirementsFiles: []string{hidden, latest}},
	}}
	registry.Record(latest, "collections", "")

	if got, want := registry.Projects[projectDir].RequirementsFiles, []string{latest, hidden}; !slices.Equal(got, want) {
		t.Fatalf("RequirementsFiles = %q, want %q", got, want)
	}
}

// TestRecordFoldsALegacyRecordsFile pins that a record written before the list
// existed contributes its requirements_file to the merge like any listed file,
// so the first record by this binary forgets nothing an older one knew.
func TestRecordFoldsALegacyRecordsFile(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	projectDir := t.TempDir()
	legacy := filepath.Join(projectDir, "galaxy.toml")
	latest := filepath.Join(projectDir, "requirements.yml")
	for _, path := range []string{legacy, latest} {
		if err := os.WriteFile(path, []byte("collections: []\n"), helpers.FileMod); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	data, err := json.Marshal(map[string]any{"projects": map[string]any{projectDir: map[string]any{
		"requirements_file": legacy, "collections_path": filepath.Join(projectDir, "collections"),
		"last_run": "2024-01-02T03:04:05Z",
	}}})
	if err != nil {
		t.Fatalf("marshal legacy registry: %v", err)
	}
	writeRegistryFile(t, cacheDir, data)

	if err := RecordProject(cacheDir, latest, "collections", ""); err != nil {
		t.Fatalf("RecordProject: %v", err)
	}
	if got, want := mustLoadProjectRecord(t, cacheDir, projectDir).RequirementsFiles, []string{legacy, latest}; !slices.Equal(got, want) {
		t.Fatalf("RequirementsFiles = %q, want %q", got, want)
	}
}

// TestProjectRecordFiles pins how a record reads: the list and the latest file
// together, sorted and deduplicated, so a record written without the list is
// its one file and a list out of step with requirements_file loses nothing.
func TestProjectRecordFiles(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name   string
		record ProjectRecord
		want   []string
	}{
		{name: "a record written before the list", record: ProjectRecord{RequirementsFile: "/p/a.yml"}, want: []string{"/p/a.yml"}},
		{
			name:   "a list holding the latest file",
			record: ProjectRecord{RequirementsFile: "/p/b.yml", RequirementsFiles: []string{"/p/a.yml", "/p/b.yml"}},
			want:   []string{"/p/a.yml", "/p/b.yml"},
		},
		{
			name:   "a list missing the latest file",
			record: ProjectRecord{RequirementsFile: "/p/c.yml", RequirementsFiles: []string{"/p/b.yml", "/p/a.yml"}},
			want:   []string{"/p/a.yml", "/p/b.yml", "/p/c.yml"},
		},
		{name: "an empty record", record: ProjectRecord{}, want: []string{}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := row.record.Files(); !slices.Equal(got, row.want) {
				t.Fatalf("Files() = %q, want %q", got, row.want)
			}
		})
	}
}
