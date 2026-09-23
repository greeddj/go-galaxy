package tartree

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/treearchive"
	"github.com/greeddj/go-galaxy/internal/safeout"
)

// extractDirPattern names the private directory one Load extracts into,
// beside gitfetch's "go-galaxy-git-*" object stores under the run's temp dir.
const extractDirPattern = "go-galaxy-role-tar-*"

// The two meta spellings identify a directory as a role root, tried in the
// order ansible tries them. Only a regular file counts; rolebuild refuses a
// tree carrying both on its own.
const (
	metaMainYML  = "meta/main.yml"
	metaMainYAML = "meta/main.yaml"
)

// Tree is the extracted tarball served as a treearchive.Source, rooted at the
// role root Load detected. Cleanup removes the extraction directory and is
// idempotent.
type Tree struct {
	root    *os.Root
	dir     string
	skipped string
}

// Load extracts the tar.gz at tarPath into a private directory under tempDir()
// and returns the tree rooted at the single role root. On any error the
// extraction directory is already gone.
func Load(ctx context.Context, tarPath string, tempDir func() string) (*Tree, error) {
	dir, err := os.MkdirTemp(tempDir(), extractDirPattern)
	if err != nil {
		return nil, fmt.Errorf("creating role tarball extraction directory: %w", err)
	}
	tree, err := load(ctx, tarPath, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return tree, nil
}

func load(ctx context.Context, tarPath, dir string) (*Tree, error) {
	if err := archive.ExtractTarGz(ctx, tarPath, dir); err != nil {
		return nil, err
	}
	outer, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("opening extracted role tarball: %w", err)
	}
	root, skipped, err := detectRoleRoot(outer)
	if err != nil {
		_ = outer.Close()
		return nil, err
	}
	return &Tree{root: root, dir: dir, skipped: skipped}, nil
}

// detectRoleRoot returns the role's os.Root and the top-level directory it
// stripped ("" at the archive root). The archive root wins when it carries a
// meta file, the shortest parent as in ansible's scan.
func detectRoleRoot(outer *os.Root) (*os.Root, string, error) {
	if hasRoleMeta(outer, ".") {
		return outer, "", nil
	}
	entries, err := readDirSorted(outer, ".")
	if err != nil {
		return nil, "", err
	}
	var carriers []string
	for _, entry := range entries {
		if entry.IsDir() && hasRoleMeta(outer, entry.Name()) {
			carriers = append(carriers, entry.Name())
		}
	}
	if len(carriers) != 1 {
		return nil, "", fmt.Errorf("%w: meta/main.yml sits at the root of %d top-level directories and not at the archive root",
			helpers.ErrRoleTarballLayout, len(carriers))
	}
	inner, err := outer.OpenRoot(carriers[0])
	if err != nil {
		return nil, "", fmt.Errorf("opening role root %q: %w", safeout.Clean(carriers[0]), err)
	}
	if err := outer.Close(); err != nil {
		_ = inner.Close()
		return nil, "", fmt.Errorf("closing extracted role tarball root: %w", err)
	}
	return inner, carriers[0], nil
}

// hasRoleMeta reports whether dir carries a meta file as a regular file,
// judged with Lstat so a symlink does not count - the same reading rolebuild
// gives the tree it is then handed.
func hasRoleMeta(root *os.Root, dir string) bool {
	for _, name := range []string{metaMainYML, metaMainYAML} {
		info, err := root.Lstat(path.Join(dir, name))
		if err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// SkippedPrefix is the top-level directory Load stripped to reach the role
// root, "" when the role sat at the archive root. Callers name it in the
// warning that tells the operator what the artifact really held.
func (t *Tree) SkippedPrefix() string { return t.skipped }

// Cleanup removes the extraction directory. It is idempotent and safe to call
// beside an open Tree; nothing reads the tree after the build that consumes
// it.
func (t *Tree) Cleanup() {
	_ = t.root.Close()
	_ = os.RemoveAll(t.dir)
}

// CommitTime is the Unix epoch: a url tarball names no commit, and the fixed
// stamp keeps one origin artifact repacking to one byte sequence under one
// toolchain.
func (t *Tree) CommitTime() time.Time { return time.Unix(0, 0).UTC() }

// ReadDir lists the entries of a directory in byte order, each name validated
// on the way out. Path "" is the role root, as treearchive.Source specifies.
func (t *Tree) ReadDir(dir string) ([]treearchive.Entry, error) {
	entries, err := readDirSorted(t.root, dirOrDot(dir))
	if err != nil {
		return nil, err
	}
	out := make([]treearchive.Entry, 0, len(entries))
	for _, entry := range entries {
		mapped, err := t.mapEntry(dir, entry)
		if err != nil {
			return nil, err
		}
		out = append(out, mapped)
	}
	return out, nil
}

// Open streams a file's bytes or a symlink's target string. A directory is
// never opened: treearchive walks directories through ReadDir alone.
func (t *Tree) Open(p string) (io.ReadCloser, error) {
	info, err := t.root.Lstat(p)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := t.root.Readlink(p)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(strings.NewReader(target)), nil
	}
	return t.root.Open(p)
}

func (t *Tree) mapEntry(dir string, entry os.DirEntry) (treearchive.Entry, error) {
	name := entry.Name()
	if err := checkEntryName(name); err != nil {
		return treearchive.Entry{}, err
	}
	full := path.Join(dirOrDot(dir), name)
	info, err := t.root.Lstat(full)
	if err != nil {
		return treearchive.Entry{}, err
	}
	mode := info.Mode()
	switch {
	case mode.IsDir():
		return treearchive.Entry{Name: name, Kind: treearchive.EntryDir}, nil
	case mode&os.ModeSymlink != 0:
		target, err := t.root.Readlink(full)
		if err != nil {
			return treearchive.Entry{}, err
		}
		return treearchive.Entry{Name: name, Kind: treearchive.EntrySymlink, Size: int64(len(target))}, nil
	case mode.IsRegular() && mode&0o111 != 0:
		return treearchive.Entry{Name: name, Kind: treearchive.EntryExecutable, Size: info.Size()}, nil
	case mode.IsRegular():
		return treearchive.Entry{Name: name, Kind: treearchive.EntryFile, Size: info.Size()}, nil
	default:
		return treearchive.Entry{}, fmt.Errorf("%w: %q is neither a file, a directory nor a symlink",
			helpers.ErrRoleTarballEntryInvalid, safeout.Clean(name))
	}
}

// checkEntryName refuses a name this tool's extractor would refuse on the way
// back out of the repacked artifact: a control rune or a backslash. Slash, NUL,
// "." and ".." cannot reach here from a filesystem listing.
func checkEntryName(name string) error {
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return fmt.Errorf("%w: name carries a control rune or a backslash: %q",
				helpers.ErrRoleTarballEntryInvalid, safeout.Clean(name))
		}
	}
	return nil
}

func dirOrDot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

func readDirSorted(root *os.Root, dir string) ([]os.DirEntry, error) {
	f, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}
