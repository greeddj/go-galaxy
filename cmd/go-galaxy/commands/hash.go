package commands

import (
	"context"
	"fmt"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/urfave/cli/v3"
)

// Hash returns the CLI command that prints a deterministic CI cache key,
// `sha256:<hex>` on one line: the canonical lockfile hash, or the digest of
// what the requirements file asks for when there is no lockfile.
func Hash() *cli.Command {
	return &cli.Command{
		Name:    "hash",
		Aliases: []string{"h"},
		Usage:   "Print a deterministic cache key for CI (sha256 of lockfile or requirements)",
		Flags:   cliflags.LockInspectFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			req, warning, err := config.RequirementsPath(c)
			if err != nil {
				return err
			}
			if warning != "" {
				progress.Warnf("%s", warning)
			}
			lockPath, err := lockfilePath(c, req)
			if err != nil {
				return err
			}
			key, err := computeHash(req, lockPath)
			if err != nil {
				return err
			}
			fmt.Println(key) //nolint:forbidigo // hash is the command's only output.
			return nil
		},
	}
}

// computeHash prefers the lockfile's canonical hash and falls back to the
// requirements digest only when the lockfile is missing: one that fails to load
// is an error, since a fallback would hide it behind a plausible key.
func computeHash(requirementsFile, lockPath string) (string, error) {
	lf, err := lockfile.Load(lockPath)
	switch {
	case err == nil:
		hash, hashErr := lf.Hash()
		if hashErr != nil {
			return "", hashErr
		}
		return "sha256:" + hash, nil
	case lockfile.IsNotExist(err):
		// No lockfile: fall through to hashing the requirements file below.
	default:
		return "", err
	}

	// Parsed with no default server, as tree parses it: the key covers what the
	// file asks for, never a setting, so a file no command can load has no key.
	file, err := requirements.Load(requirementsFile, "")
	if err != nil {
		return "", fmt.Errorf("load requirements %s: %w", requirementsFile, err)
	}
	return "sha256:" + file.Hash(), nil
}
