package lockfile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "galaxy.lock")

	f := &File{
		Server: "https://galaxy.ansible.com",
		Collections: []Entry{
			{
				Name:    "community.general",
				Version: "11.1.0",
				Source:  "https://galaxy.ansible.com",
				SHA256:  "deadbeef",
				Deps:    []string{"ansible.posix"},
			},
			{
				Name:    "ansible.posix",
				Version: "2.0.0",
				Source:  "https://galaxy.ansible.com",
				SHA256:  "feedface",
			},
		},
	}
	if err := Save(path, f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Fatalf("schema=%d, want %d", got.SchemaVersion, SchemaVersion)
	}
	if len(got.Collections) != 2 {
		t.Fatalf("expected 2 collections, got %d", len(got.Collections))
	}
	if got.Collections[0].Name != "ansible.posix" {
		t.Fatalf("expected sorted by name first=ansible.posix, got %q", got.Collections[0].Name)
	}
}

func TestLoadMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "missing.yml"))
	if !IsNotExist(err) {
		t.Fatalf("expected not-exist, got %v", err)
	}
}

// TestLoadWrapsUnreadableFileAsInvalid pins that a path Load cannot read (a
// directory: chmod 0000 proves nothing as root) is ErrLockfileInvalid and not
// IsNotExist, and that a valid file at the same path then loads.
func TestLoadWrapsUnreadableFileAsInvalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "lockfile-is-a-dir.yml")
	if err := os.Mkdir(path, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}

	_, err := Load(path)
	if !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("expected errors.Is helpers.ErrLockfileInvalid, got %v", err)
	}
	if IsNotExist(err) {
		t.Fatalf("expected IsNotExist(err) == false, got true for %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove directory: %v", err)
	}
	valid := &File{SchemaVersion: SchemaVersion, Collections: []Entry{{Name: "a.a", Version: "1.0.0"}}}
	if err := Save(path, valid); err != nil {
		t.Fatalf("save valid lockfile at the same path: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load after replacing the directory with a valid lockfile: %v", err)
	}
}

// TestLoadAbsentFileIsNotInvalid pins the other side of Load's dichotomy: an
// absent path is IsNotExist and never also helpers.ErrLockfileInvalid.
func TestLoadAbsentFileIsNotInvalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "missing.yml"))
	if !IsNotExist(err) {
		t.Fatalf("expected IsNotExist, got %v", err)
	}
	if errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("expected errors.Is helpers.ErrLockfileInvalid == false, got true for %v", err)
	}
}

func TestLoadInvalidSchema(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yml")
	if err := os.WriteFile(path, []byte("schema_version: 9999\ncollections: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("expected ErrLockfileInvalid, got %v", err)
	}
}

func TestLoadRejectsDuplicateNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "dup.yml")
	yamlContent := fmt.Sprintf(
		"schema_version: %d\ncollections:\n  - name: a.a\n    version: 1.0.0\n  - name: a.a\n    version: 2.0.0\n",
		SchemaVersion,
	)
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("expected ErrLockfileInvalid, got %v", err)
	}
}

// TestLoadRejectsNonExactVersion pins that Load refuses a constraint ("*") as
// a pinned version, which a --frozen install would resolve to the server's
// highest; TestSaveLoadRoundTrip is the positive control.
func TestLoadRejectsNonExactVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wildcard.yml")
	yamlContent := fmt.Sprintf(
		"schema_version: %d\ncollections:\n  - name: a.a\n    version: \"*\"\n",
		SchemaVersion,
	)
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("expected ErrLockfileInvalid, got %v", err)
	}
}

func TestHashIsStable(t *testing.T) {
	t.Parallel()
	a := &File{Collections: []Entry{
		{Name: "b.b", Version: "1.0.0", Deps: []string{"z.z", "a.a"}},
		{Name: "a.a", Version: "1.0.0"},
	}}
	b := &File{Collections: []Entry{
		{Name: "a.a", Version: "1.0.0"},
		{Name: "b.b", Version: "1.0.0", Deps: []string{"a.a", "z.z"}},
	}}
	ah, err := a.Hash()
	if err != nil {
		t.Fatal(err)
	}
	bh, err := b.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if ah != bh {
		t.Fatalf("expected stable hash regardless of collection/deps order, got %q vs %q", ah, bh)
	}
}

