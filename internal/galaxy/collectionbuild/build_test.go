package collectionbuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/manifest"
	"github.com/greeddj/go-galaxy/internal/testing/faketree"
)

// archiveEntry is one tar entry read back from a built artifact.
type archiveEntry struct {
	header *tar.Header
	data   []byte
}

// readArtifact decompresses and lists every entry of the artifact at p.
func readArtifact(t *testing.T, p string) []archiveEntry {
	t.Helper()
	//nolint:gosec // p is an artifact this test just built under its own temp directory.
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() {
		t.Fatalf("gzip header carries name %q and mtime %v, want both zero", gz.Name, gz.ModTime)
	}
	tr := tar.NewReader(gz)
	var entries []archiveEntry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar body: %v", err)
		}
		entries = append(entries, archiveEntry{header: hdr, data: data})
	}
}

func entryNames(entries []archiveEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.header.Name)
	}
	return names
}

func findEntry(t *testing.T, entries []archiveEntry, name string) archiveEntry {
	t.Helper()
	for _, e := range entries {
		if e.header.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %q in %q", name, entryNames(entries))
	return archiveEntry{}
}

// buildFrom discovers the one candidate under subdir and builds it.
func buildFrom(t *testing.T, src Source, subdir string) Built {
	t.Helper()
	cand := candidateFor(t, src, subdir)
	built, err := Build(context.Background(), src, cand, tempFileIn(t.TempDir()))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(built.Cleanup)
	return built
}

// fixtureSource is the tree the structural tests build: a mix of kept,
// ignored, executable and linked entries.
func fixtureSource() *faketree.Tree {
	return faketree.New().
		File("galaxy.yml", minimalGalaxyYML+"build_ignore: ['*.bak']\nlicense_file: ''\ntags: [web]\n"+
			"dependencies: {acme.base: '>=1.0.0'}\n").
		File("README.md", "# app\n").
		File("notes.bak", "ignored").
		File("acme-app-1.0.0.tar.gz", "ignored").
		File("plugins/modules/run.py", "print()\n").
		Exec("plugins/modules/tool.sh", "#!/bin/sh\n").
		File("plugins/modules/__pycache__/run.pyc", "ignored").
		File("plugins/modules/run.pyc", "ignored").
		File("tests/output/junit.xml", "ignored").
		File("tests/unit/test_x.py", "assert True\n").
		Dir("empty").
		Symlink("docs/readme-link.md", "../README.md").
		Symlink("modules-link", "plugins/modules")
}

const (
	readmeContent  = "# app\n"
	baseConstraint = ">=1.0.0"
)

func TestBuildStructure(t *testing.T) {
	t.Parallel()
	built := buildFrom(t, fixtureSource(), "")
	if built.Namespace != "acme" || built.Name != "app" || built.Version != "1.2.3" || built.Subdir != "" {
		t.Fatalf("identity = %+v", built)
	}
	if built.Dependencies["acme.base"] != baseConstraint {
		t.Fatalf("dependencies = %v", built.Dependencies)
	}
	if len(built.Warnings) != 0 {
		t.Fatalf("warnings = %q", built.Warnings)
	}
	entries := readArtifact(t, built.ArtifactPath)
	wantNames := []string{
		"MANIFEST.json", "FILES.json", "README.md", "docs", "docs/readme-link.md", "empty", "modules-link",
		"plugins", "plugins/modules", "plugins/modules/run.py", "plugins/modules/tool.sh", "tests", "tests/unit", "tests/unit/test_x.py",
	}
	if got := entryNames(entries); strings.Join(got, "|") != strings.Join(wantNames, "|") {
		t.Fatalf("entries = %q, want %q", got, wantNames)
	}
	checkHeaders(t, entries)
	checkModes(t, entries)
	checkContents(t, entries)
	checkFilesRows(t, entries)
	checkManifest(t, entries)
}

// checkContents asserts the link targets and one file body.
func checkContents(t *testing.T, entries []archiveEntry) {
	t.Helper()
	if link := findEntry(t, entries, "docs/readme-link.md").header.Linkname; link != "../README.md" {
		t.Errorf("readme-link target %q", link)
	}
	if link := findEntry(t, entries, "modules-link").header.Linkname; link != "plugins/modules" {
		t.Errorf("modules-link target %q", link)
	}
	if string(findEntry(t, entries, "README.md").data) != readmeContent {
		t.Errorf("README content mismatch")
	}
}

// checkHeaders asserts the fields every entry shares: no owner, the commit
// time, no other times, and the USTAR format.
func checkHeaders(t *testing.T, entries []archiveEntry) {
	t.Helper()
	for _, e := range entries {
		h := e.header
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s: owner %d:%d %q:%q, want 0:0 with empty names", h.Name, h.Uid, h.Gid, h.Uname, h.Gname)
		}
		if !h.ModTime.Equal(faketree.FixedCommitTime()) {
			t.Errorf("%s: mtime %v, want %v", h.Name, h.ModTime, faketree.FixedCommitTime())
		}
		if !h.AccessTime.IsZero() || !h.ChangeTime.IsZero() {
			t.Errorf("%s: atime/ctime set", h.Name)
		}
		if h.Format != tar.FormatUSTAR {
			t.Errorf("%s: format %v, want USTAR", h.Name, h.Format)
		}
	}
}

