package commands

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// signatureSurfaceFlags is the flag set a verifying command really mounts,
// taken from Install() rather than reassembled here, since a rebuilt union
// would pass while the shipped command mounted something else.
func signatureSurfaceFlags() []cli.Flag {
	return Install().Flags
}

// TestSignatureFlagsRoundTrip pins that every signature flag value reaches its
// Config field through the real flag declarations, which internal/galaxy/config
// cannot import.
func TestSignatureFlagsRoundTrip(t *testing.T) {
	neutralizeAnsibleDiscovery(t)

	args := []string{
		"--keyring=/keys.gpg",
		"--required-valid-signature-count=+2",
		"--ignore-signature-status-code=BADSIG",
		"--ignore-signature-status-code=NO_PUBKEY",
		"--disable-gpg-verify",
	}
	cfg, err := buildConfigFor(t, "install", signatureSurfaceFlags(), args)
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}

	assertConfigField(t, "Signature.KeyringPath", cfg.Signature.KeyringPath, "/keys.gpg")
	assertConfigField(t, "Signature.RequiredCount", cfg.Signature.RequiredCount, "+2")
	assertConfigField(t, "Signature.DisableGPGVerify", cfg.Signature.DisableGPGVerify, true)
	if got := cfg.Signature.IgnoreStatusCodes; !slices.Equal(got, []string{"BADSIG", "NO_PUBKEY"}) {
		t.Fatalf("Signature.IgnoreStatusCodes = %v, want [BADSIG NO_PUBKEY]", got)
	}
}

// TestKeyringEnvNames pins the keyring flag's env spellings in order:
// GO_GALAXY_KEYRING is read, and it outranks ANSIBLE_GALAXY_GPG_KEYRING.
func TestKeyringEnvNames(t *testing.T) {
	t.Run("the go-galaxy spelling is read", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_KEYRING", "/from-go-galaxy.gpg")

		cfg, err := buildConfigFor(t, "install", signatureSurfaceFlags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		assertConfigField(t, "Signature.KeyringPath", cfg.Signature.KeyringPath, "/from-go-galaxy.gpg")
	})

	t.Run("it outranks the ansible spelling", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_KEYRING", "/from-go-galaxy.gpg")
		t.Setenv("ANSIBLE_GALAXY_GPG_KEYRING", "/from-ansible.gpg")

		cfg, err := buildConfigFor(t, "install", signatureSurfaceFlags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		assertConfigField(t, "Signature.KeyringPath", cfg.Signature.KeyringPath, "/from-go-galaxy.gpg")
	})
}

// TestIgnoreStatusCodesEnvSplit pins that urfave/cli splits an env list on ","
// without trimming, which is why signature.ParseStatusCodes trims each element;
// the value carries a space so the untrimmed element is observable.
func TestIgnoreStatusCodesEnvSplit(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	t.Setenv("ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES", "BADSIG, NO_PUBKEY")

	cfg, err := buildConfigFor(t, "install", signatureSurfaceFlags(), nil)
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	if got := cfg.Signature.IgnoreStatusCodes; !slices.Equal(got, []string{"BADSIG", " NO_PUBKEY"}) {
		t.Fatalf("Signature.IgnoreStatusCodes = %q, want [\"BADSIG\" \" NO_PUBKEY\"]", got)
	}
}

// runCommandWith runs the named command carrying flags and returns whatever
// app.Run produced, without failing the test on an error - which is what
// separates it from buildConfigFor, whose contract is that the parse succeeds.
func runCommandWith(t *testing.T, name string, flags []cli.Flag, args []string) error {
	t.Helper()

	app := &cli.Command{
		Name:  "go-galaxy",
		Flags: cliflags.CommonFlags(),
		Commands: []*cli.Command{
			{
				Name:   name,
				Flags:  flags,
				Action: func(_ context.Context, _ *cli.Command) error { return nil },
			},
		},
	}

	return app.Run(context.Background(), append([]string{"go-galaxy", name}, args...))
}

// TestSignatureFlagsAreNotPartOfCollectionFlags pins that CollectionFlags alone
// refuses --keyring as undefined, while install's real flag set accepts the
// same command line as the positive control.
func TestSignatureFlagsAreNotPartOfCollectionFlags(t *testing.T) {
	neutralizeAnsibleDiscovery(t)

	t.Run("collection flags alone refuse it", func(t *testing.T) {
		err := runCommandWith(t, "install", cliflags.CollectionFlags(), []string{"--keyring=/keys.gpg"})
		if err == nil {
			t.Fatal("app.Run() error = nil, want an unknown-flag refusal")
		}
		if !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("app.Run() error = %v, want an unknown-flag refusal", err)
		}
	})

	t.Run("adding the signature flags accepts it", func(t *testing.T) {
		if err := runCommandWith(t, "install", signatureSurfaceFlags(), []string{"--keyring=/keys.gpg"}); err != nil {
			t.Fatalf("app.Run() error = %v, want nil", err)
		}
	})
}

// TestVerifyingCommandsMountTheSignatureFlags pins that install and warm, the
// two verifying commands, each mount every signature flag, checked through each
// real constructor so a mount deleted from either one fails.
func TestVerifyingCommandsMountTheSignatureFlags(t *testing.T) {
	for _, cmd := range []*cli.Command{Install(), Warm()} {
		t.Run(cmd.Name, func(t *testing.T) {
			if !mountsFlag(cmd, "keyring") {
				t.Fatalf("%s does not mount --keyring", cmd.Name)
			}
			for _, name := range []string{"required-valid-signature-count", "ignore-signature-status-code", "disable-gpg-verify"} {
				if !mountsFlag(cmd, name) {
					t.Errorf("%s does not mount --%s", cmd.Name, name)
				}
			}
		})
	}
}

// mountsFlag reports whether cmd declares a flag by that name, matching on the
// name urfave/cli itself parses rather than on the flag's Go type, so a flag
// whose type changes still counts as mounted.
func mountsFlag(cmd *cli.Command, name string) bool {
	return slices.ContainsFunc(cmd.Flags, func(flag cli.Flag) bool {
		return slices.Contains(flag.Names(), name)
	})
}

// TestCleanupSignatureDefaults pins that a command registering no signature
// flags, like cleanup, still resolves a valid count, since BuildCollectionConfig
// validates it for every command.
func TestCleanupSignatureDefaults(t *testing.T) {
	neutralizeAnsibleDiscovery(t)

	cfg, err := buildConfigFor(t, "cleanup", cliflags.S3Flags(), []string{"--cache-dir=" + t.TempDir()})
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	assertConfigField(t, "Signature.RequiredCount", cfg.Signature.RequiredCount,
		galaxyhelpers.DefaultRequiredValidSignatureCount)
	assertConfigField(t, "Signature.KeyringPath", cfg.Signature.KeyringPath, "")
}

// TestWarmParsesTheSignatureFlags pins that the real Warm().Flags parse every
// signature flag, so a script sharing one flag block with install does not die
// with "flag provided but not defined".
func TestWarmParsesTheSignatureFlags(t *testing.T) {
	neutralizeAnsibleDiscovery(t)

	args := []string{
		"--keyring=/keys.gpg",
		"--required-valid-signature-count=+all",
		"--ignore-signature-status-code=BADSIG",
		"--disable-gpg-verify",
	}
	if err := runCommandWith(t, "warm", Warm().Flags, args); err != nil {
		t.Fatalf("app.Run(warm) error = %v, want nil", err)
	}
}
