package treearchive_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/treearchive"
	"github.com/greeddj/go-galaxy/internal/testing/faketree"
)

// errInjected is the failure faultySource reports for a path it was told to
// break.
var errInjected = errors.New("injected source failure")

// faultySource wraps a faketree and fails ReadDir, Open or the read of an
// opened blob for the paths named, so the planner's I/O error paths run.
type faultySource struct {
	*faketree.Tree

	readDir  map[string]bool
	open     map[string]bool
	readBody map[string]bool
}

func (s *faultySource) ReadDir(p string) ([]treearchive.Entry, error) {
	if s.readDir[p] {
		return nil, errInjected
	}
	return s.Tree.ReadDir(p)
}

func (s *faultySource) Open(p string) (io.ReadCloser, error) {
	if s.open[p] {
		return nil, errInjected
	}
	if s.readBody[p] {
		return io.NopCloser(failingReader{}), nil
	}
	return s.Tree.Open(p)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errInjected }

// planCase is one PlanTree refusal: the tree, the options and the sentinel
// the plan must fail with.
type planCase struct {
	src     treearchive.Source
	wantErr error
	name    string
	opts    treearchive.Options
}

// deepTree nests depth directories, each named "d", with a file at the bottom.
func deepTree(depth int) *faketree.Tree {
	return faketree.New().File(strings.Repeat("d/", depth)+"f", "x")
}

func planRefusalCases() []planCase {
	longPath := strings.Repeat(strings.Repeat("n", 200)+"/", 6) + "f"
	digests := treearchive.Options{Digests: true}
	return []planCase{
		{name: "missing root", src: faketree.New().File("a", "x"), opts: treearchive.Options{Root: "absent"}, wantErr: faketree.ErrNoEntry},
		{name: "tree deeper than the cap", src: deepTree(helpers.GitTreeMaxDepth), wantErr: helpers.ErrGitTreeTooDeep},
		{
			name: "directory past the entry budget", src: faketree.New().Dir("d"),
			opts: treearchive.Options{Reserved: helpers.ArchiveMaxEntryCount}, wantErr: helpers.ErrArchiveTooManyEntries,
		},
		{name: "unknown entry kind", src: faketree.New().Put("odd", treearchive.EntryKind(99), ""), wantErr: helpers.ErrGitTreeEntryInvalid},
		{name: "name past the cap", src: faketree.New().File(longPath, "x"), wantErr: helpers.ErrArchiveEntryNameTooLong},
		{
			name: "file past the entry size cap", src: faketree.New().File("big", "x").DeclareSize("big", helpers.ArchiveMaxEntrySize+1),
			wantErr: helpers.ErrArchiveEntryIsTooLarge,
		},
		{
			name: "blob shorter than declared", src: faketree.New().File("f", "abc").DeclareSize("f", 10),
			opts: digests, wantErr: helpers.ErrGitCommitMismatch,
		},
		{name: "dangling link", src: faketree.New().Symlink("l", "nowhere"), wantErr: helpers.ErrGitSymlinkUnresolvable},
		{name: "link through a file", src: faketree.New().File("f", "x").Symlink("l", "f/inner"), wantErr: helpers.ErrGitSymlinkUnresolvable},
		{name: "link loop", src: faketree.New().Symlink("a", "b").Symlink("b", "a"), wantErr: helpers.ErrGitSymlinkUnresolvable},
		{name: "link chain to an empty target", src: faketree.New().Symlink("a", "b").Symlink("b", ""), wantErr: helpers.ErrSymlinkTargetIsEmpty},
		{name: "empty link target", src: faketree.New().Symlink("l", ""), wantErr: helpers.ErrSymlinkTargetIsEmpty},
		{name: "link target with a NUL", src: faketree.New().Symlink("l", "a\x00b"), wantErr: helpers.ErrSymlinkTarget},
		{
			name: "link target declared past the cap", src: faketree.New().Symlink("l", "x").DeclareSize("l", helpers.ArchiveMaxEntryNameLen+1),
			wantErr: helpers.ErrArchiveEntryNameTooLong,
		},
		{
			name:    "link target longer than declared and the cap",
			src:     faketree.New().Symlink("l", strings.Repeat("t", helpers.ArchiveMaxEntryNameLen+1)).DeclareSize("l", 1),
			wantErr: helpers.ErrArchiveEntryNameTooLong,
		},
		{
			name: "link to an unknown entry kind", src: faketree.New().Symlink("a", "z").Put("z", treearchive.EntryKind(99), ""),
			wantErr: helpers.ErrGitTreeEntryInvalid,
		},
		{
			name: "link to a blob shorter than declared", src: faketree.New().Symlink("a", "z").File("z", "abc").DeclareSize("z", 10),
			opts: digests, wantErr: helpers.ErrGitCommitMismatch,
		},
		{name: "rendered link past the cap", src: renderedLinkTooLong(), wantErr: helpers.ErrArchiveEntryNameTooLong},
	}
}