// checkModes asserts the typeflag and mode of one entry of each kind.
func checkModes(t *testing.T, entries []archiveEntry) {
	t.Helper()
	for _, want := range []struct {
		name     string
		typeflag byte
		mode     int64
	}{
		{"MANIFEST.json", tar.TypeReg, 0o644},
		{"FILES.json", tar.TypeReg, 0o644},
		{"README.md", tar.TypeReg, 0o644},
		{"plugins/modules/tool.sh", tar.TypeReg, 0o755},
		{"plugins", tar.TypeDir, 0o755},
		{"empty", tar.TypeDir, 0o755},
		{"docs/readme-link.md", tar.TypeSymlink, 0o777},
		{"modules-link", tar.TypeSymlink, 0o777},
	} {
		h := findEntry(t, entries, want.name).header
		if h.Typeflag != want.typeflag || h.Mode != want.mode {
			t.Errorf("%s: typeflag %q mode %o, want %q %o", want.name, h.Typeflag, h.Mode, want.typeflag, want.mode)
		}
	}
}

// listedRow is one FILES.json row as a test reads it back.
type listedRow struct {
	ChksumType   *string `json:"chksum_type"`
	ChksumSha256 *string `json:"chksum_sha256"`
	Name         string  `json:"name"`
	Ftype        string  `json:"ftype"`
	Format       int     `json:"format"`
}

// readRows parses FILES.json out of entries and indexes its rows by name.
func readRows(t *testing.T, entries []archiveEntry) ([]listedRow, map[string]listedRow) {
	t.Helper()
	var files struct {
		Files  []listedRow `json:"files"`
		Format int         `json:"format"`
	}
	if err := json.Unmarshal(findEntry(t, entries, "FILES.json").data, &files); err != nil {
		t.Fatalf("FILES.json: %v", err)
	}
	if files.Format != 1 {
		t.Fatalf("FILES.json format = %d", files.Format)
	}
	byName := make(map[string]listedRow, len(files.Files))
	for _, row := range files.Files {
		byName[row.Name] = row
	}
	return files.Files, byName
}

// checkFilesRows asserts the FILES.json row shapes of the fixture build.
func checkFilesRows(t *testing.T, entries []archiveEntry) {
	t.Helper()
	rows, byName := readRows(t, entries)
	if len(rows) == 0 || rows[0].Name != "." || rows[0].Ftype != ftypeDir {
		t.Fatalf("FILES.json head = %+v", rows)
	}
	for i := range rows {
		checkRowShape(t, &rows[i])
	}
	if len(rows) != len(entries)-1 {
		t.Errorf("%d rows for %d archive entries, want rows = entries - 1", len(rows), len(entries))
	}
	checkLinkedRows(t, byName)
}

