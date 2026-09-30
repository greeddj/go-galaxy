package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/greeddj/go-galaxy/internal/safeout"
	"github.com/urfave/cli/v3"
)

// Migrate returns the CLI command that writes galaxy.toml beside a
// requirements.yml, carrying its collections and roles and nothing else.
func Migrate() *cli.Command {
	return &cli.Command{
		Name:   "migrate",
		Usage:  "Write a galaxy.toml holding the collections and roles of a requirements.yml",
		Flags:  cliflags.MigrateFlags(),
		Action: runMigrate,
	}
}

// runMigrate reads files only, like hash: no Config, ansible.cfg, cache or
// network. Both paths judge the target before any write, so --dry-run exits
// as a real run would; WriteFileExclusive still refuses a racing file.
func runMigrate(_ context.Context, c *cli.Command) error {
	src, err := config.MigrateSourcePath(c)
	if err != nil {
		return err
	}
	m, err := migrateFile(src)
	if err != nil {
		return err
	}
	for _, notice := range m.Notices {
		progress.Warnf("%s: %s", src, notice)
	}
	target := filepath.Join(filepath.Dir(src), helpers.RequirementsTOMLName)
	if c.Bool("dry-run") {
		if _, err := safeout.NewWriter(os.Stdout).Write(m.TOML); err != nil {
			return err
		}
		return checkMigrateTarget(target)
	}
	if err := checkMigrateTarget(target); err != nil {
		return err
	}
	if err := helpers.WriteFileExclusive(target, m.TOML); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return projectFileExists(target)
		}
		return err
	}
	progress.Okf("Wrote %s (collections: %d, roles: %d)", target, m.Collections, m.Roles)
	progress.PersistentPrintf("%s", migrateNextStep(src, target))
	return nil
}

// migrateFile reads src and renders it as galaxy.toml. A round trip failure
// says nothing was written, since it is a defect and not the input's fault.
func migrateFile(src string) (requirements.Migration, error) {
	data, err := requirements.Read(src)
	if err != nil {
		return requirements.Migration{}, fmt.Errorf("load requirements %s: %w", src, err)
	}
	name, err := migrateProjectName(src)
	if err != nil {
		return requirements.Migration{}, err
	}
	m, err := requirements.MigrateYAML(data, name)
	if errors.Is(err, helpers.ErrMigrateRoundTrip) {
		return requirements.Migration{}, fmt.Errorf("%s: %w; nothing was written", src, err)
	}
	if err != nil {
		return requirements.Migration{}, fmt.Errorf("load requirements %s: %w", src, err)
	}
	return m, nil
}

// migrateProjectName is the name of the directory galaxy.toml lands in, by
// the logical path the shell shows; "" at the file system root.
func migrateProjectName(src string) (string, error) {
	dir, err := filepath.Abs(filepath.Dir(src))
	if err != nil {
		return "", err
	}
	if name := filepath.Base(dir); name != string(filepath.Separator) {
		return name, nil
	}
	return "", nil
}

// checkMigrateTarget is the verdict both paths reach before any write, even
// in a directory migrate cannot write to: anything at target, a dangling
// symlink included, is ErrProjectFileExists.
func checkMigrateTarget(target string) error {
	_, err := os.Lstat(target)
	switch {
	case err == nil:
		return projectFileExists(target)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return err
	}
}

func projectFileExists(target string) error {
	return fmt.Errorf("%w: %s; migrate never replaces it", helpers.ErrProjectFileExists, target)
}

// migrateNextStep names the check to run on the new file, lock --check when a
// regular galaxy.lock is already beside it, and when requirements.yml may go.
func migrateNextStep(src, target string) string {
	check := "go-galaxy install --dry-run -r " + target
	if info, err := os.Stat(lockfile.ResolveDefaultPath(target, "")); err == nil && info.Mode().IsRegular() {
		check = "go-galaxy lock --check -r " + target
	}
	return fmt.Sprintf("Check %s with %s, then delete %s unless another tool still reads it", target, check, src)
}
