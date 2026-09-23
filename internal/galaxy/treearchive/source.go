package treearchive

import (
	"io"
	"time"
)

// EntryKind classifies one entry of a source tree.
type EntryKind uint8

const (
	// EntryFile is a regular, non-executable file.
	EntryFile EntryKind = iota + 1
	// EntryExecutable is a regular file with the executable bit set.
	EntryExecutable
	// EntryDir is a directory.
	EntryDir
	// EntrySymlink is a symbolic link; Open returns its target as the blob.
	EntrySymlink
	// EntrySubmodule is a git submodule entry, which the builder skips with a
	// warning: nothing is ever fetched for it.
	EntrySubmodule
)

// Entry is one directory entry of a source tree. Name is a single, already
// validated path element; Size is the blob size for a file, executable or
// symlink and zero otherwise.
type Entry struct {
	Name string
	Kind EntryKind
	Size int64
}

// Source is a read-only tree at one commit, paths "/"-joined from the root "".
// It validates every name ReadDir returns and caps Open at the per-entry size;
// CommitTime stamps every entry, so one toolchain rebuilds identical bytes.
type Source interface {
	ReadDir(path string) ([]Entry, error)
	Open(path string) (io.ReadCloser, error)
	CommitTime() time.Time
}
