package requirements

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// documentedGalaxyTOML is the documented galaxy.toml: every entry form the
// [project] table admits.
const documentedGalaxyTOML = `[project]
name = "infra"
version = "1.0.0"
description = "My infra collections"
collections = [
  "sc.internal >= 0.0.20",
  "ansible.utils",
  "community.crypto >= 2.0, < 3.0",
  "community.general == 11.1.0",
  "acme.legacy ~1.5",
  "git+https://git.example.com/acme/mono.git#collections/app,main",
  "https://dl.example.com/acme-app-1.4.0.tar.gz",
  { name = "acme.app", version = ">= 1.4.0", source = "automation_hub" },
  { name = "acme.signed", version = "*", signatures = ["https://keys.example.com/a.asc"] },
  { name = "acme.net", type = "git", source = "https://git.example.com/acme/net.git", version = "v2.0.1" },
  { name = "https://dl.example.com/acme-lib-2.1.0.tar.gz", type = "url", version = "2.1.0" },
]
roles = [
  "geerlingguy.docker,7.4.1",
  "git+https://github.com/acme/ansible-role-nginx.git,v1.2.0,nginx",
  "https://dl.example.com/acme-role-1.0.0.tar.gz",
  { name = "postgres", src = "geerlingguy.postgresql", version = "3.5.0" },
]
`

// tomlCollections wraps one collections array item into a galaxy.toml.
func tomlCollections(item string) string {
	return "[project]\ncollections = [\n  " + item + ",\n]\n"
}

// yamlCollections wraps one collections list item into a requirements.yml.
func yamlCollections(item string) string {
	return "collections:\n  - " + item + "\n"
}

// tomlRoles wraps one roles array item into a galaxy.toml.
func tomlRoles(item string) string {
	return "[project]\nroles = [\n  " + item + ",\n]\n"
}

// yamlRoles wraps one roles list item into a requirements.yml.
func yamlRoles(item string) string {
	return "roles:\n  - " + item + "\n"
}

// tomlYAMLPair is one row of TestParseTOMLMatchesYAML: a galaxy.toml and the
// requirements.yml that must parse to the same File.
type tomlYAMLPair struct {
	name string
	toml string
	yaml string
}

// constraintPairs are the dependency-string spellings, one per operator and
// per range form semver admits, each beside the YAML mapping it stands for.
func constraintPairs() []tomlYAMLPair {
	return []tomlYAMLPair{
		{name: "bare name", toml: tomlCollections(`"ansible.utils"`), yaml: yamlCollections("ansible.utils")},
		{name: "ge", toml: tomlCollections(`"ns.name >= 1.0"`), yaml: yamlCollections(`name: ns.name` + "\n    version: '>= 1.0'")},
		{name: "gt", toml: tomlCollections(`"ns.name > 1.0"`), yaml: yamlCollections(`name: ns.name` + "\n    version: '> 1.0'")},
		{name: "lt", toml: tomlCollections(`"ns.name < 2.0"`), yaml: yamlCollections(`name: ns.name` + "\n    version: '< 2.0'")},
		{name: "le", toml: tomlCollections(`"ns.name <= 2.0"`), yaml: yamlCollections(`name: ns.name` + "\n    version: '<= 2.0'")},
		{name: "eq pair", toml: tomlCollections(`"ns.name == 1.2.3"`), yaml: yamlCollections(`name: ns.name` + "\n    version: '== 1.2.3'")},
		{name: "eq", toml: tomlCollections(`"ns.name = 1.2.3"`), yaml: yamlCollections(`name: ns.name` + "\n    version: '= 1.2.3'")},
		{name: "ne", toml: tomlCollections(`"ns.name != 1.0"`), yaml: yamlCollections(`name: ns.name` + "\n    version: '!= 1.0'")},
		{name: "tilde", toml: tomlCollections(`"ns.name ~1.5"`), yaml: yamlCollections(`name: ns.name` + "\n    version: '~1.5'")},
		{name: "caret", toml: tomlCollections(`"ns.name ^1.2"`), yaml: yamlCollections(`name: ns.name` + "\n    version: '^1.2'")},
		{
			name: "comma and", toml: tomlCollections(`"community.crypto >= 2.0, < 3.0"`),
			yaml: yamlCollections("name: community.crypto\n    version: '>= 2.0, < 3.0'"),
		},
		{
			name: "whitespace and", toml: tomlCollections(`"ns.name >= 2.0 < 3.0"`),
			yaml: yamlCollections("name: ns.name\n    version: '>= 2.0 < 3.0'"),
		},
		{name: "or", toml: tomlCollections(`"ns.name ^1 || ^2"`), yaml: yamlCollections("name: ns.name\n    version: '^1 || ^2'")},
		{name: "hyphen range", toml: tomlCollections(`"ns.name 1.2 - 1.4"`), yaml: yamlCollections("name: ns.name\n    version: '1.2 - 1.4'")},
		{name: "x range", toml: tomlCollections(`"ns.name 1.x"`), yaml: yamlCollections("name: ns.name\n    version: '1.x'")},
		{name: "star", toml: tomlCollections(`"ns.name *"`), yaml: yamlCollections("name: ns.name\n    version: '*'")},
		{name: "exact", toml: tomlCollections(`"ns.name 1.2.3"`), yaml: yamlCollections("name: ns.name\n    version: '1.2.3'")},
		{name: "no space", toml: tomlCollections(`"ns.name>=1.0"`), yaml: yamlCollections("name: ns.name\n    version: '>=1.0'")},
		{name: "tab", toml: tomlCollections(`"ns.name\t1.2.3"`), yaml: yamlCollections("name: ns.name\n    version: '1.2.3'")},
	}
}

