package gitfetch

import (
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/greeddj/go-galaxy/internal/galaxy/collectionbuild"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// memObjects builds raw git objects in memory, unvalidated, so treeSource can
// be handed trees no real remote would serve and objects a pack left out.
type memObjects struct {
	tb testing.TB
	st *memory.Storage
}

func newMemObjects(tb testing.TB) *memObjects {
	tb.Helper()
	return &memObjects{tb: tb, st: memory.NewStorage()}
}

func (m *memObjects) store(obj plumbing.EncodedObject) plumbing.Hash {
	m.tb.Helper()
	h, err := m.st.SetEncodedObject(obj)
	if err != nil {
		m.tb.Fatalf("store object: %v", err)
	}
	return h
}

// blob stores data as a blob. A declared size other than len(data) is set
// after the hash is fixed, so the object claims a size no content backs.
func (m *memObjects) blob(data string, declared int64) plumbing.Hash {
	m.tb.Helper()
	obj := m.st.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		m.tb.Fatalf("blob writer: %v", err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		m.tb.Fatalf("write blob: %v", err)
	}
	if err := w.Close(); err != nil {
		m.tb.Fatalf("close blob: %v", err)
	}
	_ = obj.Hash()
	if declared != int64(len(data)) {
		obj.SetSize(declared)
	}
	return m.store(obj)
}

// tree encodes entries, git-sorted, as one tree object.
func (m *memObjects) tree(entries ...object.TreeEntry) plumbing.Hash {
	m.tb.Helper()
	sorted := append([]object.TreeEntry(nil), entries...)
	sort.Sort(object.TreeEntrySorter(sorted))
	obj := m.st.NewEncodedObject()
	if err := (&object.Tree{Entries: sorted}).Encode(obj); err != nil {
		m.tb.Fatalf("encode tree: %v", err)
	}
	return m.store(obj)
}

// source commits root and opens a treeSource over it.
func (m *memObjects) source(root plumbing.Hash) (*treeSource, error) {
	m.tb.Helper()
	when := time.Date(2024, time.May, 1, 10, 0, 0, 900, time.FixedZone("x", 3600))
	sig := object.Signature{Name: "t", Email: "t@example.invalid", When: when}
	obj := m.st.NewEncodedObject()
	if err := (&object.Commit{Author: sig, Committer: sig, Message: "m\n", TreeHash: root}).Encode(obj); err != nil {
		m.tb.Fatalf("encode commit: %v", err)
	}
	commit, err := object.GetCommit(m.st, m.store(obj))
	if err != nil {
		m.tb.Fatalf("read commit back: %v", err)
	}
	return newTreeSource(m.st, commit)
}

func (m *memObjects) mustSource(root plumbing.Hash) *treeSource {
	m.tb.Helper()
	src, err := m.source(root)
	if err != nil {
		m.tb.Fatalf("newTreeSource: %v", err)
	}
	return src
}

func entry(name string, mode filemode.FileMode, h plumbing.Hash) object.TreeEntry {
	return object.TreeEntry{Name: name, Mode: mode, Hash: h}
}

// missingHash names an object the store does not hold.
func missingHash() plumbing.Hash { return plumbing.NewHash(strings.Repeat("ab", 20)) }

// hostileTreeCase is one tree validateEntries must refuse, by sentinel.
type hostileTreeCase struct {
	wantErr error
	name    string
	entries []object.TreeEntry
}

// TestTreeSourceRefusesHostileNames pins validateEntries on every tree it
// decodes, root and subtree alike: unsafe names, git's metadata directory in
// any case or 8.3 spelling, and case-folded duplicates.
func TestTreeSourceRefusesHostileNames(t *testing.T) {
	t.Parallel()
	invalid := func(name, entryName string, mode filemode.FileMode) hostileTreeCase {
		entries := []object.TreeEntry{entry(entryName, mode, plumbing.ZeroHash)}
		return hostileTreeCase{name: name, wantErr: helpers.ErrGitTreeEntryInvalid, entries: entries}
	}
	for _, tc := range []hostileTreeCase{
		invalid("parent directory", "..", filemode.Regular),
		invalid("backslash", `a\b`, filemode.Regular),
		invalid("git directory", ".git", filemode.Dir),
		invalid("git directory in upper case", ".GIT", filemode.Regular),
		invalid("git directory short name", "Git~1", filemode.Dir),
		{name: "case-folded duplicate", wantErr: helpers.ErrGitTreeDuplicateEntry, entries: []object.TreeEntry{
			entry("README", filemode.Regular, plumbing.ZeroHash), entry("readme", filemode.Regular, plumbing.ZeroHash),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newMemObjects(t)
			if _, err := m.source(m.tree(tc.entries...)); !errors.Is(err, tc.wantErr) {
				t.Fatalf("root tree: error = %v, want %v", err, tc.wantErr)
			}
			src := m.mustSource(m.tree(entry("sub", filemode.Dir, m.tree(tc.entries...))))
			_, err := src.ReadDir("sub")
			if !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), "(under sub)") {
				t.Fatalf("subtree: error = %v, want %v under sub", err, tc.wantErr)
			}
		})
	}
}

