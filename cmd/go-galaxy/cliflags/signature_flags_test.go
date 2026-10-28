package cliflags

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// signatureFlagEnvKeys is every variable a signature flag reads, plus the
// ansible disable variable that must not be one of its sources.
func signatureFlagEnvKeys() []string {
	return []string{
		"GO_GALAXY_KEYRING", "ANSIBLE_GALAXY_GPG_KEYRING",
		"GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT", "ANSIBLE_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT",
		"GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE", "ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES",
		"GO_GALAXY_DISABLE_GPG_VERIFY", "ANSIBLE_GALAXY_DISABLE_GPG_VERIFY",
	}
}

// signatureFlagValues is what one run of SignatureFlags read.
type signatureFlagValues struct {
	keyring      string
	count        string
	ignore       []string
	disable      bool
	disableIsSet bool
	keyringIsSet bool
	countIsSet   bool
}

// runSignatureFlags runs a command mounting SignatureFlags under env, every
// other signature variable unset, and returns what each flag read.
func runSignatureFlags(t *testing.T, env map[string]string) signatureFlagValues {
	t.Helper()
	for _, key := range signatureFlagEnvKeys() {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
	var got signatureFlagValues
	cmd := &cli.Command{
		Name:  "install",
		Flags: SignatureFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			got = signatureFlagValues{
				keyring:      c.String("keyring"),
				count:        c.String("required-valid-signature-count"),
				ignore:       c.StringSlice("ignore-signature-status-code"),
				disable:      c.Bool("disable-gpg-verify"),
				disableIsSet: c.IsSet("disable-gpg-verify"),
				keyringIsSet: c.IsSet("keyring"),
				countIsSet:   c.IsSet("required-valid-signature-count"),
			}
			return nil
		},
	}
	if err := cmd.Run(context.Background(), []string{"install"}); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return got
}

// TestSignatureFlagsDefaultsVerifyNothing pins that with no source set no
// keyring is named, the count is the documented default and nothing is
// tolerated or disabled.
func TestSignatureFlagsDefaultsVerifyNothing(t *testing.T) {
	got := runSignatureFlags(t, nil)
	if got.keyring != "" || got.keyringIsSet {
		t.Errorf("keyring = %q (set %t), want unset", got.keyring, got.keyringIsSet)
	}
	if got.count != galaxyhelpers.DefaultRequiredValidSignatureCount || got.countIsSet {
		t.Errorf("count = %q (set %t), want the unset default %q", got.count, got.countIsSet, galaxyhelpers.DefaultRequiredValidSignatureCount)
	}
	if len(got.ignore) != 0 || got.disable || got.disableIsSet {
		t.Errorf("ignore = %q, disable = %t (set %t), want neither", got.ignore, got.disable, got.disableIsSet)
	}
}

// TestSignatureFlagsReadAnsibleSpellingsBelowGoGalaxy pins each flag's
// sources: the ansible variable is read when alone and the GO_GALAXY one
// outranks it when both are set.
func TestSignatureFlagsReadAnsibleSpellingsBelowGoGalaxy(t *testing.T) {
	ansibleOnly := runSignatureFlags(t, map[string]string{
		"ANSIBLE_GALAXY_GPG_KEYRING":                    "/ansible/keyring.kbx",
		"ANSIBLE_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT": "all",
		"ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES":  "BADSIG,NO_PUBKEY",
	})
	if ansibleOnly.keyring != "/ansible/keyring.kbx" || ansibleOnly.count != "all" {
		t.Errorf("keyring, count = %q, %q, want the ansible values", ansibleOnly.keyring, ansibleOnly.count)
	}
	if want := []string{"BADSIG", "NO_PUBKEY"}; !slices.Equal(ansibleOnly.ignore, want) {
		t.Errorf("ignore = %q, want %q", ansibleOnly.ignore, want)
	}

	both := runSignatureFlags(t, map[string]string{
		"ANSIBLE_GALAXY_GPG_KEYRING":                    "/ansible/keyring.kbx",
		"GO_GALAXY_KEYRING":                             "/go-galaxy/keyring.kbx",
		"ANSIBLE_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT": "all",
		"GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT":      "+1",
		"ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES":  "BADSIG",
		"GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE":        "EXPSIG",
	})
	if both.keyring != "/go-galaxy/keyring.kbx" || both.count != "+1" {
		t.Errorf("keyring, count = %q, %q, want the GO_GALAXY values", both.keyring, both.count)
	}
	if want := []string{"EXPSIG"}; !slices.Equal(both.ignore, want) {
		t.Errorf("ignore = %q, want %q", both.ignore, want)
	}
}

// TestDisableGPGVerifyHasNoAnsibleSource pins that the ansible disable
// variable is no source of the flag, so its yes/no spellings cannot abort the
// command, while GO_GALAXY_DISABLE_GPG_VERIFY is.
func TestDisableGPGVerifyHasNoAnsibleSource(t *testing.T) {
	ansible := runSignatureFlags(t, map[string]string{"ANSIBLE_GALAXY_DISABLE_GPG_VERIFY": "yes"})
	if ansible.disable || ansible.disableIsSet {
		t.Errorf("disable = %t (set %t) from the ansible variable, want unset", ansible.disable, ansible.disableIsSet)
	}
	own := runSignatureFlags(t, map[string]string{"GO_GALAXY_DISABLE_GPG_VERIFY": "true"})
	if !own.disable || !own.disableIsSet {
		t.Errorf("disable = %t (set %t) from GO_GALAXY_DISABLE_GPG_VERIFY, want true", own.disable, own.disableIsSet)
	}
}

// TestDefaultCacheDirFallsBackWithoutHome pins that the cache lives under
// $HOME, and under /root when no home directory can be resolved.
func TestDefaultCacheDirFallsBackWithoutHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := defaultCacheDir(), filepath.Join(home, ".cache", "go-galaxy"); got != want {
		t.Errorf("defaultCacheDir() with HOME set = %q, want %q", got, want)
	}
	t.Setenv("HOME", "")
	if got, want := defaultCacheDir(), "/root/.cache/go-galaxy"; got != want {
		t.Errorf("defaultCacheDir() without HOME = %q, want %q", got, want)
	}
}

// TestAnsiblePathSourceRendersItsNameOnly pins how the ANSIBLE_ path source
// names itself in an error and under %#v: by the variable's name, never by
// the value it holds.
func TestAnsiblePathSourceRendersItsNameOnly(t *testing.T) {
	t.Setenv("ANSIBLE_ROLES_PATH", "/value/must/not/print")
	chain := findStringFlag(t, LockFlags(), "roles-path").Sources.Chain
	src := chain[len(chain)-1]
	if got, want := fmt.Sprint(src), `environment variable "ANSIBLE_ROLES_PATH"`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := fmt.Sprintf("%#v", src), `&ansiblePathEnvSource{Key:"ANSIBLE_ROLES_PATH"}`; got != want {
		t.Errorf("GoString() = %q, want %q", got, want)
	}
}
