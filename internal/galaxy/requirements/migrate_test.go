package requirements

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// documentedRequirementsYAML is documentedGalaxyTOML spelled as a
// requirements.yml, every entry a mapping, with a comment for migrate to name.
const documentedRequirementsYAML = `---
# go-galaxy requirements
collections:
  - name: sc.internal
    version: '>= 0.0.20'
  - name: ansible.utils
  - name: community.crypto
    version: '>= 2.0, < 3.0'
  - name: community.general
    version: '== 11.1.0'
  - name: acme.legacy
    version: '~1.5'
  - name: git+https://git.example.com/acme/mono.git#collections/app,main
  - name: https://dl.example.com/acme-app-1.4.0.tar.gz
  - name: acme.app
    version: '>= 1.4.0'
    source: automation_hub
  - name: acme.signed
    version: '*'
    signatures:
      - https://keys.example.com/a.asc
  - name: acme.net
    type: git
    source: https://git.example.com/acme/net.git
    version: v2.0.1
  - name: https://dl.example.com/acme-lib-2.1.0.tar.gz
    type: url
    version: 2.1.0
roles:
  - src: geerlingguy.docker
    version: 7.4.1
  - src: git+https://github.com/acme/ansible-role-nginx.git
    version: v1.2.0
    name: nginx
  - src: https://dl.example.com/acme-role-1.0.0.tar.gz
  - name: postgres
    src: geerlingguy.postgresql
    version: 3.5.0
`

// documentedMigration is the galaxy.toml migrate writes from
// documentedRequirementsYAML in a directory named infra.
const documentedMigration = `[project]
name = "infra"
collections = [
  "sc.internal >= 0.0.20",
  "ansible.utils",
  "community.crypto >= 2.0, < 3.0",
  "community.general == 11.1.0",
  "acme.legacy ~1.5",
  "git+https://git.example.com/acme/mono.git#collections/app,main",
  "https://dl.example.com/acme-app-1.4.0.tar.gz",
  { name = "acme.app", version = ">= 1.4.0", source = "automation_hub" },
  { name = "acme.signed", signatures = ["https://keys.example.com/a.asc"] },
  { name = "acme.net", type = "git", source = "https://git.example.com/acme/net.git", version = "v2.0.1" },
  { name = "https://dl.example.com/acme-lib-2.1.0.tar.gz", version = "2.1.0" },
]
roles = [
  "geerlingguy.docker,7.4.1",
  "git+https://github.com/acme/ansible-role-nginx.git,v1.2.0,nginx",
  "https://dl.example.com/acme-role-1.0.0.tar.gz",
  "geerlingguy.postgresql,3.5.0,postgres",
]
`

// mustMigrate migrates data for a project named p and fails on any error.
func mustMigrate(t *testing.T, data string) Migration {
	t.Helper()
	m, err := MigrateYAML([]byte(data), "p")
	if err != nil {
		t.Fatalf("MigrateYAML(%q): %v", data, err)
	}
	return m
}

// mustParseView parses data in the given format and returns its view.
func mustParseView(t *testing.T, data []byte, toml bool) File {
	t.Helper()
	parse := Parse
	if toml {
		parse = ParseTOML
	}
	f, err := parse(data, "")
	if err != nil {
		t.Fatalf("parse(%q): %v", data, err)
	}
	return migrationView(f)
}

// TestMigrateYAMLMatchesTOMLFixtures pins every YAML side of the paired
// fixtures: the migrated bytes read back as the YAML reads and as the
// hand-written galaxy.toml twin reads.
func TestMigrateYAMLMatchesTOMLFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range append(append(constraintPairs(), sourcePairs()...), rolePairs()...) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := mustMigrate(t, tc.yaml)
			got := mustParseView(t, m.TOML, true)
			if want := mustParseView(t, []byte(tc.yaml), false); !reflect.DeepEqual(got, want) {
				t.Fatalf("migrated = %+v\nyaml     = %+v\nbytes:\n%s", got, want, m.TOML)
			}
			if fixture := mustParseView(t, []byte(tc.toml), true); !reflect.DeepEqual(got, fixture) {
				t.Fatalf("migrated = %+v\nfixture  = %+v\nbytes:\n%s", got, fixture, m.TOML)
			}
		})
	}
}