// sourcePairs are the string spellings that pass whole (git pointers, a
// tarball URL) and the inline tables, each beside its YAML mapping.
func sourcePairs() []tomlYAMLPair {
	return []tomlYAMLPair{
		{
			name: "git pointer with subdir and ref",
			toml: tomlCollections(`"git+https://git.example.com/acme/mono.git#collections/app,main"`),
			yaml: yamlCollections("git+https://git.example.com/acme/mono.git#collections/app,main"),
		},
		{
			name: "git at pointer", toml: tomlCollections(`"git@github.com:acme/app.git"`),
			yaml: yamlCollections("git@github.com:acme/app.git"),
		},
		{
			name: "https tarball", toml: tomlCollections(`"https://dl.example.com/acme-app-1.4.0.tar.gz"`),
			yaml: yamlCollections("https://dl.example.com/acme-app-1.4.0.tar.gz"),
		},
		{
			name: "galaxy table with source",
			toml: tomlCollections(`{ name = "acme.app", version = ">= 1.4.0", source = "automation_hub" }`),
			yaml: yamlCollections("name: acme.app\n    version: '>= 1.4.0'\n    source: automation_hub"),
		},
		{
			name: "galaxy table with signatures",
			toml: tomlCollections(`{ name = "acme.signed", version = "*", signatures = ["https://keys.example.com/a.asc"] }`),
			yaml: yamlCollections("name: acme.signed\n    version: '*'\n    signatures:\n      - https://keys.example.com/a.asc"),
		},
		{
			name: "git table by source with name and version",
			toml: tomlCollections(`{ name = "acme.net", type = "git", source = "https://git.example.com/acme/net.git", version = "v2.0.1" }`),
			yaml: yamlCollections("name: acme.net\n    type: git\n    source: https://git.example.com/acme/net.git\n    version: v2.0.1"),
		},
		{
			name: "url table with exact version",
			toml: tomlCollections(`{ name = "https://dl.example.com/acme-lib-2.1.0.tar.gz", type = "url", version = "2.1.0" }`),
			yaml: yamlCollections("name: https://dl.example.com/acme-lib-2.1.0.tar.gz\n    type: url\n    version: '2.1.0'"),
		},
		{
			name: "explicit namespace and name", toml: tomlCollections(`{ namespace = "acme", name = "app" }`),
			yaml: yamlCollections("namespace: acme\n    name: app"),
		},
		{
			name: "array of tables",
			toml: "[[project.collections]]\nname = \"acme.app\"\nversion = \">= 1\"\n\n[[project.collections]]\nname = \"acme.lib\"\n",
			yaml: "collections:\n  - name: acme.app\n    version: '>= 1'\n  - name: acme.lib\n",
		},
	}
}

