package requirements

import (
	"reflect"
	"testing"
)

// hashGoldenFixture holds one entry of every collection and role kind, with
// a spaced constraint and signatures out of order, one carrying a query.
const hashGoldenFixture = `collections:
  - name: acme.app
    version: ">= 1.0.0, < 2.0.0"
    signatures:
      - https://sig.example.com/b.asc?X-Amz-Signature=abc
      - https://sig.example.com/a.asc
  - name: git+https://git.example.com/acme/mono.git#collections/tool,v1.2.0
  - name: https://files.example.com/acme-lib-1.0.0.tar.gz
    type: url
    version: 1.0.0
roles:
  - name: geerlingguy.docker
    version: 8.0.0
  - src: https://git.example.com/acme/ansible-role-base.git
    scm: git
    version: v1.4.0
    name: base
  - src: https://files.example.com/role-web.tar.gz
    name: web
`

// mustHash parses data as requirements.yml with no default server and
// returns its Hash, failing the test when the file does not load.
func mustHash(t *testing.T, data string) string {
	t.Helper()
	f, err := Parse([]byte(data), "")
	if err != nil {
		t.Fatalf("Parse(%q): %v", data, err)
	}
	return f.Hash()
}

// TestHashGoldenRecordLayout pins the digest of a fixed file. The literal was
// computed outside Go; a record layout change needs a new version word in
// digestDomain and a row in the upgrade notes, since it moves every key.
func TestHashGoldenRecordLayout(t *testing.T) {
	t.Parallel()
	const want = "f63857bfa6a47297653f73418028cf2f4a5757cd0990af6a8dcaf49d2464133d"
	if got := mustHash(t, hashGoldenFixture); got != want {
		t.Errorf("Hash() = %q, want %q", got, want)
	}
}

// hashPair is two requirements files a test compares by their Hash.
type hashPair struct {
	name, a, b string
}

// hashSpellingPairs are files that ask for the same thing in other words.
func hashSpellingPairs() []hashPair {
	return append([]hashPair{
		{
			name: "comments and indentation",
			a:    "collections:\n  - name: acme.app\n    version: 1.0.0\n",
			b:    "# pinned\ncollections:\n    -   name: acme.app  # the app\n        version: 1.0.0\n",
		},
		{name: "string and mapping", a: "collections:\n  - acme.app\n", b: "collections:\n  - name: acme.app\n"},
		{
			name: "namespace and name keys, and a dotted name",
			a:    "collections:\n  - namespace: acme\n    name: app\n",
			b:    "collections:\n  - name: acme.app\n",
		},
		{name: "no version and *", a: "collections:\n  - name: acme.app\n", b: "collections:\n  - name: acme.app\n    version: '*'\n"},
		{name: "bare and = exact version", a: "collections:\n  - name: acme.app\n    version: 1.0.0\n",
			b: "collections:\n  - name: acme.app\n    version: '=1.0.0'\n"},
		{name: "bare and == exact version", a: "collections:\n  - name: acme.app\n    version: 1.0.0\n",
			b: "collections:\n  - name: acme.app\n    version: '==1.0.0'\n"},
		{name: "space and comma between clauses", a: "collections:\n  - name: acme.app\n    version: '>=1.0 <2.0'\n",
			b: "collections:\n  - name: acme.app\n    version: '>=1.0,<2.0'\n"},
		{name: "collection order", a: "collections:\n  - acme.app\n  - acme.lib\n", b: "collections:\n  - acme.lib\n  - acme.app\n"},
		{
			name: "signature order and query",
			a: "collections:\n  - name: acme.app\n    signatures:\n" +
				"      - https://sig.example.com/b.asc?X-Amz-Signature=abc\n      - https://sig.example.com/a.asc\n",
			b: "collections:\n  - name: acme.app\n    signatures:\n" +
				"      - https://sig.example.com/a.asc\n      - https://sig.example.com/b.asc\n",
		},
	}, hashLayoutSpellingPairs()...)
}

// hashLayoutSpellingPairs are equal files that differ in the file's layout or
// in a default spelled out.
func hashLayoutSpellingPairs() []hashPair {
	return []hashPair{
		{name: "empty collections and empty roles", a: "collections: []\n", b: "roles: []\n"},
		{name: "bare collections key and an empty list", a: "collections:\n", b: "collections: []\n"},
		{name: "bare list and mapping", a: "- acme.app\n", b: "collections:\n  - acme.app\n"},
		{
			name: "git without a ref and with HEAD",
			a:    "collections:\n  - name: git+https://git.example.com/acme/mono.git#collections/tool\n",
			b:    "collections:\n  - name: git+https://git.example.com/acme/mono.git#collections/tool,HEAD\n",
		},
		{
			name: "type galaxy and no type",
			a:    "collections:\n  - name: acme.app\n    type: galaxy\n",
			b:    "collections:\n  - name: acme.app\n",
		},
	}
}

// TestHashIgnoresSpelling pins that what a file asks for, not how it is
// written, decides the key.
func TestHashIgnoresSpelling(t *testing.T) {
	t.Parallel()
	for _, tt := range hashSpellingPairs() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if a, b := mustHash(t, tt.a), mustHash(t, tt.b); a != b {
				t.Errorf("Hash differs for\n%s\nand\n%s", tt.a, tt.b)
			}
		})
	}
}

