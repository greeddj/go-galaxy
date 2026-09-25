package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

func roleLockfile() *lockfile.File {
	return &lockfile.File{
		SchemaVersion: lockfile.SchemaVersionRoles,
		Collections: []lockfile.Entry{{
			Name: "acme.app", Version: "1.0.0", Source: "https://galaxy.example", DownloadURL: testDownloadURL("acme.app", "1.0.0"),
		}},
		Roles: []lockfile.RoleEntry{
			{Name: "base", Type: lockfile.RoleTypeGit, Version: "main", Source: gitTestSource, Ref: "main", Commit: gitTestCommit},
			{
				Name: "geerlingguy.docker", Type: lockfile.RoleTypeGalaxy, Version: "7.4.1", Galaxy: "geerlingguy.docker",
				Source: "https://galaxy.example", Repository: "https://github.com/geerlingguy/ansible-role-docker",
				Ref: "7.4.1", Commit: gitTestCommit, Deps: []string{"base", "missing"},
			},
		},
	}
}

// TestPrintRoleTreeRendersRoles pins the roles half of tree's output: its own
// header, one tree per root, provenance on each dependency, a missing one named,
// and nothing for a lockfile without roles.
func TestPrintRoleTreeRendersRoles(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	printRoleTree(&buf, roleLockfile(), []string{"geerlingguy.docker"})
	want := strings.Join([]string{
		"roles:",
		"└── geerlingguy.docker 7.4.1 (galaxy geerlingguy.docker via https://github.com/geerlingguy/ansible-role-docker @" + gitTestCommit + ")",
		"    ├── base main (git " + gitTestSource + " @" + gitTestCommit + ")",
		"    └── missing (missing in lockfile)",
		"",
	}, "\n")
	if buf.String() != want {
		t.Fatalf("printRoleTree() =\n%s\nwant\n%s", buf.String(), want)
	}

	var empty strings.Builder
	printRoleTree(&empty, &lockfile.File{Collections: roleLockfile().Collections}, nil)
	if empty.Len() != 0 {
		t.Fatalf("a lockfile without roles printed %q", empty.String())
	}
}

// TestLoadRootFQDNsReadsRoles pins that the requirements file's roles come
// back as roots beside the collections.
func TestLoadRootFQDNsReadsRoles(t *testing.T) {
	t.Parallel()
	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	body := "collections:\n  - acme.app\nroles:\n  - geerlingguy.docker\n  - src: git+" + gitTestSource + "\n    name: base\n"
	if err := os.WriteFile(reqPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	roots, roleRoots, err := loadRootFQDNs(reqPath, roleLockfile())
	if err != nil {
		t.Fatalf("loadRootFQDNs: %v", err)
	}
	if strings.Join(roots, ",") != "acme.app" || strings.Join(roleRoots, ",") != "geerlingguy.docker,base" {
		t.Fatalf("roots = %v, roleRoots = %v", roots, roleRoots)
	}
}

// TestPrintExplainRole pins explain's role section: reachable by install
// name, with provenance, parents and dependencies.
func TestPrintExplainRole(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	roots := map[string]bool{"geerlingguy.docker": true}
	if err := printExplain(&buf, roleLockfile(), "geerlingguy.docker", "requirements.yml", nil, roots); err != nil {
		t.Fatalf("printExplain: %v", err)
	}
	for _, want := range []string{
		"role geerlingguy.docker 7.4.1", "type       : galaxy", "galaxy     : geerlingguy.docker",
		"repository : https://github.com/geerlingguy/ansible-role-docker", "commit     : " + gitTestCommit,
		"    - requirements.yml (root)", "  depends on:", "    - role base", "    - role missing",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("printExplain lacks %q:\n%s", want, buf.String())
		}
	}
}

