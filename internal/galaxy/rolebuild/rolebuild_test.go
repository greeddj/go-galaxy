package rolebuild

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/testing/faketree"
)

const (
	minimalMeta = "galaxy_info:\n  author: test\n  role_name: app\ndependencies:\n  - acme.base\n"
	tasksMain   = "- debug: msg=hi\n"
)

func tempFileIn(dir string) TempFileFunc {
	return func(_ context.Context) (*os.File, func(), error) {
		f, err := os.CreateTemp(dir, ".download-*")
		if err != nil {
			return nil, nil, err
		}
		name := f.Name()
		return f, func() { _ = os.Remove(name) }, nil
	}
}

// minimalRole is the smallest tree every build fixture starts from.
func minimalRole() *faketree.Tree {
	return faketree.New().File("meta/main.yml", minimalMeta).File("tasks/main.yml", tasksMain)
}

func build(t *testing.T, src *faketree.Tree) Built {
	t.Helper()
	built, err := Build(context.Background(), src, tempFileIn(t.TempDir()))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(built.Cleanup)
	return built
}

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

// TestBuildArtifactShape pins what the archive holds: every entry at the
// top level in the tree's order, no lead document, the fixed modes and the
// committer time, and the same sha256 from two builds of one tree.
func TestBuildArtifactShape(t *testing.T) {
	t.Parallel()
	src := minimalRole().
		File("README.md", "# app\n").
		File("defaults/main.yml", "---\n").
		Exec("files/run.sh", "#!/bin/sh\n").
		Dir("templates").
		Symlink("docs/readme-link.md", "../README.md").
		File(".git/HEAD", "ref: refs/heads/main\n").
		File("vendor/.git/config", "x").
		File("vendor/keep.txt", "kept")
	first := build(t, src)
	second := build(t, src)
	if first.SHA256 != second.SHA256 || !helpers.IsSHA256Hex(first.SHA256) {
		t.Fatalf("sha256 = %q and %q, want one valid digest", first.SHA256, second.SHA256)
	}
	if len(first.Warnings) != 0 {
		t.Fatalf("warnings = %q", first.Warnings)
	}
	entries := readArtifact(t, first.ArtifactPath)
	want := []string{
		"README.md", "defaults", "defaults/main.yml", "docs", "docs/readme-link.md", "files", "files/run.sh",
		"meta", "meta/main.yml", "tasks", "tasks/main.yml", "templates", "vendor", "vendor/keep.txt",
	}
	if got := entryNames(entries); !slices.Equal(got, want) {
		t.Fatalf("entries = %q, want %q", got, want)
	}
	assertTopLevelEntries(t, entries)
	assertEntryModes(t, entries)
	if e := findEntry(t, entries, "tasks/main.yml"); string(e.data) != tasksMain {
		t.Fatalf("tasks/main.yml = %q", e.data)
	}
}

// TestBuildCommitTimeReadsBackFromTheFirstEntry pins the round trip an install
// line's date depends on: the committer time a build stamps is what
// archive.FirstEntryModTime reads back, in UTC.
func TestBuildCommitTimeReadsBackFromTheFirstEntry(t *testing.T) {
	t.Parallel()

	when := time.Date(2026, time.October, 29, 16, 55, 0, 0, time.FixedZone("", 3*60*60))
	built := build(t, minimalRole().SetCommitTime(when))

	got, err := archive.FirstEntryModTime(t.Context(), built.ArtifactPath)
	if err != nil {
		t.Fatalf("FirstEntryModTime = %v, want nil", err)
	}
	if !got.Equal(when) || got.Location() != time.UTC {
		t.Fatalf("FirstEntryModTime = %v (%v), want %v in UTC", got, got.Location(), when.UTC())
	}
}

// assertTopLevelEntries checks no entry carries a prefix or a manifest name
// and every one is stamped with the committer time and no owner.
func assertTopLevelEntries(t *testing.T, entries []archiveEntry) {
	t.Helper()
	for _, e := range entries {
		name := e.header.Name
		if strings.HasPrefix(name, "./") || strings.Contains(name, "MANIFEST.json") || strings.Contains(name, "FILES.json") {
			t.Fatalf("entry %q: a role artifact carries no prefix and no manifest", name)
		}
		if !e.header.ModTime.Equal(faketree.FixedCommitTime()) || e.header.Uid != 0 || e.header.Uname != "" {
			t.Fatalf("entry %q: time %v uid %d uname %q", name, e.header.ModTime, e.header.Uid, e.header.Uname)
		}
	}
}

