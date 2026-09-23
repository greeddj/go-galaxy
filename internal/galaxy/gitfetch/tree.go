package gitfetch

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/greeddj/go-galaxy/internal/galaxy/collectionbuild"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// gitDirNames returns the entry names refused at every depth regardless of
// case: the first is git's own metadata directory, the second its Windows
// 8.3 short name, which a checkout on such a filesystem would resolve to it.
func gitDirNames() [2]string { return [2]string{".git", "git~1"} }

// treeSource is collectionbuild.Source over one fetched commit. Trees decode
// through object.GetTree, whose error propagates (go-git's TreeWalker ends
// silently on a missing tree), and entry names are validated on decode.
type treeSource struct {
	storer storer.EncodedObjectStorer
	root   *object.Tree
	trees  map[string]*object.Tree
	when   time.Time
	mu     sync.Mutex
}

func newTreeSource(st storer.EncodedObjectStorer, commit *object.Commit) (*treeSource, error) {
	root, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("%w: reading the commit tree: %w", helpers.ErrGitCommitMismatch, err)
	}
	if err := validateEntries(root.Entries); err != nil {
		return nil, err
	}
	return &treeSource{
		storer: st,
		root:   root,
		trees:  map[string]*object.Tree{"": root},
		when:   commit.Committer.When.UTC().Truncate(time.Second),
	}, nil
}

// CommitTime is the committer time, in UTC and whole seconds.
func (s *treeSource) CommitTime() time.Time { return s.when }

// ReadDir lists the directory at path in git's own entry order.
func (s *treeSource) ReadDir(path string) ([]collectionbuild.Entry, error) {
	tree, err := s.tree(path)
	if err != nil {
		return nil, err
	}
	entries := make([]collectionbuild.Entry, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		kind, err := entryKind(e.Mode)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", err, joinPath(path, e.Name))
		}
		entry := collectionbuild.Entry{Name: e.Name, Kind: kind}
		if kind == collectionbuild.EntryFile || kind == collectionbuild.EntryExecutable || kind == collectionbuild.EntrySymlink {
			size, err := s.storer.EncodedObjectSize(e.Hash)
			if err != nil {
				return nil, fmt.Errorf("%w: sizing %s: %w", helpers.ErrGitCommitMismatch, joinPath(path, e.Name), err)
			}
			entry.Size = size
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// Open streams the file or symlink blob at path under the per-entry archive
// cap, checked on the declared size before any byte is read; an oversized blob
// is reported as helpers.ErrArchiveEntryIsTooLarge, as the archive would.
func (s *treeSource) Open(path string) (io.ReadCloser, error) {
	dir, name := splitPath(path)
	tree, err := s.tree(dir)
	if err != nil {
		return nil, err
	}
	entry, ok := findEntry(tree, name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", helpers.ErrGitCollectionNotFound, path)
	}
	kind, err := entryKind(entry.Mode)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, path)
	}
	if kind == collectionbuild.EntryDir || kind == collectionbuild.EntrySubmodule {
		return nil, fmt.Errorf("%w: %s is not a file", helpers.ErrGitTreeEntryInvalid, path)
	}
	blob, err := object.GetBlob(s.storer, entry.Hash)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", helpers.ErrGitCommitMismatch, path, err)
	}
	if blob.Size > helpers.ArchiveMaxEntrySize {
		return nil, fmt.Errorf("%w: %s is %d bytes", helpers.ErrArchiveEntryIsTooLarge, path, blob.Size)
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("%w: opening %s: %w", helpers.ErrGitCommitMismatch, path, err)
	}
	return &cappedBlob{
		Reader: helpers.NewSizeLimitedReader(r, helpers.ArchiveMaxEntrySize),
		closer: r,
		path:   path,
	}, nil
}

// cappedBlob is a blob reader whose size-limit refusal is reported as the
// archive's own per-entry sentinel.
type cappedBlob struct {
	io.Reader

	closer io.Closer
	path   string
}

