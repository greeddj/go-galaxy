package commands

import (
	"fmt"

	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/projectfile"
	"github.com/urfave/cli/v3"
)

// lockfilePath resolves the lockfile hash, tree and explain read: --lock-file
// or its variable when set, else a galaxy.toml's lock_file (one that fails to
// load is an error, for hash too), else galaxy.lock beside the requirements file.
func lockfilePath(c *cli.Command, reqPath string) (string, error) {
	if c.IsSet("lock-file") || !projectfile.IsTOMLPath(reqPath) {
		return lockfile.ResolveDefaultPath(reqPath, c.String("lock-file")), nil
	}
	settings, err := projectfile.LoadSettings(reqPath)
	if err != nil {
		return "", fmt.Errorf("%s: %w", reqPath, err)
	}
	return lockfile.ResolveDefaultPath(reqPath, settings.LockFile), nil
}