// TestMigrateYAMLDocumentedExample pins the documented example byte for byte,
// defaults left out, and the one notice its comment earns.
func TestMigrateYAMLDocumentedExample(t *testing.T) {
	t.Parallel()
	m, err := MigrateYAML([]byte(documentedRequirementsYAML), "infra")
	if err != nil {
		t.Fatalf("MigrateYAML: %v", err)
	}
	if string(m.TOML) != documentedMigration {
		t.Fatalf("MigrateYAML =\n%s\nwant\n%s", m.TOML, documentedMigration)
	}
	if want := []string{"comments are not carried into galaxy.toml"}; !reflect.DeepEqual(m.Notices, want) {
		t.Fatalf("Notices = %q, want %q", m.Notices, want)
	}
	if m.Collections != 11 || m.Roles != 4 {
		t.Fatalf("counts = %d, %d; want 11, 4", m.Collections, m.Roles)
	}
}

// migratedList is the galaxy.toml migrate writes for project p holding one
// entry in list.
func migratedList(list, entry string) string {
	return "[project]\nname = \"p\"\n" + list + " = [\n  " + entry + ",\n]\n"
}

// migrateEntryCase is one YAML entry and the galaxy.toml line it becomes.
type migrateEntryCase struct {
	name string
	yaml string
	list string
	want string
}

// assertMigratedEntries migrates each case and compares the whole file.
func assertMigratedEntries(t *testing.T, cases []migrateEntryCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := mustMigrate(t, tc.yaml)
			if want := migratedList(tc.list, tc.want); string(m.TOML) != want {
				t.Fatalf("MigrateYAML(%q) =\n%s\nwant\n%s", tc.yaml, m.TOML, want)
			}
		})
	}
}

// TestMigrateYAMLDropsDefaults pins that a value the entry gets anyway is
// never written.
func TestMigrateYAMLDropsDefaults(t *testing.T) {
	t.Parallel()
	assertMigratedEntries(t, []migrateEntryCase{
		{
			name: "star and type galaxy", yaml: yamlCollections("{name: ns.name, version: '*', type: galaxy}"),
			list: "collections", want: `"ns.name"`,
		},
		{
			name: "type url", yaml: yamlCollections("{name: 'https://dl.example.com/a-1.0.0.tar.gz', type: url}"),
			list: "collections", want: `"https://dl.example.com/a-1.0.0.tar.gz"`,
		},
		{name: "empty signatures", yaml: yamlCollections("{name: ns.name, signatures: []}"), list: "collections", want: `"ns.name"`},
		{
			name: "HEAD after the comma", yaml: yamlCollections("'git+https://h.example/r.git,HEAD'"),
			list: "collections", want: `"git+https://h.example/r.git"`,
		},
		{
			name: "HEAD version", yaml: yamlCollections("{name: 'git+https://h.example/r.git', version: HEAD}"),
			list: "collections", want: `"git+https://h.example/r.git"`,
		},
		{name: "role star", yaml: yamlRoles("{src: geerlingguy.docker, version: '*'}"), list: "roles", want: `"geerlingguy.docker"`},
		{
			name: "role own name", yaml: yamlRoles("{src: geerlingguy.docker, name: geerlingguy.docker}"),
			list: "roles", want: `"geerlingguy.docker"`,
		},
		{
			name: "git role derived name", yaml: yamlRoles("{src: 'git+https://h.example/x.git', name: x}"),
			list: "roles", want: `"git+https://h.example/x.git"`,
		},
	})
}

