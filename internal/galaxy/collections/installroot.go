package collections

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// collectionsDirName is the directory ansible looks for collections under, the
// only entry of the collections path this tool writes or reads; one constant
// so a writer and a reader cannot spell it differently.
const collectionsDirName = "ansible_collections"

// errEmptyDownloadPath names the misconfiguration directly rather than
// letting os.OpenRoot("") surface as a bare ENOENT, which tells an operator
// nothing about which setting to fix.
var errEmptyDownloadPath = errors.New("--download-path (or [defaults] collections_path) is empty")

// errCollectionsTreeNotUsable is the cause dry-run probes hand
// classifyCollectionsRootError in place of a kernel error, since a preview
// never runs the MkdirAll or RemoveAll that would produce one.
var errCollectionsTreeNotUsable = errors.New("not usable as a collections directory")

// installTarget is one collection's or role's location under root. rel, info
// and marker are slash-separated (path.Join) for os.Root and root.FS; path is
// the OS-native install directory, only for the unrooted untar and log lines.
type installTarget struct {
	root       *os.Root
	rel        string
	path       string
	info       string
	marker     string
	infoPrefix string
}

// newInstallTarget builds col's installTarget, reporting false for a nil root
// (warm) or an unsafe identity. Namespace, name and version are each checked
// before any join, as removeInstalled does, since a ".." version escapes.
func newInstallTarget(root *os.Root, cfg *config.Config, col collection) (installTarget, bool) {
	if root == nil {
		return installTarget{}, false
	}
	if !helpers.IsPathElement(col.Namespace) || !helpers.IsPathElement(col.Name) || !helpers.IsPathElement(col.Version) {
		return installTarget{}, false
	}
	rel := path.Join(collectionsDirName, col.Namespace, col.Name)
	infoPrefix := col.Namespace + "." + col.Name + "-"
	info := path.Join(collectionsDirName, infoPrefix+col.Version+infoDirSuffix)
	return installTarget{
		root:       root,
		rel:        rel,
		path:       filepath.Join(cfg.DownloadPath, rel),
		info:       info,
		marker:     info,
		infoPrefix: infoPrefix,
	}, true
}

// openCollectionsRoot opens the os.Root every install write goes through at
// downloadPath: OpenRoot follows a symlink at the path it opens, so rooting at
// ansible_collections would adopt a planted one. Without create, nothing is made.
func openCollectionsRoot(downloadPath string, create bool) (*os.Root, error) {
	if strings.TrimSpace(downloadPath) == "" {
		return nil, errEmptyDownloadPath
	}

	if !create {
		root, err := os.OpenRoot(downloadPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil //nolint:nilnil // absent DownloadPath on a dry run is "nothing to describe yet", not a failure.
			}
			return nil, err
		}
		if err := probeAnsibleCollectionsUsable(root); err != nil {
			_ = root.Close()
			return nil, err
		}
		return root, nil
	}

	if err := os.MkdirAll(downloadPath, helpers.DirMod); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(downloadPath)
	if err != nil {
		return nil, err
	}
	if err := root.MkdirAll(collectionsDirName, helpers.DirMod); err != nil {
		return nil, classifyCollectionsRootError(root, collectionsDirName, err)
	}
	return root, nil
}

// probeAnsibleCollectionsUsable predicts, without writing, whether a real run's
// MkdirAll of ansible_collections succeeds: Stat must find a directory or Lstat
// nothing at all, so a dangling symlink (absent to Stat) is not passed as ok.
func probeAnsibleCollectionsUsable(root *os.Root) error {
	if info, err := root.Stat("ansible_collections"); err == nil && info.IsDir() {
		return nil
	}
	if _, err := root.Lstat("ansible_collections"); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return classifyCollectionsRootError(root, "ansible_collections", errCollectionsTreeNotUsable)
}

// classifyCollectionsRootError wraps err in helpers.ErrCollectionsPathEscape
// at rel's first symlink component or Lstat failure other than not-exist or
// ENOTDIR. os.Root has already refused the write; this only picks the class.
func classifyCollectionsRootError(root *os.Root, rel string, err error) error {
	var walked string
	for component := range strings.SplitSeq(rel, "/") {
		if walked == "" {
			walked = component
		} else {
			walked = walked + "/" + component
		}
		info, statErr := root.Lstat(walked)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) || errors.Is(statErr, syscall.ENOTDIR) {
				continue
			}
			return fmt.Errorf("%w: %q: point --download-path/collections_path at the real directory instead of a symlink: %w",
				helpers.ErrCollectionsPathEscape, walked, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %q: point --download-path/collections_path at the real directory instead of a symlink: %w",
				helpers.ErrCollectionsPathEscape, walked, err)
		}
	}
	return err
}
