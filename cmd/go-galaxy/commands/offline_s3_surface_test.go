package commands

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitfetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
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

// TestOfflineWiresTheRefusingGitClient pins, over each command mounting
// --offline, that runCollectionCommand hands the action gitsource.Offline,
// and the real fetcher without the flag. Not parallel: t.Setenv.
func TestOfflineWiresTheRefusingGitClient(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	for _, newCmd := range []func() *cli.Command{Install, Warm, Lock, Outdated} {
		name := newCmd().Name
		if _, ok := gitClientWiredFor(t, newCmd(), "--offline").(gitsource.Offline); !ok {
			t.Errorf("%s --offline: runtime.Git is not gitsource.Offline", name)
		}
		if _, ok := gitClientWiredFor(t, newCmd()).(*gitfetch.Fetcher); !ok {
			t.Errorf("%s: runtime.Git is not the gitfetch fetcher", name)
		}
	}
}

// TestOfflineGitClientRefusesEveryCall pins that each gitsource.Offline method
// refuses with ErrOfflineMode naming the repository, a commit ref included,
// which the real fetcher would answer without a round trip.
func TestOfflineGitClientRefusesEveryCall(t *testing.T) {
	t.Parallel()
	const repo = "git@git.example:acme/mono.git"
	u, err := gitsource.ParseURL(repo)
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	commit, err := gitsource.ParseRef(strings.Repeat("a", 40))
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	var client gitsource.Client = gitsource.Offline{}
	ctx := context.Background()
	_, _, advErr := client.Advertise(ctx, u, commit, gitsource.Credential{})
	_, acqErr := client.Acquire(ctx, gitsource.Request{URL: u, Ref: commit})
	_, roleErr := client.AcquireRole(ctx, gitsource.RoleRequest{URL: u, Ref: commit})
	for call, err := range map[string]error{"Advertise": advErr, "Acquire": acqErr, "AcquireRole": roleErr} {
		if !errors.Is(err, galaxyhelpers.ErrOfflineMode) || !strings.Contains(err.Error(), repo) {
			t.Errorf("%s: error = %v, want ErrOfflineMode naming %s", call, err, repo)
		}
	}
}

// gitClientWiredFor runs cmd through the root flag set with args and a cache
// dir under the test's temp dir, returning the git client its action received.
func gitClientWiredFor(t *testing.T, cmd *cli.Command, args ...string) gitsource.Client {
	t.Helper()
	var got gitsource.Client
	cmd.Action = func(ctx context.Context, c *cli.Command) error {
		return runCollectionCommand(ctx, c, func(_ context.Context, _ *config.Config, runtime *infra.Infra) error {
			got = runtime.Git
			return nil
		})
	}
	app := &cli.Command{Name: "go-galaxy", Flags: cliflags.CommonFlags(), Commands: []*cli.Command{cmd}}
	fullArgs := append([]string{"go-galaxy", cmd.Name, "--cache-dir=" + t.TempDir()}, args...)
	if err := app.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("%s: app.Run() error = %v, want nil", cmd.Name, err)
	}
	return got
}