// renderedLinkTooLong is a tree where every name and link target fits the
// cap, but the deep link's rendered target, climbing to a 1001-byte path
// through a root-level hop, does not.
func renderedLinkTooLong() *faketree.Tree {
	final := strings.Repeat(strings.Repeat("t", 99)+"/", 10) + "f"
	return faketree.New().
		File(final, "x").
		Symlink("m", final).
		Symlink(strings.Repeat("s/", 20)+"l", strings.Repeat("../", 20)+"m")
}

// TestPlanTreeRefusals pins every refusal the planner raises on its own, each
// by sentinel, before a byte of the artifact is written.
func TestPlanTreeRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range planRefusalCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := treearchive.PlanTree(context.Background(), tc.src, tc.opts)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("PlanTree error = %v, want %v", err, tc.wantErr)
			}
			if plan != nil {
				t.Fatalf("PlanTree returned a plan beside its error")
			}
		})
	}
}

// TestPlanTreeAtTheDepthCapIsAccepted is the positive control for the depth
// refusal: one level fewer plans normally.
func TestPlanTreeAtTheDepthCapIsAccepted(t *testing.T) {
	t.Parallel()
	if _, err := treearchive.PlanTree(context.Background(), deepTree(helpers.GitTreeMaxDepth-1), treearchive.Options{}); err != nil {
		t.Fatalf("PlanTree at the depth cap: %v", err)
	}
}

// TestPlanTreeSourceFailures pins that a source's own I/O error reaches the
// caller, wrapped where the planner names the path, from every read it makes.
func TestPlanTreeSourceFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		src  *faultySource
		name string
		opts treearchive.Options
	}{
		{
			name: "directory listing during the walk",
			src:  &faultySource{Tree: faketree.New().File("d/f", "x"), readDir: map[string]bool{"d": true}},
		},
		{
			name: "directory listing while resolving a link",
			src:  &faultySource{Tree: faketree.New().Symlink("a", "d/f").File("d/f", "x"), readDir: map[string]bool{"d": true}},
		},
		{
			name: "opening a blob to digest it",
			src:  &faultySource{Tree: faketree.New().File("f", "x"), open: map[string]bool{"f": true}},
			opts: treearchive.Options{Digests: true},
		},
		{
			name: "reading a blob to digest it",
			src:  &faultySource{Tree: faketree.New().File("f", "x"), readBody: map[string]bool{"f": true}},
			opts: treearchive.Options{Digests: true},
		},
		{
			name: "opening a link target",
			src:  &faultySource{Tree: faketree.New().Symlink("l", "x"), open: map[string]bool{"l": true}},
		},
		{
			name: "reading a link target",
			src:  &faultySource{Tree: faketree.New().Symlink("l", "x"), readBody: map[string]bool{"l": true}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := treearchive.PlanTree(context.Background(), tc.src, tc.opts); !errors.Is(err, errInjected) {
				t.Fatalf("PlanTree error = %v, want the source's own error", err)
			}
		})
	}
}

// TestPlanTreeObservesCancellation pins that a context already done stops the
// walk before its first entry.
func TestPlanTreeObservesCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := treearchive.PlanTree(ctx, faketree.New().File("a", "x"), treearchive.Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("PlanTree error = %v, want context.Canceled", err)
	}
}

// TestPlanTreeLinkResolution pins that a resolved link points straight at its
// final entry, through chains and directory links, and reuses that blob's digest.
func TestPlanTreeLinkResolution(t *testing.T) {
	t.Parallel()
	src := faketree.New().
		File("a", "x").
		Symlink("chain1", "chain2").
		Symlink("chain2", "a").
		Symlink("deep", "d/f").
		Symlink("dirlink", "d").
		File("d/f", "y").
		Symlink("via", "dirlink/f")
	opts := treearchive.Options{Digests: true}
	plan, err := treearchive.PlanTree(context.Background(), src, opts)
	if err != nil {
		t.Fatalf("PlanTree: %v", err)
	}
	digests := map[string]string{}
	for _, row := range plan.Rows() {
		digests[row.Name] = row.Digest
	}
	if digests["chain1"] == "" || digests["chain1"] != digests["a"] {
		t.Errorf("chain1 digest = %q, want a's %q", digests["chain1"], digests["a"])
	}
	if digests["via"] == "" || digests["via"] != digests["d/f"] || digests["deep"] != digests["d/f"] {
		t.Errorf("via/deep digests = %q/%q, want d/f's %q", digests["via"], digests["deep"], digests["d/f"])
	}
	if len(plan.Warnings()) != 0 {
		t.Errorf("warnings = %q, want none", plan.Warnings())
	}

	p, _ := writeTo(t, t.TempDir(), src, opts, nil)
	want := []string{"a", "chain1 -> a", "chain2 -> a", "d", "d/f", "deep -> d/f", "dirlink -> d", "via -> d/f"}
	if got := listNames(t, p); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries = %q, want %q", got, want)
	}
}