// checkLinkedRows asserts the rows the fixture's links and empty directory
// produce.
func checkLinkedRows(t *testing.T, byName map[string]listedRow) {
	t.Helper()
	readmeSum := sha256Hex([]byte(readmeContent))
	if *byName["README.md"].ChksumSha256 != readmeSum || *byName["docs/readme-link.md"].ChksumSha256 != readmeSum {
		t.Errorf("README digest not carried by the file and its link")
	}
	if byName["modules-link"].Ftype != ftypeDir || byName["empty"].Ftype != ftypeDir {
		t.Errorf("dir link or empty dir not listed as dir")
	}
	if _, listed := byName["modules-link/run.py"]; listed {
		t.Errorf("a dir symlink was descended")
	}
}

func wellFormedDigest(row *listedRow) bool {
	return row.ChksumType != nil && *row.ChksumType == "sha256" && row.ChksumSha256 != nil && len(*row.ChksumSha256) == 64
}

// checkRowShape asserts one row carries the checksum fields its ftype
// warrants and format 1.
func checkRowShape(t *testing.T, row *listedRow) {
	t.Helper()
	if row.Format != 1 {
		t.Errorf("row %s format %d", row.Name, row.Format)
	}
	switch row.Ftype {
	case ftypeDir:
		if row.ChksumType != nil || row.ChksumSha256 != nil {
			t.Errorf("dir row %s carries a checksum", row.Name)
		}
	case ftypeFile:
		if !wellFormedDigest(row) {
			t.Errorf("file row %s checksum malformed", row.Name)
		}
	default:
		t.Errorf("row %s ftype %q", row.Name, row.Ftype)
	}
}

// checkManifest asserts the MANIFEST.json shape of the fixture build.
func checkManifest(t *testing.T, entries []archiveEntry) {
	t.Helper()
	var doc map[string]json.RawMessage
	manifestJSON := findEntry(t, entries, "MANIFEST.json").data
	if err := json.Unmarshal(manifestJSON, &doc); err != nil {
		t.Fatalf("MANIFEST.json: %v", err)
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(doc["collection_info"], &info); err != nil {
		t.Fatalf("collection_info: %v", err)
	}
	for key, want := range map[string]string{
		"namespace": `"acme"`, "name": `"app"`, "version": `"1.2.3"`, "readme": `"README.md"`, "authors": `["A. Author"]`,
		"tags": `["web"]`, "description": `""`, "license": `[]`, "license_file": `null`,
		"dependencies": `{"acme.base":"` + baseConstraint + `"}`, "repository": `""`, "documentation": `""`, "homepage": `""`, "issues": `""`,
	} {
		if got := compactJSON(t, info[key]); got != want {
			t.Errorf("collection_info.%s = %s, want %s", key, got, want)
		}
	}
	if got := compactJSON(t, doc["format"]); got != "1" {
		t.Errorf("format = %s", got)
	}
	filesJSON := findEntry(t, entries, "FILES.json").data
	wantPointer := `{"name":"FILES.json","ftype":"file","chksum_type":"sha256","chksum_sha256":"` + sha256Hex(filesJSON) + `","format":1}`
	if got := compactJSON(t, doc["file_manifest_file"]); got != wantPointer {
		t.Errorf("file_manifest_file = %s, want %s", got, wantPointer)
	}
	if !bytes.HasPrefix(manifestJSON, []byte("{\n \"")) {
		t.Errorf("MANIFEST.json is not indented by one space: %q", manifestJSON[:10])
	}
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if raw == nil {
		return "<absent>"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact: %v", err)
	}
	return buf.String()
}

func TestBuildIsDeterministicAndVerifies(t *testing.T) {
	t.Parallel()
	first := buildFrom(t, fixtureSource(), "")
	second := buildFrom(t, fixtureSource(), "")
	a, err := os.ReadFile(first.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(second.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("two builds of one tree differ")
	}
	sum := sha256.Sum256(a)
	if first.SHA256 != hex.EncodeToString(sum[:]) || first.SHA256 != second.SHA256 {
		t.Fatalf("SHA256 %s does not match the file's %x", first.SHA256, sum)
	}

	checkVerifiesAndExtracts(t, first.ArtifactPath)
}

// checkVerifiesAndExtracts runs the artifact through the two readers the
// pipeline applies and reads through the extracted links.
func checkVerifiesAndExtracts(t *testing.T, artifactPath string) {
	t.Helper()
	ctx := context.Background()
	manifestJSON, err := manifest.ReadFromTarGz(ctx, artifactPath)
	if err != nil {
		t.Fatalf("ReadFromTarGz: %v", err)
	}
	if err := manifest.VerifyChain(ctx, artifactPath, manifestJSON); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	dst := t.TempDir()
	if err := archive.ExtractTarGz(ctx, artifactPath, dst); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	if got := readExtracted(t, dst, "docs", "readme-link.md"); got != readmeContent {
		t.Fatalf("reading through the extracted link: %q", got)
	}
	if target, err := os.Readlink(filepath.Join(dst, "modules-link")); err != nil || target != "plugins/modules" {
		t.Fatalf("modules-link = %q, %v", target, err)
	}
	if got := readExtracted(t, dst, "modules-link", "run.py"); got != "print()\n" {
		t.Fatalf("reading through the extracted dir link: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "notes.bak")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ignored file was extracted: %v", err)
	}
}

func readExtracted(t *testing.T, dst string, parts ...string) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(append([]string{dst}, parts...)...))
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	return string(got)
}