// rolePairs are the roles spellings: strings pass whole in ansible's
// src[,version[,name]] form, tables carry the same keys as a YAML mapping.
func rolePairs() []tomlYAMLPair {
	return []tomlYAMLPair{
		{
			name: "galaxy role with version and name", toml: tomlRoles(`"geerlingguy.docker,7.4.1,docker"`),
			yaml: yamlRoles("geerlingguy.docker,7.4.1,docker"),
		},
		{
			name: "git role string", toml: tomlRoles(`"git+https://github.com/acme/ansible-role-nginx.git,v1.2.0,nginx"`),
			yaml: yamlRoles("git+https://github.com/acme/ansible-role-nginx.git,v1.2.0,nginx"),
		},
		{
			name: "url role string", toml: tomlRoles(`"https://dl.example.com/acme-role-1.0.0.tar.gz"`),
			yaml: yamlRoles("https://dl.example.com/acme-role-1.0.0.tar.gz"),
		},
		{
			name: "role table with name src version",
			toml: tomlRoles(`{ name = "postgres", src = "geerlingguy.postgresql", version = "3.5.0" }`),
			yaml: yamlRoles("name: postgres\n    src: geerlingguy.postgresql\n    version: '3.5.0'"),
		},
		{
			name: "role table with role key", toml: tomlRoles(`{ role = "geerlingguy.docker", version = "7.4.1" }`),
			yaml: yamlRoles("role: geerlingguy.docker\n    version: '7.4.1'"),
		},
	}
}

// TestParseTOMLMatchesYAML pins that every galaxy.toml spelling parses to
// the File its requirements.yml counterpart parses to, Warnings included, so
// the two formats can never drift in what an entry means.
func TestParseTOMLMatchesYAML(t *testing.T) {
	t.Parallel()
	cases := append(append(constraintPairs(), sourcePairs()...), rolePairs()...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseTOML([]byte(tc.toml), "https://default")
			if err != nil {
				t.Fatalf("ParseTOML: %v", err)
			}
			want, err := Parse([]byte(tc.yaml), "https://default")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(got.Collections)+len(got.Roles) == 0 {
				t.Fatalf("ParseTOML produced no entries from %q", tc.toml)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ParseTOML = %+v\nParse    = %+v", got, want)
			}
		})
	}
}

// TestParseTOMLReadsDocumentedExample pins the documented file as a whole:
// eleven collections and four roles, no warning.
func TestParseTOMLReadsDocumentedExample(t *testing.T) {
	t.Parallel()
	f, err := ParseTOML([]byte(documentedGalaxyTOML), "https://default")
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if len(f.Collections) != 11 || len(f.Roles) != 4 || len(f.Warnings) != 0 {
		t.Fatalf("ParseTOML = %d collections, %d roles, %d warnings; want 11, 4, 0", len(f.Collections), len(f.Roles), len(f.Warnings))
	}
}

// splitSpecCase is one row of TestSplitCollectionSpec: the value the grammar
// must produce (the string itself, or a name/version mapping), or the
// sentinel it must refuse with and a fragment its message must carry.
type splitSpecCase struct {
	want    any
	wantErr error
	name    string
	spec    string
	wantMsg string
}

const constraintHint = "put a space or a version operator between the name and its constraint"

