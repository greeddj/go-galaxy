package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func galaxyRoleEntry() RoleEntry {
	return RoleEntry{
		Name:       "geerlingguy.docker",
		Type:       RoleTypeGalaxy,
		Version:    "7.4.1",
		Galaxy:     "geerlingguy.docker",
		Source:     "https://galaxy.ansible.com",
		Repository: "https://github.com/geerlingguy/ansible-role-docker",
		Ref:        "7.4.1",
		Commit:     gitTestCommit,
		Deps:       []string{"base"},
	}
}

func gitRoleEntry() RoleEntry {
	return RoleEntry{
		Name:    "base",
		Type:    RoleTypeGit,
		Version: "main",
		Source:  "https://git.example/acme/base.git",
		Ref:     "main",
		Commit:  gitTestCommit,
	}
}

// TestRolesSchemaFollowsTheEntries pins that a file with a role is written as
// schema 3 and round-trips in canonical order, with no sha256 on a role entry;
// TestRolesSchemaDropsBackWithoutRoles covers removing the last role.
func TestRolesSchemaFollowsTheEntries(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), DefaultName)
	f := &File{
		Server:      "https://galaxy.ansible.com",
		Collections: []Entry{gitEntry()},
		Roles:       []RoleEntry{galaxyRoleEntry(), gitRoleEntry()},
	}
	loaded := saveAndLoad(t, path, f)
	if loaded.SchemaVersion != SchemaVersionRoles {
		t.Fatalf("schema = %d, want %d", loaded.SchemaVersion, SchemaVersionRoles)
	}
	if len(loaded.Roles) != 2 || loaded.Roles[0].Name != "base" {
		t.Fatalf("roles did not round-trip in canonical order: %+v", loaded.Roles)
	}
	assertGalaxyRoleEntry(t, loaded.Roles[1])
	data, err := os.ReadFile(path) //nolint:gosec // the file this test just wrote
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "roles:\n") || !strings.Contains(string(data), "schema_version: 3\n") {
		t.Fatalf("saved file lacks the roles list or schema 3:\n%s", data)
	}
	if strings.Contains(string(data), "sha256") {
		t.Fatalf("a role entry must carry no sha256:\n%s", data)
	}
}

// TestRolesSchemaDropsBackWithoutRoles pins that removing the last role
// takes the file back to the schema its collections warrant, with no
// mention of roles left in it.
func TestRolesSchemaDropsBackWithoutRoles(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), DefaultName)
	f := &File{Collections: []Entry{gitEntry()}, Roles: []RoleEntry{gitRoleEntry()}}
	saveAndLoad(t, path, f)
	f.Roles = nil
	loaded := saveAndLoad(t, path, f)
	if loaded.SchemaVersion != SchemaVersionGit {
		t.Fatalf("schema after removing the roles = %d, want %d", loaded.SchemaVersion, SchemaVersionGit)
	}
	withoutRoles, err := os.ReadFile(path) //nolint:gosec // the file this test just wrote
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(withoutRoles), "roles") {
		t.Fatalf("a file without roles must not mention them:\n%s", withoutRoles)
	}
	if got := SchemaVersionFor(nil, []RoleEntry{gitRoleEntry()}); got != SchemaVersionRoles {
		t.Fatalf("SchemaVersionFor(roles) = %d", got)
	}
}

// assertGalaxyRoleEntry checks got against galaxyRoleEntry field by field:
// Load returns a fresh Deps slice, so the structs are not comparable.
func assertGalaxyRoleEntry(t *testing.T, got RoleEntry) {
	t.Helper()
	want := galaxyRoleEntry()
	if !sameRoleEntry(got, want) || got.Name != want.Name {
		t.Fatalf("galaxy role entry = %+v, want %+v", got, want)
	}
}