func TestBuildFromSubdir(t *testing.T) {
	t.Parallel()
	src := faketree.New().
		File("README.md", "top level, not part of the collection").
		File("collections/app/galaxy.yml", minimalGalaxyYML).
		File("collections/app/README.md", "inner").
		Symlink("collections/app/top", "../../README.md").
		Symlink("collections/app/self", "README.md")
	built := buildFrom(t, src, "collections/app")
	if built.Subdir != "collections/app" {
		t.Fatalf("Subdir = %q", built.Subdir)
	}
	want := []string{"MANIFEST.json", "FILES.json", "README.md", "self"}
	if got := entryNames(readArtifact(t, built.ArtifactPath)); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries = %q, want %q", got, want)
	}
	wantWarning := "skipping symlink collections/app/top: target outside the collection"
	if len(built.Warnings) != 1 || !strings.Contains(built.Warnings[0], wantWarning) {
		t.Fatalf("warnings = %q", built.Warnings)
	}
}

func TestBuildFromManifestOnlyTree(t *testing.T) {
	t.Parallel()
	src := faketree.New().
		File("MANIFEST.json", manifestOnlyJSON).
		File("FILES.json", "stale").
		File("README.md", "r").
		File("plugins/x.py", "x")
	built := buildFrom(t, src, "")
	if built.Namespace != "acme" || built.Name != "built" || built.Version != "3.0.0" {
		t.Fatalf("identity = %+v", built)
	}
	entries := readArtifact(t, built.ArtifactPath)
	want := []string{"MANIFEST.json", "FILES.json", "README.md", "plugins", "plugins/x.py"}
	if got := entryNames(entries); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries = %q, want %q", got, want)
	}
	if string(findEntry(t, entries, "FILES.json").data) == "stale" {
		t.Fatal("the shipped FILES.json was copied instead of regenerated")
	}
}

type symlinkCase struct {
	wantErr      error
	source       func() *faketree.Tree
	name         string
	wantWarning  string
	wantLinkname string
	wantFtype    string
	wantEntries  []string
}

// symlinkBase is the tree every symlink case adds one or two links to.
func symlinkBase() *faketree.Tree {
	return faketree.New().File("galaxy.yml", minimalGalaxyYML).File("README.md", "r").File("d/f", "content")
}

func symlinkCases() []symlinkCase {
	return slices.Concat(symlinkWrittenCases(), symlinkSkippedCases(), symlinkRefusedCases())
}