// TestMigrateYAMLPicksStringOrTable pins the documented string form wherever
// it can carry the entry, and an inline table wherever it cannot.
func TestMigrateYAMLPicksStringOrTable(t *testing.T) {
	t.Parallel()
	assertMigratedEntries(t, append(collectionStringOrTableCases(), roleStringOrTableCases()...))
}

// collectionStringOrTableCases are the collection rows of
// TestMigrateYAMLPicksStringOrTable.
func collectionStringOrTableCases() []migrateEntryCase {
	return []migrateEntryCase{
		{
			name: "source", yaml: yamlCollections("{name: acme.app, source: hub}"),
			list: "collections", want: `{ name = "acme.app", source = "hub" }`,
		},
		{
			name: "signatures", yaml: yamlCollections("{name: acme.app, version: '>= 1', signatures: ['https://k.example/a.asc']}"),
			list: "collections", want: `{ name = "acme.app", version = ">= 1", signatures = ["https://k.example/a.asc"] }`,
		},
		{
			name: "named git", yaml: yamlCollections("{name: acme.net, type: git, source: 'https://h.example/net.git', version: v2}"),
			list: "collections", want: `{ name = "acme.net", type = "git", source = "https://h.example/net.git", version = "v2" }`,
		},
		{
			name: "url with version", yaml: yamlCollections("{name: 'https://dl.example.com/a-2.1.0.tar.gz', version: 2.1.0}"),
			list: "collections", want: `{ name = "https://dl.example.com/a-2.1.0.tar.gz", version = "2.1.0" }`,
		},
		{
			name: "scp pointer", yaml: yamlCollections("'git@github.com:acme/mono.git#sub/app,main'"),
			list: "collections", want: `"git@github.com:acme/mono.git#sub/app,main"`,
		},
		{
			name: "ref with a comma", yaml: yamlCollections("'git+https://h.example/r.git,a,b'"),
			list: "collections", want: `"git+https://h.example/r.git,a,b"`,
		},
	}
}

// roleStringOrTableCases are the role rows of
// TestMigrateYAMLPicksStringOrTable.
func roleStringOrTableCases() []migrateEntryCase {
	return []migrateEntryCase{
		{
			name: "role ref with a comma", yaml: yamlRoles("{src: 'git+https://h.example/y.git', version: 'a,b'}"),
			list: "roles", want: `{ src = "git+https://h.example/y.git", version = "a,b" }`,
		},
		{
			name: "renamed without version", yaml: yamlRoles("{src: geerlingguy.docker, name: dock}"),
			list: "roles", want: `{ name = "dock", src = "geerlingguy.docker" }`,
		},
		{
			name: "url role renamed", yaml: yamlRoles("{src: 'https://h.example/dl/--.tar.gz', name: cache}"),
			list: "roles", want: `{ name = "cache", src = "https://h.example/dl/--.tar.gz" }`,
		},
		{
			name: "github role", yaml: yamlRoles("https://github.com/acme/ansible-role-cache"),
			list: "roles", want: `"git+https://github.com/acme/ansible-role-cache"`,
		},
		{
			name: "scm git", yaml: yamlRoles("{src: 'https://git.example.com/x.git', scm: git, version: release/1.x}"),
			list: "roles", want: `"git+https://git.example.com/x.git,release/1.x"`,
		},
	}
}

// TestMigrateYAMLCarriesTaggedScalarsAsText pins that a tagged scalar reaches
// galaxy.toml as the text written and reads back equal.
func TestMigrateYAMLCarriesTaggedScalarsAsText(t *testing.T) {
	t.Parallel()
	assertMigratedEntries(t, []migrateEntryCase{
		{
			name: "binary source", yaml: "collections: [{name: ns.name, source: !!binary aHVi/w==}]\n",
			list: "collections", want: `{ name = "ns.name", source = "aHVi/w==" }`,
		},
		{
			name: "binary git version", yaml: "collections: [{name: 'git+https://h.example/r.git', version: !!binary dv8=}]\n",
			list: "collections", want: `"git+https://h.example/r.git,dv8="`,
		},
	})
}

