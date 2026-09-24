package projectfile

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// documentedFixture is the documented galaxy.toml shape: every entry form the
// [project] table admits, with comments between them.
const documentedFixture = `# go-galaxy project file
[project]
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
# roles follow
roles = [
  "geerlingguy.docker,7.4.1",
  "git+https://github.com/acme/ansible-role-nginx.git,v1.2.0,nginx",
  "https://dl.example.com/acme-role-1.0.0.tar.gz",
  { name = "postgres", src = "geerlingguy.postgresql", version = "3.5.0" },
]
`

// arrayOfTablesFixture is the [[project.collections]] spelling of two entries.
const arrayOfTablesFixture = `[[project.collections]]
name = "acme.app"
version = ">= 1"

[[project.collections]]
name = "acme.lib"
`

// TestDecodeAcceptsDocumentedShapes pins that the documented example decodes
// with its metadata intact, string items left as strings and inline tables as
// map[string]any, and that the other documented spellings decode too.
func TestDecodeAcceptsDocumentedShapes(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		check func(t *testing.T)
		name  string
	}{
		{name: "the documented example", check: checkDocumentedExample},
		{name: "the array-of-tables spelling", check: checkArrayOfTables},
		{name: "roles only", check: checkRolesOnly},
		{name: "an empty collections array is present and empty", check: checkEmptyCollections},
		{name: "metadata absent stays empty", check: checkMetadataAbsent},
		{name: "a non-array list value passes through", check: checkNonArrayPassesThrough},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tt.check(t)
		})
	}
}

// mustDecode decodes src or fails the test.
func mustDecode(t *testing.T, src string) Document {
	t.Helper()

	doc, err := Decode([]byte(src))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return doc
}

func checkDocumentedExample(t *testing.T) {
	t.Helper()

	doc := mustDecode(t, documentedFixture)
	if doc.Project.Name != "infra" || doc.Project.Version != "1.0.0" || doc.Project.Description != "My infra collections" {
		t.Fatalf("metadata = %q %q %q", doc.Project.Name, doc.Project.Version, doc.Project.Description)
	}
	collections := itemsOf(t, doc.Project.Collections)
	if len(collections) != 11 {
		t.Fatalf("collections has %d items, want 11", len(collections))
	}
	assertStrings(t, collections[:7], []string{
		"sc.internal >= 0.0.20",
		"ansible.utils",
		"community.crypto >= 2.0, < 3.0",
		"community.general == 11.1.0",
		"acme.legacy ~1.5",
		"git+https://git.example.com/acme/mono.git#collections/app,main",
		"https://dl.example.com/acme-app-1.4.0.tar.gz",
	})
	assertTables(t, collections[7:], []string{"acme.app", "acme.signed", "acme.net", "https://dl.example.com/acme-lib-2.1.0.tar.gz"})
	roles := itemsOf(t, doc.Project.Roles)
	if len(roles) != 4 {
		t.Fatalf("roles has %d items, want 4", len(roles))
	}
	assertStrings(t, roles[:3], []string{
		"geerlingguy.docker,7.4.1",
		"git+https://github.com/acme/ansible-role-nginx.git,v1.2.0,nginx",
		"https://dl.example.com/acme-role-1.0.0.tar.gz",
	})
	assertTables(t, roles[3:], []string{"postgres"})
}

func checkArrayOfTables(t *testing.T) {
	t.Helper()

	doc := mustDecode(t, arrayOfTablesFixture)
	if doc.Project.Roles != nil {
		t.Fatalf("roles = %#v, want nil for an absent key", doc.Project.Roles)
	}
	assertTables(t, itemsOf(t, doc.Project.Collections), []string{"acme.app", "acme.lib"})
}

func checkRolesOnly(t *testing.T) {
	t.Helper()

	doc := mustDecode(t, "[project]\nroles = [\"geerlingguy.docker\"]\n")
	if doc.Project.Collections != nil {
		t.Fatalf("collections = %#v, want nil for an absent key", doc.Project.Collections)
	}
	assertStrings(t, itemsOf(t, doc.Project.Roles), []string{"geerlingguy.docker"})
}

func checkEmptyCollections(t *testing.T) {
	t.Helper()

	doc := mustDecode(t, "[project]\ncollections = []\n")
	if items := itemsOf(t, doc.Project.Collections); items == nil || len(items) != 0 {
		t.Fatalf("collections = %#v, want a non-nil empty slice", doc.Project.Collections)
	}
}

func checkMetadataAbsent(t *testing.T) {
	t.Helper()

	doc := mustDecode(t, "# a comment\n\n[project]\n# another\ncollections = [\"acme.app\"]\n")
	if doc.Project.Name != "" || doc.Project.Version != "" || doc.Project.Description != "" {
		t.Fatalf("metadata = %q %q %q, want all empty", doc.Project.Name, doc.Project.Version, doc.Project.Description)
	}
}

func checkNonArrayPassesThrough(t *testing.T) {
	t.Helper()

	doc := mustDecode(t, "[project]\ncollections = \"acme.app\"\n")
	if got, ok := doc.Project.Collections.(string); !ok || got != "acme.app" {
		t.Fatalf("collections = %#v, want the string passed through", doc.Project.Collections)
	}
}

// itemsOf asserts value is the []any every list reaches requirements as.
func itemsOf(t *testing.T, value any) []any {
	t.Helper()

	items, ok := value.([]any)
	if !ok {
		t.Fatalf("list is a %T, want []any", value)
	}
	return items
}

// assertStrings asserts every item is a string and the items spell want.
func assertStrings(t *testing.T, items []any, want []string) {
	t.Helper()

	got := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("item %d is a %T, want a string", i, item)
		}
		got = append(got, s)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("strings = %q, want %q", got, want)
	}
}