// symlinkWrittenCases are links that land in the artifact.
func symlinkWrittenCases() []symlinkCase {
	base := symlinkBase
	return []symlinkCase{
		{
			name:         "relative in-tree file",
			source:       func() *faketree.Tree { return base().Symlink("a/l", "../d/f") },
			wantEntries:  []string{"README.md", "a", "a/l", "d", "d/f"},
			wantLinkname: "../d/f", wantFtype: "file",
		},
		{
			name:         "dir symlink not descended",
			source:       func() *faketree.Tree { return base().Symlink("ld", "d") },
			wantEntries:  []string{"README.md", "d", "d/f", "ld"},
			wantLinkname: "d", wantFtype: "dir",
		},
		{
			name:         "chain flattened to the final file",
			source:       func() *faketree.Tree { return base().Symlink("l1", "l2").Symlink("l2", "d/l3").Symlink("d/l3", "f") },
			wantEntries:  []string{"README.md", "d", "d/f", "d/l3", "l1", "l2"},
			wantLinkname: "d/f", wantFtype: "file",
		},
		{
			name:         "through a directory symlink",
			source:       func() *faketree.Tree { return base().Symlink("dl", "d").Symlink("x/l", "../dl/f") },
			wantEntries:  []string{"README.md", "d", "d/f", "dl", "x", "x/l"},
			wantLinkname: "../d/f", wantFtype: "file",
		},
		{
			name:         "dir symlink to the parent directory",
			source:       func() *faketree.Tree { return base().Symlink("d/sub/up", "..") },
			wantEntries:  []string{"README.md", "d", "d/f", "d/sub", "d/sub/up"},
			wantLinkname: "..", wantFtype: "dir",
		},
		{
			name: "chain of eight",
			source: func() *faketree.Tree {
				s := base()
				for i := range 8 {
					s.Symlink("l"+string(rune('0'+i)), "l"+string(rune('1'+i)))
				}
				return s.File("l8", "end")
			},
			wantEntries:  []string{"README.md", "d", "d/f", "l0", "l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8"},
			wantLinkname: "l8", wantFtype: "file",
		},
		{
			name:        "link named by a pattern is ignored silently",
			source:      func() *faketree.Tree { return base().Symlink("x.pyc", "README.md") },
			wantEntries: []string{"README.md", "d", "d/f"},
		},
		{
			name:        "dir link with a pruned basename is ignored silently",
			source:      func() *faketree.Tree { return base().Symlink("a/__pycache__", "../d") },
			wantEntries: []string{"README.md", "a", "d", "d/f"},
		},
	}
}

// symlinkSkippedCases are links left out with a warning.
func symlinkSkippedCases() []symlinkCase {
	base := symlinkBase
	return []symlinkCase{
		{
			name:        "dir symlink to an ancestor",
			source:      func() *faketree.Tree { return base().Symlink("d/sub/up", "../..") },
			wantEntries: []string{"README.md", "d", "d/f", "d/sub"},
			wantWarning: "skipping symlink d/sub/up: target outside the collection",
		},
		{
			name:        "outside the collection",
			source:      func() *faketree.Tree { return base().Symlink("l", "../etc/passwd") },
			wantEntries: []string{"README.md", "d", "d/f"},
			wantWarning: "skipping symlink l: target outside the collection",
		},
		{
			name:        "absolute",
			source:      func() *faketree.Tree { return base().Symlink("l", "/etc/passwd") },
			wantEntries: []string{"README.md", "d", "d/f"},
			wantWarning: "skipping symlink l: target outside the collection",
		},
		{
			name:        "chain leaving the collection",
			source:      func() *faketree.Tree { return base().Symlink("l", "l2").Symlink("l2", "../out") },
			wantEntries: []string{"README.md", "d", "d/f"},
			wantWarning: "skipping symlink l: target outside the collection",
		},
		{
			name:        "target excluded by the ignore rules",
			source:      func() *faketree.Tree { return base().Symlink("l", "galaxy.yml") },
			wantEntries: []string{"README.md", "d", "d/f"},
			wantWarning: "skipping symlink l: target is excluded from the build",
		},
		{
			name:        "target under an excluded directory",
			source:      func() *faketree.Tree { return base().File("tests/output/x", "x").Symlink("l", "tests/output/x") },
			wantEntries: []string{"README.md", "d", "d/f", "tests"},
			wantWarning: "target is excluded from the build",
		},
		{
			name:        "own directory",
			source:      func() *faketree.Tree { return base().Symlink("d/l", ".") },
			wantEntries: []string{"README.md", "d", "d/f"},
			wantWarning: "skipping symlink d/l: target resolves to its own directory",
		},
	}
}