// TestPrintExplainRoleParents pins that a dependency names the roles that
// require it and is not an orphan.
func TestPrintExplainRoleParents(t *testing.T) {
	t.Parallel()
	var base strings.Builder
	if err := printExplain(&base, roleLockfile(), "base", "requirements.yml", nil, nil); err != nil {
		t.Fatalf("printExplain(base): %v", err)
	}
	if !strings.Contains(base.String(), "    - role geerlingguy.docker 7.4.1") || strings.Contains(base.String(), "orphan") {
		t.Fatalf("base's parents:\n%s", base.String())
	}
}

// TestPrintExplainRoleHeaderByType pins that a role's header prints the pin its
// type carries, a url role's sha256 where the others print ref and commit, and
// no line for a field its type leaves empty.
func TestPrintExplainRoleHeaderByType(t *testing.T) {
	t.Parallel()
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	path := filepath.Join(t.TempDir(), "galaxy.lock")
	body := "schema_version: 4\nroles:\n" +
		"  - {name: base, type: git, version: main, source: '" + gitTestSource + "', ref: main, commit: " + gitTestCommit + "}\n" +
		"  - {name: docker, type: galaxy, version: 7.4.1, galaxy: geerlingguy.docker, source: 'https://galaxy.example'," +
		" repository: 'https://github.com/geerlingguy/ansible-role-docker', ref: 7.4.1, commit: " + gitTestCommit + "}\n" +
		"  - {name: myrole, type: url, version: 0123456789ab, source: 'https://example.com/roles/myrole.tar.gz', sha256: " + digest + "}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	lf, err := lockfile.LoadRequired(path)
	if err != nil {
		t.Fatalf("LoadRequired: %v", err)
	}
	roots := map[string]bool{"base": true, "docker": true, "myrole": true}
	for target, header := range map[string][]string{
		"base": {"role base main", "  type       : git", "  source     : " + gitTestSource,
			"  ref        : main", "  commit     : " + gitTestCommit},
		"docker": {"role docker 7.4.1", "  type       : galaxy", "  galaxy     : geerlingguy.docker",
			"  source     : https://galaxy.example", "  repository : https://github.com/geerlingguy/ansible-role-docker",
			"  ref        : 7.4.1", "  commit     : " + gitTestCommit},
		"myrole": {"role myrole 0123456789ab", "  type       : url",
			"  source     : https://example.com/roles/myrole.tar.gz", "  sha256     : " + digest},
	} {
		var buf strings.Builder
		if err := printExplain(&buf, lf, target, "requirements.yml", nil, roots); err != nil {
			t.Fatalf("printExplain(%s): %v", target, err)
		}
		want := strings.Join(append(header, "  required by:", "    - requirements.yml (root)", ""), "\n")
		if buf.String() != want {
			t.Errorf("printExplain(%s) =\n%s\nwant\n%s", target, buf.String(), want)
		}
	}
}

// TestPrintExplainBothKinds pins that a name matching both a collection and
// a role prints both sections, the collection first, and that a name
// matching neither names both kinds in its refusal.
func TestPrintExplainBothKinds(t *testing.T) {
	t.Parallel()
	both := roleLockfile()
	both.Roles = append(both.Roles, lockfile.RoleEntry{
		Name: "acme.app", Type: lockfile.RoleTypeGit, Version: "v1", Source: gitTestSource, Ref: "v1", Commit: gitTestCommit,
	})
	var out strings.Builder
	if err := printExplain(&out, both, "acme.app", "requirements.yml", map[string]bool{"acme.app": true}, nil); err != nil {
		t.Fatalf("printExplain(both): %v", err)
	}
	if !strings.HasPrefix(out.String(), "acme.app 1.0.0\n") || !strings.Contains(out.String(), "\nrole acme.app v1\n") {
		t.Fatalf("a name that is both prints both sections, collection first:\n%s", out.String())
	}
	err := printExplain(&strings.Builder{}, both, "nothing", "requirements.yml", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "collection or role") {
		t.Fatalf("unknown target: %v", err)
	}
}
