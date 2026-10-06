package commands

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
)

const (
	gitTestSource = "https://git.example/org/repo.git"
	gitTestCommit = "0123456789abcdef0123456789abcdef01234567"
)

// canonicalGitSource runs gitTestSource through the same grammar lockfile.Load
// applies, so a git entry built from it is accepted on the way back in.
func canonicalGitSource(t *testing.T) string {
	t.Helper()
	u, err := gitsource.ParseURL(gitTestSource)
	if err != nil {
		t.Fatalf("ParseURL(%q): %v", gitTestSource, err)
	}
	return u.String()
}

func gitEntry(name, source, subdir string) lockfile.Entry {
	return lockfile.Entry{
		Name: name, Version: "1.0.0", Type: lockfile.TypeGit,
		Source: source, Ref: "main", Commit: gitTestCommit, Subdir: subdir,
	}
}

type gitRootFQDNsCase struct {
	lf   *lockfile.File
	name string
	req  requirements.CollectionRequirement
	want []string
}

func gitRootFQDNsCases(source string) []gitRootFQDNsCase {
	monorepo := &lockfile.File{Collections: []lockfile.Entry{
		gitEntry("acme.one", source, "collections/one"),
		gitEntry("acme.two", source, "collections/two"),
		gitEntry("other.thing", "https://git.example/other/repo.git", "collections/three"),
	}}
	req := requirements.CollectionRequirement{Type: requirements.TypeGit, Source: source, Subdir: "collections", Ref: "main"}
	locator := gitsource.Locator{URL: source, Subdir: "collections"}.String()
	return []gitRootFQDNsCase{
		{name: "every entry under the subdir", req: req, lf: monorepo, want: []string{"acme.one", "acme.two"}},
		{name: "explicit name narrows to one", req: requirements.CollectionRequirement{
			Type: requirements.TypeGit, Source: source, Subdir: "collections", Ref: "main", Namespace: "acme", Name: "two",
		}, lf: monorepo, want: []string{"acme.two"}},
		{name: "entries from another ref, charged to it", req: requirements.CollectionRequirement{
			Type: requirements.TypeGit, Source: source, Subdir: "collections", Ref: "dev",
		}, lf: monorepo, want: []string{"acme.one", "acme.two"}},
		{name: "unrelated source only", req: req, lf: &lockfile.File{Collections: []lockfile.Entry{
			gitEntry("other.thing", "https://git.example/other/repo.git", "collections/one"),
		}}, want: []string{locator}},
		{name: "nil lockfile", req: req, want: []string{locator}},
		{name: "nil lockfile with an explicit name", req: requirements.CollectionRequirement{
			Type: requirements.TypeGit, Source: source, Ref: "main", Namespace: "acme", Name: "one",
		}, want: []string{"acme.one"}},
	}
}

func TestGitRootFQDNs(t *testing.T) {
	t.Parallel()
	for _, tc := range gitRootFQDNsCases(canonicalGitSource(t)) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := gitRootFQDNs(gitRootMatches([]requirements.CollectionRequirement{tc.req}, tc.lf)[0])
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("gitRootFQDNs() = %q, want %q", got, tc.want)
			}
		})
	}
}

// saveAndLoad round-trips a lockfile through Save, which derives the schema
// from the entries, and Load, which refuses a git entry that is not canonical.
func saveAndLoad(t *testing.T, lf *lockfile.File) *lockfile.File {
	t.Helper()
	want := lockfile.SchemaVersionFor(lf.Collections, lf.Roles)
	p := filepath.Join(t.TempDir(), "galaxy.lock")
	if err := lockfile.Save(p, lf); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := lockfile.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.SchemaVersion != want {
		t.Fatalf("schema_version = %d, want %d", loaded.SchemaVersion, want)
	}
	return loaded
}

// testDownloadURL is the download_url a hand-written Galaxy lockfile entry
// for name at version needs to load.
func testDownloadURL(name, version string) string {
	return "https://galaxy.example.invalid/download/" + strings.ReplaceAll(name, ".", "-") + "-" + version + ".tar.gz"
}

// TestPrintTreeGitOrigin proves a git entry prints its repository, subdir and
// commit after the version, and a git root the lockfile holds nothing for is
// shown under its locator text as missing.
func TestPrintTreeGitOrigin(t *testing.T) {
	t.Parallel()
	source := canonicalGitSource(t)
	lf := saveAndLoad(t, &lockfile.File{Collections: []lockfile.Entry{
		gitEntry("acme.one", source, "collections/one"),
		{Name: "ansible.utils", Version: "6.0.2", DownloadURL: testDownloadURL("ansible.utils", "6.0.2")},
	}})
	missing := gitsource.Locator{URL: "https://git.example/org/absent.git", Subdir: ""}.String()

	var buf strings.Builder
	printTree(&buf, "requirements.yml", lf, []string{"acme.one", "ansible.utils", missing})
	out := buf.String()
	for _, want := range []string{
		"acme.one 1.0.0 (git " + source + "#collections/one @" + gitTestCommit + ")",
		"ansible.utils 6.0.2\n",
		missing + " (missing in lockfile)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("printTree() output missing %q; got:\n%s", want, out)
		}
	}
}

