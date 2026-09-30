package cleanup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// standInLock pins ns.name 1.0.0 as a Galaxy entry, the shape lock writes.
const standInLock = `server: https://galaxy.example
collections:
  - name: ns.name
    version: 1.0.0
    source: https://galaxy.example
    download_url: https://galaxy.example/download/ns-name-1.0.0.tar.gz
    sha256: 2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae
schema_version: 5
`

// standInCase is one project directory whose recorded file is gone: the
// files written beside it and what cleanup must do with ns.name's install.
type standInCase struct {
	files    map[string]string
	wantErr  error
	name     string
	recorded string
	fifo     string
	wantKept bool
}

func standInCases() []standInCase {
	return []standInCase{
		{name: "nothing beside it aborts", recorded: "requirements.yml",
			wantErr: helpers.ErrProjectRequirementsMissing, wantKept: true},
		{name: "a galaxy.toml stands in", recorded: "requirements.yml",
			files: map[string]string{"galaxy.toml": "[project]\ncollections = [\"ns.name\"]\n"}, wantKept: true},
		{name: "a requirements.yml stands in for a gone galaxy.toml", recorded: "galaxy.toml",
			files: map[string]string{"requirements.yml": "collections:\n  - name: ns.name\n"}, wantKept: true},
		{name: "a galaxy.lock stands in", recorded: "requirements.yml",
			files: map[string]string{"galaxy.lock": standInLock}, wantKept: true},
		{name: "a stand-in naming nothing lets the install go", recorded: "requirements.yml",
			files: map[string]string{"galaxy.toml": "[project]\ncollections = []\n"}},
		{name: "a broken galaxy.toml aborts", recorded: "requirements.yml",
			files:   map[string]string{"galaxy.toml": "[project\n"},
			wantErr: helpers.ErrProjectRequirementsUnreadable, wantKept: true},
		{name: "a broken galaxy.lock aborts", recorded: "requirements.yml",
			files:   map[string]string{"galaxy.lock": "schema_version: 0\n"},
			wantErr: helpers.ErrLockfileInvalid, wantKept: true},
		{name: "a galaxy.lock pipe aborts without blocking", recorded: "requirements.yml",
			fifo: "galaxy.lock", wantErr: helpers.ErrLockfileInvalid, wantKept: true},
	}
}

// TestStartReadsStandInsForAGoneFile pins that once a recorded file is gone
// from a remaining directory, its galaxy.toml, requirements.yml and galaxy.lock
// stand in, each under the per-file policy, and none at all aborts.
func TestStartReadsStandInsForAGoneFile(t *testing.T) {
	t.Parallel()
	for _, tc := range standInCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cacheDir := t.TempDir()
			downloadPath := t.TempDir()
			installDir := seedInstallTree(t, downloadPath)
			projectDir := t.TempDir()
			for name, body := range tc.files {
				if err := os.WriteFile(filepath.Join(projectDir, name), []byte(body), helpers.FileMod); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}
			if tc.fifo != "" {
				if err := syscall.Mkfifo(filepath.Join(projectDir, tc.fifo), 0o600); err != nil {
					t.Skipf("mkfifo unsupported: %v", err)
				}
			}
			registerCleanupProjectAt(t, cacheDir, downloadPath, filepath.Join(projectDir, tc.recorded))

			err := Start(t.Context(), &config.Config{CacheDir: cacheDir}, newTestRuntime())
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Start: %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && err != nil {
				t.Fatalf("Start: %v, want success", err)
			}
			assertInstallTreeKept(t, installDir, tc.wantKept)
		})
	}
}

// TestStartReadsStandInsBesideARemainingFile pins that one gone file of two
// is stood in for too, so a galaxy.toml replacing it keeps what it reached
// while the other remembered file still loads.
func TestStartReadsStandInsBesideARemainingFile(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	projectDir := t.TempDir()
	dev := filepath.Join(projectDir, "requirements-dev.yml")
	if err := os.WriteFile(dev, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", dev, err)
	}
	toml := filepath.Join(projectDir, helpers.RequirementsTOMLName)
	if err := os.WriteFile(toml, []byte("[project]\ncollections = [\"ns.name\"]\n"), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", toml, err)
	}
	gone := filepath.Join(projectDir, helpers.RequirementsYAMLName)
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{Projects: map[string]store.ProjectRecord{
		projectDir: {RequirementsFile: dev, RequirementsFiles: []string{dev, gone}, CollectionsPath: downloadPath, LastRun: time.Now().UTC()},
	}})

	printer := &recordingPrinter{}
	if err := Start(t.Context(), &config.Config{CacheDir: cacheDir}, newTestRuntimeWith(printer)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !printer.hasWarningContaining(`keeping what "` + toml + `" reaches`) {
		t.Fatalf("expected the stand-in warning naming %s, got %v", toml, printer.warnings)
	}
	assertManifestSurvives(t, installDir)
}

