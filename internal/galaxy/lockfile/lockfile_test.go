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

// TestLoadWrapsUnreadableFileAsInvalid proves a lockfile path that exists but
// cannot be read as a regular file - a directory sitting there - fails
// closed with helpers.ErrLockfileInvalid rather than an unclassified error,
// and is not also IsNotExist: the two are mutually exclusive by construction,
// because Load's fs.ErrNotExist guard runs first and this arm is only
// reached once that guard did not match (Load then wraps the os.ReadFile
// cause with %s, never %w, in that arm - the %s choice matches the sibling
// YAML-unmarshal arm and is not itself what keeps the two exclusive). A
// directory is used rather than chmod 0000: EACCES never fires when tests
// run as root, which is the normal case inside a CI container, so a
// permission-denied fixture would silently pass there for the wrong reason;
// ELOOP (a symlink cycle) is fiddly to construct portably and would prove
// the identical arm anyway.
//
// The positive control removes the directory and writes a valid lockfile at
// the identical path, proving Load can still succeed there - the failure
// above is about what currently occupies the path, not about the path
// itself being permanently unusable.
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

// TestLoadAbsentFileIsNotInvalid states the exclusivity property Load's own
// doc comment promises, from the other side of
// TestLoadWrapsUnreadableFileAsInvalid: a genuinely absent path is
// IsNotExist and never also helpers.ErrLockfileInvalid. This is what pins
// the fs.ErrNotExist guard in Load - without it, every error the function
// returns would satisfy helpers.ErrLockfileInvalid, absence included.
//
// Mutation (deleting the `if errors.Is(err, fs.ErrNotExist) { return nil,
// err }` guard, so every os.ReadFile failure is wrapped) confirmed to fail
// this test with:
//
//	lockfile_test.go:146: expected IsNotExist, got lockfile is invalid: open
//	/.../missing.yml: no such file or directory
//	--- FAIL: TestLoadAbsentFileIsNotInvalid (0.00s)
//
// The same mutation also fails the pre-existing TestLoadMissing, and - one
// level up - collections.TestLockFrozenFailsOnAMissingLockfile, since
// lockFrozen's own `if lockfile.IsNotExist(err)` branch stops seeing a
// missing file as IsNotExist and falls through to the generic
// helpers.ErrLockfileInvalid wrap instead of helpers.ErrLockfileMissing.
// It also perturbs lockDryRunBaseline: a cold-cache `lock --dry-run` (no
// lockfile on disk at all) newly emits "existing lockfile
// .../galaxy.lock cannot be read (lockfile is invalid: ... no
// such file or directory); reporting every collection as added" - a warning
// about a file that was never there in the first place. That perturbation
// is caught, not silent: collections.TestLockDryRunWritesNoLockfileAndReportsAdds
// asserts the absence of exactly that warning, which is the silence half of
// lockDryRunBaseline's documented policy.
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

// TestLoadRejectsNonExactVersion proves Load refuses a lockfile entry whose
// version is a constraint rather than an exact version - "*" here, a shape
// that reads as unpinned to exactVersionFromConstraints, so a --frozen
// install would otherwise resolve it against the server's highest available
// version instead of the pin the operator wrote. TestSaveLoadRoundTrip is
// this test's positive control on the same Load/validate path: it already
// proves an exact version ("11.1.0", "2.0.0") round-trips cleanly, so this
// test only needs to show the constraint shape specifically is what
// validate refuses.
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

// TestLoadRejectsAnInvalidCollectionName pins the read boundary for a
// lockfile entry's name: a name outside the alphabet a Galaxy server itself
// accepts makes the whole file invalid, so nothing downstream ever holds it.
//
// Without this boundary such a name loads successfully and is carried until
// something else trips over it - for a name carrying a newline, that is URL
// construction, which reports a network failure and so invites a CI to retry
// a file no retry could ever repair. The rows here are the two ways a name
// can be wrong and the control that proves the fixture loads at all.
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

// writeSourceLockfile writes a one-entry lockfile whose single collection
// carries source, and returns its path. The entry is otherwise valid - a
// well-formed name and an exact version - so the source is the only thing
// left for validate to object to.
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

