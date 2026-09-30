package helpers

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to a temp file in path's directory and renames
// it onto path, so a reader never sees a truncated file and a symlink at path
// is replaced, not followed. The mode is FileMod despite umask, so not private.
func WriteFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, DirMod); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if err = writeSyncedFile(tmp, data); err != nil {
		return err
	}
	err = os.Rename(tmpName, path)
	return err
}

// WriteFileExclusive writes data to path only when nothing stands there: a
// synced temp beside it is hard-linked into place, so the file appears whole,
// and anything at path, a dangling symlink included, is an fs.ErrExist.
func WriteFileExclusive(path string, data []byte) error {
	return writeFileExclusive(path, data, os.Link)
}

// writeFileExclusive is WriteFileExclusive with the link step injected. A
// link refused for any reason but existence falls back to an O_EXCL create,
// as on a file system without hard links; the temp is removed either way.
func writeFileExclusive(path string, data []byte, link func(oldname, newname string) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := writeSyncedFile(tmp, data); err != nil {
		return err
	}
	err = link(tmpName, path)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		return existsError(path)
	default:
		return createExclusive(path, data)
	}
}

// createExclusive creates path with O_EXCL, which refuses anything already
// there, a symlink included, and removes what it created when a write fails.
func createExclusive(path string, data []byte) error {
	//nolint:gosec // G304: path is the operator's own target, beside the file it named.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMod)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return existsError(path)
		}
		return err
	}
	if err := writeSyncedFile(f, data); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// writeSyncedFile writes data, forces FileMod past the umask, syncs and
// closes; a failed step before the close still closes the file.
func writeSyncedFile(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(FileMod); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// existsError names the target, never the temp a failed link reports.
func existsError(path string) error {
	return &fs.PathError{Op: "create", Path: path, Err: fs.ErrExist}
}