// hashMeaningPairs are files that ask for different things.
func hashMeaningPairs() []hashPair {
	return []hashPair{
		{
			name: "an explicit source and none",
			a:    "collections:\n  - name: acme.app\n    source: https://hub.example.com/api/galaxy/\n",
			b:    "collections:\n  - name: acme.app\n",
		},
		{name: "a hyphen range and a prerelease", a: "collections:\n  - name: acme.app\n    version: '1.0 - 2.0'\n",
			b: "collections:\n  - name: acme.app\n    version: '1.0-2.0'\n"},
		{name: "== match-all and no version", a: "collections:\n  - name: acme.app\n    version: '==*'\n",
			b: "collections:\n  - name: acme.app\n"},
		{name: "1.0 and 1.0.0", a: "collections:\n  - name: acme.app\n    version: '1.0'\n",
			b: "collections:\n  - name: acme.app\n    version: 1.0.0\n"},
		{name: "clause order", a: "collections:\n  - name: acme.app\n    version: '>=1,<2'\n",
			b: "collections:\n  - name: acme.app\n    version: '<2,>=1'\n"},
		{name: "role order", a: "roles:\n  - name: acme.web\n  - name: acme.db\n", b: "roles:\n  - name: acme.db\n  - name: acme.web\n"},
		{
			name: "another git ref",
			a:    "collections:\n  - name: git+https://git.example.com/acme/mono.git#collections/tool,v1.0.0\n",
			b:    "collections:\n  - name: git+https://git.example.com/acme/mono.git#collections/tool,v1.1.0\n",
		},
		{
			name: "another git subdir",
			a:    "collections:\n  - name: git+https://git.example.com/acme/mono.git#collections/tool\n",
			b:    "collections:\n  - name: git+https://git.example.com/acme/mono.git#collections/app\n",
		},
		{
			name: "a url entry with an asserted version and without",
			a:    "collections:\n  - name: https://files.example.com/acme-lib-1.0.0.tar.gz\n    type: url\n    version: 1.0.0\n",
			b:    "collections:\n  - name: https://files.example.com/acme-lib-1.0.0.tar.gz\n    type: url\n",
		},
		{
			name: "an added signature",
			a:    "collections:\n  - name: acme.app\n    signatures:\n      - https://sig.example.com/a.asc\n",
			b:    "collections:\n  - name: acme.app\n",
		},
		{
			name: "another role version",
			a:    "roles:\n  - name: geerlingguy.docker\n    version: 8.0.0\n",
			b:    "roles:\n  - name: geerlingguy.docker\n    version: 8.1.0\n",
		},
	}
}

// TestHashSeparatesMeaning pins that files asking for different things never
// share a key.
func TestHashSeparatesMeaning(t *testing.T) {
	t.Parallel()
	for _, tt := range hashMeaningPairs() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if a, b := mustHash(t, tt.a), mustHash(t, tt.b); a == b {
				t.Errorf("Hash is equal for\n%s\nand\n%s", tt.a, tt.b)
			}
		})
	}
}

// hashTwinYAML and hashTwinTOML ask for the same entries: comments, flow
// style, respelled constraints, a project name and a settings table apart.
const (
	hashTwinYAML = `# the same entries as galaxy.toml
collections: [{name: ns.y, version: "==1.4.0"}, {name: ns.x, version: ">= 1.0.0, <2.0.0"}]
roles:
  - name: geerlingguy.docker  # pinned
    version: 8.0.0
`
	hashTwinTOML = `[project]
name = "infra"
collections = ["ns.x >=1.0.0,<2.0.0", { name = "ns.y", version = "1.4.0" }]
roles = ["geerlingguy.docker,8.0.0"]

[tool.go-galaxy]
workers = 8
`
)

// TestHashMatchesAcrossFormats pins that a requirements.yml and a galaxy.toml
// parsed to different Files still share a key when they ask for the same.
func TestHashMatchesAcrossFormats(t *testing.T) {
	t.Parallel()
	yamlFile, err := Parse([]byte(hashTwinYAML), "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tomlFile, err := ParseTOML([]byte(hashTwinTOML), "")
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if reflect.DeepEqual(yamlFile, tomlFile) {
		t.Fatalf("the two files parse to equal Files, so they cannot show the key ignoring spelling")
	}
	if a, b := yamlFile.Hash(), tomlFile.Hash(); a != b {
		t.Errorf("Hash of requirements.yml = %q, of galaxy.toml = %q, want them equal", a, b)
	}
}

// TestHashRecordsEveryEntryField counts the entry fields, so a new one fails
// here until collectionRecord or the role record in Hash writes it.
func TestHashRecordsEveryEntryField(t *testing.T) {
	t.Parallel()
	if n := reflect.TypeFor[CollectionRequirement]().NumField(); n != 8 {
		t.Errorf("CollectionRequirement has %d fields, want 8: a new field must enter collectionRecord", n)
	}
	if n := reflect.TypeFor[RoleRequirement]().NumField(); n != 4 {
		t.Errorf("RoleRequirement has %d fields, want 4: a new field must enter the role record in Hash", n)
	}
}