func TestHashPureNoMutation(t *testing.T) {
	t.Parallel()
	f := &File{Collections: []Entry{
		{Name: "b.b", Version: "1.0.0", Deps: []string{"z.z", "a.a"}},
		{Name: "a.a", Version: "1.0.0", Deps: []string{"y.y", "b.b"}},
	}}
	origNames := collectionNames(f)
	origDeps := collectionDeps(f)

	h1, err := f.Hash()
	if err != nil {
		t.Fatalf("Hash (1st call): %v", err)
	}
	h2, err := f.Hash()
	if err != nil {
		t.Fatalf("Hash (2nd call): %v", err)
	}
	if h1 != h2 {
		t.Fatalf("expected repeated Hash calls to agree, got %q vs %q", h1, h2)
	}

	if got := collectionNames(f); !equalStrings(got, origNames) {
		t.Fatalf("Hash mutated Collections order: got %v, want %v", got, origNames)
	}
	if got := collectionDeps(f); !equalDeps(got, origDeps) {
		t.Fatalf("Hash mutated Deps order: got %v, want %v", got, origDeps)
	}
}

func TestSaveDoesNotMutate(t *testing.T) {
	t.Parallel()
	f := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "b.b", Version: "1.0.0", Deps: []string{"z.z", "a.a"}},
		{Name: "a.a", Version: "1.0.0", Deps: []string{"y.y", "b.b"}},
	}}
	origNames := collectionNames(f)
	origDeps := collectionDeps(f)

	path := filepath.Join(t.TempDir(), DefaultName)
	if err := Save(path, f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if got := collectionNames(f); !equalStrings(got, origNames) {
		t.Fatalf("Save mutated Collections order: got %v, want %v", got, origNames)
	}
	if got := collectionDeps(f); !equalDeps(got, origDeps) {
		t.Fatalf("Save mutated Deps order: got %v, want %v", got, origDeps)
	}

	// The file on disk is still canonical (sorted by name) regardless of the
	// in-memory order Save was handed.
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := collectionNames(loaded); !equalStrings(got, []string{"a.a", "b.b"}) {
		t.Fatalf("saved lockfile is not canonical: got %v", got)
	}
}

func collectionNames(f *File) []string {
	names := make([]string, len(f.Collections))
	for i, e := range f.Collections {
		names[i] = e.Name
	}
	return names
}

func collectionDeps(f *File) [][]string {
	deps := make([][]string, len(f.Collections))
	for i, e := range f.Collections {
		deps[i] = append([]string(nil), e.Deps...)
	}
	return deps
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalDeps(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !equalStrings(a[i], b[i]) {
			return false
		}
	}
	return true
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "galaxy.lock")

	f := &File{Collections: []Entry{{Name: "a.a", Version: "1.0.0"}}}
	if err := Save(path, f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no leftover temp files, found %v", matches)
	}
}

func TestSaveDoesNotClobberOnFailure(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "galaxy.lock")

	original := &File{Collections: []Entry{
		{Name: "a.a", Version: "1.0.0", SHA256: "original"},
	}}
	if err := Save(path, original); err != nil {
		t.Fatalf("Save (seed): %v", err)
	}

	//nolint:gosec // G302: 0o500 is a directory mode (read+traverse, no write), needed to force Save to fail.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod dir: %v", err)
	}
	defer func() {
		if err := os.Chmod(dir, helpers.DirMod); err != nil {
			t.Fatalf("restore dir mode: %v", err)
		}
	}()

	updated := &File{Collections: []Entry{
		{Name: "a.a", Version: "2.0.0", SHA256: "updated"},
	}}
	if err := Save(path, updated); err == nil {
		t.Fatalf("expected Save to fail against a read-only directory")
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load after failed Save: %v", err)
	}
	if len(got.Collections) != 1 || got.Collections[0].SHA256 != "original" {
		t.Fatalf("expected original content to survive the failed Save, got %+v", got.Collections)
	}
}

func TestResolveDefaultPath(t *testing.T) {
	t.Parallel()
	if got := ResolveDefaultPath("/proj/requirements.yml", ""); got != "/proj/"+DefaultName {
		t.Fatalf("got %q", got)
	}
	if got := ResolveDefaultPath("/proj/requirements.yml", "/etc/lock.yml"); got != "/etc/lock.yml" {
		t.Fatalf("got %q", got)
	}
	if got := ResolveDefaultPath("", ""); got != DefaultName {
		t.Fatalf("got %q", got)
	}
}

