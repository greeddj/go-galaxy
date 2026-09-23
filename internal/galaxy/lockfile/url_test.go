package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"go.yaml.in/yaml/v3"
)

const (
	urlTestSHA    = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	urlTestSource = "https://example.com/dl/acme-app-1.2.3.tar.gz"
)

func urlEntry() Entry {
	return Entry{
		Name:    "acme.app",
		Type:    TypeURL,
		Version: "1.2.3",
		Source:  urlTestSource,
		SHA256:  urlTestSHA,
		Deps:    []string{"acme.lib"},
	}
}

func urlRoleEntry() RoleEntry {
	return RoleEntry{
		Name:    "myrole",
		Type:    RoleTypeURL,
		Version: "1.2.3",
		Source:  "https://example.com/dl/myrole-1.2.3.tar.gz",
		SHA256:  urlTestSHA,
	}
}

// TestSchemaFollowsURLEntries pins schema 4 ranking over the rest: a url
// collection alone or a url role beside git entries is 4, roles without a
// url entry stay 3, and dropping the last url entry goes back down.
func TestSchemaFollowsURLEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultName)

	f := &File{Collections: []Entry{urlEntry()}}
	loaded := saveAndLoad(t, path, f)
	if loaded.SchemaVersion != SchemaVersionURL {
		t.Fatalf("schema = %d, want %d", loaded.SchemaVersion, SchemaVersionURL)
	}
	if got := loaded.Collections[0]; !got.IsURL() || got.SHA256 != urlTestSHA || got.Source != urlTestSource {
		t.Fatalf("url entry did not round-trip: %+v", got)
	}
	checkSavedURLFile(t, path)

	f = &File{Collections: []Entry{gitEntry()}, Roles: []RoleEntry{urlRoleEntry()}}
	if loaded = saveAndLoad(t, path, f); loaded.SchemaVersion != SchemaVersionURL {
		t.Fatalf("schema with a url role = %d, want %d", loaded.SchemaVersion, SchemaVersionURL)
	}

	f = &File{Roles: []RoleEntry{gitRoleEntry()}}
	if loaded = saveAndLoad(t, path, f); loaded.SchemaVersion != SchemaVersionRoles {
		t.Fatalf("schema without a url entry = %d, want %d", loaded.SchemaVersion, SchemaVersionRoles)
	}

	table := []struct {
		entries []Entry
		roles   []RoleEntry
		want    int
	}{
		{entries: []Entry{urlEntry()}, want: SchemaVersionURL},
		{roles: []RoleEntry{urlRoleEntry()}, want: SchemaVersionURL},
		{entries: []Entry{gitEntry()}, roles: []RoleEntry{gitRoleEntry()}, want: SchemaVersionRoles},
		{entries: []Entry{gitEntry()}, want: SchemaVersionGit},
		{want: SchemaVersion},
	}
	for _, tt := range table {
		if got := SchemaVersionFor(tt.entries, tt.roles); got != tt.want {
			t.Fatalf("SchemaVersionFor(%+v, %+v) = %d, want %d", tt.entries, tt.roles, got, tt.want)
		}
	}
}

// checkSavedURLFile asserts the on-disk rendering of a schema-4 file with one
// url collection entry: the type, source and sha256 are spelled, and no git
// field is rendered.
func checkSavedURLFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- path is built from this test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type: url", "source: " + urlTestSource, "sha256: " + urlTestSHA, "schema_version: 4"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("saved file lacks %q:\n%s", want, data)
		}
	}
	for _, absent := range []string{"ref:", "commit:", "subdir:", "roles:"} {
		if strings.Contains(string(data), absent) {
			t.Fatalf("saved file renders %q for a url-only file:\n%s", absent, data)
		}
	}
}

// TestURLRoleRendersNoRefOrCommit pins the omitempty flip on RoleEntry: a url
// role renders neither ref nor commit, while a git role beside it still
// renders both.
func TestURLRoleRendersNoRefOrCommit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), DefaultName)
	f := &File{Roles: []RoleEntry{urlRoleEntry(), gitRoleEntry()}}
	saveAndLoad(t, path, f)
	data, err := os.ReadFile(path) // #nosec G304 -- path is built from this test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "ref:") != 1 || strings.Count(string(data), "commit:") != 1 {
		t.Fatalf("expected exactly the git role's ref and commit:\n%s", data)
	}
	if strings.Count(string(data), "sha256: "+urlTestSHA) != 1 {
		t.Fatalf("expected exactly the url role's sha256:\n%s", data)
	}
}

type urlLoadCase struct {
	mutate func(*File)
	name   string
}