// TestPlanTreeSkipsLinksTheArtifactCannotCarry pins the links skipped with a
// warning rather than refused, and a directory link the rules drop silently.
func TestPlanTreeSkipsLinksTheArtifactCannotCarry(t *testing.T) {
	t.Parallel()
	src := faketree.New().
		Dir("cache-real").
		Symlink("cache", "cache-real").
		File("build/out.txt", "x").
		Symlink("excluded-link", "build/out.txt").
		File("d/f", "x").
		Symlink("d/self", ".").
		Symlink("mod-link", "mod").
		Submodule("mod").
		File("notes.bak", "x").
		Symlink("pattern-link", "notes.bak")
	rules := treearchive.Rules{Patterns: []string{"*.bak"}, DirNames: map[string]struct{}{"build": {}, "cache": {}}}
	plan, err := treearchive.PlanTree(context.Background(), src, treearchive.Options{Rules: rules})
	if err != nil {
		t.Fatalf("PlanTree: %v", err)
	}
	want := []string{
		"skipping symlink d/self: target resolves to its own directory, which the extractor refuses",
		"skipping symlink excluded-link: target is excluded from the build",
		"skipping submodule mod: submodules are never fetched",
		"skipping symlink mod-link: target is a submodule, which is never fetched",
		"skipping symlink pattern-link: target is excluded from the build",
	}
	if got := plan.Warnings(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("warnings = %q, want %q", got, want)
	}
	for _, row := range plan.Rows() {
		switch row.Name {
		case "cache", "d/self", "excluded-link", "mod-link", "pattern-link":
			t.Errorf("a skipped link is still planned: %+v", row)
		}
	}
}

// TestWriteSourceFailures pins the write pass's own refusals: a blob that
// cannot be opened again, and one that shrank since it was planned.
func TestWriteSourceFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		wantErr error
		mutate  func(src *faultySource)
		name    string
	}{
		{name: "blob no longer opens", wantErr: errInjected, mutate: func(src *faultySource) { src.open = map[string]bool{"f": true} }},
		{name: "blob shrank", wantErr: helpers.ErrGitCommitMismatch, mutate: func(src *faultySource) { src.File("f", "a") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := &faultySource{Tree: faketree.New().File("f", "abc")}
			plan, err := treearchive.PlanTree(context.Background(), src, treearchive.Options{})
			if err != nil {
				t.Fatalf("PlanTree: %v", err)
			}
			tc.mutate(src)
			f, err := os.CreateTemp(t.TempDir(), "artifact-*")
			if err != nil {
				t.Fatalf("temp: %v", err)
			}
			defer func() { _ = f.Close() }()
			if _, err := plan.Write(context.Background(), f, nil); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Write error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestWriteReportsAFileItCannotWrite pins that a destination refusing writes
// fails the build at the first stage that flushes: a lead document, an
// entry, or the closing of an empty archive.
func TestWriteReportsAFileItCannotWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		src  *faketree.Tree
		name string
		want string
		lead []treearchive.Document
	}{
		{
			name: "lead document", src: faketree.New(), want: "writing MANIFEST.json",
			lead: []treearchive.Document{{Name: "MANIFEST.json", Data: []byte("{}")}},
		},
		{name: "planned entry", src: faketree.New().File("f", "x"), want: "writing f"},
		{name: "empty archive", src: faketree.New(), want: "closing the tar stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := treearchive.PlanTree(context.Background(), tc.src, treearchive.Options{})
			if err != nil {
				t.Fatalf("PlanTree: %v", err)
			}
			f, err := os.CreateTemp(t.TempDir(), "artifact-*")
			if err != nil {
				t.Fatalf("temp: %v", err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			_, err = plan.Write(context.Background(), f, tc.lead)
			if !errors.Is(err, os.ErrClosed) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Write error = %v, want os.ErrClosed while %s", err, tc.want)
			}
		})
	}
}

// TestPlanTreeKeepsTheCommitTime pins that every entry carries the source's
// commit time truncated to the second, whatever its sub-second part.
func TestPlanTreeKeepsTheCommitTime(t *testing.T) {
	t.Parallel()
	src := faketree.New().File("a", "x").SetCommitTime(faketree.FixedCommitTime().Add(750 * time.Millisecond))
	p, _ := writeTo(t, t.TempDir(), src, treearchive.Options{}, nil)
	if got := listNames(t, p); len(got) != 1 || got[0] != "a" {
		t.Fatalf("entries = %q, want [a]", got)
	}
}