// symlinkRefusedCases are links that fail the build.
func symlinkRefusedCases() []symlinkCase {
	base := symlinkBase
	return []symlinkCase{
		{
			name:    "dangling",
			source:  func() *faketree.Tree { return base().Symlink("l", "missing") },
			wantErr: helpers.ErrGitSymlinkUnresolvable,
		},
		{
			name:    "through a file",
			source:  func() *faketree.Tree { return base().Symlink("l", "README.md/x") },
			wantErr: helpers.ErrGitSymlinkUnresolvable,
		},
		{
			name:    "loop",
			source:  func() *faketree.Tree { return base().Symlink("a", "b").Symlink("b", "a") },
			wantErr: helpers.ErrGitSymlinkUnresolvable,
		},
		{
			name:    "self loop",
			source:  func() *faketree.Tree { return base().Symlink("a", "a") },
			wantErr: helpers.ErrGitSymlinkUnresolvable,
		},
		{
			name: "chain of nine",
			source: func() *faketree.Tree {
				s := base()
				for i := range 9 {
					s.Symlink("l"+string(rune('0'+i)), "l"+string(rune('1'+i)))
				}
				return s.File("l9", "end")
			},
			wantErr: helpers.ErrGitSymlinkUnresolvable,
		},
		{
			name:    "empty target",
			source:  func() *faketree.Tree { return base().Symlink("l", "") },
			wantErr: helpers.ErrSymlinkTargetIsEmpty,
		},
		{
			name:    "NUL in target",
			source:  func() *faketree.Tree { return base().Symlink("l", "a\x00b") },
			wantErr: helpers.ErrSymlinkTarget,
		},
		{
			name:        "target to a submodule",
			source:      func() *faketree.Tree { return base().Submodule("sub").Symlink("l", "sub") },
			wantEntries: []string{"README.md", "d", "d/f"},
			wantWarning: "target is a submodule",
		},
	}
}

func TestBuildSymlinkPolicy(t *testing.T) {
	t.Parallel()
	for _, tt := range symlinkCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := tt.source()
			cand := candidateFor(t, src, "")
			built, err := Build(context.Background(), src, cand, tempFileIn(t.TempDir()))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Build error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			t.Cleanup(built.Cleanup)
			checkSymlinkOutcome(t, &tt, built)
		})
	}
}

// checkSymlinkOutcome asserts one accepted symlink case: the entry list,
// the warnings, the link row, and that the extractor takes the result.
func checkSymlinkOutcome(t *testing.T, tt *symlinkCase, built Built) {
	t.Helper()
	entries := readArtifact(t, built.ArtifactPath)
	want := append([]string{"MANIFEST.json", "FILES.json"}, tt.wantEntries...)
	if got := entryNames(entries); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries = %q, want %q", got, want)
	}
	if tt.wantWarning == "" && len(built.Warnings) != 0 {
		t.Fatalf("warnings = %q, want none", built.Warnings)
	}
	if tt.wantWarning != "" && !slices.ContainsFunc(built.Warnings, func(w string) bool { return strings.Contains(w, tt.wantWarning) }) {
		t.Fatalf("warnings = %q, want one containing %q", built.Warnings, tt.wantWarning)
	}
	if tt.wantLinkname != "" {
		checkLinkRow(t, entries, tt.wantLinkname, tt.wantFtype)
	}
	if err := archive.ExtractTarGz(context.Background(), built.ArtifactPath, t.TempDir()); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
}

// checkLinkRow finds the one symlink entry whose Linkname is linkname and
// checks its FILES.json row has ftype.
func checkLinkRow(t *testing.T, entries []archiveEntry, linkname, ftype string) {
	t.Helper()
	var name string
	for _, e := range entries {
		if e.header.Typeflag == tar.TypeSymlink && e.header.Linkname == linkname {
			name = e.header.Name
		}
	}
	if name == "" {
		t.Fatalf("no symlink entry with target %q", linkname)
	}
	_, byName := readRows(t, entries)
	row, ok := byName[name]
	if !ok {
		t.Fatalf("no row for %s", name)
	}
	if row.Ftype != ftype {
		t.Fatalf("row %s ftype %q, want %q", name, row.Ftype, ftype)
	}
}

