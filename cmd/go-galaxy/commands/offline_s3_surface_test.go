package commands

import (
	"errors"
	"testing"

	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// s3CacheArgs enables the S3 cache with both keys, so the credential check
// passes and only the pairing with --offline is judged.
func s3CacheArgs() []string {
	return []string{"--s3-bucket=b", "--s3-access-key=k", "--s3-secret-key=s"}
}

// TestOfflineWithS3CacheIsRefused pins, over each command's own flag set, that
// --offline or GO_GALAXY_OFFLINE with an S3 cache fails the config on every
// command mounting both. Not parallel: t.Setenv.
func TestOfflineWithS3CacheIsRefused(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	for _, cmd := range []*cli.Command{Install(), Warm(), Lock(), Outdated()} {
		_, err := buildConfigFor(t, cmd.Name, cmd.Flags, append([]string{"--offline"}, s3CacheArgs()...))
		if !errors.Is(err, galaxyhelpers.ErrS3CacheOffline) {
			t.Errorf("%s: BuildCollectionConfig() error = %v, want errors.Is ErrS3CacheOffline", cmd.Name, err)
		}
	}

	t.Run("the variable is refused like the flag", func(t *testing.T) {
		t.Setenv("GO_GALAXY_OFFLINE", "true")
		t.Setenv("GO_GALAXY_S3_BUCKET", "b")
		_, err := buildConfigFor(t, "install", Install().Flags, []string{"--s3-access-key=k", "--s3-secret-key=s"})
		if !errors.Is(err, galaxyhelpers.ErrS3CacheOffline) {
			t.Fatalf("BuildCollectionConfig() error = %v, want errors.Is ErrS3CacheOffline", err)
		}
	})

	t.Run("missing S3 keys are reported first", func(t *testing.T) {
		_, err := buildConfigFor(t, "install", Install().Flags, []string{"--offline", "--s3-bucket=b"})
		if !errors.Is(err, galaxyhelpers.ErrS3EmptyCreds) {
			t.Fatalf("BuildCollectionConfig() error = %v, want errors.Is ErrS3EmptyCreds", err)
		}
	})
}

// TestOfflineAloneOrS3AloneIsAccepted pins that each half of the refused pair
// builds on its own, and that cleanup, which mounts no --offline, ignores
// GO_GALAXY_OFFLINE beside an S3 cache. Not parallel: t.Setenv.
func TestOfflineAloneOrS3AloneIsAccepted(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	rows := map[string][]string{"--offline alone": {"--offline"}, "S3 cache alone": s3CacheArgs()}
	for name, args := range rows {
		if _, err := buildConfigFor(t, "install", Install().Flags, args); err != nil {
			t.Errorf("install, %s: BuildCollectionConfig() error = %v, want nil", name, err)
		}
	}

	t.Setenv("GO_GALAXY_OFFLINE", "true")
	cfg, err := buildConfigFor(t, "cleanup", Cleanup().Flags, s3CacheArgs())
	if err != nil {
		t.Fatalf("cleanup: BuildCollectionConfig() error = %v, want nil", err)
	}
	assertConfigField(t, "Offline", cfg.Offline, false)
	assertConfigField(t, "S3Cache.Enabled", cfg.S3Cache.Enabled, true)
}
