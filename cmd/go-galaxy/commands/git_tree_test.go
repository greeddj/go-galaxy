package commands

import (
	"path/filepath"
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

// canonicalGitSource runs a fixture URL through the same grammar lockfile.Load
// applies, so a git entry built from it is accepted on the way back in.
func canonicalGitSource(t *testing.T, raw string) string {
	t.Helper()
	u, err := gitsource.ParseURL(raw)
	if err != nil {
		t.Fatalf("ParseURL(%q): %v", raw, err)
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
	req := requirements.CollectionRequirement{Type: requirements.TypeGit, Source: source, Subdir: "collections"}
	locator := gitsource.Locator{URL: source, Subdir: "collections"}.String()
	return []gitRootFQDNsCase{
		{name: "every entry under the subdir", req: req, lf: monorepo, want: []string{"acme.one", "acme.two"}},
		{name: "explicit name narrows to one", req: requirements.CollectionRequirement{
			Type: requirements.TypeGit, Source: source, Subdir: "collections", Namespace: "acme", Name: "two",
		}, lf: monorepo, want: []string{"acme.two"}},
		{name: "unrelated source only", req: req, lf: &lockfile.File{Collections: []lockfile.Entry{
			gitEntry("other.thing", "https://git.example/other/repo.git", "collections/one"),
		}}, want: []string{locator}},
		{name: "nil lockfile", req: req, want: []string{locator}},
		{name: "nil lockfile with an explicit name", req: requirements.CollectionRequirement{
			Type: requirements.TypeGit, Source: source, Namespace: "acme", Name: "one",
		}, want: []string{"acme.one"}},
	}
}

func TestGitRootFQDNs(t *testing.T) {
	t.Parallel()
	for _, tc := range gitRootFQDNsCases(canonicalGitSource(t, gitTestSource)) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := gitRootFQDNs(tc.req, tc.lf)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("gitRootFQDNs() = %q, want %q", got, tc.want)
			}
		})
	}
}

type gitSubdirWithinCase struct {
	entry string
	root  string
	want  bool
}

func gitSubdirWithinCases() []gitSubdirWithinCase {
	return []gitSubdirWithinCase{
		{entry: "collections/one", root: "collections", want: true},
		{entry: "collections", root: "collections", want: true},
		{entry: "collections/a/b", root: "collections", want: false},
		{entry: "", root: "", want: true},
		{entry: "x", root: "", want: true},
		{entry: "", root: "x", want: false},
	}
}

func TestGitSubdirWithin(t *testing.T) {
	t.Parallel()
	for _, tc := range gitSubdirWithinCases() {
		t.Run(tc.entry+" in "+tc.root, func(t *testing.T) {
			t.Parallel()
			if got := gitSubdirWithin(tc.entry, tc.root); got != tc.want {
				t.Fatalf("gitSubdirWithin(%q, %q) = %t, want %t", tc.entry, tc.root, got, tc.want)
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
	source := canonicalGitSource(t, gitTestSource)
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
	source := canonicalGitSource(t, gitTestSource)
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