// TestMigrateYAMLKeepsListPresence pins that a list the file names is written
// even when empty, so the output always decodes, and an absent one is not.
func TestMigrateYAMLKeepsListPresence(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, yaml, want string }{
		{name: "null collections", yaml: "collections:\n", want: "[project]\nname = \"p\"\ncollections = []\n"},
		{
			name: "null collections beside roles", yaml: "collections:\nroles:\n  - geerlingguy.docker\n",
			want: "[project]\nname = \"p\"\ncollections = []\nroles = [\n  \"geerlingguy.docker\",\n]\n",
		},
		{name: "bare empty list", yaml: "[]\n", want: "[project]\nname = \"p\"\ncollections = []\n"},
		{
			name: "roles only", yaml: "roles:\n  - geerlingguy.docker\n",
			want: "[project]\nname = \"p\"\nroles = [\n  \"geerlingguy.docker\",\n]\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if m := mustMigrate(t, tc.yaml); string(m.TOML) != tc.want {
				t.Fatalf("MigrateYAML(%q) = %q, want %q", tc.yaml, m.TOML, tc.want)
			}
		})
	}
}

// Notices migrate prints for comments and for later YAML documents.
const (
	commentsNotice  = "comments are not carried into galaxy.toml"
	documentsNotice = "YAML documents after the first are not carried into galaxy.toml; go-galaxy reads only the first"
)

// migrateNoticeCase is one requirements.yml and the notices it earns.
type migrateNoticeCase struct {
	name        string
	yaml        string
	projectName string
	want        []string
}

// TestMigrateYAMLNotices pins each notice text and when it is given, and that
// a trailing document marker earns none.
func TestMigrateYAMLNotices(t *testing.T) {
	t.Parallel()
	for _, tc := range append(keyNoticeCases(), documentNoticeCases()...) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			projectName := tc.projectName
			if projectName == "" {
				projectName = "p"
			}
			m, err := MigrateYAML([]byte(tc.yaml), projectName)
			if err != nil {
				t.Fatalf("MigrateYAML: %v", err)
			}
			if !reflect.DeepEqual(m.Notices, tc.want) {
				t.Fatalf("Notices = %q, want %q", m.Notices, tc.want)
			}
			for _, notice := range m.Notices {
				if strings.Contains(notice, "ignoring unknown key") {
					t.Fatalf("notice %q repeats a parse warning", notice)
				}
			}
		})
	}
}

// keyNoticeCases are the rows of TestMigrateYAMLNotices about keys and names.
func keyNoticeCases() []migrateNoticeCase {
	return []migrateNoticeCase{
		{
			name: "top-level key", yaml: "collections: [a.b]\nextra: 1\n",
			want: []string{`top-level key "extra" is not carried into galaxy.toml`},
		},
		{
			name: "collection key", yaml: "collections:\n  - name: a.b\n    foo: x\n",
			want: []string{`collections[0]: key "foo" is not carried into galaxy.toml`},
		},
		{
			name: "role key", yaml: "roles:\n  - src: a.b\n    foo: x\n",
			want: []string{`roles[0]: key "foo" is not carried into galaxy.toml`},
		},
		{
			name: "flow key with no space", yaml: "collections: [{name: ns.name, version:}]\n",
			want: []string{`collections[0]: key "version:" is not carried into galaxy.toml`},
		},
		{
			name: "git version beside a comma", yaml: "collections:\n  - name: 'git+https://h.example/r.git,main'\n    version: v2\n",
			want: []string{`collections[0]: key "version" is ignored beside the ref after the comma; it is not carried`},
		},
		{
			name: "directory name not UTF-8", yaml: "collections: [a.b]\n", projectName: "\xff",
			want: []string{"the directory name is not UTF-8 text; galaxy.toml gets no project name"},
		},
	}
}

