package tartree

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/rolebuild"
	"github.com/greeddj/go-galaxy/internal/galaxy/treearchive"
)

type fixtureEntry struct {
	name string
	body string
	link string
	mode int64
	dir  bool
}

func file(name, body string) fixtureEntry {
	return fixtureEntry{name: name, body: body, mode: 0o644}
}

func executable(name, body string) fixtureEntry {
	return fixtureEntry{name: name, body: body, mode: 0o755}
}

func dir(name string) fixtureEntry {
	return fixtureEntry{name: name, dir: true, mode: 0o755}
}

func symlink(name, target string) fixtureEntry {
	return fixtureEntry{name: name, link: target, mode: 0o777}
}

func tarHeader(e fixtureEntry) *tar.Header {
	hdr := &tar.Header{Name: e.name, Mode: e.mode, ModTime: time.Unix(1700000000, 0)}
	switch {
	case e.dir:
		hdr.Typeflag = tar.TypeDir
	case e.link != "":
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = e.link
	default:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = int64(len(e.body))
	}
	return hdr
}

func writeTarGz(t *testing.T, entries []fixtureEntry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := tarHeader(e)
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%q): %v", e.name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("Write(%q): %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	path := filepath.Join(t.TempDir(), "role.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

func loadFixture(t *testing.T, entries []fixtureEntry) *Tree {
	t.Helper()
	tree, err := Load(context.Background(), writeTarGz(t, entries), t.TempDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(tree.Cleanup)
	return tree
}

func flatRole() []fixtureEntry {
	return []fixtureEntry{
		dir("meta"),
		file("meta/main.yml", "dependencies: []\n"),
		dir("tasks"),
		file("tasks/main.yml", "- name: hello\n"),
		executable("files/run.sh", "#!/bin/sh\n"),
		symlink("tasks/alias.yml", "main.yml"),
	}
}

func prefixed(prefix string, entries []fixtureEntry) []fixtureEntry {
	out := make([]fixtureEntry, 0, 1+len(entries))
	out = append(out, dir(prefix))
	for _, e := range entries {
		e.name = prefix + "/" + e.name
		out = append(out, e)
	}
	return out
}

// readAll reads rc to the end and closes it, failing the test on either
// error.
func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	content, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return string(content)
}

func TestLoadFlatLayout(t *testing.T) {
	t.Parallel()
	tree := loadFixture(t, flatRole())
	if tree.SkippedPrefix() != "" {
		t.Fatalf("SkippedPrefix() = %q, want empty", tree.SkippedPrefix())
	}
	if !tree.CommitTime().Equal(time.Unix(0, 0)) {
		t.Fatalf("CommitTime() = %v, want epoch", tree.CommitTime())
	}
	entries, err := tree.ReadDir("")
	if err != nil {
		t.Fatalf("ReadDir(root): %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	want := []string{"files", "meta", "tasks"}
	if len(names) != len(want) {
		t.Fatalf("root entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("root entries = %v, want %v (byte order)", names, want)
		}
	}
}

func TestEntryKinds(t *testing.T) {
	t.Parallel()
	tree := loadFixture(t, flatRole())
	tasks, err := tree.ReadDir("tasks")
	if err != nil {
		t.Fatalf("ReadDir(tasks): %v", err)
	}
	kinds := map[string]treearchive.EntryKind{}
	for _, e := range tasks {
		kinds[e.Name] = e.Kind
	}
	if kinds["main.yml"] != treearchive.EntryFile {
		t.Fatalf("main.yml kind = %v, want file", kinds["main.yml"])
	}
	if kinds["alias.yml"] != treearchive.EntrySymlink {
		t.Fatalf("alias.yml kind = %v, want symlink", kinds["alias.yml"])
	}
	files, err := tree.ReadDir("files")
	if err != nil {
		t.Fatalf("ReadDir(files): %v", err)
	}
	if len(files) != 1 || files[0].Kind != treearchive.EntryExecutable {
		t.Fatalf("files entries = %+v, want one executable", files)
	}
}

func TestOpenFileAndSymlink(t *testing.T) {
	t.Parallel()
	tree := loadFixture(t, flatRole())
	blob, err := tree.Open("tasks/main.yml")
	if err != nil {
		t.Fatalf("Open(file): %v", err)
	}
	if content := readAll(t, blob); content != "- name: hello\n" {
		t.Fatalf("file content = %q", content)
	}
	link, err := tree.Open("tasks/alias.yml")
	if err != nil {
		t.Fatalf("Open(symlink): %v", err)
	}
	if target := readAll(t, link); target != "main.yml" {
		t.Fatalf("symlink blob = %q, want target string", target)
	}
}

func TestLoadPrefixedLayout(t *testing.T) {
	t.Parallel()
	tree := loadFixture(t, prefixed("myrole-1.2.3", flatRole()))
	if tree.SkippedPrefix() != "myrole-1.2.3" {
		t.Fatalf("SkippedPrefix() = %q", tree.SkippedPrefix())
	}
	entries, err := tree.ReadDir("")
	if err != nil {
		t.Fatalf("ReadDir(root): %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("root entries = %+v, want the role's three directories", entries)
	}
}

func TestLoadRootMetaWins(t *testing.T) {
	t.Parallel()
	entries := flatRole()
	entries = append(entries, prefixed("vendored", flatRole())...)
	tree := loadFixture(t, entries)
	if tree.SkippedPrefix() != "" {
		t.Fatalf("SkippedPrefix() = %q, want empty: the archive root is the shortest parent", tree.SkippedPrefix())
	}
}

func TestLoadMainYamlSpelling(t *testing.T) {
	t.Parallel()
	tree := loadFixture(t, []fixtureEntry{
		dir("meta"),
		file("meta/main.yaml", "dependencies: []\n"),
	})
	if tree.SkippedPrefix() != "" {
		t.Fatalf("SkippedPrefix() = %q", tree.SkippedPrefix())
	}
}

func TestLoadLayoutRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entries []fixtureEntry
	}{
		{name: "no meta anywhere", entries: []fixtureEntry{dir("tasks"), file("tasks/main.yml", "x")}},
		{
			name:    "two top-level roles",
			entries: append(prefixed("a", flatRole()), prefixed("b", flatRole())...),
		},
		{
			name: "meta is a symlink",
			entries: []fixtureEntry{
				dir("meta"),
				file("real.yml", "dependencies: []\n"),
				symlink("meta/main.yml", "../real.yml"),
			},
		},
		{
			name:    "meta two levels down",
			entries: prefixed("outer", prefixed("inner", flatRole())),
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(context.Background(), writeTarGz(t, tt.entries), t.TempDir)
			if !errors.Is(err, helpers.ErrRoleTarballLayout) {
				t.Fatalf("Load error = %v, want ErrRoleTarballLayout", err)
			}
		})
	}
}

func TestLoadRefusesNonTarGz(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "not.tar.gz")
	if err := os.WriteFile(path, []byte("plain text"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	if _, err := Load(context.Background(), path, t.TempDir); err == nil {
		t.Fatalf("Load accepted a non-archive")
	}
}

func TestLoadCleansUpOnError(t *testing.T) {
	t.Parallel()
	temp := t.TempDir()
	_, err := Load(context.Background(), writeTarGz(t, []fixtureEntry{dir("x")}), func() string { return temp })
	if !errors.Is(err, helpers.ErrRoleTarballLayout) {
		t.Fatalf("Load error = %v", err)
	}
	entries, err := os.ReadDir(temp)
	if err != nil {
		t.Fatalf("ReadDir(temp): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("extraction directory survived a failed Load: %v", entries)
	}
}

func TestControlRuneNameRefusedAtRead(t *testing.T) {
	t.Parallel()
	tree := loadFixture(t, append(flatRole(), file("tasks/bad\x01name", "x")))
	if _, err := tree.ReadDir("tasks"); !errors.Is(err, helpers.ErrRoleTarballEntryInvalid) {
		t.Fatalf("ReadDir error = %v, want ErrRoleTarballEntryInvalid", err)
	}
}

// TestRolebuildRepackIsDeterministic pins the property the url role pipeline
// rests on: one origin tarball, loaded twice, repacks into byte-identical
// artifacts with the dependencies read from its meta.
func TestRolebuildRepackIsDeterministic(t *testing.T) {
	t.Parallel()
	entries := prefixed("myrole-1.0", []fixtureEntry{
		dir("meta"),
		file("meta/main.yml", "dependencies:\n  - other.role\n"),
		dir("tasks"),
		file("tasks/main.yml", "- name: hello\n"),
	})
	path := writeTarGz(t, entries)
	tempFile := func(_ context.Context) (*os.File, func(), error) {
		f, err := os.CreateTemp(t.TempDir(), "repack-*")
		if err != nil {
			return nil, nil, err
		}
		return f, func() { _ = os.Remove(f.Name()) }, nil
	}
	build := func() rolebuild.Built {
		tree, err := Load(context.Background(), path, t.TempDir)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		defer tree.Cleanup()
		built, err := rolebuild.Build(context.Background(), tree, tempFile)
		if err != nil {
			t.Fatalf("rolebuild.Build: %v", err)
		}
		return built
	}
	first := build()
	defer first.Cleanup()
	second := build()
	defer second.Cleanup()
	if first.SHA256 != second.SHA256 {
		t.Fatalf("repack is not deterministic: %s != %s", first.SHA256, second.SHA256)
	}
	if len(first.Meta.Dependencies) != 1 || first.Meta.Dependencies[0].Src != "other.role" {
		t.Fatalf("meta dependencies = %+v", first.Meta.Dependencies)
	}
}