func (c *cappedBlob) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	if err != nil && errors.Is(err, helpers.ErrResponseTooLarge) {
		return n, fmt.Errorf("%w: %s", helpers.ErrArchiveEntryIsTooLarge, c.path)
	}
	return n, err
}

func (c *cappedBlob) Close() error { return c.closer.Close() }

// tree returns the decoded tree at path, walking component by component from
// the root and caching every tree it decodes. A component that is not a
// directory, or a path deeper than helpers.GitTreeMaxDepth, is refused.
func (s *treeSource) tree(path string) (*object.Tree, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.trees[path]; ok {
		return t, nil
	}
	components := strings.Split(path, "/")
	if len(components) > helpers.GitTreeMaxDepth {
		return nil, fmt.Errorf("%w: %s", helpers.ErrGitTreeTooDeep, path)
	}
	current := s.root
	walked := ""
	for _, component := range components {
		walked = joinPath(walked, component)
		if t, ok := s.trees[walked]; ok {
			current = t
			continue
		}
		entry, ok := findEntry(current, component)
		if !ok || entry.Mode != filemode.Dir {
			return nil, fmt.Errorf("%w: %s is not a directory of the repository", helpers.ErrGitCollectionNotFound, walked)
		}
		sub, err := object.GetTree(s.storer, entry.Hash)
		if err != nil {
			return nil, fmt.Errorf("%w: reading tree %s: %w", helpers.ErrGitCommitMismatch, walked, err)
		}
		if err := validateEntries(sub.Entries); err != nil {
			return nil, fmt.Errorf("%w (under %s)", err, walked)
		}
		s.trees[walked] = sub
		current = sub
	}
	return current, nil
}

func findEntry(tree *object.Tree, name string) (object.TreeEntry, bool) {
	for _, e := range tree.Entries {
		if e.Name == name {
			return e, true
		}
	}
	return object.TreeEntry{}, false
}

// validateEntries refuses an unsafe path element, a backslash, a git metadata
// name in any case, a malformed mode, or two names equal once case-folded,
// since the install destination may be a case-insensitive filesystem.
func validateEntries(entries []object.TreeEntry) error {
	seen := make(map[string]string, len(entries))
	for _, e := range entries {
		if !helpers.IsPathElement(e.Name) || strings.ContainsRune(e.Name, '\\') {
			return fmt.Errorf("%w: name %q", helpers.ErrGitTreeEntryInvalid, e.Name)
		}
		for _, gitName := range gitDirNames() {
			if strings.EqualFold(e.Name, gitName) {
				return fmt.Errorf("%w: name %q", helpers.ErrGitTreeEntryInvalid, e.Name)
			}
		}
		if _, err := entryKind(e.Mode); err != nil {
			return fmt.Errorf("%w: name %q", err, e.Name)
		}
		folded := strings.ToLower(e.Name)
		if other, dup := seen[folded]; dup {
			return fmt.Errorf("%w: %q and %q", helpers.ErrGitTreeDuplicateEntry, other, e.Name)
		}
		seen[folded] = e.Name
	}
	return nil
}

func entryKind(mode filemode.FileMode) (collectionbuild.EntryKind, error) {
	switch mode {
	case filemode.Regular, filemode.Deprecated:
		return collectionbuild.EntryFile, nil
	case filemode.Executable:
		return collectionbuild.EntryExecutable, nil
	case filemode.Dir:
		return collectionbuild.EntryDir, nil
	case filemode.Symlink:
		return collectionbuild.EntrySymlink, nil
	case filemode.Submodule:
		return collectionbuild.EntrySubmodule, nil
	case filemode.Empty:
		return 0, fmt.Errorf("%w: empty mode", helpers.ErrGitTreeEntryInvalid)
	default:
		return 0, fmt.Errorf("%w: mode %s", helpers.ErrGitTreeEntryInvalid, mode)
	}
}

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// splitPath splits path at its last slash into directory and entry name.
func splitPath(path string) (string, string) {
	if dir, name, ok := strings.CutLast(path, "/"); ok {
		return dir, name
	}
	return "", path
}