// documentNoticeCases are the rows of TestMigrateYAMLNotices about comments
// and YAML documents.
func documentNoticeCases() []migrateNoticeCase {
	return []migrateNoticeCase{
		{name: "head comment", yaml: "# head\ncollections: [a.b]\n", want: []string{commentsNotice}},
		{name: "line comment", yaml: "collections: [a.b] # line\n", want: []string{commentsNotice}},
		{name: "foot comment", yaml: "collections:\n  - a.b\n\n# foot\n", want: []string{commentsNotice}},
		{name: "comment in a later document", yaml: "collections: [a.b]\n---\n# late\n", want: []string{commentsNotice}},
		{name: "later document", yaml: "collections: [a.b]\n---\nx: 1\n", want: []string{documentsNotice}},
		{name: "unreadable later document", yaml: "collections: [a.b]\n---\n[\n", want: []string{documentsNotice}},
		{name: "trailing document marker", yaml: "collections: [a.b]\n---\n"},
		{name: "document end marker", yaml: "collections: [a.b]\n...\n"},
	}
}

// TestMigrateYAMLRefusals pins that a refusal comes back classified, never as
// ErrMigrateRoundTrip, and with a zero Migration.
func TestMigrateYAMLRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		wantErr    error
		name       string
		yaml       string
		wantMsg    string
		wantPrefix string
		rolesError bool
	}{
		{
			name: "constraint semver refuses", yaml: "collections:\n  - name: ns.name\n    version: latest\n",
			wantErr: helpers.ErrInvalidCollectionConstraint, wantPrefix: "collections[0]: ",
		},
		{name: "not YAML", yaml: "collections: [\n", wantErr: helpers.ErrInvalidRequirementsYAML},
		{name: "neither list", yaml: "foo: 1\n", wantErr: helpers.ErrUnsupportedRequirementsFormat},
		{name: "empty file", yaml: "", wantErr: helpers.ErrUnsupportedRequirementsFormat},
		{name: "roles refusal", yaml: "roles:\n  - include: other.yml\n", wantErr: helpers.ErrUnsupportedRoleInclude, rolesError: true},
		{
			name: "version with no value", yaml: "collections:\n  - name: ns.name\n    version:\n",
			wantErr: helpers.ErrInvalidCollectionEntry, wantMsg: "version has no value",
		},
		{name: "list under type", yaml: "collections:\n  - name: ns.name\n    type: [git]\n", wantErr: helpers.ErrInvalidCollectionEntry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := MigrateYAML([]byte(tc.yaml), "p")
			assertRefusal(t, err, tc.wantErr, tc.wantMsg)
			if !strings.HasPrefix(err.Error(), tc.wantPrefix) {
				t.Fatalf("error = %q, want the prefix %q", err, tc.wantPrefix)
			}
			if errors.Is(err, helpers.ErrMigrateRoundTrip) {
				t.Fatalf("error = %v, want no ErrMigrateRoundTrip", err)
			}
			if _, ok := errors.AsType[*RolesError](err); ok != tc.rolesError {
				t.Fatalf("error = %T, want a *RolesError: %v", err, tc.rolesError)
			}
			if !reflect.DeepEqual(m, Migration{}) {
				t.Fatalf("Migration = %+v, want the zero value", m)
			}
		})
	}
}

// TestVerifyMigrationRefusesADifference pins the self-check on bytes that do
// not match: the entry that differs, a parse refusal that must not classify
// the exit through its own sentinel, and a changed project name.
func TestVerifyMigrationRefusesADifference(t *testing.T) {
	t.Parallel()
	want, err := Parse([]byte("collections: [a.b, c.d]\n"), "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cases := []struct{ name, out, wantMsg string }{
		{name: "other entry", out: "[project]\nname = \"p\"\ncollections = [\"a.b\", \"c.e\"]\n", wantMsg: "collections[1] differs"},
		{name: "no list", out: "[project]\n", wantMsg: "does not read back"},
		{name: "other name", out: "[project]\nname = \"q\"\ncollections = [\"a.b\", \"c.d\"]\n", wantMsg: "[project] name differs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := verifyMigration(want, "p", []byte(tc.out))
			assertRefusal(t, err, helpers.ErrMigrateRoundTrip, tc.wantMsg)
			if errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) {
				t.Fatalf("error = %v carries the parse sentinel", err)
			}
		})
	}
}