// TestHashCoversRoleFields pins that every role field moves the hash, and
// that role order does not.
func TestHashCoversRoleFields(t *testing.T) {
	t.Parallel()
	base := &File{Collections: []Entry{gitEntry()}, Roles: []RoleEntry{galaxyRoleEntry(), gitRoleEntry()}}
	baseHash := mustHash(t, base)
	reordered := &File{Collections: []Entry{gitEntry()}, Roles: []RoleEntry{gitRoleEntry(), galaxyRoleEntry()}}
	if mustHash(t, reordered) != baseHash {
		t.Fatalf("role order moved the hash")
	}
	mutations := map[string]func(e *RoleEntry){
		"version":    func(e *RoleEntry) { e.Version = "7.4.2" },
		"commit":     func(e *RoleEntry) { e.Commit = strings.Repeat("f", 40) },
		"ref":        func(e *RoleEntry) { e.Ref = "main" },
		"repository": func(e *RoleEntry) { e.Repository = "https://github.com/acme/other" },
		"source":     func(e *RoleEntry) { e.Source = "https://hub.example" },
		"deps":       func(e *RoleEntry) { e.Deps = nil },
		"galaxy":     func(e *RoleEntry) { e.Galaxy = "acme.docker" },
	}
	for field, mutate := range mutations {
		e := galaxyRoleEntry()
		mutate(&e)
		f := &File{Collections: []Entry{gitEntry()}, Roles: []RoleEntry{e, gitRoleEntry()}}
		if mustHash(t, f) == baseHash {
			t.Errorf("changing %s did not move the hash", field)
		}
	}
}

func mustHash(t *testing.T, f *File) string {
	t.Helper()
	h, err := f.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	return h
}

type roleLoadCase struct {
	mutate  func(e *RoleEntry)
	name    string
	wantMsg string
	schema  int
}

func roleLoadCases() []roleLoadCase {
	return []roleLoadCase{
		{name: "schema 2 with a role", schema: SchemaVersionGit, wantMsg: "requires schema_version 3"},
		{name: "bad install name", mutate: func(e *RoleEntry) { e.Name = ".hidden" }, wantMsg: "not a role install name"},
		{name: "bad version", mutate: func(e *RoleEntry) { e.Version = "a b" }, wantMsg: "not a role version"},
		{name: "bad ref", mutate: func(e *RoleEntry) { e.Ref = "" }, wantMsg: "not a canonical git ref"},
		{name: "short commit", mutate: func(e *RoleEntry) { e.Commit = "0123456" }, wantMsg: "40-hex commit"},
		{name: "bad dep", mutate: func(e *RoleEntry) { e.Deps = []string{"a/b"} }, wantMsg: "not a role install name"},
		{name: "unknown type", mutate: func(e *RoleEntry) { e.Type = "hg" }, wantMsg: "unsupported role type"},
		{name: "galaxy without repository", mutate: func(e *RoleEntry) { e.Repository = "" }, wantMsg: "repository is not a canonical"},
		{name: "galaxy bad name", mutate: func(e *RoleEntry) { e.Galaxy = "docker" }, wantMsg: "not owner.role"},
		{name: "galaxy source userinfo", mutate: func(e *RoleEntry) { e.Source = "https://u:p@hub.example" }, wantMsg: "userinfo"},
		{name: "git with galaxy fields", mutate: func(e *RoleEntry) {
			*e = gitRoleEntry()
			e.Galaxy = "a.b"
		}, wantMsg: "belong to a galaxy role"},
		{name: "git non-canonical source", mutate: func(e *RoleEntry) {
			*e = gitRoleEntry()
			e.Source = "https://git.example/acme/base.git/"
		}, wantMsg: "not a canonical git repository URL"},
	}
}

func TestLoadJudgesRoleEntries(t *testing.T) {
	t.Parallel()
	for _, tc := range roleLoadCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := galaxyRoleEntry()
			if tc.mutate != nil {
				tc.mutate(&e)
			}
			schema := tc.schema
			if schema == 0 {
				schema = SchemaVersionRoles
			}
			path := writeRawRoleLockfile(t, schema, e)
			_, err := Load(path)
			if !errors.Is(err, helpers.ErrLockfileInvalid) || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("Load = %v, want ErrLockfileInvalid with %q", err, tc.wantMsg)
			}
		})
	}
}

// writeRawRoleLockfile writes a lockfile by hand, bypassing Save's
// canonicalization, so a schema a producer would never write can be tested.
func writeRawRoleLockfile(t *testing.T, schema int, e RoleEntry) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("collections: []\nroles:\n")
	b.WriteString("  - name: " + quoteYAML(e.Name) + "\n")
	b.WriteString("    type: " + quoteYAML(e.Type) + "\n")
	b.WriteString("    version: " + quoteYAML(e.Version) + "\n")
	if e.Galaxy != "" {
		b.WriteString("    galaxy: " + quoteYAML(e.Galaxy) + "\n")
	}
	b.WriteString("    source: " + quoteYAML(e.Source) + "\n")
	if e.Repository != "" {
		b.WriteString("    repository: " + quoteYAML(e.Repository) + "\n")
	}
	b.WriteString("    ref: " + quoteYAML(e.Ref) + "\n")
	b.WriteString("    commit: " + quoteYAML(e.Commit) + "\n")
	if len(e.Deps) > 0 {
		b.WriteString("    deps:\n")
		for _, d := range e.Deps {
			b.WriteString("      - " + quoteYAML(d) + "\n")
		}
	}
	b.WriteString("schema_version: " + itoa(schema) + "\n")
	path := filepath.Join(t.TempDir(), DefaultName)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestLoadAcceptsAHandWrittenSchemaThreeFile pins the on-disk shape a
