package commands

// These tests pin the ansible.cfg half of the signature surface through a real
// discovered file and the real BuildCollectionConfig with install's flag set:
// the key names reach the Config, the values never do.

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// ansibleCfgWithSignatureKeys writes an ansible.cfg carrying ansible's four
// signature keys and points $ANSIBLE_CONFIG at it, overriding the nonexistent
// path neutralizeAnsibleDiscovery sets.
func ansibleCfgWithSignatureKeys(t *testing.T) string {
	t.Helper()
	neutralizeAnsibleDiscovery(t)
	path := filepath.Join(t.TempDir(), "ansible.cfg")
	body := "[galaxy]\n" +
		"gpg_keyring = /repo/keys.gpg\n" +
		"required_valid_signature_count = 0\n" +
		"ignore_signature_status_codes = BADSIG\n" +
		"disable_gpg_verify = yes\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write ansible.cfg: %v", err)
	}
	t.Setenv("ANSIBLE_CONFIG", path)

	return path
}

// TestAnsibleSignatureKeysReachTheConfig pins that a discovered ansible.cfg's
// four signature key names reach cfg.AnsibleSignatureKeys for the warning,
// while none of their values reaches cfg.Signature.
func TestAnsibleSignatureKeysReachTheConfig(t *testing.T) {
	path := ansibleCfgWithSignatureKeys(t)

	cfg, err := buildConfigFor(t, "install", signatureSurfaceFlags(), nil)
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}

	want := []string{"gpg_keyring", "required_valid_signature_count", "ignore_signature_status_codes", "disable_gpg_verify"}
	if got := cfg.AnsibleSignatureKeys; !slices.Equal(got, want) {
		t.Fatalf("AnsibleSignatureKeys = %v, want the four ansible signature key names", got)
	}
	assertConfigField(t, "AnsibleConfigPath", cfg.AnsibleConfigPath, path)

	assertConfigField(t, "Signature.KeyringPath", cfg.Signature.KeyringPath, "")
	assertConfigField(t, "Signature.RequiredCount", cfg.Signature.RequiredCount,
		galaxyhelpers.DefaultRequiredValidSignatureCount)
	assertConfigField(t, "Signature.DisableGPGVerify", cfg.Signature.DisableGPGVerify, false)
	if len(cfg.Signature.IgnoreStatusCodes) != 0 {
		t.Fatalf("Signature.IgnoreStatusCodes = %v, want none: ansible.cfg must contribute no signature value", cfg.Signature.IgnoreStatusCodes)
	}
}
