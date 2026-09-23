package main

import (
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestOfflineWithS3CacheExitsUsage pins, through the root command main runs,
// that --offline with an S3 cache exits ExitUsage on every command mounting
// both, a dry run included. Not parallel: t.Setenv.
func TestOfflineWithS3CacheExitsUsage(t *testing.T) {
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "absent.cfg"))
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cache := t.TempDir()
	// No row dials the endpoint: the config refuses the pair, and past it the
	// offline client would refuse every request before dialing.
	s3 := []string{
		"--offline", "--quiet", "--cache-dir", cache, "--s3-bucket", "b",
		"--s3-endpoint", "http://127.0.0.1:1", "--s3-access-key", "a", "--s3-secret-key", "s",
	}
	rows := map[string][]string{
		"install":           {"install"},
		"install --dry-run": {"install", "--dry-run"},
		"warm":              {"warm"},
		"lock":              {"lock"},
		"outdated":          {"outdated"},
	}
	for name, cmd := range rows {
		assertExitsUsageWith(t, name, runRootCommand(t, append(cmd, s3...)), helpers.ErrS3CacheOffline)
	}
}