// assertEntryModes checks the fixed mode of each entry kind and the link's
// relative target.
func assertEntryModes(t *testing.T, entries []archiveEntry) {
	t.Helper()
	modes := map[string]int64{"meta/main.yml": 0o644, "files/run.sh": 0o755, "templates": 0o755, "docs/readme-link.md": 0o777}
	for name, mode := range modes {
		if e := findEntry(t, entries, name); e.header.Mode != mode {
			t.Fatalf("%s mode = %o, want %o", name, e.header.Mode, mode)
		}
	}
	if e := findEntry(t, entries, "docs/readme-link.md"); e.header.Typeflag != tar.TypeSymlink || e.header.Linkname != "../README.md" {
		t.Fatalf("link = %+v", e.header)
	}
}

// TestBuildReadsMeta covers how the metadata reaches the result: the role
// name, and the dependencies of main.yml with requirements.yml behind them.
func TestBuildReadsMeta(t *testing.T) {
	t.Parallel()
	src := minimalRole().File("meta/requirements.yml", "- src: https://example.com/extra.git\n  name: extra\n- acme.more\n")
	built := build(t, src)
	want := Meta{
		Dependencies: []gitsource.RoleDependency{
			{Src: "acme.base"},
			{Src: "https://example.com/extra.git", Name: "extra"},
			{Src: "acme.more"},
		},
		RoleName: "app",
	}
	if !reflect.DeepEqual(built.Meta, want) {
		t.Fatalf("Meta = %#v, want %#v", built.Meta, want)
	}
}

// TestBuildReadsYAMLSpellings proves the .yaml spelling of both files is
// read when the .yml one is absent.
func TestBuildReadsYAMLSpellings(t *testing.T) {
	t.Parallel()
	src := faketree.New().
		File("meta/main.yaml", "dependencies: [acme.one]\n").
		File("meta/requirements.yaml", "- acme.two\n")
	built := build(t, src)
	want := []gitsource.RoleDependency{{Src: "acme.one"}, {Src: "acme.two"}}
	if !reflect.DeepEqual(built.Meta.Dependencies, want) || built.Meta.RoleName != "" {
		t.Fatalf("Meta = %#v", built.Meta)
	}
}

// TestBuildWarningsFollowTheWalk pins the order of Built.Warnings - the
// walk's skipped link and submodule, then the meta parser's - and that a
// skipped entry is not in the archive.
func TestBuildWarningsFollowTheWalk(t *testing.T) {
	t.Parallel()
	src := faketree.New().
		File("meta/main.yml", "galaxy_info: text\n").
		Symlink("escape", "../outside").
		Submodule("vendored")
	built := build(t, src)
	if len(built.Warnings) != 3 {
		t.Fatalf("warnings = %q, want 3", built.Warnings)
	}
	if !strings.Contains(built.Warnings[0], "target outside the role") ||
		!strings.Contains(built.Warnings[1], "submodule") ||
		!strings.Contains(built.Warnings[2], "galaxy_info is a YAML str") {
		t.Fatalf("warnings = %q", built.Warnings)
	}
	if !slices.Equal(built.Meta.Warnings, built.Warnings[2:]) {
		t.Fatalf("Meta.Warnings = %q", built.Meta.Warnings)
	}
	names := entryNames(readArtifact(t, built.ArtifactPath))
	if slices.Contains(names, "escape") || slices.Contains(names, "vendored") {
		t.Fatalf("entries = %q", names)
	}
}

type refusalCase struct {
	src      *faketree.Tree
	wantErr  error
	name     string
	wantText string
}

// notARoleCases are the trees that are not a role: no meta directory, or one
// listing neither main spelling as a regular file.
func notARoleCases() []refusalCase {
	return []refusalCase{
		{
			name: "no meta directory", src: faketree.New().File("tasks/main.yml", tasksMain),
			wantErr: helpers.ErrRoleMetaNotFound, wantText: "no meta directory",
		},
		{name: "meta is a file", src: faketree.New().File("meta", "x"), wantErr: helpers.ErrRoleMetaNotFound, wantText: "no meta directory"},
		{
			name: "meta without main", src: faketree.New().File("meta/runtime.yml", "x"),
			wantErr: helpers.ErrRoleMetaNotFound, wantText: "neither main.yml nor main.yaml",
		},
		{
			name: "main is a symlink", src: faketree.New().Symlink("meta/main.yml", "other.yml").File("meta/other.yml", ""),
			wantErr: helpers.ErrRoleMetaNotFound,
		},
		{name: "main is a directory", src: faketree.New().Dir("meta/main.yml"), wantErr: helpers.ErrRoleMetaNotFound},
	}
}

