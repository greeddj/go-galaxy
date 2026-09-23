package cleanup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// testRoleMarkerSHA is the digest every seeded role's extract marker names.
const testRoleMarkerSHA = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

// seedMarkedRole writes rolesDir/name carrying this tool's extract marker and
// a meta/main.yml depending on deps, the only evidence the scan reads with no
// snapshot record.
func seedMarkedRole(t *testing.T, rolesDir, name string, deps ...string) {
	t.Helper()
	metaDir := filepath.Join(rolesDir, name, "meta")
	if err := os.MkdirAll(metaDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create %s: %v", metaDir, err)
	}
	meta := "dependencies: []\n"
	if len(deps) > 0 {
		meta = "dependencies:\n  - role: " + strings.Join(deps, "\n  - role: ") + "\n"
	}
	if err := os.WriteFile(filepath.Join(metaDir, "main.yml"), []byte(meta), helpers.FileMod); err != nil {
		t.Fatalf("failed to write meta of role %s: %v", name, err)
	}
	marker := filepath.Join(rolesDir, name, helpers.ExtractMarkerPrefix+testRoleMarkerSHA)
	if err := os.WriteFile(marker, nil, helpers.FileMod); err != nil {
		t.Fatalf("failed to write marker of role %s: %v", name, err)
	}
}

// roleProject is one registered project of a role reachability case: its
// requirements body and the marked roles under its roles path.
type roleProject struct {
	roles        map[string][]string
	requirements string
}

type unreadRolesCase struct {
	projects map[string]roleProject
	name     string
	kept     []string
	removed  []string
}

// unreadRolesCases pins that project a's refused roles list keeps its roles
// as roots whose dependencies stay reachable, whether b names the same role
// or not; owner.stale, which nothing reaches, still goes.
func unreadRolesCases() []unreadRolesCase {
	const refused = "roles: \"not a list\"\n"
	const namesWeb = "roles:\n  - src: git+https://git.example/acme/web.git\n    name: web\n"
	return []unreadRolesCase{
		{
			name: "a later project names a kept role",
			projects: map[string]roleProject{
				"a": {requirements: refused, roles: map[string][]string{"web": nil}},
				"b": {requirements: namesWeb, roles: map[string][]string{
					"web": {"owner.common"}, "owner.common": nil, "owner.stale": nil,
				}},
			},
			kept:    []string{"a/roles/web", "b/roles/web", "b/roles/owner.common"},
			removed: []string{"b/roles/owner.stale"},
		},
		{
			name: "a kept role depends on a copy under another project",
			projects: map[string]roleProject{
				"a": {requirements: refused, roles: map[string][]string{"web": {"owner.common"}}},
				"b": {requirements: "roles: []\n", roles: map[string][]string{"owner.common": nil, "owner.stale": nil}},
			},
			kept:    []string{"a/roles/web", "b/roles/owner.common"},
			removed: []string{"b/roles/owner.stale"},
		},
	}
}

func TestUnreadRolesListKeepsDependenciesOfItsRoles(t *testing.T) {
	t.Parallel()
	for _, tc := range unreadRolesCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkUnreadRolesCase(t, tc)
		})
	}
}

func checkUnreadRolesCase(t *testing.T, tc unreadRolesCase) {
	t.Helper()
	base := t.TempDir()
	registry := &store.ProjectRegistry{Projects: make(map[string]store.ProjectRecord)}
	for dir, project := range tc.projects {
		projectPath := filepath.Join(base, dir)
		rolesPath := filepath.Join(projectPath, "roles")
		for name, deps := range project.roles {
			seedMarkedRole(t, rolesPath, name, deps...)
		}
		reqPath := filepath.Join(projectPath, "requirements.yml")
		if err := os.WriteFile(reqPath, []byte(project.requirements), helpers.FileMod); err != nil {
			t.Fatalf("failed to write requirements of project %s: %v", dir, err)
		}
		registry.Projects[projectPath] = store.ProjectRecord{
			RequirementsFile: reqPath,
			CollectionsPath:  filepath.Join(projectPath, "collections"),
			RolesPath:        rolesPath,
			LastRun:          time.Now().UTC(),
		}
	}
	cacheDir := t.TempDir()
	writeProjectRegistry(t, cacheDir, registry)

	printer := &recordingPrinter{}
	if err := Start(t.Context(), &config.Config{CacheDir: cacheDir}, newTestRuntimeWith(printer)); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	if !printer.hasWarningContaining("its roles are kept this run") {
		t.Fatalf("expected a warning about the unreadable roles list, got %q", printer.warnings)
	}
	for _, rel := range tc.kept {
		if _, err := os.Stat(filepath.Join(base, rel)); err != nil {
			t.Fatalf("expected role %s to be kept: %v", rel, err)
		}
	}
	for _, rel := range tc.removed {
		if _, err := os.Stat(filepath.Join(base, rel)); !os.IsNotExist(err) {
			t.Fatalf("expected unreachable role %s to be removed, stat error: %v", rel, err)
		}
	}
}