func TestBuildSkipsSubmoduleWithWarning(t *testing.T) {
	t.Parallel()
	src := faketree.New().File("galaxy.yml", minimalGalaxyYML).Submodule("vendor/lib").Submodule("tests/output")
	built := buildFrom(t, src, "")
	want := []string{"MANIFEST.json", "FILES.json", "tests", "vendor"}
	if got := entryNames(readArtifact(t, built.ArtifactPath)); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries = %q, want %q", got, want)
	}
	if len(built.Warnings) != 1 || built.Warnings[0] != "skipping submodule vendor/lib: submodules are never fetched" {
		t.Fatalf("warnings = %q", built.Warnings)
	}
}

func TestBuildBudgets(t *testing.T) {
	t.Parallel()
	t.Run("entry count", func(t *testing.T) {
		t.Parallel()
		src := faketree.New().File("galaxy.yml", minimalGalaxyYML)
		for i := range helpers.ArchiveMaxEntryCount {
			src.File("f"+strconv.Itoa(int(i)), "x")
		}
		_, err := Build(context.Background(), src, candidateFor(t, src, ""), tempFileIn(t.TempDir()))
		if !errors.Is(err, helpers.ErrArchiveTooManyEntries) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("entry size declared", func(t *testing.T) {
		t.Parallel()
		src := faketree.New().File("galaxy.yml", minimalGalaxyYML).File("big", "small").DeclareSize("big", helpers.ArchiveMaxEntrySize+1)
		_, err := Build(context.Background(), src, candidateFor(t, src, ""), tempFileIn(t.TempDir()))
		if !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestBuildShapeBudgets(t *testing.T) {
	t.Parallel()
	t.Run("name length", func(t *testing.T) {
		t.Parallel()
		src := faketree.New().File("galaxy.yml", minimalGalaxyYML).File(strings.Repeat("n", helpers.ArchiveMaxEntryNameLen+1), "x")
		_, err := Build(context.Background(), src, candidateFor(t, src, ""), tempFileIn(t.TempDir()))
		if !errors.Is(err, helpers.ErrArchiveEntryNameTooLong) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("link target length", func(t *testing.T) {
		t.Parallel()
		src := faketree.New().File("galaxy.yml", minimalGalaxyYML).Symlink("l", strings.Repeat("t", helpers.ArchiveMaxEntryNameLen+1))
		_, err := Build(context.Background(), src, candidateFor(t, src, ""), tempFileIn(t.TempDir()))
		if !errors.Is(err, helpers.ErrArchiveEntryNameTooLong) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("depth", func(t *testing.T) {
		t.Parallel()
		deep := strings.TrimSuffix(strings.Repeat("d/", helpers.GitTreeMaxDepth), "/") + "/f"
		src := faketree.New().File("galaxy.yml", minimalGalaxyYML).File(deep, "x")
		_, err := Build(context.Background(), src, candidateFor(t, src, ""), tempFileIn(t.TempDir()))
		if !errors.Is(err, helpers.ErrGitTreeTooDeep) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("blob shorter than declared", func(t *testing.T) {
		t.Parallel()
		src := faketree.New().File("galaxy.yml", minimalGalaxyYML).File("f", "abc").DeclareSize("f", 10)
		_, err := Build(context.Background(), src, candidateFor(t, src, ""), tempFileIn(t.TempDir()))
		if !errors.Is(err, helpers.ErrGitCommitMismatch) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestBuildErrorsLeaveNoTempFile(t *testing.T) {
	t.Parallel()
	t.Run("cancel during the write", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		dir := t.TempDir()
		inner := tempFileIn(dir)
		tempFile := func(ctx context.Context) (*os.File, func(), error) {
			f, cleanup, err := inner(ctx)
			cancel()
			return f, cleanup, err
		}
		src := faketree.New().File("galaxy.yml", minimalGalaxyYML).File("README.md", "r")
		_, err := Build(ctx, src, candidateFor(t, src, ""), tempFile)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		assertEmptyDir(t, dir)
	})
	t.Run("cancel during the walk", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		src := faketree.New().File("galaxy.yml", minimalGalaxyYML).File("README.md", "r").File("b/c", "x")
		src.SetOnOpen(func(string) { cancel() })
		dir := t.TempDir()
		_, err := Build(ctx, src, candidateFor(t, src, ""), tempFileIn(dir))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		assertEmptyDir(t, dir)
	})
	t.Run("blob grows between passes", func(t *testing.T) {
		t.Parallel()
		src := faketree.New().File("galaxy.yml", minimalGalaxyYML).File("f", "abc")
		opens := 0
		src.SetOnOpen(func(p string) {
			if p == "f" {
				opens++
				if opens == 2 {
					src.File("f", "abcdef")
				}
			}
		})
		dir := t.TempDir()
		_, err := Build(context.Background(), src, candidateFor(t, src, ""), tempFileIn(dir))
		if !errors.Is(err, helpers.ErrGitCommitMismatch) {
			t.Fatalf("error = %v", err)
		}
		assertEmptyDir(t, dir)
	})
	t.Run("nil temp file func", func(t *testing.T) {
		t.Parallel()
		src := faketree.New().File("galaxy.yml", minimalGalaxyYML)
		if _, err := Build(context.Background(), src, candidateFor(t, src, ""), nil); !errors.Is(err, helpers.ErrConfigIsNil) {
			t.Fatalf("error = %v", err)
		}
	})
}

// TestBuildRefusesOversizedFilesManifest pins that a listing past
// helpers.FilesManifestMaxBytes fails as that budget, before a temp file
// exists, and never as a self-check defect.
func TestBuildRefusesOversizedFilesManifest(t *testing.T) {
	t.Parallel()
	src := faketree.New().File("galaxy.yml", minimalGalaxyYML)
	const rowOverhead = 150
	rows := helpers.FilesManifestMaxBytes/(helpers.ArchiveMaxEntryNameLen+rowOverhead) + 1
	if rows >= helpers.ArchiveMaxEntryCount {
		t.Fatalf("%d rows needed, the entry cap is %d", rows, helpers.ArchiveMaxEntryCount)
	}
	prefix := strings.Repeat("n", helpers.ArchiveMaxEntryNameLen-8)
	for i := range rows {
		src.File(prefix+fmt.Sprintf("%08d", i), "x")
	}
	dir := t.TempDir()
	_, err := Build(context.Background(), src, candidateFor(t, src, ""), tempFileIn(dir))
	if !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
		t.Fatalf("error = %v, want ErrArchiveEntryIsTooLarge", err)
	}
	if errors.Is(err, helpers.ErrGitArtifactSelfCheck) {
		t.Fatalf("error = %v, classified as a self-check defect", err)
	}
	if !strings.Contains(err.Error(), helpers.FilesManifestFileName) {
		t.Fatalf("error = %v, does not name %s", err, helpers.FilesManifestFileName)
	}
	assertEmptyDir(t, dir)
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("temp dir still holds %d entries", len(left))
	}
}

func TestBuiltCleanupIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := faketree.New().File("galaxy.yml", minimalGalaxyYML)
	built, err := Build(context.Background(), src, candidateFor(t, src, ""), tempFileIn(dir))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := os.Stat(built.ArtifactPath); err != nil {
		t.Fatalf("artifact missing before cleanup: %v", err)
	}
	built.Cleanup()
	built.Cleanup()
	assertEmptyDir(t, dir)
}

func TestBuiltDependenciesAreACopy(t *testing.T) {
	t.Parallel()
	src := faketree.New().File("galaxy.yml", minimalGalaxyYML+"dependencies: {acme.base: '*'}\n")
	cand := candidateFor(t, src, "")
	built, err := Build(context.Background(), src, cand, tempFileIn(t.TempDir()))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(built.Cleanup)
	built.Dependencies["acme.base"] = "changed"
	if cand.Meta.Dependencies["acme.base"] != "*" {
		t.Fatal("Built.Dependencies aliases the candidate's map")
	}
}

// TestBuildGoldenDigest pins fixtureSource's artifact sha256. A Go bump may
// move it through compress/flate alone; if TestBuildStructure still passes,
// only the deflate bytes moved and the new literal is the answer.
func TestBuildGoldenDigest(t *testing.T) {
	t.Parallel()
	const want = "38564251bbd361a84da9a0bbdd480617506572830608866058183fda9695b4c1"
	built := buildFrom(t, fixtureSource(), "")
	if built.SHA256 != want {
		t.Fatalf("SHA256 = %s, want %s", built.SHA256, want)
	}
}