// invalidMetaCases are roles whose metadata this tool cannot read, and two
// tree defects the walk refuses, for the temp-file discipline.
func invalidMetaCases() []refusalCase {
	overCap := strings.Repeat("x", metadataMaxBytes+1)
	return []refusalCase{
		{
			name: "both main spellings", src: minimalRole().File("meta/main.yaml", ""),
			wantErr: helpers.ErrRoleMetaInvalid, wantText: "both main.yml and main.yaml",
		},
		{
			name: "both requirements spellings", src: minimalRole().File("meta/requirements.yml", "").File("meta/requirements.yaml", ""),
			wantErr: helpers.ErrRoleMetaInvalid, wantText: "both requirements.yml and requirements.yaml",
		},
		{
			name: "main over the cap", src: faketree.New().File("meta/main.yml", overCap),
			wantErr: helpers.ErrRoleMetaInvalid, wantText: "larger than",
		},
		{
			name: "requirements over the cap", src: minimalRole().File("meta/requirements.yml", overCap),
			wantErr: helpers.ErrRoleMetaInvalid, wantText: "larger than",
		},
		{
			name: "main is not a mapping", src: faketree.New().File("meta/main.yml", "- a\n"),
			wantErr: helpers.ErrRoleMetaInvalid, wantText: "meta/main.yml is not a mapping",
		},
		{
			name: "invalid dependency item", src: faketree.New().File("meta/main.yml", "dependencies:\n  - acme.ok\n  - 7\n"),
			wantErr: helpers.ErrRoleMetaInvalid, wantText: "meta/main.yml dependencies[1]",
		},
		{
			name: "invalid requirements item", src: minimalRole().File("meta/requirements.yaml", "- {version: 1}\n"),
			wantErr: helpers.ErrRoleMetaInvalid, wantText: "meta/requirements.yaml[0] names no role, name or src",
		},
		{name: "dangling link", src: minimalRole().Symlink("broken", "nowhere"), wantErr: helpers.ErrGitSymlinkUnresolvable},
		{name: "blob shorter than declared", src: minimalRole().File("f", "abc").DeclareSize("f", 10), wantErr: helpers.ErrGitCommitMismatch},
	}
}

// TestBuildRefusals pins what is not a role and what is a role with
// metadata this tool cannot read, each under its sentinel and with no temp
// file left behind.
func TestBuildRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range slices.Concat(notARoleCases(), invalidMetaCases()) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			_, err := Build(context.Background(), tc.src, tempFileIn(dir))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error %q does not name %q", err.Error(), tc.wantText)
			}
			assertEmptyDir(t, dir)
		})
	}
}

var errRefusedByTest = errors.New("no temp file")

// TestBuildCancellationLeavesNoTempFile proves a context canceled once the
// temp file exists, or in the middle of streaming a blob, removes the file
// and reports the cancellation itself.
func TestBuildCancellationLeavesNoTempFile(t *testing.T) {
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
		_, err := Build(ctx, minimalRole(), tempFile)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		assertEmptyDir(t, dir)
	})
	t.Run("cancel during a blob read", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		src := minimalRole().File("b/c", "x")
		src.SetOnOpen(func(p string) {
			if p != "meta/main.yml" {
				cancel()
			}
		})
		dir := t.TempDir()
		_, err := Build(ctx, src, tempFileIn(dir))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		assertEmptyDir(t, dir)
	})
}

// TestBuildSourceErrorsLeaveNoTempFile proves a Source that changes under
// the build, or a temp file that cannot be had, fails with the cause and
// leaves nothing on disk.
func TestBuildSourceErrorsLeaveNoTempFile(t *testing.T) {
	t.Parallel()
	t.Run("blob grows after the plan", func(t *testing.T) {
		t.Parallel()
		src := minimalRole().File("f", "abc")
		src.SetOnOpen(func(p string) {
			if p == "f" {
				src.File("f", "abcdef")
			}
		})
		dir := t.TempDir()
		_, err := Build(context.Background(), src, tempFileIn(dir))
		if !errors.Is(err, helpers.ErrGitCommitMismatch) {
			t.Fatalf("error = %v", err)
		}
		assertEmptyDir(t, dir)
	})
	t.Run("open fails mid-write", func(t *testing.T) {
		t.Parallel()
		src := minimalRole().File("f", "abc")
		src.SetOnOpen(func(p string) {
			if p == "f" {
				src.Dir("f")
			}
		})
		dir := t.TempDir()
		_, err := Build(context.Background(), src, tempFileIn(dir))
		if !errors.Is(err, faketree.ErrNoEntry) {
			t.Fatalf("error = %v", err)
		}
		assertEmptyDir(t, dir)
	})
	t.Run("temp file refused", func(t *testing.T) {
		t.Parallel()
		_, err := Build(context.Background(), minimalRole(), func(context.Context) (*os.File, func(), error) {
			return nil, nil, errRefusedByTest
		})
		if !errors.Is(err, errRefusedByTest) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("nil temp file func", func(t *testing.T) {
		t.Parallel()
		if _, err := Build(context.Background(), minimalRole(), nil); !errors.Is(err, helpers.ErrConfigIsNil) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestBuiltCleanupIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	built, err := Build(context.Background(), minimalRole(), tempFileIn(dir))
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
