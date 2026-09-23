package collections_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// writeGitVerifyKeyring writes an armored public keyring with one fresh EdDSA
// key and returns its path. Nothing is signed with it: it only turns
// verification on, so the unsigned git collection meets the configured policy.
func writeGitVerifyKeyring(t *testing.T) string {
	t.Helper()
	entity, err := openpgp.NewEntity("go-galaxy git fixture", "", "git-fixture@example.invalid",
		&packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatalf("generate keyring entity: %v", err)
	}
	var buf bytes.Buffer
	block, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatalf("open armor block: %v", err)
	}
	if err := entity.Serialize(block); err != nil {
		t.Fatalf("serialize public key: %v", err)
	}
	if err := block.Close(); err != nil {
		t.Fatalf("close armor block: %v", err)
	}
	path := filepath.Join(t.TempDir(), "keyring.asc")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write keyring: %v", err)
	}
	return path
}

// TestGitInstallUnderSignatureVerification asserts an unsigned git collection
// installs with the vacuous-pass warning under the default required count and
// is refused with the signature sentinel under the strict "+1" spelling.
func TestGitInstallUnderSignatureVerification(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\n")
	f.cfg.Signature = config.SignatureConfig{
		KeyringPath:   writeGitVerifyKeyring(t),
		RequiredCount: helpers.DefaultRequiredValidSignatureCount,
	}
	f.mustInstall(t)
	assertManifestInstalled(t, f.downloadPath, "app")
	if !f.printer.hasWarnContaining("Nothing verified acme.app@1.2.3 and the policy passed anyway") {
		t.Fatalf("no vacuous-pass warning names acme.app; warnings: %v", f.printer.warns)
	}

	g := newGitFixture(t)
	g.writeRequirements(t, "collections:\n  - name: acme.one\n    type: git\n    source: "+gitMonoURL+"#collections\n")
	g.cfg.Signature = config.SignatureConfig{KeyringPath: writeGitVerifyKeyring(t), RequiredCount: "+1"}
	err := g.install(t)
	if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
		t.Fatalf("strict count over an unsigned git collection: %v, want ErrSignatureVerificationFailed", err)
	}
	assertPathAbsent(t, installPathFor(g.downloadPath, "one"))
}