func splitSpecSplitCases() []splitSpecCase {
	return []splitSpecCase{
		{name: "space", spec: "ns.name >= 1.0", want: map[string]any{"name": "ns.name", "version": ">= 1.0"}},
		{name: "no space", spec: "ns.name>=1.0", want: map[string]any{"name": "ns.name", "version": ">=1.0"}},
		{name: "tab", spec: "ns.name\t1.2.3", want: map[string]any{"name": "ns.name", "version": "1.2.3"}},
		{name: "star", spec: "ns.name *", want: map[string]any{"name": "ns.name", "version": "*"}},
		{name: "outer whitespace", spec: "  ns.name 1.2.3  ", want: map[string]any{"name": "ns.name", "version": "1.2.3"}},
		{name: "upper case stays in the name", spec: "Ns.Name >= 1", want: map[string]any{"name": "Ns.Name", "version": ">= 1"}},
	}
}

func splitSpecPassThroughCases() []splitSpecCase {
	return []splitSpecCase{
		{name: "bare name", spec: "ns.name", want: "ns.name"},
		{name: "empty", spec: "", want: ""},
		{name: "digits run into the name", spec: "ns.name1.0.0", want: "ns.name1.0.0"},
		{name: "git pointer keeps ref and subdir", spec: "git+https://h/r.git#sub,main", want: "git+https://h/r.git#sub,main"},
		{name: "git at pointer", spec: "git@h:r.git", want: "git@h:r.git"},
		{name: "http url", spec: "https://h/x.tar.gz", want: "https://h/x.tar.gz"},
		{name: "relative path", spec: "./x", want: "./x"},
		{name: "parent path", spec: "../x", want: "../x"},
		{name: "absolute path", spec: "/abs", want: "/abs"},
		{name: "home path", spec: "~/x", want: "~/x"},
		{name: "ssh url", spec: "ssh://h/r.git", want: "ssh://h/r.git"},
		{name: "file url", spec: "file:///x", want: "file:///x"},
		{name: "git pointer after a name", spec: "ns.name @ git+https://h/r.git", want: "ns.name @ git+https://h/r.git"},
		{name: "operator with no name", spec: ">= 1.0", want: ">= 1.0"},
	}
}

func splitSpecRefusedCases() []splitSpecCase {
	return []splitSpecCase{
		{name: "broken constraint", spec: "ns.name >= 0..20", wantErr: helpers.ErrInvalidCollectionConstraint, wantMsg: `">= 0..20" for ns.name`},
		{name: "triple equals", spec: "ns.name ===1.0.0", wantErr: helpers.ErrInvalidCollectionConstraint, wantMsg: `"===1.0.0" for ns.name`},
		{name: "colon after a space", spec: "ns.name :>=1", wantErr: helpers.ErrInvalidCollectionConstraint, wantMsg: `":>=1" for ns.name`},
		{name: "colon separator", spec: "ns.name:>=1", wantErr: helpers.ErrInvalidCollectionName, wantMsg: constraintHint},
		{name: "at separator", spec: "ns.name@1.0", wantErr: helpers.ErrInvalidCollectionName, wantMsg: constraintHint},
		{name: "hyphen separator", spec: "ns.name-1.0", wantErr: helpers.ErrInvalidCollectionName, wantMsg: constraintHint},
		{name: "dollar separator", spec: "ns.name${V}", wantErr: helpers.ErrInvalidCollectionName, wantMsg: constraintHint},
		{name: "slash separator", spec: "ns.name/1.0", wantErr: helpers.ErrInvalidCollectionName, wantMsg: constraintHint},
	}
}

// TestSplitCollectionSpec pins the dependency-string grammar row by row: what
// splits, what passes whole, and what is refused with which sentinel.
func TestSplitCollectionSpec(t *testing.T) {
	t.Parallel()
	cases := append(append(splitSpecSplitCases(), splitSpecPassThroughCases()...), splitSpecRefusedCases()...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := splitCollectionSpec(tc.spec)
			if tc.wantErr != nil {
				assertRefusal(t, err, tc.wantErr, tc.wantMsg)
				return
			}
			if err != nil {
				t.Fatalf("splitCollectionSpec(%q) error = %v, want nil", tc.spec, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("splitCollectionSpec(%q) = %#v, want %#v", tc.spec, got, tc.want)
			}
		})
	}
}

