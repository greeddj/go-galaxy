package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const gitTestCommit = "0123456789abcdef0123456789abcdef01234567"

func gitEntry() Entry {
	return Entry{
		Name:    "acme.app",
		Type:    TypeGit,
		Version: "1.2.3",
		Source:  "https://github.com/acme/app.git",
		Ref:     "main",
		Commit:  gitTestCommit,
		Subdir:  "",
		Deps:    []string{"acme.lib"},
	}
}

// TestSchemaFollowsTheEntries pins that the schema is a function of content:
// a git entry alone makes schema 2, a Galaxy entry beside it 5, removing both
// returns to schema 1, and a producer-set schema overrides none of them.
func TestSchemaFollowsTheEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultName)
	f := &File{
		Server:        "https://galaxy.ansible.com",
		SchemaVersion: SchemaVersion, // deliberately stale: canonicalize must override it
		Collections:   []Entry{gitEntry()},
	}
	loaded := saveAndLoad(t, path, f)
	if loaded.SchemaVersion != SchemaVersionGit {
		t.Fatalf("schema = %d, want %d", loaded.SchemaVersion, SchemaVersionGit)
	}
	if got := loaded.Collections[0]; got.Name != "acme.app" || !got.IsGit() || got.Commit != gitTestCommit || got.Ref != "main" {
		t.Fatalf("git entry did not round-trip: %+v", got)
	}
	checkSavedGitFile(t, path)

	f.Collections = append(f.Collections, Entry{
		Name: "acme.lib", Version: "1.0.0", Source: "https://galaxy.ansible.com", DownloadURL: downloadURLFor("acme.lib", "1.0.0"),
	})
	f.SchemaVersion = SchemaVersionGit // stale again
	if loaded = saveAndLoad(t, path, f); loaded.SchemaVersion != SchemaVersionDownloadURL {
		t.Fatalf("schema with a Galaxy entry = %d, want %d", loaded.SchemaVersion, SchemaVersionDownloadURL)
	}

	f.Collections = nil
	f.SchemaVersion = SchemaVersionDownloadURL // stale the other way
	loaded = saveAndLoad(t, path, f)
	if loaded.SchemaVersion != SchemaVersion {
		t.Fatalf("schema after removing every entry = %d, want %d", loaded.SchemaVersion, SchemaVersion)
	}
	if SchemaVersionFor(nil, nil) != SchemaVersion || SchemaVersionFor([]Entry{gitEntry()}, nil) != SchemaVersionGit {
		t.Fatalf("SchemaVersionFor disagrees with the round trip")
	}
}

// saveAndLoad writes f to path and reads it back, failing the test on either
// error.
func saveAndLoad(t *testing.T, path string, f *File) *File {
	t.Helper()
	if err := Save(path, f); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return loaded
}

// checkSavedGitFile asserts the on-disk rendering of a schema-2 file with one
// git entry: the git fields and the schema are spelled, empty subdir and
// sha256 are omitted.
func checkSavedGitFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- path is built from this test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type: git", "ref: main", "commit: " + gitTestCommit, "schema_version: 2"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("saved file lacks %q:\n%s", want, data)
		}
	}
	if strings.Contains(string(data), "subdir:") || strings.Contains(string(data), "sha256:") {
		t.Fatalf("saved file renders an empty subdir or sha256:\n%s", data)
	}
}

// TestHashCoversGitFields pins that every git field takes part in the file
// identity: a changed commit, ref or subdir changes the hash, exactly as a
// changed version does for a Galaxy entry.
func TestHashCoversGitFields(t *testing.T) {
	t.Parallel()
	base := &File{Collections: []Entry{gitEntry()}}
	baseHash, err := base.Hash()
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(e *Entry){
		"commit": func(e *Entry) { e.Commit = strings.Repeat("f", 40) },
		"ref":    func(e *Entry) { e.Ref = "dev" },
		"subdir": func(e *Entry) { e.Subdir = "collections/app" },
		"source": func(e *Entry) { e.Source = "https://github.com/acme/app" },
	}
	for name, mutate := range mutations {
		e := gitEntry()
		mutate(&e)
		h, err := (&File{Collections: []Entry{e}}).Hash()
		if err != nil {
			t.Fatal(err)
		}
		if h == baseHash {
			t.Fatalf("changing %s did not change the hash", name)
		}
	}
}