// TestLoadRejectsAnInvalidCollectionName pins that a name outside the Galaxy
// alphabet invalidates the file at load, rather than failing later as a
// network error a CI would retry; one row is the well-formed control.
func TestLoadRejectsAnInvalidCollectionName(t *testing.T) {
	t.Parallel()
	for _, tc := range invalidCollectionNameCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "galaxy.lock")
			body := "schema_version: 1\ncollections:\n  - name: " + tc.entryName + "\n    version: 1.0.0\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write lockfile: %v", err)
			}

			_, err := Load(path)
			if tc.wantValid {
				if err != nil {
					t.Fatalf("Load with a well-formed name: %v", err)
				}
				return
			}
			if !errors.Is(err, helpers.ErrLockfileInvalid) {
				t.Fatalf("Load(%s) error = %v, want errors.Is helpers.ErrLockfileInvalid", tc.entryName, err)
			}
		})
	}
}

// invalidCollectionNameCase is one row of TestLoadRejectsAnInvalidCollectionName.
// entryName is written into the YAML as-is, so a row needing quoting supplies
// its own.
type invalidCollectionNameCase struct {
	name      string
	entryName string
	wantValid bool
}

// invalidCollectionNameCases covers a name that splits into two halves but
// fails the alphabet, one that fails the split itself, and the well-formed
// control on the identical fixture shape.
func invalidCollectionNameCases() []invalidCollectionNameCase {
	return []invalidCollectionNameCase{
		{name: "well formed", entryName: "acme.widgets", wantValid: true},
		{name: "forged line", entryName: `"acme.widgets\nUp to date: nothing"`},
		{name: "path traversal", entryName: `"../../../../etc/passwd"`},
		{name: "uppercase half", entryName: "Acme.widgets"},
		{name: "three parts", entryName: "acme.sub.widgets"},
	}
}

// writeSourceLockfile writes a one-entry lockfile whose otherwise valid
// collection carries source, and returns its path.
func writeSourceLockfile(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "galaxy.lock")
	body := fmt.Sprintf(
		"schema_version: %d\ncollections:\n  - name: acme.widgets\n    version: 1.0.0\n    source: %q\n",
		SchemaVersion, source,
	)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadRejectsSourceWithUserinfo pins that a source embedding a credential
// is refused at load under both sentinels without leaking the credential;
// TestLoadAcceptsSourceWithoutUserinfo is its positive control.
func TestLoadRejectsSourceWithUserinfo(t *testing.T) {
	t.Parallel()
	path := writeSourceLockfile(t, "https://user:hunter2@hub.example.invalid/")

	_, err := Load(path)

	// Asserted ahead of ErrLockfileInvalid so a deleted userinfo check fails
	// here, on the sentinel it owns.
	if !errors.Is(err, helpers.ErrGalaxyServerURLUserinfo) {
		t.Fatalf("Load = %v, want errors.Is helpers.ErrGalaxyServerURLUserinfo", err)
	}
	// Load's contract: every error it returns is IsNotExist or wraps
	// ErrLockfileInvalid.
	if !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("Load = %v, want errors.Is helpers.ErrLockfileInvalid", err)
	}
	// The password is why the source is never printed. Pinnable on its own: an
	// error reaching here already carries both sentinels, so only the
	// formatting decides whether the secret rides along.
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("Load error text leaks the source password: %v", err)
	}
}

// TestLoadAcceptsSourceWithoutUserinfo is the positive control described on
// TestLoadRejectsSourceWithUserinfo: the same fixture with the credential
// removed must load, and must carry the source through unchanged.
func TestLoadAcceptsSourceWithoutUserinfo(t *testing.T) {
	t.Parallel()
	const source = "https://hub.example.invalid/"
	path := writeSourceLockfile(t, source)

	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if got := f.Collections[0].Source; got != source {
		t.Fatalf("Source = %q, want %q", got, source)
	}
}