// TestStartSkipsARecordedStandIn pins that a galaxy.toml the record already
// names is read once as a remembered file, never again as a stand-in for the
// gone requirements.yml beside it.
func TestStartSkipsARecordedStandIn(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	projectDir := t.TempDir()
	toml := filepath.Join(projectDir, helpers.RequirementsTOMLName)
	if err := os.WriteFile(toml, []byte("[project]\ncollections = [\"ns.name\"]\n"), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", toml, err)
	}
	gone := filepath.Join(projectDir, helpers.RequirementsYAMLName)
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{Projects: map[string]store.ProjectRecord{
		projectDir: {RequirementsFile: toml, RequirementsFiles: []string{toml, gone}, CollectionsPath: downloadPath, LastRun: time.Now().UTC()},
	}})

	printer := &recordingPrinter{}
	if err := Start(t.Context(), &config.Config{CacheDir: cacheDir}, newTestRuntimeWith(printer)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if printer.hasWarningContaining(`keeping what "` + toml + `"`) {
		t.Fatalf("expected %s read only as a remembered file, got warnings %v", toml, printer.warnings)
	}
	assertManifestSurvives(t, installDir)
}

// TestStartNamesEveryProjectLeftWithNothingToRead pins that one error names
// every such project, sorted, and the registry to edit, so one pass fixes all.
func TestStartNamesEveryProjectLeftWithNothingToRead(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	a, b := t.TempDir(), t.TempDir()
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{Projects: map[string]store.ProjectRecord{
		a: {RequirementsFile: filepath.Join(a, "requirements.yml"), LastRun: time.Now().UTC()},
		b: {RequirementsFile: filepath.Join(b, "requirements.yml"), LastRun: time.Now().UTC()},
	}})
	err := Start(t.Context(), &config.Config{CacheDir: cacheDir}, newTestRuntime())
	if !errors.Is(err, helpers.ErrProjectRequirementsMissing) {
		t.Fatalf("Start: %v, want ErrProjectRequirementsMissing", err)
	}
	for _, want := range []string{
		`"` + a + `"`, `"` + b + `"`, filepath.Join(cacheDir, helpers.StoreDBProjects),
		"(for each: restore a galaxy.toml, requirements.yml or galaxy.lock in that directory, " +
			"or, if no run there reads one any more, delete its entry from ",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Start error %q does not name %s", err, want)
		}
	}
	first, second := min(a, b), max(a, b)
	if strings.Index(err.Error(), `"`+first+`"`) > strings.Index(err.Error(), `"`+second+`"`) {
		t.Fatalf("Start error %q does not name %s before %s", err, first, second)
	}
}

// TestStartLeavesStandInsUnreadWhileEveryFileLoads pins that a broken
// galaxy.toml or galaxy.lock beside remembered files that all load is not
// read: the run succeeds and keeps only what those files reach.
func TestStartLeavesStandInsUnreadWhileEveryFileLoads(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	projectDir := t.TempDir()
	for name, body := range map[string]string{
		helpers.RequirementsYAMLName: "collections: []\n",
		helpers.RequirementsTOMLName: "[project\n",
		"galaxy.lock":                "schema_version: 0\n",
	} {
		if err := os.WriteFile(filepath.Join(projectDir, name), []byte(body), helpers.FileMod); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, filepath.Join(projectDir, helpers.RequirementsYAMLName))

	printer := &recordingPrinter{}
	if err := Start(t.Context(), &config.Config{CacheDir: cacheDir}, newTestRuntimeWith(printer)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if printer.hasWarningContaining("keeping what") {
		t.Fatalf("expected no stand-in read, got warnings %v", printer.warnings)
	}
	assertInstallTreeKept(t, installDir, false)
}

// standInLockWithRole pins ns.name 1.0.0 and the git role web, nothing else.
const standInLockWithRole = `server: https://galaxy.example
collections:
  - name: ns.name
    version: 1.0.0
    source: https://galaxy.example
    download_url: https://galaxy.example/download/ns-name-1.0.0.tar.gz
    sha256: 2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae
roles:
  - name: web
    type: git
    version: main
    source: https://git.example/acme/web.git
    ref: main
    commit: 0123456789abcdef0123456789abcdef01234567
schema_version: 5
`

// TestStartKeepsExactlyWhatAStandInLockPins pins that a galaxy.lock standing
// in keeps the collection keys and role names it pins and nothing more.
func TestStartKeepsExactlyWhatAStandInLockPins(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	seedManifestAt(t, downloadPath, "other", "coll", "1.0.0")
	markCollectionInstalled(t, downloadPath, "other", "coll", "1.0.0")
	rolesDir := t.TempDir()
	seedMarkedRole(t, rolesDir, "web")
	seedMarkedRole(t, rolesDir, "stale")
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "galaxy.lock"), []byte(standInLockWithRole), helpers.FileMod); err != nil {
		t.Fatalf("write galaxy.lock: %v", err)
	}
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{Projects: map[string]store.ProjectRecord{
		projectDir: {RequirementsFile: filepath.Join(projectDir, "requirements.yml"), CollectionsPath: downloadPath,
			RolesPath: rolesDir, LastRun: time.Now().UTC()},
	}})
	if err := Start(t.Context(), &config.Config{CacheDir: cacheDir}, newTestRuntime()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestSurvives(t, installDir)
	assertManifestAbsentAt(t, downloadPath, "other", "coll")
	if _, err := os.Stat(filepath.Join(rolesDir, "web")); err != nil {
		t.Fatalf("expected role web kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rolesDir, "stale")); !os.IsNotExist(err) {
		t.Fatalf("expected role stale removed, stat error: %v", err)
	}
}
