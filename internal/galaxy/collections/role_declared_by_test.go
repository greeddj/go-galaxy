package collections

import (
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
)

// TestRootDeclaredBy pins that a root role is attributed to the base name of
// whichever file was read, and to the conventional requirements.yml when no
// path is configured, so a message never names the wrong file.
func TestRootDeclaredBy(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		cfg  *config.Config
		want string
	}{
		"galaxy.toml":      {cfg: &config.Config{RequirementsFile: "/proj/galaxy.toml"}, want: "galaxy.toml"},
		"requirements.yml": {cfg: &config.Config{RequirementsFile: "/proj/requirements.yml"}, want: "requirements.yml"},
		"custom name":      {cfg: &config.Config{RequirementsFile: "deps/req.yml"}, want: "req.yml"},
		"empty path":       {cfg: &config.Config{}, want: "requirements.yml"},
		"nil config":       {cfg: nil, want: "requirements.yml"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := rootDeclaredBy(tc.cfg); got != tc.want {
				t.Fatalf("rootDeclaredBy = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRoleFirstWinsWarningNamesTheRequirementsFile pins that the first-wins
// warning attributes a root to the file resolveRoles queued it from, so a
// galaxy.toml run reads galaxy.toml and a YAML run reads requirements.yml.
func TestRoleFirstWinsWarningNamesTheRequirementsFile(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"galaxy.toml", "requirements.yml"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			printer := &capturingPrinter{}
			deps := collectionDeps{cfg: &config.Config{RequirementsFile: "/proj/" + file}, runtime: infra.New(printer, nil)}
			declaredBy := rootDeclaredBy(deps.cfg)
			level := []roleRequest{
				{req: requirements.RoleRequirement{Name: "base", Src: "git+https://git.example/acme/base.git", Version: "v1"}, declaredBy: declaredBy},
				{req: requirements.RoleRequirement{Name: "base", Src: "git+https://git.example/acme/base.git", Version: "v2"}, declaredBy: declaredBy},
			}

			kept := dedupeRoleLevel(deps, map[string]resolvedRole{}, level)
			if len(kept) != 1 || kept[0].req.Version != "v1" {
				t.Fatalf("kept = %+v, want the first request alone", kept)
			}
			if len(printer.warns) != 1 || !strings.HasSuffix(printer.warns[0], "asked for by "+file+" (first wins, as in ansible-galaxy)") {
				t.Fatalf("warnings = %q, want one naming %s", printer.warns, file)
			}
		})
	}
}