// TestLoadRejectsSourceWithUserinfo proves a lockfile entry whose source
// embeds a credential is refused at the read boundary, and that the refusal
// does not itself leak the credential it refuses.
// TestLoadAcceptsSourceWithoutUserinfo is the positive control on the same
// fixture: without it, "it refused" would be indistinguishable from a fixture
// that is invalid for some other reason entirely.
func TestLoadRejectsSourceWithUserinfo(t *testing.T) {
	t.Parallel()
	path := writeSourceLockfile(t, "https://user:hunter2@hub.example.invalid/")

	_, err := Load(path)

	// Killing mutation: deleting the sourceHasUserinfo call from File.validate
	// makes Load accept the file and fails this assertion with `Load = <nil>,
	// want errors.Is helpers.ErrGalaxyServerURLUserinfo`.
	//
	// The specific sentinel is asserted first, ahead of the general one,
	// deliberately: an assertion in a Fatalf chain is only pinned by a
	// mutation that can reach it, and deleting the check makes Load return nil,
	// which fails whichever assertion comes first. Ordered the other way, the
	// general sentinel would absorb that mutation and this one would never run.
	if !errors.Is(err, helpers.ErrGalaxyServerURLUserinfo) {
		t.Fatalf("Load = %v, want errors.Is helpers.ErrGalaxyServerURLUserinfo", err)
	}
	// Pinned by a different mutation from the one above: dropping
	// ErrLockfileInvalid from the wrap leaves the assertion above satisfied and
	// breaks Load's contract that every error it returns is either IsNotExist
	// or ErrLockfileInvalid.
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

// TestLoadAcceptsBareServerListIDAsSource pins the exception both boundaries
// share: a source naming a bare server_list id is not URL-shaped, so
// url.Parse yields no scheme and no host and the userinfo branch is
// unreachable for it. Without this, tightening the guard into "anything
// url.Parse accepts" would break every lockfile written against a named
// server rather than a URL.
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

// canonicalLockfileGolden is the exact byte sequence Save must write for the
// fixture canonicalGoldenFile builds. It is spelled out rather than derived,
// because deriving it from the same emitter that produced it would assert
// nothing: the whole point is that these bytes stay put when the emitter
// underneath them is upgraded or replaced.
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

// canonicalGoldenFile builds the fixture canonicalLockfileGolden pins. It
// covers the three shapes an entry takes on disk - one carrying a sha256 and
// dependencies, one minimal enough that its source renders as the empty
// string, and one whose dependencies arrive unsorted - and it lists the
// collections themselves out of order, so the golden states what
// canonicalization produces rather than what the fixture already was.
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

// TestSaveEmitsCanonicalBytes is the lockfile's golden test. The bytes Save
// writes are a public contract rather than an implementation detail: Hash is
// the SHA256 of exactly those bytes and `go-galaxy hash` prints it, so an
// operator who recorded that digest in a CI gate is broken by any move in the
// emitter's output - a different indent width, a different rendering of the
// empty string, a reordered or requoted scalar. Pinning the bytes literally
// makes such a move fail here instead of in somebody's pipeline.
//
// The second assertion recomputes the expected digest from the same literal
// instead of restating a hex string, so what it pins is the relationship -
// the hash is the hash of the bytes Save writes - rather than a number that
// would have to be recomputed by hand whenever the fixture changed. It is
// separately reachable rather than implied by the first: because the fixture
// is deliberately out of canonical order, a Hash that stopped canonicalizing
// its copy would hash the fixture's own order, leaving Save correct and
// failing only here.
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

	// Mutation (swapping the ansible.netcommon and ansible.posix blocks in
	// canonicalLockfileGolden, so the literal no longer states the order
	// canonicalization produces) confirmed to fail this test with:
	//
	//	Save wrote non-canonical bytes; first difference at line 3:
	//	 got: "  - name: ansible.netcommon"
	//	want: "  - name: ansible.posix"
	//	--- FAIL: TestSaveEmitsCanonicalBytes (0.01s)
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
// got and want differ, together with that line from each. Reporting one line
// rather than dumping both files keeps the failure readable: every shape this
// golden exists to catch - a changed indent, a changed empty-string
// rendering, a requoted scalar - lands on a single line, which a whole-file
// dump would bury. Identical inputs return a zero line, which the sole caller
// never reaches because it compares first.
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
