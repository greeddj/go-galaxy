package config

import (
	"os"
	"path/filepath"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// ansibleConfigDir is the directory a relative path read from the ansible.cfg
// at cfgPath resolves under: the file's own, made absolute by
// helpers.PhysicalAbs, as ansible's unfrackpath(follow=False) does; "" for none.
func ansibleConfigDir(cfgPath string) string {
	if cfgPath == "" {
		return ""
	}
	dir := filepath.Dir(cfgPath)
	if abs, err := helpers.PhysicalAbs(dir); err == nil {
		return abs
	}
	return dir
}

// resolveAnsibleConfigPath resolves a path read from ansible.cfg: expanded,
// then cleaned or joined under cfgDir, as unfrackpath normalizes it. An empty
// result stays empty, so the refusal an empty install or cache path meets fires.
func resolveAnsibleConfigPath(value, cfgDir string) string {
	path := helpers.ExpandAnsiblePath(value)
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(cfgDir, path)
}

// resolveAnsibleConfigEnv resolves $ANSIBLE_CONFIG, already expanded, as
// ansible's find_ini_config_file does: made absolute by helpers.PhysicalAbs,
// and a directory standing for the ansible.cfg inside it.
func resolveAnsibleConfigEnv(expanded string) string {
	path, err := helpers.PhysicalAbs(expanded)
	if err != nil {
		path = expanded
	}
	if isDir(path) {
		return filepath.Join(path, ansibleCfgName)
	}
	return path
}

// canonicalPath is path made absolute by helpers.PhysicalAbs with its longest
// existing prefix's symlinks resolved, so two spellings of one directory match
// before it exists; path only cleaned when the working directory is unread.
func canonicalPath(path string) string {
	abs, err := helpers.PhysicalAbs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	tail := ""
	for dir := abs; ; {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, tail)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		tail = filepath.Join(filepath.Base(dir), tail)
		dir = parent
	}
}

// isDir reports whether path is a directory, following a symlink as Python's
// os.path.isdir does; a failed Stat reads as not a directory.
func isDir(path string) bool {
	// #nosec G703 -- path is $ANSIBLE_CONFIG, the operator's own setting; this
	// is a read-only mode check that never opens or writes anything.
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