// TestPrintExplainGitEntry proves the header of a git entry carries its
// type, source, ref, commit and subdir and no sha256 line, since a git entry
// pins a commit rather than an artifact digest.
func TestPrintExplainGitEntry(t *testing.T) {
	t.Parallel()
	source := canonicalGitSource(t)
	lf := saveAndLoad(t, &lockfile.File{Collections: []lockfile.Entry{gitEntry("acme.one", source, "collections/one")}})

	var buf strings.Builder
	if err := printExplain(&buf, lf, "acme.one", "requirements.yml", map[string]bool{"acme.one": true}, nil); err != nil {
		t.Fatalf("printExplain() error = %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"acme.one 1.0.0\n",
		"  type         : git\n",
		"  source       : " + source + "\n",
		"  ref          : main\n",
		"  commit       : " + gitTestCommit + "\n",
		"  subdir       : collections/one\n",
		"requirements.yml (root)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("printExplain() output missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sha256") || strings.Contains(out, "download_url") {
		t.Fatalf("printExplain() printed a sha256 or download_url line for a git entry; got:\n%s", out)
	}
}

// TestTreeListsEachSharedGitEntryOnce pins tree over a root at collections
// from main beside one at collections/three from dev, both holding that
// directory: each locked entry is one root and prints once.
func TestTreeListsEachSharedGitEntryOnce(t *testing.T) {
	t.Parallel()
	source := canonicalGitSource(t)
	three := gitEntry("acme.three", source, "collections/three")
	three.Ref = "dev"
	lf := saveAndLoad(t, &lockfile.File{Collections: []lockfile.Entry{
		gitEntry("acme.one", source, "collections/one"), gitEntry("acme.trois", source, "collections/three"), three,
	}})
	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	body := "collections:\n" +
		"  - name: " + gitTestSource + "#collections\n    type: git\n    version: main\n" +
		"  - name: " + gitTestSource + "#collections/three\n    type: git\n    version: dev\n"
	if err := os.WriteFile(reqPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	roots, _, err := loadRootFQDNs(reqPath, lf)
	if err != nil {
		t.Fatalf("loadRootFQDNs: %v", err)
	}
	if got := strings.Join(slices.Sorted(slices.Values(roots)), ","); got != "acme.one,acme.three,acme.trois" {
		t.Fatalf("roots = %s, want acme.one,acme.three,acme.trois", got)
	}
	var buf strings.Builder
	printTree(&buf, reqPath, lf, roots)
	for _, row := range []string{"acme.one 1.0.0 (git", "acme.three 1.0.0 (git", "acme.trois 1.0.0 (git"} {
		if n := strings.Count(buf.String(), row); n != 1 {
			t.Fatalf("printTree() printed %q %d times, want once; got:\n%s", row, n, buf.String())
		}
	}
}

// TestGitRootFQDNsListsAnEntryUnderOneRequirement pins which of two requirements
// sharing collections/three lists each entry: the nearest asking for its ref,
// else the nearest it could come from, so no entry is listed twice.
func TestGitRootFQDNsListsAnEntryUnderOneRequirement(t *testing.T) {
	t.Parallel()
	source := canonicalGitSource(t)
	at := func(fqdn, subdir, ref string) lockfile.Entry {
		e := gitEntry(fqdn, source, subdir)
		e.Ref = ref
		return e
	}
	lf := &lockfile.File{Collections: []lockfile.Entry{
		at("acme.one", "collections/one", "main"), at("acme.trois", "collections/three", "main"),
		at("acme.three", "collections/three", "dev"), at("acme.four", "collections/three", "feature"),
	}}
	reqs := []requirements.CollectionRequirement{
		{Type: requirements.TypeGit, Source: source, Subdir: "collections", Ref: "main"},
		{Type: requirements.TypeGit, Source: source, Subdir: "collections/three", Ref: "dev"},
	}
	matches := gitRootMatches(reqs, lf)
	for i, want := range []string{"acme.one,acme.trois", "acme.three,acme.four"} {
		if got := strings.Join(gitRootFQDNs(matches[i]), ","); got != want {
			t.Fatalf("requirement %d lists %s, want %s", i, got, want)
		}
	}
}

// TestTreeListsANamedRequirementsCollectionOnce pins tree over a named root at
// collections beside one at collections/three, both from main, where the child
// owns the named collection's entry: tree prints that collection once.
func TestTreeListsANamedRequirementsCollectionOnce(t *testing.T) {
	t.Parallel()
	source := canonicalGitSource(t)
	lf := saveAndLoad(t, &lockfile.File{Collections: []lockfile.Entry{
		gitEntry("acme.three", source, "collections/three"), gitEntry("acme.trois", source, "collections/three"),
	}})
	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	body := "collections:\n" +
		"  - name: acme.three\n    source: " + gitTestSource + "#collections\n    type: git\n    version: main\n" +
		"  - name: " + gitTestSource + "#collections/three\n    type: git\n    version: main\n"
	if err := os.WriteFile(reqPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	roots, _, err := loadRootFQDNs(reqPath, lf)
	if err != nil {
		t.Fatalf("loadRootFQDNs: %v", err)
	}
	if got := strings.Join(slices.Sorted(slices.Values(roots)), ","); got != "acme.three,acme.trois" {
		t.Fatalf("roots = %s, want acme.three,acme.trois", got)
	}
	var buf strings.Builder
	printTree(&buf, reqPath, lf, roots)
	if n := strings.Count(buf.String(), "acme.three 1.0.0 (git"); n != 1 {
		t.Fatalf("printTree() printed acme.three %d times, want once; got:\n%s", n, buf.String())
	}
}
