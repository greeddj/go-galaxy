package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/urfave/cli/v3"
)

// Hash returns the CLI command that prints a deterministic CI cache key,
// `sha256:<hex>` on one line: the canonical lockfile hash, or the requirements
// file's SHA256 when there is no lockfile.
func Hash() *cli.Command {
	return &cli.Command{
		Name:    "hash",
		Aliases: []string{"h"},
		Usage:   "Print a deterministic cache key for CI (sha256 of lockfile or requirements)",
		Flags:   cliflags.LockInspectFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			req := c.String("requirements-file")
			lockPath := lockfile.ResolveDefaultPath(req, c.String("lock-file"))
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
// requirements file only when the lockfile is missing: one that fails to load is
// an error, since a fallback would hide it behind a plausible key.
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

	//nolint:gosec // requirementsFile is user-provided.
	data, err := os.ReadFile(requirementsFile)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
