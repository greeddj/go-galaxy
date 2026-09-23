package collections

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// testRolesPathWarning stands in for whatever config queued on
// config.Config.RoleWarnings. loadRoots decides whether to print it without
// reading it, so the text only has to be recognizable in the assertions.
const testRolesPathWarning = `roles_path lists multiple paths; using "./roles" and ignoring the rest: [./shared/roles]`

// runLoadRoots drives loadRoots over body with a queued roles_path warning
// and returns the role root count and every roles_path warning printed.
func runLoadRoots(t *testing.T, body string) (int, []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(path, []byte(body), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
	cfg := &config.Config{RequirementsFile: path, RoleWarnings: []string{testRolesPathWarning}}
	printer := &capturingPrinter{}

	_, roleRoots, err := loadRoots(cfg, infra.New(printer, nil))
	if err != nil {
		t.Fatalf("loadRoots: %v", err)
	}
	var warns []string
	for _, w := range printer.warns {
		if strings.Contains(w, "roles_path") {
			warns = append(warns, w)
		}
	}
	return len(roleRoots), warns
}

// TestRoleConfigWarningsSuppressedWithoutRolesBlock pins that a queued
// roles_path warning stays silent when the requirements file has no roles:
// block or an empty one, since such a run never reads roles_path.
func TestRoleConfigWarningsSuppressedWithoutRolesBlock(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{
		"no roles key":     "collections:\n  - name: acme.app\n    version: \"1.0.0\"\n",
		"empty roles list": "collections:\n  - name: acme.app\n    version: \"1.0.0\"\nroles: []\n",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			roleRoots, warns := runLoadRoots(t, body)
			if roleRoots != 0 {
				t.Fatalf("roleRoots = %d, want 0 for this fixture", roleRoots)
			}
			if len(warns) != 0 {
				t.Fatalf("warnings = %q, want none - this run never reads roles_path", warns)
			}
		})
	}
}

// TestRoleConfigWarningsShownWithRolesBlock is the other half: suppression
// must not swallow the warning for the run it was written for.
func TestRoleConfigWarningsShownWithRolesBlock(t *testing.T) {
	t.Parallel()
	roleRoots, warns := runLoadRoots(t,
		"roles:\n  - src: git+https://example.com/org/role-app.git\n    name: app\n")
	if roleRoots != 1 {
		t.Fatalf("roleRoots = %d, want 1", roleRoots)
	}
	if len(warns) != 1 || warns[0] != testRolesPathWarning {
		t.Fatalf("warnings = %q, want exactly the queued %q", warns, testRolesPathWarning)
	}
}