// assertTables asserts every item is a map[string]any and their names spell
// wantNames.
func assertTables(t *testing.T, items []any, wantNames []string) {
	t.Helper()

	got := make([]string, 0, len(items))
	for i, item := range items {
		table, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("item %d is a %T, want map[string]any", i, item)
		}
		name, _ := table["name"].(string)
		got = append(got, name)
	}
	if !slices.Equal(got, wantNames) {
		t.Fatalf("table names = %q, want %q", got, wantNames)
	}
}

// schemaViolation is one shape Decode refuses with
// ErrUnsupportedRequirementsFormat and the message fragment it must carry.
type schemaViolation struct {
	name    string
	src     string
	wantMsg string
}

// schemaViolations lists every schema refusal: a top-level table other than
// [project], a missing or non-table [project], an unknown or non-string key,
// and a [project] with neither list.
func schemaViolations() []schemaViolation {
	return []schemaViolation{
		{
			name:    "a subtable under an unknown [project] key",
			src:     "[project.settings]\nx = 1\n",
			wantMsg: "unknown key \"settings\" in [project]",
		},
		{name: "a file with only a comment", src: "# nothing here\n", wantMsg: "galaxy.toml has no [project] table"},
		{name: "an empty file", src: "", wantMsg: "galaxy.toml has no [project] table"},
		{name: "[[project]]", src: "[[project]]\ncollections = []\n", wantMsg: "[project] is not a table"},
		{name: "a [galaxy] table", src: "[galaxy]\ncollections = []\n", wantMsg: "unknown table \"galaxy\" in galaxy.toml"},
		{name: "a top-level collections key", src: "collections = [\"acme.app\"]\n", wantMsg: "unknown table \"collections\" in galaxy.toml"},
		{name: "an unknown [project] key", src: "[project]\nverion = \"1\"\ncollections = []\n", wantMsg: "unknown key \"verion\" in [project]"},
		{
			name:    "a foreign [tool.x] table",
			src:     "[project]\ncollections = []\n[tool.other]\nx = 1\n",
			wantMsg: "unknown table \"tool.other\" in galaxy.toml",
		},
		{name: "an integer name", src: "[project]\nname = 3\ncollections = []\n", wantMsg: "[project] name is not a string"},
		{name: "a float version", src: "[project]\nversion = 1.0\ncollections = []\n", wantMsg: "[project] version is not a string"},
		{
			name:    "an integer description",
			src:     "[project]\ndescription = 7\ncollections = []\n",
			wantMsg: "[project] description is not a string",
		},
		{name: "neither list", src: "[project]\nname = \"infra\"\n", wantMsg: "[project] has neither collections nor roles"},
	}
}

// TestDecodeRefusesSchemaViolations pins the sentinel and message of every
// shape the schema refuses, and that a message names a key, never a value.
func TestDecodeRefusesSchemaViolations(t *testing.T) {
	t.Parallel()

	for _, tt := range schemaViolations() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Decode([]byte(tt.src))
			if !errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) {
				t.Fatalf("Decode = %v, want %v", err, helpers.ErrUnsupportedRequirementsFormat)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("Decode = %q, want it to contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

// TestDecodeRefusesBrokenTOMLWithoutEchoingInput pins that a syntax error
// wraps ErrInvalidRequirementsTOML, names the line, and never repeats a string
// body or bare token from the input; the valid file is the control.
func TestDecodeRefusesBrokenTOMLWithoutEchoingInput(t *testing.T) {
	t.Parallel()

	if _, err := Decode([]byte("[project]\nname = \"secret\"\ncollections = []\n")); err != nil {
		t.Fatalf("the control file must decode: %v", err)
	}
	for _, tt := range []struct {
		name      string
		src       string
		wantMsg   string
		forbidden string
	}{
		{
			name:      "an unclosed array",
			src:       "[project]\ncollections = [\"acme.app\"\n",
			wantMsg:   "line 2 (last key \"project.collections\")",
			forbidden: "acme.app",
		},
		{name: "a stray bracket before any key", src: "]\n", wantMsg: "line 1", forbidden: "last key"},
		{
			name:      "an invalid escape inside a string",
			src:       "[project]\nname = \"secret \\xZZ\"\ncollections = []\n",
			wantMsg:   "line 2",
			forbidden: "secret",
		},
		{name: "a bare word as a value", src: "[project]\nname = mysecret\ncollections = []\n", wantMsg: "line 2", forbidden: "mysecret"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Decode([]byte(tt.src))
			if !errors.Is(err, helpers.ErrInvalidRequirementsTOML) {
				t.Fatalf("Decode = %v, want %v", err, helpers.ErrInvalidRequirementsTOML)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("Decode = %q, want it to contain %q", err.Error(), tt.wantMsg)
			}
			if strings.Contains(err.Error(), tt.forbidden) {
				t.Fatalf("Decode = %q echoes the input %q", err.Error(), tt.forbidden)
			}
		})
	}
}

// TestIsTOMLPath pins that the format is judged by extension alone, without
// regard to case.
func TestIsTOMLPath(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		path string
		want bool
	}{
		{path: "galaxy.toml", want: true},
		{path: "X.TOML", want: true},
		{path: "sub/dir/galaxy.toml", want: true},
		{path: "requirements.yml", want: false},
		{path: "galaxy", want: false},
		{path: "galaxy.toml.bak", want: false},
		{path: "", want: false},
	} {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()

			if got := IsTOMLPath(tt.path); got != tt.want {
				t.Fatalf("IsTOMLPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
