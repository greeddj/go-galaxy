// Package faketree is an in-memory treearchive.Source for tests, listed in
// byte order with a fixed commit time so two builds of one tree are
// byte-identical; the builder tests and the install pipeline's git double use it.
package faketree

import (
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/treearchive"
)

// ErrNoEntry is the refusal for a path the tree does not hold.
var ErrNoEntry = errors.New("faketree: no such entry")

// entry is one tree entry: the blob of a file or a symlink's target, nothing
// for a directory or submodule. sizeOverride, when set, is what ReadDir
// declares instead of the blob length, a size no blob backs.
type entry struct {
	data         string
	sizeOverride int64
	kind         treearchive.EntryKind
}

// Tree is the in-memory source. The zero value is not usable; use New.
type Tree struct {
	entries map[string]entry
	onOpen  func(path string)
	when    time.Time
}

// FixedCommitTime is the committer time every tree reports unless a test
// sets another through SetCommitTime.
func FixedCommitTime() time.Time {
	return time.Date(2024, time.March, 5, 12, 30, 45, 0, time.UTC)
}

// New returns an empty tree stamped with FixedCommitTime.
func New() *Tree {
	return &Tree{entries: map[string]entry{}, when: FixedCommitTime()}
}

// CommitTime implements treearchive.Source.
func (m *Tree) CommitTime() time.Time { return m.when }

// SetCommitTime replaces the committer time the tree reports.
func (m *Tree) SetCommitTime(when time.Time) *Tree {
	m.when = when
	return m
}

// SetOnOpen installs a hook called with the path of every Open, before the
// entry is looked up, so a test can cancel a context or mutate the tree in
// the middle of a build.
func (m *Tree) SetOnOpen(fn func(path string)) *Tree {
	m.onOpen = fn
	return m
}

// ReadDir implements treearchive.Source.
func (m *Tree) ReadDir(p string) ([]treearchive.Entry, error) {
	if p != "" {
		e, ok := m.entries[p]
		if !ok || e.kind != treearchive.EntryDir {
			return nil, ErrNoEntry
		}
	}
	var names []string
	for name := range m.entries {
		if parentOf(name) == p {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]treearchive.Entry, 0, len(names))
	for _, name := range names {
		out = append(out, treearchive.Entry{Name: path.Base(name), Kind: m.entries[name].kind, Size: m.entries[name].declaredSize()})
	}
	return out, nil
}

func (e entry) declaredSize() int64 {
	switch {
	case e.sizeOverride != 0:
		return e.sizeOverride
	case e.kind == treearchive.EntryFile || e.kind == treearchive.EntryExecutable || e.kind == treearchive.EntrySymlink:
		return int64(len(e.data))
	default:
		return 0
	}
}

func parentOf(p string) string {
	dir := path.Dir(p)
	if dir == "." {
		return ""
	}
	return dir
}

// Open implements treearchive.Source.
func (m *Tree) Open(p string) (io.ReadCloser, error) {
	if m.onOpen != nil {
		m.onOpen(p)
	}
	e, ok := m.entries[p]
	if !ok || e.kind == treearchive.EntryDir || e.kind == treearchive.EntrySubmodule {
		return nil, ErrNoEntry
	}
	return io.NopCloser(strings.NewReader(e.data)), nil
}

// Put stores one entry and implies its parent directories. Storing a path
// again replaces the entry.
func (m *Tree) Put(p string, kind treearchive.EntryKind, data string) *Tree {
	m.entries[p] = entry{kind: kind, data: data}
	for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if _, ok := m.entries[dir]; !ok {
			m.entries[dir] = entry{kind: treearchive.EntryDir}
		}
	}
	return m
}

// File stores a regular file.
func (m *Tree) File(p, data string) *Tree { return m.Put(p, treearchive.EntryFile, data) }

// Exec stores an executable file.
func (m *Tree) Exec(p, data string) *Tree { return m.Put(p, treearchive.EntryExecutable, data) }

// Dir stores an empty directory.
func (m *Tree) Dir(p string) *Tree { return m.Put(p, treearchive.EntryDir, "") }

// Symlink stores a symbolic link whose blob is target.
func (m *Tree) Symlink(p, target string) *Tree { return m.Put(p, treearchive.EntrySymlink, target) }

// Submodule stores a submodule entry.
func (m *Tree) Submodule(p string) *Tree { return m.Put(p, treearchive.EntrySubmodule, "") }

// DeclareSize makes ReadDir declare size for p instead of its blob length.
func (m *Tree) DeclareSize(p string, size int64) *Tree {
	e := m.entries[p]
	e.sizeOverride = size
	m.entries[p] = e
	return m
}