type gitLoadCase struct {
	mutate  func(e *Entry)
	name    string
	schema  int
	wantErr bool
}

func gitLoadCases() []gitLoadCase {
	return []gitLoadCase{
		{name: "canonical git entry", schema: SchemaVersionGit},
		{name: "with subdir", schema: SchemaVersionGit, mutate: func(e *Entry) { e.Subdir = "ns/coll" }},
		{name: "scp-like source", schema: SchemaVersionGit, mutate: func(e *Entry) { e.Source = "git@github.com:acme/app.git" }},
		{name: "commit ref", schema: SchemaVersionGit, mutate: func(e *Entry) { e.Ref = gitTestCommit }},
		{name: "qualified ref", schema: SchemaVersionGit, mutate: func(e *Entry) { e.Ref = "refs/tags/v1.2.3" }},
		{name: "git entry in schema 1", schema: SchemaVersion, wantErr: true},
		{name: "unknown type", schema: SchemaVersionGit, wantErr: true, mutate: func(e *Entry) { e.Type = "url" }},
		{
			name: "non-canonical source", schema: SchemaVersionGit, wantErr: true,
			mutate: func(e *Entry) { e.Source = "HTTPS://github.com/acme/app.git" },
		},
		{
			name: "galaxy base as source", schema: SchemaVersionGit, wantErr: true,
			mutate: func(e *Entry) { e.Source = "https://galaxy.ansible.com" },
		},
		{
			name: "userinfo in source", schema: SchemaVersionGit, wantErr: true,
			mutate: func(e *Entry) { e.Source = "https://u:p@github.com/acme/app.git" },
		},
		{name: "missing ref", schema: SchemaVersionGit, wantErr: true, mutate: func(e *Entry) { e.Ref = "" }},
		{name: "abbreviated ref", schema: SchemaVersionGit, wantErr: true, mutate: func(e *Entry) { e.Ref = "0123456" }},
		{name: "missing commit", schema: SchemaVersionGit, wantErr: true, mutate: func(e *Entry) { e.Commit = "" }},
		{
			name: "upper-case commit", schema: SchemaVersionGit, wantErr: true,
			mutate: func(e *Entry) { e.Commit = strings.ToUpper(gitTestCommit) },
		},
		{name: "short commit", schema: SchemaVersionGit, wantErr: true, mutate: func(e *Entry) { e.Commit = gitTestCommit[:39] }},
		{name: "unsafe subdir", schema: SchemaVersionGit, wantErr: true, mutate: func(e *Entry) { e.Subdir = "../x" }},
		{name: "non-canonical subdir", schema: SchemaVersionGit, wantErr: true, mutate: func(e *Entry) { e.Subdir = "/ns/coll/" }},
		{name: "sha256 on a git entry", schema: SchemaVersionGit, wantErr: true, mutate: func(e *Entry) { e.SHA256 = strings.Repeat("a", 64) }},
		{name: "non-exact version", schema: SchemaVersionGit, wantErr: true, mutate: func(e *Entry) { e.Version = "*" }},
		{name: "galaxy entry with a commit", schema: SchemaVersionGit, wantErr: true, mutate: func(e *Entry) { e.Type = "" }},
	}
}