// assertRefusal checks err carries want and its message carries wantMsg.
func assertRefusal(t *testing.T, err, want error, wantMsg string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want errors.Is %v", err, want)
	}
	if wantMsg != "" && !strings.Contains(err.Error(), wantMsg) {
		t.Fatalf("error = %q, want it to contain %q", err, wantMsg)
	}
}

// tomlRefusalCase is one row of TestParseTOMLRefusals: the sentinel, a
// fragment the message must carry, one it must not, whether the refusal is a
// RolesError, and how many collections ride beside it.
type tomlRefusalCase struct {
	wantErr         error
	name            string
	toml            string
	wantMsg         string
	notMsg          string
	wantCollections int
	wantRolesErr    bool
}

func tomlStringRefusalCases() []tomlRefusalCase {
	return []tomlRefusalCase{
		{name: "upper case name", toml: tomlCollections(`"Ns.Name >= 1"`), wantErr: helpers.ErrInvalidCollectionName},
		{name: "empty string", toml: tomlCollections(`""`), wantErr: helpers.ErrEmptyCollectionName},
		{name: "relative path", toml: tomlCollections(`"./x"`), wantErr: helpers.ErrUnsupportedCollectionSource},
		{name: "parent path", toml: tomlCollections(`"../x"`), wantErr: helpers.ErrUnsupportedCollectionSource},
		{name: "absolute path", toml: tomlCollections(`"/abs"`), wantErr: helpers.ErrUnsupportedCollectionSource},
		{name: "home path", toml: tomlCollections(`"~/x"`), wantErr: helpers.ErrUnsupportedCollectionSource},
		{name: "ssh url", toml: tomlCollections(`"ssh://h/r.git"`), wantErr: helpers.ErrUnsupportedCollectionSource},
		{name: "file url", toml: tomlCollections(`"file:///x"`), wantErr: helpers.ErrUnsupportedCollectionSource},
		{
			name: "git pointer after a name", toml: tomlCollections(`"ns.name @ git+https://h/r.git"`),
			wantErr: helpers.ErrUnsupportedCollectionSource,
		},
		{name: "operator with no name", toml: tomlCollections(`">= 1.0"`), wantErr: helpers.ErrInvalidCollectionName},
		{name: "digits run into the name", toml: tomlCollections(`"ns.name1.0.0"`), wantErr: helpers.ErrInvalidCollectionName},
		{
			name: "credential in a source-shaped string", toml: tomlCollections(`"ssh://u:hunter2@h/r.git"`),
			wantErr: helpers.ErrUnsupportedCollectionSource, wantMsg: "ssh://h/r.git", notMsg: "hunter2",
		},
		{
			name: "nested array", toml: tomlCollections(`["https://u:hunter2@h/x.tar.gz"]`),
			wantErr: helpers.ErrUnsupportedCollectionFormat, wantMsg: "[]interface {}", notMsg: "hunter2",
		},
		{name: "collections is a string", toml: "[project]\ncollections = \"x\"\n", wantErr: helpers.ErrInvalidCollectionsList},
		{name: "collections is one table", toml: "[project.collections]\nname = \"ns.name\"\n", wantErr: helpers.ErrInvalidCollectionsList},
	}
}