// TestEntryKindRefusesAMalformedMode pins the mode guard for entries that
// reach validateEntries without go-git's decoder, which canonicalizes every
// mode it reads to one of the five kinds.
func TestEntryKindRefusesAMalformedMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []filemode.FileMode{filemode.Empty, filemode.FileMode(0o100600), filemode.FileMode(0o160001)} {
		if kind, err := entryKind(mode); !errors.Is(err, helpers.ErrGitTreeEntryInvalid) {
			t.Errorf("entryKind(%o) = (%v, %v), want ErrGitTreeEntryInvalid", uint32(mode), kind, err)
		}
		err := validateEntries([]object.TreeEntry{entry("f", mode, plumbing.ZeroHash)})
		if !errors.Is(err, helpers.ErrGitTreeEntryInvalid) || !strings.Contains(err.Error(), `"f"`) {
			t.Errorf("validateEntries with mode %o = %v, want ErrGitTreeEntryInvalid naming the entry", uint32(mode), err)
		}
	}
	if kind, err := entryKind(filemode.Deprecated); err != nil || kind != collectionbuild.EntryFile {
		t.Errorf("entryKind(Deprecated) = (%v, %v), want a regular file", kind, err)
	}
}

// TestTreeSourceRefusesAMissingRootTree pins that a commit whose tree the
// pack did not carry is an integrity failure, not an empty tree.
func TestTreeSourceRefusesAMissingRootTree(t *testing.T) {
	t.Parallel()
	m := newMemObjects(t)
	if _, err := m.source(missingHash()); !errors.Is(err, helpers.ErrGitCommitMismatch) {
		t.Fatalf("newTreeSource error = %v, want ErrGitCommitMismatch", err)
	}
}

// TestTreeSourceCommitTime pins that the committer time is reported in UTC
// and whole seconds, whatever zone the commit was written in.
func TestTreeSourceCommitTime(t *testing.T) {
	t.Parallel()
	m := newMemObjects(t)
	src := m.mustSource(m.tree(entry("f", filemode.Regular, m.blob("x", 1))))
	want := time.Date(2024, time.May, 1, 9, 0, 0, 0, time.UTC)
	if got := src.CommitTime(); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("CommitTime = %v, want %v in UTC", got, want)
	}
}

// TestTreeSourceReadDir pins the listing: every kind mapped, blob sizes read
// from the store, and nested trees reached through their parents.
func TestTreeSourceReadDir(t *testing.T) {
	t.Parallel()
	m := newMemObjects(t)
	inner := m.tree(entry("deep.txt", filemode.Regular, m.blob("deep", 4)))
	root := m.tree(
		entry("file.txt", filemode.Regular, m.blob("hello", 5)),
		entry("legacy.txt", filemode.Deprecated, m.blob("old", 3)),
		entry("run.sh", filemode.Executable, m.blob("#!/bin/sh\n", 10)),
		entry("link", filemode.Symlink, m.blob("file.txt", 8)),
		entry("sub", filemode.Dir, m.tree(entry("inner", filemode.Dir, inner))),
		entry("vendored", filemode.Submodule, missingHash()),
	)
	src := m.mustSource(root)

	got, err := src.ReadDir("")
	if err != nil {
		t.Fatalf("ReadDir(root): %v", err)
	}
	want := map[string]collectionbuild.Entry{
		"file.txt":   {Name: "file.txt", Kind: collectionbuild.EntryFile, Size: 5},
		"legacy.txt": {Name: "legacy.txt", Kind: collectionbuild.EntryFile, Size: 3},
		"run.sh":     {Name: "run.sh", Kind: collectionbuild.EntryExecutable, Size: 10},
		"link":       {Name: "link", Kind: collectionbuild.EntrySymlink, Size: 8},
		"sub":        {Name: "sub", Kind: collectionbuild.EntryDir},
		"vendored":   {Name: "vendored", Kind: collectionbuild.EntrySubmodule},
	}
	if len(got) != len(want) {
		t.Fatalf("ReadDir(root) = %+v, want %d entries", got, len(want))
	}
	for _, e := range got {
		if e != want[e.Name] {
			t.Errorf("entry %s = %+v, want %+v", e.Name, e, want[e.Name])
		}
	}
	nested, err := src.ReadDir("sub/inner")
	if err != nil || len(nested) != 1 || nested[0].Name != "deep.txt" {
		t.Fatalf("ReadDir(sub/inner) = %+v, %v", nested, err)
	}
	if again, err := src.ReadDir("sub/inner"); err != nil || len(again) != 1 {
		t.Fatalf("cached ReadDir(sub/inner) = %+v, %v", again, err)
	}
}

