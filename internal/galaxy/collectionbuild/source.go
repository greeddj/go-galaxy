package collectionbuild

import "github.com/greeddj/go-galaxy/internal/galaxy/treearchive"

// The tree vocabulary is treearchive's; these aliases keep code spelled
// against this package working, since a collection build reads the same tree
// a role build does.
type (
	// Source is treearchive.Source: a read-only view of a source tree at one
	// commit.
	Source = treearchive.Source
	// Entry is treearchive.Entry: one directory entry of a source tree.
	Entry = treearchive.Entry
	// EntryKind is treearchive.EntryKind.
	EntryKind = treearchive.EntryKind
)

const (
	// EntryFile is a regular, non-executable file.
	EntryFile = treearchive.EntryFile
	// EntryExecutable is a regular file with the executable bit set.
	EntryExecutable = treearchive.EntryExecutable
	// EntryDir is a directory.
	EntryDir = treearchive.EntryDir
	// EntrySymlink is a symbolic link; Open returns its target as the blob.
	EntrySymlink = treearchive.EntrySymlink
	// EntrySubmodule is a git submodule entry, which the builder skips with a
	// warning: nothing is ever fetched for it.
	EntrySubmodule = treearchive.EntrySubmodule
)