// TestLoadJudgesGitEntries writes each row straight to disk, bypassing Save's
// canonicalization, because the file is repository content and what Load
// accepts is the contract.
func TestLoadJudgesGitEntries(t *testing.T) {
	t.Parallel()
	for _, tt := range gitLoadCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := gitEntry()
			if tt.mutate != nil {
				tt.mutate(&e)
			}
			path := writeRawLockfile(t, tt.schema, e)
			_, err := Load(path)
			if tt.wantErr {
				if !errors.Is(err, helpers.ErrLockfileInvalid) {
					t.Fatalf("Load error = %v, want ErrLockfileInvalid", err)
				}
				if strings.Contains(err.Error(), "u:p@") {
					t.Fatalf("error echoes the credential: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
}

// writeRawLockfile renders one entry with the exact schema asked for, without
// going through Save (which would rewrite the schema from the content).
func writeRawLockfile(t *testing.T, schema int, e Entry) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("collections:\n")
	b.WriteString("    - name: " + e.Name + "\n")
	if e.Type != "" {
		b.WriteString("      type: " + e.Type + "\n")
	}
	b.WriteString("      version: " + quoteYAML(e.Version) + "\n")
	b.WriteString("      source: " + e.Source + "\n")
	if e.Ref != "" {
		b.WriteString("      ref: " + e.Ref + "\n")
	}
	if e.Commit != "" {
		b.WriteString("      commit: " + e.Commit + "\n")
	}
	if e.Subdir != "" {
		b.WriteString("      subdir: " + e.Subdir + "\n")
	}
	if e.SHA256 != "" {
		b.WriteString("      sha256: " + e.SHA256 + "\n")
	}
	b.WriteString("schema_version: " + itoa(schema) + "\n")
	path := filepath.Join(t.TempDir(), DefaultName)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func quoteYAML(s string) string { return `"` + s + `"` }

func itoa(i int) string { return strconv.Itoa(i) }

// TestCompareReportsGitFields pins that Change.Fields names each git field,
// in the documented order, and that sameEntry treats them as identity.
func TestCompareReportsGitFields(t *testing.T) {
	t.Parallel()
	before := &File{SchemaVersion: SchemaVersionGit, Collections: []Entry{gitEntry()}}
	changed := gitEntry()
	changed.Ref = "dev"
	changed.Commit = strings.Repeat("e", 40)
	changed.Subdir = "sub"
	after := &File{SchemaVersion: SchemaVersionGit, Collections: []Entry{changed}}
	diff := Compare(before, after)
	if len(diff.Updated) != 1 || len(diff.Added)+len(diff.Removed) != 0 {
		t.Fatalf("Compare = %+v, want one update", diff)
	}
	fields := diff.Updated[0].Fields()
	names := make([]string, 0, len(fields))
	for _, fc := range fields {
		names = append(names, fc.Field)
	}
	if got := strings.Join(names, ","); got != "ref,commit,subdir" {
		t.Fatalf("Fields = %s, want ref,commit,subdir", got)
	}
	if !Compare(before, &File{SchemaVersion: SchemaVersionGit, Collections: []Entry{gitEntry()}}).Empty() {
		t.Fatalf("identical git entries reported as different")
	}
	// A git entry dropped to schema 1 by removing it is not drift of the
	// remaining entries: the schema is not a compared field.
	galaxy := Entry{Name: "acme.lib", Version: "1.0.0", Source: "https://galaxy.ansible.com"}
	two := &File{SchemaVersion: SchemaVersionGit, Collections: []Entry{gitEntry(), galaxy}}
	one := &File{SchemaVersion: SchemaVersion, Collections: []Entry{galaxy}}
	d := Compare(two, one)
	if len(d.Removed) != 1 || len(d.Updated) != 0 || len(d.Added) != 0 {
		t.Fatalf("dropping the git entry reported %+v, want exactly one removal", d)
	}
}

// gitOtherCommit is a second forty-hex commit, for a ref that names another
// commit than the entry pins.
const gitOtherCommit = "fedcba9876543210fedcba9876543210fedcba98"

// TestLoadRefusesACommitRefLockedAtAnotherCommit pins that a git entry whose
// ref is a commit pins that same commit: --frozen holds the ref to the
// requirement and installs the commit, so the two may not disagree.
func TestLoadRefusesACommitRefLockedAtAnotherCommit(t *testing.T) {
	t.Parallel()
	e := gitEntry()
	e.Ref = gitOtherCommit
	_, err := Load(writeRawLockfile(t, SchemaVersionGit, e))
	const want = `lockfile is invalid: acme.app: ref "fedcba9876543210fedcba9876543210fedcba98" ` +
		`is a commit and differs from commit "0123456789abcdef0123456789abcdef01234567"`
	if !errors.Is(err, helpers.ErrLockfileInvalid) || err.Error() != want {
		t.Fatalf("Load = %v, want ErrLockfileInvalid reading %q", err, want)
	}
	e.Ref = gitTestCommit
	if _, err := Load(writeRawLockfile(t, SchemaVersionGit, e)); err != nil {
		t.Fatalf("Load of a commit ref locked at that commit: %v", err)
	}
}

// gitSSHSource is how lock records an ssh remote: gitsource requires the
// scheme form to name its user, and keeps a port other than 22.
const gitSSHSource = "ssh://git@git.example.com:2222/acme/app.git"

// TestLoadAcceptsAnSSHGitSourceNamingItsUser pins that the file lock writes
// for a git collection and a git role fetched over ssh loads back: the user
// such a URL must name is no credential, and gitsource's grammar judges it.
func TestLoadAcceptsAnSSHGitSourceNamingItsUser(t *testing.T) {
	t.Parallel()
	collection := gitEntry()
	collection.Source = gitSSHSource
	role := gitRoleEntry()
	role.Source = gitSSHSource
	loaded := saveAndLoad(t, filepath.Join(t.TempDir(), DefaultName),
		&File{Collections: []Entry{collection}, Roles: []RoleEntry{role}})
	if got := loaded.Collections[0].Source; got != gitSSHSource {
		t.Fatalf("collection source = %q, want %q", got, gitSSHSource)
	}
	if got := loaded.Roles[0].Source; got != gitSSHSource {
		t.Fatalf("role source = %q, want %q", got, gitSSHSource)
	}
}

// credentialSourceCase is a source carrying a credential, written by write
// onto the entry kind that must refuse it, and the exact refusal Load returns.
type credentialSourceCase struct {
	write    func(t *testing.T, source string) string
	name     string
	source   string
	want     string
	userinfo bool
}

const (
	userinfoRefusal  = "galaxy server url must not contain userinfo"
	gitSourceRefusal = "lockfile is invalid: acme.app: source is not a canonical git repository URL"
)

func credentialSourceCases() []credentialSourceCase {
	return []credentialSourceCase{
		{
			name: "user in a Galaxy source", write: writeSourceLockfile, source: "https://git@hub.example.invalid/",
			want: "lockfile is invalid: acme.widgets: " + userinfoRefusal, userinfo: true,
		},
		{
			name: "ssh git spelling as a Galaxy source", write: writeSourceLockfile, source: gitSSHSource,
			want: "lockfile is invalid: acme.widgets: " + userinfoRefusal, userinfo: true,
		},
		{
			name: "user in a url source", write: writeURLSourceLockfile, source: "https://git@example.com/dl/acme-app-1.2.3.tar.gz",
			want: "lockfile is invalid: acme.app: " + userinfoRefusal, userinfo: true,
		},
		{
			name: "user in an https git source", write: writeGitSourceLockfile,
			source: "https://git@github.com/acme/app.git", want: gitSourceRefusal,
		},
		{ //nolint:gosec // a fixture URL, not a credential
			name: "password in an https git source", write: writeGitSourceLockfile,
			source: "https://u:hunter2@github.com/acme/app.git", want: gitSourceRefusal,
		},
		{ //nolint:gosec // a fixture URL, not a credential
			name: "password in an ssh git source", write: writeGitSourceLockfile,
			source: "ssh://git:hunter2@git.example.com:2222/acme/app.git", want: gitSourceRefusal,
		},
	}
}

// TestLoadStillRefusesACredentialInASource is the control for
// TestLoadAcceptsAnSSHGitSourceNamingItsUser: only a git entry's ssh user is
// let through, and the exact text pins that no refusal prints the source.
func TestLoadStillRefusesACredentialInASource(t *testing.T) {
	t.Parallel()
	for _, tt := range credentialSourceCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(tt.write(t, tt.source))
			if !errors.Is(err, helpers.ErrLockfileInvalid) || err.Error() != tt.want {
				t.Fatalf("Load = %v, want ErrLockfileInvalid reading %q", err, tt.want)
			}
			if got := errors.Is(err, helpers.ErrGalaxyServerURLUserinfo); got != tt.userinfo {
				t.Fatalf("errors.Is(err, ErrGalaxyServerURLUserinfo) = %t, want %t", got, tt.userinfo)
			}
		})
	}
}

// writeGitSourceLockfile writes gitEntry with source in place of its own.
func writeGitSourceLockfile(t *testing.T, source string) string {
	t.Helper()
	e := gitEntry()
	e.Source = source
	return writeRawLockfile(t, SchemaVersionGit, e)
}

// writeURLSourceLockfile writes urlEntry with source in place of its own.
func writeURLSourceLockfile(t *testing.T, source string) string {
	t.Helper()
	e := urlEntry()
	e.Source = source
	return writeRawLockfile(t, SchemaVersionURL, e)
}