// TestTreeSourceReadDirRefusals pins the listing's refusals: a path that is
// not a directory, a path past the depth cap, and a tree or blob the pack did
// not carry.
func TestTreeSourceReadDirRefusals(t *testing.T) {
	t.Parallel()
	m := newMemObjects(t)
	root := m.tree(
		entry("file.txt", filemode.Regular, m.blob("x", 1)),
		entry("lost-tree", filemode.Dir, missingHash()),
		entry("lost-blob", filemode.Dir, m.tree(entry("gone.txt", filemode.Regular, missingHash()))),
	)
	src := m.mustSource(root)
	for _, tc := range []struct {
		wantErr error
		path    string
	}{
		{path: "absent", wantErr: helpers.ErrGitCollectionNotFound},
		{path: "file.txt", wantErr: helpers.ErrGitCollectionNotFound},
		{path: "file.txt/below", wantErr: helpers.ErrGitCollectionNotFound},
		{path: strings.Repeat("d/", helpers.GitTreeMaxDepth) + "d", wantErr: helpers.ErrGitTreeTooDeep},
		{path: "lost-tree", wantErr: helpers.ErrGitCommitMismatch},
		{path: "lost-blob", wantErr: helpers.ErrGitCommitMismatch},
	} {
		if _, err := src.ReadDir(tc.path); !errors.Is(err, tc.wantErr) {
			t.Errorf("ReadDir(%q) = %v, want %v", tc.path, err, tc.wantErr)
		}
	}
}

// TestTreeSourceOpen pins that a blob streams its bytes, and that a missing
// entry, a directory, a submodule, a lost blob and an oversized one are refused.
func TestTreeSourceOpen(t *testing.T) {
	t.Parallel()
	m := newMemObjects(t)
	root := m.tree(
		entry("file.txt", filemode.Regular, m.blob("hello", 5)),
		entry("huge.bin", filemode.Regular, m.blob("x", helpers.ArchiveMaxEntrySize+1)),
		entry("lost.txt", filemode.Regular, missingHash()),
		entry("sub", filemode.Dir, m.tree(entry("inner.txt", filemode.Regular, m.blob("inner", 5)))),
		entry("vendored", filemode.Submodule, missingHash()),
	)
	src := m.mustSource(root)

	rc, err := src.Open("sub/inner.txt")
	if err != nil {
		t.Fatalf("Open(sub/inner.txt): %v", err)
	}
	body, err := io.ReadAll(rc)
	if err != nil || string(body) != "inner" {
		t.Fatalf("sub/inner.txt = %q, %v", body, err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, tc := range []struct {
		wantErr error
		path    string
	}{
		{path: "absent.txt", wantErr: helpers.ErrGitCollectionNotFound},
		{path: "absent/x.txt", wantErr: helpers.ErrGitCollectionNotFound},
		{path: "sub", wantErr: helpers.ErrGitTreeEntryInvalid},
		{path: "vendored", wantErr: helpers.ErrGitTreeEntryInvalid},
		{path: "lost.txt", wantErr: helpers.ErrGitCommitMismatch},
		{path: "huge.bin", wantErr: helpers.ErrArchiveEntryIsTooLarge},
	} {
		if _, err := src.Open(tc.path); !errors.Is(err, tc.wantErr) {
			t.Errorf("Open(%q) = %v, want %v", tc.path, err, tc.wantErr)
		}
	}
}

// TestCappedBlobReportsTheArchiveSentinel pins that a blob streaming past its
// cap fails with the archive's per-entry sentinel, naming the path.
func TestCappedBlobReportsTheArchiveSentinel(t *testing.T) {
	t.Parallel()
	blob := &cappedBlob{
		Reader: helpers.NewSizeLimitedReader(strings.NewReader("abcdef"), 3),
		closer: io.NopCloser(nil),
		path:   "files/big.bin",
	}
	_, err := io.ReadAll(blob)
	if !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) || !strings.Contains(err.Error(), "files/big.bin") {
		t.Fatalf("ReadAll error = %v, want ErrArchiveEntryIsTooLarge naming the path", err)
	}
}