// reader meets, spelled out rather than produced by Save.
func TestLoadAcceptsAHandWrittenSchemaThreeFile(t *testing.T) {
	t.Parallel()
	raw := `server: https://galaxy.ansible.com
collections:
  - name: community.general
    version: "11.1.0"
    source: https://galaxy.ansible.com
    sha256: ` + strings.Repeat("a", 64) + `
roles:
  - name: geerlingguy.docker
    type: galaxy
    version: "7.4.1"
    galaxy: geerlingguy.docker
    source: https://galaxy.ansible.com
    repository: https://github.com/geerlingguy/ansible-role-docker
    ref: "7.4.1"
    commit: ` + gitTestCommit + `
  - name: base
    type: git
    version: main
    source: https://git.example/acme/base.git
    ref: main
    commit: ` + gitTestCommit + `
    deps: [common]
schema_version: 3
`
	path := filepath.Join(t.TempDir(), DefaultName)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Roles) != 2 || f.Roles[0].RepositoryURL() != "https://github.com/geerlingguy/ansible-role-docker" ||
		f.Roles[1].RepositoryURL() != "https://git.example/acme/base.git" || f.Roles[1].Deps[0] != "common" {
		t.Fatalf("roles = %+v", f.Roles)
	}
}

// TestCompareReportsRoleFields pins the role half of Compare: added, updated
// with the changed fields named, removed, and Empty/HasRoles reading them.
func TestCompareReportsRoleFields(t *testing.T) {
	t.Parallel()
	before := &File{Roles: []RoleEntry{galaxyRoleEntry(), gitRoleEntry()}}
	changed := galaxyRoleEntry()
	changed.Version, changed.Ref, changed.Commit = "7.5.0", "7.5.0", strings.Repeat("e", 40)
	added := RoleEntry{
		Name: "extra", Type: RoleTypeGit, Version: "v1", Source: "https://git.example/x/extra.git", Ref: "v1", Commit: gitTestCommit,
	}
	after := &File{Roles: []RoleEntry{changed, added}}
	diff := Compare(before, after)
	if diff.Empty() || !diff.HasRoles() {
		t.Fatalf("diff = %+v, want a role diff", diff)
	}
	if len(diff.RolesAdded) != 1 || diff.RolesAdded[0].Name != "extra" {
		t.Fatalf("RolesAdded = %+v", diff.RolesAdded)
	}
	if len(diff.RolesRemoved) != 1 || diff.RolesRemoved[0].Name != "base" {
		t.Fatalf("RolesRemoved = %+v", diff.RolesRemoved)
	}
	if len(diff.RolesUpdated) != 1 {
		t.Fatalf("RolesUpdated = %+v", diff.RolesUpdated)
	}
	if got := changedRoleFields(diff.RolesUpdated[0]); got != "version,ref,commit" {
		t.Fatalf("changed fields = %s, want version,ref,commit", got)
	}
}

// changedRoleFields joins the names of a role change's fields.
func changedRoleFields(c RoleChange) string {
	fields := c.Fields()
	names := make([]string, 0, len(fields))
	for _, fc := range fields {
		names = append(names, fc.Field)
	}
	return strings.Join(names, ",")
}

// TestCompareRolesIgnoresOrder pins that a file compared with itself, or
// with its roles reordered, reads as no drift.
func TestCompareRolesIgnoresOrder(t *testing.T) {
	t.Parallel()
	before := &File{Roles: []RoleEntry{galaxyRoleEntry(), gitRoleEntry()}}
	if !Compare(before, before).Empty() {
		t.Fatalf("a file compared with itself is not empty")
	}
	same := &File{Roles: []RoleEntry{gitRoleEntry(), galaxyRoleEntry()}}
	if !Compare(before, same).Empty() {
		t.Fatalf("role order alone reads as drift")
	}
}