func urlLoadCases() []urlLoadCase {
	return []urlLoadCase{
		{name: "url entry in a schema-3 file", mutate: func(f *File) {
			f.SchemaVersion = SchemaVersionRoles
			f.Roles = nil
		}},
		{name: "missing sha256", mutate: func(f *File) { f.Collections[0].SHA256 = "" }},
		{name: "uppercase sha256", mutate: func(f *File) { f.Collections[0].SHA256 = strings.ToUpper(urlTestSHA) }},
		{name: "short sha256", mutate: func(f *File) { f.Collections[0].SHA256 = "abc123" }},
		{name: "non-canonical source", mutate: func(f *File) { f.Collections[0].Source = "HTTPS://example.com/x.tar.gz" }},
		{name: "git url source", mutate: func(f *File) { f.Collections[0].Source = "git+https://h/r.git" }},
		{name: "ref on a url entry", mutate: func(f *File) { f.Collections[0].Ref = "stray" }},
		{name: "commit on a url entry", mutate: func(f *File) { f.Collections[0].Commit = gitTestCommit }},
		{name: "subdir on a url entry", mutate: func(f *File) { f.Collections[0].Subdir = "sub" }},
		{name: "url role in a schema-3 file", mutate: func(f *File) {
			f.SchemaVersion = SchemaVersionRoles
			f.Collections = nil
			f.Roles = []RoleEntry{urlRoleEntry()}
		}},
		{name: "url role missing sha256", mutate: func(f *File) {
			role := urlRoleEntry()
			role.SHA256 = ""
			f.Roles = []RoleEntry{role}
		}},
		{name: "url role with a ref", mutate: func(f *File) {
			role := urlRoleEntry()
			role.Ref = "stray"
			f.Roles = []RoleEntry{role}
		}},
		{name: "url role with a commit", mutate: func(f *File) {
			role := urlRoleEntry()
			role.Commit = gitTestCommit
			f.Roles = []RoleEntry{role}
		}},
		{name: "url role with a galaxy name", mutate: func(f *File) {
			role := urlRoleEntry()
			role.Galaxy = "acme.role"
			f.Roles = []RoleEntry{role}
		}},
		{name: "url role with a git source", mutate: func(f *File) {
			role := urlRoleEntry()
			role.Source = "git+https://github.com/acme/role.git"
			f.Roles = []RoleEntry{role}
		}},
		{name: "git role with a sha256", mutate: func(f *File) {
			role := gitRoleEntry()
			role.SHA256 = urlTestSHA
			f.Roles = []RoleEntry{role}
		}},
	}
}

// TestLoadJudgesURLEntries walks every refusal a hand-edited url entry can
// earn, plus the sha256 a git or galaxy role may not carry.
func TestLoadJudgesURLEntries(t *testing.T) {
	t.Parallel()
	for _, tt := range urlLoadCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), DefaultName)
			f := &File{Collections: []Entry{urlEntry()}}
			if err := Save(path, f); err != nil {
				t.Fatalf("Save: %v", err)
			}
			loaded, err := Load(path)
			if err != nil {
				t.Fatalf("Load of the untouched file: %v", err)
			}
			tt.mutate(loaded)
			rewriteRaw(t, path, loaded)
			if _, err := Load(path); !errors.Is(err, helpers.ErrLockfileInvalid) {
				t.Fatalf("Load error = %v, want ErrLockfileInvalid", err)
			}
		})
	}
}

// rewriteRaw writes f to path preserving its fields as they are, bypassing
// Save's canonicalize: the tests above model a hand-edited file, which Save
// would silently repair.
func rewriteRaw(t *testing.T, path string, f *File) {
	t.Helper()
	data, err := yaml.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestHashCoversURLFields pins that the url pin takes part in the file
// identity: a changed sha256 or source changes the hash.
func TestHashCoversURLFields(t *testing.T) {
	t.Parallel()
	base := &File{Collections: []Entry{urlEntry()}}
	baseHash, err := base.Hash()
	if err != nil {
		t.Fatal(err)
	}
	changedSHA := &File{Collections: []Entry{urlEntry()}}
	changedSHA.Collections[0].SHA256 = strings.Repeat("0", 64)
	changedSource := &File{Collections: []Entry{urlEntry()}}
	changedSource.Collections[0].Source = "https://example.com/dl/other.tar.gz"
	for name, f := range map[string]*File{"sha256": changedSHA, "source": changedSource} {
		h, err := f.Hash()
		if err != nil {
			t.Fatal(err)
		}
		if h == baseHash {
			t.Fatalf("hash did not change with the %s", name)
		}
	}
}

// TestCompareReportsRoleSHA256 pins that a url role's changed sha256 is
// reported as drift with its own field line.
func TestCompareReportsRoleSHA256(t *testing.T) {
	t.Parallel()
	before := &File{Roles: []RoleEntry{urlRoleEntry()}}
	after := &File{Roles: []RoleEntry{urlRoleEntry()}}
	after.Roles[0].SHA256 = strings.Repeat("0", 64)
	diff := Compare(before, after)
	if len(diff.RolesUpdated) != 1 {
		t.Fatalf("RolesUpdated = %+v, want one change", diff.RolesUpdated)
	}
	fields := diff.RolesUpdated[0].Fields()
	if len(fields) != 1 || fields[0].Field != fieldSHA256 {
		t.Fatalf("Fields() = %+v, want one sha256 change", fields)
	}
}