// TestLoadAcceptsBareServerListIDAsSource pins the exception both userinfo
// checks share: a bare server_list id is not URL-shaped and loads, so a guard
// on anything url.Parse accepts would break every named-server lockfile.
func TestLoadAcceptsBareServerListIDAsSource(t *testing.T) {
	t.Parallel()
	const source = "internal"
	path := writeSourceLockfile(t, source)

	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if got := f.Collections[0].Source; got != source {
		t.Fatalf("Source = %q, want %q", got, source)
	}
}

// canonicalLockfileGolden is the exact byte sequence Save must write for
// canonicalGoldenFile, spelled out rather than derived from the emitter so an
// emitter upgrade that moves a byte fails here.
const canonicalLockfileGolden = `server: https://galaxy.ansible.com
collections:
  - name: ansible.netcommon
    version: 7.2.1
    source: https://galaxy.ansible.com
    deps:
      - a.first
      - m.middle
      - z.last
  - name: ansible.posix
    version: 2.0.0
    source: ""
  - name: community.general
    version: 11.1.0
    source: https://galaxy.ansible.com
    sha256: 3b1f2c4d5e6a7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e
    deps:
      - ansible.netcommon
      - ansible.posix
schema_version: 1
`

// canonicalGoldenFile builds the fixture canonicalLockfileGolden pins: an
// entry with sha256 and deps, one whose source renders as "", unsorted deps,
// and collections out of order, so the golden shows canonicalization.
func canonicalGoldenFile() *File {
	return &File{
		Server:        "https://galaxy.ansible.com",
		SchemaVersion: SchemaVersion,
		Collections: []Entry{
			{
				Name:    "community.general",
				Version: "11.1.0",
				Source:  "https://galaxy.ansible.com",
				SHA256:  "3b1f2c4d5e6a7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e",
				Deps:    []string{"ansible.posix", "ansible.netcommon"},
			},
			{Name: "ansible.posix", Version: "2.0.0"},
			{
				Name:    "ansible.netcommon",
				Version: "7.2.1",
				Source:  "https://galaxy.ansible.com",
				Deps:    []string{"z.last", "a.first", "m.middle"},
			},
		},
	}
}

// TestSaveEmitsCanonicalBytes pins Save's bytes literally, since Hash is their
// SHA256 and `go-galaxy hash` prints it, and pins that Hash digests exactly
// those bytes even though the fixture is out of canonical order.
func TestSaveEmitsCanonicalBytes(t *testing.T) {
	t.Parallel()

	// The literal names the schema version textually, so a bump would
	// otherwise leave it describing a file this package no longer writes.
	if tail := fmt.Sprintf("schema_version: %d\n", SchemaVersion); !strings.HasSuffix(canonicalLockfileGolden, tail) {
		t.Fatalf("golden literal does not end in %q: the schema version moved without it", tail)
	}

	f := canonicalGoldenFile()
	path := filepath.Join(t.TempDir(), DefaultName)
	if err := Save(path, f); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path is built from this test's own t.TempDir
	if err != nil {
		t.Fatalf("reading back %s: %v", path, err)
	}

	if got := string(data); got != canonicalLockfileGolden {
		line, gotLine, wantLine := firstLineDifference(got, canonicalLockfileGolden)
		t.Fatalf("Save wrote non-canonical bytes; first difference at line %d:\n got: %q\nwant: %q", line, gotLine, wantLine)
	}

	sum := sha256.Sum256([]byte(canonicalLockfileGolden))
	want := hex.EncodeToString(sum[:])
	got, err := f.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if got != want {
		t.Fatalf("Hash = %s, want %s: the SHA256 of the bytes Save writes", got, want)
	}
}

// firstLineDifference returns the 1-based number of the first line on which
// got and want differ, with that line from each, since every shape the golden
// catches lands on one line; identical inputs return 0.
func firstLineDifference(got, want string) (int, string, string) {
	gotLines := strings.Split(got, "\n")
	wantLines := strings.Split(want, "\n")
	for i := range max(len(gotLines), len(wantLines)) {
		gotLine, wantLine := lineAt(gotLines, i), lineAt(wantLines, i)
		if gotLine != wantLine {
			return i + 1, gotLine, wantLine
		}
	}
	return 0, "", ""
}

// lineAt returns lines[i], or a marker when i is past the end - the shape a
// file that gained or lost trailing lines produces.
func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "<no such line>"
}