func tomlTableRefusalCases() []tomlRefusalCase {
	return []tomlRefusalCase{
		{
			name: "unknown key", toml: tomlCollections(`{ name = "ns.name", foo = "x" }`),
			wantErr: helpers.ErrInvalidCollectionEntry, wantMsg: `unknown key "foo" on a collection entry`,
		},
		{
			name: "float version", toml: tomlCollections(`{ name = "ns.name", version = 1.0 }`),
			wantErr: helpers.ErrInvalidCollectionEntry, wantMsg: "version is a float64, not a string", notMsg: "1",
		},
		{
			name: "integer name", toml: tomlCollections(`{ name = 3 }`),
			wantErr: helpers.ErrInvalidCollectionEntry, wantMsg: "name is a int64, not a string", notMsg: "3",
		},
		{
			name: "broken constraint in a table", toml: tomlCollections(`{ name = "ns.name", version = ">= 0..20" }`),
			wantErr: helpers.ErrInvalidCollectionConstraint, wantMsg: `">= 0..20" for ns.name`,
		},
		{
			name: "signatures on a git table", toml: tomlCollections(`{ name = "git+https://h/r.git", signatures = ["https://k/a.asc"] }`),
			wantErr: helpers.ErrInvalidCollectionEntry, wantMsg: "not supported on a git requirement",
		},
		{
			name: "url table with a range version", toml: tomlCollections(`{ name = "https://h/x.tar.gz", type = "url", version = ">= 1.0" }`),
			wantErr: helpers.ErrInvalidCollectionVersion,
		},
		{
			name: "source with userinfo", toml: tomlCollections(`{ name = "ns.name", source = "https://user:hunter2@galaxy.example/api/" }`),
			wantErr: helpers.ErrGalaxyServerURLUserinfo, notMsg: "hunter2",
		},
		{
			name: "integer namespace", toml: tomlCollections(`{ namespace = 3, name = "name" }`),
			wantErr: helpers.ErrInvalidCollectionEntry, wantMsg: "namespace is a int64, not a string",
		},
		{
			name: "boolean source", toml: tomlCollections(`{ name = "ns.name", source = true }`),
			wantErr: helpers.ErrInvalidCollectionEntry, wantMsg: "source is a bool, not a string",
		},
		{
			name: "integer type", toml: tomlCollections(`{ name = "ns.name", type = 1 }`),
			wantErr: helpers.ErrInvalidCollectionEntry, wantMsg: "type is a int64, not a string",
		},
		{
			name: "two faults name the first key in sorted order", toml: tomlCollections(`{ name = "ns.name", version = 1.0, aaa = "x" }`),
			wantErr: helpers.ErrInvalidCollectionEntry, wantMsg: `unknown key "aaa"`,
		},
		{
			name: "git source without a type", toml: tomlCollections(`{ name = "acme.app", source = "git+https://h/r.git", version = "main" }`),
			wantErr: helpers.ErrUnsupportedCollectionSource, wantMsg: "spell the entry with type: git",
		},
		{
			name:    "credential in a source-shaped name",
			toml:    tomlCollections(`{ name = "ssh://deploy:hunter2@h/x", version = "1.0.0.0" }`),
			wantErr: helpers.ErrUnsupportedCollectionSource, notMsg: "hunter2",
		},
	}
}

func tomlRoleRefusalCases() []tomlRefusalCase {
	withCollections := func(role string) string {
		return "[project]\ncollections = [\"ns.name\"]\nroles = [\n  " + role + ",\n]\n"
	}
	return []tomlRefusalCase{
		{
			name: "role unknown key", toml: withCollections(`{ src = "geerlingguy.docker", foo = "x" }`),
			wantErr: helpers.ErrInvalidRoleEntry, wantMsg: `roles[0]: invalid role entry: unknown key "foo" on a role entry`,
			wantRolesErr: true, wantCollections: 1,
		},
		{
			name: "role float version", toml: withCollections(`{ src = "geerlingguy.docker", version = 7.4 }`),
			wantErr: helpers.ErrInvalidRoleEntry, wantMsg: "version is a float64, not a string", notMsg: "7",
			wantRolesErr: true, wantCollections: 1,
		},
		{
			name: "role include", toml: withCollections(`{ include = "other.yml" }`),
			wantErr: helpers.ErrUnsupportedRoleInclude, wantRolesErr: true, wantCollections: 1,
		},
		{
			name: "role integer name", toml: withCollections(`{ src = "geerlingguy.docker", name = 1 }`),
			wantErr: helpers.ErrInvalidRoleEntry, wantMsg: "name is a int64, not a string", wantRolesErr: true, wantCollections: 1,
		},
		{
			name: "role integer role", toml: withCollections(`{ role = 2 }`),
			wantErr: helpers.ErrInvalidRoleEntry, wantMsg: "role is a int64, not a string", wantRolesErr: true, wantCollections: 1,
		},
		{
			name: "role integer src", toml: withCollections(`{ src = 3 }`),
			wantErr: helpers.ErrInvalidRoleEntry, wantMsg: "src is a int64, not a string", wantRolesErr: true, wantCollections: 1,
		},
		{
			name: "role integer scm", toml: withCollections(`{ src = "git+https://h/r.git", scm = 4 }`),
			wantErr: helpers.ErrInvalidRoleEntry, wantMsg: "scm is a int64, not a string", wantRolesErr: true, wantCollections: 1,
		},
		{
			name: "role unknown key without collections", toml: tomlRoles(`{ src = "geerlingguy.docker", foo = "x" }`),
			wantErr: helpers.ErrInvalidRoleEntry, wantRolesErr: true,
		},
		{
			name: "roles is a string", toml: "[project]\nroles = \"x\"\n",
			wantErr: helpers.ErrInvalidRolesList, wantRolesErr: true,
		},
		{
			name:    "collections refusal outranks the roles refusal",
			toml:    "[project]\ncollections = [\"Ns.Name\"]\nroles = [{ src = \"geerlingguy.docker\", foo = \"x\" }]\n",
			wantErr: helpers.ErrInvalidCollectionName,
		},
	}
}