// TestMigrationViewFoldsOnlyFormatDifferences pins the normalizations the
// comparison allows, warnings and empty versus absent lists, and that every
// other difference still shows.
func TestMigrationViewFoldsOnlyFormatDifferences(t *testing.T) {
	t.Parallel()
	base := File{
		Collections: Collections{
			{Namespace: "a", Name: "b", Version: ">= 1.0", Source: "hub"},
			{Namespace: "c", Name: "d", Version: "*", Type: TypeGit, Source: "https://h.example/r.git", Ref: "main"},
		},
	}
	folded := File{
		Collections: Collections{
			{Namespace: "a", Name: "b", Version: ">= 1.0", Source: "hub", Signatures: []string{}},
			base.Collections[1],
		},
		Roles:    []RoleRequirement{},
		Warnings: []string{"ignoring unknown key x on a role entry"},
	}
	if where := firstDifference(migrationView(base), migrationView(folded)); where != "" {
		t.Fatalf("firstDifference = %q, want none", where)
	}
	if where := firstDifference(migrationView(File{}), migrationView(File{Collections: Collections{}})); where != "" {
		t.Fatalf("firstDifference(nil, empty) = %q, want none", where)
	}
	changes := map[string]func(f *File){
		"constraint spacing": func(f *File) { f.Collections[0].Version = ">=1.0" },
		"order":              func(f *File) { f.Collections[0], f.Collections[1] = f.Collections[1], f.Collections[0] },
		"source":             func(f *File) { f.Collections[0].Source = "other" },
		"ref":                func(f *File) { f.Collections[1].Ref = "dev" },
	}
	for name, change := range changes {
		changed := File{Collections: append(Collections(nil), base.Collections...)}
		change(&changed)
		if where := firstDifference(migrationView(base), migrationView(changed)); where == "" {
			t.Errorf("%s: firstDifference found none", name)
		}
	}
}

// FuzzMigrateYAML pins that whatever Parse accepts either migrates to bytes
// that read back equal or is refused by a classified sentinel, never by the
// round trip.
func FuzzMigrateYAML(f *testing.F) {
	for _, tc := range append(append(constraintPairs(), sourcePairs()...), rolePairs()...) {
		f.Add([]byte(tc.yaml))
	}
	f.Add([]byte("collections: [{name: ns.name, source: !!binary /w==}]\n"))
	f.Add([]byte("collections:\n  - name: ns.name\n    version: 1.10\nroles:\n" +
		"  - {src: 'git+https://h.example/y.git', version: 'a,b'}\n  - {src: https://h.example/dl/--.tar.gz, name: cache}\n"))
	f.Add([]byte("# c\n[a.b, 'git@github.com:acme/mono.git#sub/app,main']\n---\nx: 1\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		want, err := Parse(data, "")
		if err != nil {
			return
		}
		m, err := MigrateYAML(data, "p")
		if errors.Is(err, helpers.ErrMigrateRoundTrip) {
			t.Fatalf("round trip failed for %q: %v", data, err)
		}
		if err != nil {
			return
		}
		got, err := ParseTOML(m.TOML, "")
		if err != nil {
			t.Fatalf("ParseTOML(%q): %v", m.TOML, err)
		}
		if !reflect.DeepEqual(migrationView(got), migrationView(want)) {
			t.Fatalf("migrated %q reads back as %+v, want %+v", data, got, want)
		}
	})
}