// TestParseTOMLRefusals pins each refusal galaxy.toml adds over YAML (a
// strict key set, typed scalars, a judged constraint) and that a roles
// refusal still rides as a RolesError beside the parsed collections.
func TestParseTOMLRefusals(t *testing.T) {
	t.Parallel()
	cases := append(append(tomlStringRefusalCases(), tomlTableRefusalCases()...), tomlRoleRefusalCases()...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, err := ParseTOML([]byte(tc.toml), "https://default")
			assertRefusal(t, err, tc.wantErr, tc.wantMsg)
			if tc.notMsg != "" && strings.Contains(err.Error(), tc.notMsg) {
				t.Fatalf("error = %q, must not contain %q", err, tc.notMsg)
			}
			if _, ok := errors.AsType[*RolesError](err); ok != tc.wantRolesErr {
				t.Fatalf("error = %v, RolesError = %v, want %v", err, ok, tc.wantRolesErr)
			}
			if len(f.Collections) != tc.wantCollections {
				t.Fatalf("collections beside the refusal = %+v, want %d", f.Collections, tc.wantCollections)
			}
		})
	}
}

// TestParseTOMLDoesNotExpandEnvironment pins that a ${VAR} in galaxy.toml
// stays literal even with VAR exported: metadata keeps it verbatim and a
// dependency string is refused as the constraint it is not.
func TestParseTOMLDoesNotExpandEnvironment(t *testing.T) {
	t.Setenv("V", "1.0.0")
	t.Setenv("X", "expanded")
	f, err := ParseTOML([]byte("[project]\ndescription = \"${X}\"\ncollections = [\"ns.name\"]\n"), "https://default")
	if err != nil || len(f.Collections) != 1 {
		t.Fatalf("ParseTOML with a ${X} description = %+v, %v; want one collection and no error", f, err)
	}
	_, err = ParseTOML([]byte(tomlCollections(`"ns.name ${V}"`)), "https://default")
	assertRefusal(t, err, helpers.ErrInvalidCollectionConstraint, `"${V}" for ns.name`)
	_, err = ParseTOML([]byte(tomlCollections(`"ns.name${V}"`)), "https://default")
	assertRefusal(t, err, helpers.ErrInvalidCollectionName, constraintHint)
}

// TestYAMLParserRefusesGalaxyTOML pins that the YAML path, which an older
// binary runs on a recorded galaxy.toml, fails outright and not as a
// RolesError, so that binary's cleanup fails closed instead of reading it.
func TestYAMLParserRefusesGalaxyTOML(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(documentedGalaxyTOML), "https://default")
	if err == nil {
		t.Fatal("Parse accepted galaxy.toml as YAML")
	}
	if _, ok := errors.AsType[*RolesError](err); ok {
		t.Fatalf("Parse error = %v is a RolesError; an older binary would act on its collections", err)
	}
}
