package collections

// This file tests verifyCollectionSignatures and its context over artifacts
// and signatures generated here with an ephemeral key: each test signs the
// MANIFEST.json it just built, which no committed fixture signature could match.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/signature"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// testSignedCollection is the identity every fixture here signs and installs,
// kept plain so its sources key and collection key read well in a failure.
//
//nolint:gochecknoglobals // a fixed fixture identity, not mutable shared state.
var testSignedCollection = collection{Namespace: "acme", Name: "app", Version: "1.0.0"}

// testOtherDigest is a well-formed sha256 that is not the digest of anything a
// fixture here carries, so a listing naming it is wrong about content rather
// than malformed.
const testOtherDigest = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"

// testSigningEntity generates one Ed25519 entity per test binary and shares it:
// key generation is this file's only expensive step, far cheaper than RSA.
//
//nolint:gochecknoglobals // a lazily built, immutable fixture, not mutable shared state.
var testSigningEntity = sync.OnceValues(func() (*openpgp.Entity, error) {
	return openpgp.NewEntity("go-galaxy fixture", "signature fixture", "fixture@example.invalid",
		&packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
})

// mustTestSigningEntity returns the shared fixture entity, failing the test if
// it could not be generated.
func mustTestSigningEntity(t *testing.T) *openpgp.Entity {
	t.Helper()
	entity, err := testSigningEntity()
	if err != nil {
		t.Fatalf("generate the fixture signing key: %v", err)
	}
	return entity
}

// writeTestKeyring writes the fixture entity's PUBLIC half as an armored
// keyring and returns its path. Only the public half is exported, which is what
// LoadKeyring accepts: a file carrying secret material is refused by design.
func writeTestKeyring(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	block, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatalf("open the keyring armor block: %v", err)
	}
	if err := mustTestSigningEntity(t).Serialize(block); err != nil {
		t.Fatalf("serialize the fixture public key: %v", err)
	}
	if err := block.Close(); err != nil {
		t.Fatalf("close the keyring armor block: %v", err)
	}

	path := filepath.Join(t.TempDir(), "keyring.asc")
	mustWriteFile(t, path, buf.Bytes())
	return path
}

// signTestBytes returns an armored detached signature over message, made by the
// fixture entity.
func signTestBytes(t *testing.T, message []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&buf, mustTestSigningEntity(t), bytes.NewReader(message), nil); err != nil {
		t.Fatalf("sign the fixture manifest: %v", err)
	}
	return buf.Bytes()
}

// writeTestSignature writes a detached signature over message into a fresh
// temp file and returns a file:// URL naming it - the shape a requirements
// file's signatures: entry takes for a local source.
func writeTestSignature(t *testing.T, message []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "collection.asc")
	mustWriteFile(t, path, signTestBytes(t, message))
	return "file://" + path
}

// buildSignedArtifact builds an artifact carrying MANIFEST.json, FILES.json and
// README.md and returns its path and the manifest bytes to sign; breakChain
// makes FILES.json list a README.md digest the archive does not match.
func buildSignedArtifact(t *testing.T, breakChain bool) (string, []byte) {
	t.Helper()

	return buildSignedArtifactAs(t,
		testSignedCollection.Namespace, testSignedCollection.Name, testSignedCollection.Version, breakChain)
}

// buildSignedArtifactAs is buildSignedArtifact with the manifest's identity as
// parameters, so an internally perfect artifact can name another collection.
func buildSignedArtifactAs(t *testing.T, namespace, name, version string, breakChain bool) (string, []byte) {
	t.Helper()
	readme := []byte("# " + namespace + "." + name + "\n")
	listed := sha256Hex(readme)
	if breakChain {
		listed = testOtherDigest
	}

	filesJSON := []byte(`{"files":[` +
		`{"name":".","ftype":"dir","chksum_type":null,"chksum_sha256":null},` +
		fmt.Sprintf(`{"name":"README.md","ftype":"file","chksum_type":"sha256","chksum_sha256":%q}`, listed) +
		`],"format":1}`)
	manifestJSON := fmt.Appendf(nil,
		`{"format":1,"collection_info":{"namespace":%q,"name":%q,"version":%q},`+
			`"file_manifest_file":{"name":"FILES.json","ftype":"file","chksum_type":"sha256","chksum_sha256":%q}}`,
		namespace, name, version, sha256Hex(filesJSON))

	// A fixed filename: a fixture may declare an identity no filesystem accepts
	// as a name, and nothing reads the artifact's own file name.
	path := filepath.Join(t.TempDir(), "collection.tar.gz")
	mustWriteFile(t, path, buildTarGz(t, []tarEntry{
		{name: helpers.ManifestFileName, body: manifestJSON},
		{name: helpers.FilesManifestFileName, body: filesJSON},
		{name: "README.md", body: readme},
	}))
	return path, manifestJSON
}

// buildArtifactWithManifest builds a chain-correct artifact whose MANIFEST.json
// opens with collectionInfo verbatim, an unterminated object, so a fixture can
// express a defect in the document's shape, such as a key declared twice.
func buildArtifactWithManifest(t *testing.T, collectionInfo string) (string, []byte) {
	t.Helper()
	readme := []byte("# fixture\n")
	filesJSON := []byte(`{"files":[` +
		`{"name":".","ftype":"dir","chksum_type":null,"chksum_sha256":null},` +
		fmt.Sprintf(`{"name":"README.md","ftype":"file","chksum_type":"sha256","chksum_sha256":%q}`, sha256Hex(readme)) +
		`],"format":1}`)
	manifestJSON := fmt.Appendf(nil,
		`%s,"file_manifest_file":{"name":"FILES.json","ftype":"file","chksum_type":"sha256","chksum_sha256":%q}}`,
		collectionInfo, sha256Hex(filesJSON))

	path := filepath.Join(t.TempDir(), "collection.tar.gz")
	mustWriteFile(t, path, buildTarGz(t, []tarEntry{
		{name: helpers.ManifestFileName, body: manifestJSON},
		{name: helpers.FilesManifestFileName, body: filesJSON},
		{name: "README.md", body: readme},
	}))

	return path, manifestJSON
}

// tarEntry is one regular file of a fixture archive.
type tarEntry struct {
	name string
	body []byte
}

// buildTarGz writes entries as a gzipped tar, in order. It is
// buildTarGzWithEntry (lock_pin_test.go) widened to several entries, which a
// chain-carrying artifact needs and that one cannot express.
func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{Typeflag: tar.TypeReg, Name: entry.name, Size: int64(len(entry.body)), Mode: 0o644}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("write tar header for %s: %v", entry.name, err)
		}
		if _, err := tw.Write(entry.body); err != nil {
			t.Fatalf("write tar body for %s: %v", entry.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// verifyFixture is one verification scenario: the deps verifyCollectionSignatures
// is called with, and the printer whose lines it wrote.
type verifyFixture struct {
	deps    installDeps
	printer *capturingPrinter
}

// newVerifyFixture builds installDeps through the real newVerifyContext, as an
// install does; an empty keyring is the run that verifies nothing.
func newVerifyFixture(t *testing.T, keyring, count string, sources []string) *verifyFixture {
	t.Helper()
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{
		Workers: 1,
		Signature: config.SignatureConfig{
			KeyringPath:   keyring,
			RequiredCount: count,
		},
	}
	root := testSignedCollection
	root.Signatures = sources

	verify, err := newVerifyContext(cfg, runtime, []collection{root})
	if err != nil {
		t.Fatalf("newVerifyContext() error = %v, want nil", err)
	}

	return &verifyFixture{
		deps:    newInstallDeps(cfg, runtime, store.New(), nil, nil, nil, nil, verify),
		printer: printer,
	}
}

// serverSignatureMeta builds the version metadata a Galaxy server serves for a
// signed collection: the wire shape is a list of objects carrying the armored
// signature under "signature", which is what serverSignatureBlobs reads.
func serverSignatureMeta(blobs ...[]byte) *types.GalaxyCollectionVersionInfo {
	meta := &types.GalaxyCollectionVersionInfo{Href: "https://galaxy.example/api/v3/collections/acme/app/versions/1.0.0/"}
	list := make([]any, 0, len(blobs))
	for _, blob := range blobs {
		list = append(list, map[string]any{"signature": string(blob)})
	}
	meta.Signatures = list
	return meta
}

// verifyPayload builds the installPayload verifyCollectionSignatures reads:
// the metadata carrying a server's signatures and the artifact to check.
func verifyPayload(meta *types.GalaxyCollectionVersionInfo, tarPath string) installPayload {
	return installPayload{meta: meta, artifact: artifactData{Path: tarPath}}
}

// TestVerifyDisabledDoesNoWork pins that a run verifying nothing never reads the
// artifact: a non-archive passes with verification off and fails with it on.
func TestVerifyDisabledDoesNoWork(t *testing.T) {
	t.Parallel()
	tarPath := filepath.Join(t.TempDir(), "not-an-archive.tar.gz")
	mustWriteFile(t, tarPath, []byte("this is not a gzip stream at all"))

	t.Run("verification off", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, "", "1", nil)
		err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath))
		if err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
		}
	})

	t.Run("verification on refuses the same bytes", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath))
		if !errors.Is(err, helpers.ErrArtifactNotTarGz) {
			t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrArtifactNotTarGz", err)
		}
	})
}

// TestVerifySucceedsWithServerSignature covers the source an ordinary Galaxy
// install has: the server carries the signature in its own version metadata,
// the requirements file names none, and the artifact's chain is intact.
func TestVerifySucceedsWithServerSignature(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifact(t, false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)

	meta := serverSignatureMeta(signTestBytes(t, manifestJSON))
	if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath)); err != nil {
		t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
	}
	// The needle is warnVacuousPass' own leading words: a collection something
	// verified must not carry that line at all, and asserting on text no
	// production line contains would pass whatever the code did.
	if fx.printer.hasWarnContaining("Nothing verified") {
		t.Fatalf("a verified collection must not warn about a vacuous pass: %v", fx.printer.warns)
	}
}

// TestVerifySucceedsWithRequirementSignature covers the other source: the
// requirements file declares a signature and the server carries none.
func TestVerifySucceedsWithRequirementSignature(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifact(t, false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", []string{writeTestSignature(t, manifestJSON)})

	if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
		t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
	}
}

// TestVerifyFailsOnBadSignature pins that a well-formed signature by a trusted
// key over other bytes is a verification verdict naming the collection, not a
// keyring or armor failure.
func TestVerifyFailsOnBadSignature(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifact(t, false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)

	meta := serverSignatureMeta(signTestBytes(t, []byte("a document this artifact does not carry")))
	err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
	if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
		t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
	}
	if !strings.Contains(err.Error(), testSignedCollection.key()) {
		t.Fatalf("verifyCollectionSignatures() = %v, want it to name %s", err, testSignedCollection.key())
	}
}

// TestVerifyFailsOnChainMismatch pins the second half of the check: the
// signature verifies, so the chain is walked, and the archive does not agree
// with the listing its own signed manifest vouches for.
func TestVerifyFailsOnChainMismatch(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifact(t, true)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)

	meta := serverSignatureMeta(signTestBytes(t, manifestJSON))
	err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrManifestChainMismatch", err)
	}
}

// TestVerifySkipsChainWhenNothingVerified pins that a vacuous pass does not walk
// the manifest chain, since nothing backs it; one good signature makes the same
// broken chain fail (the control).
func TestVerifySkipsChainWhenNothingVerified(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifact(t, true)

	t.Run("nothing verified, chain not walked", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
		}
	})

	t.Run("one signature verified, same broken chain refused", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		meta := serverSignatureMeta(signTestBytes(t, manifestJSON))
		err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
		if !errors.Is(err, helpers.ErrManifestChainMismatch) {
			t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrManifestChainMismatch", err)
		}
	})
}

// TestVacuousPassIsWarned pins that a pass over an empty signature set warns,
// naming the collection and the strict spelling that would have required one,
// where ansible-galaxy passes silently.
func TestVacuousPassIsWarned(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifact(t, false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)

	if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
		t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
	}
	if !fx.printer.hasWarnContaining(testSignedCollection.key()) {
		t.Fatalf("no vacuous-pass warning naming %s; warns=%v", testSignedCollection.key(), fx.printer.warns)
	}
	if !fx.printer.hasWarnContaining(`"+1"`) {
		t.Fatalf("the vacuous-pass warning does not name the strict spelling; warns=%v", fx.printer.warns)
	}
}

// TestMetadataFailureUnderVerificationCountPolicyDecides pins that missing
// metadata gathers nothing and the count policy decides: the default passes
// with a warning, its strict spelling refuses the collection.
func TestMetadataFailureUnderVerificationCountPolicyDecides(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifact(t, false)
	keyring := writeTestKeyring(t)

	t.Run("the default count passes vacuously", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, keyring, "1", nil)
		if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
		}
		if !fx.printer.hasWarnContaining(testSignedCollection.key()) {
			t.Fatalf("no vacuous-pass warning naming %s; warns=%v", testSignedCollection.key(), fx.printer.warns)
		}
	})

	t.Run("the strict spelling refuses it", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, keyring, "+1", nil)
		err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath))
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
	})
}

// TestServerSignatureListIsCapped pins helpers.MaxSignaturesPerCollection over a
// server list, so ten thousand entries cost no more than the cap: a good
// signature past the cap is never reached, while the same one alone verifies.
func TestServerSignatureListIsCapped(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifact(t, false)
	good := signTestBytes(t, manifestJSON)
	junk := signTestBytes(t, []byte("a document this artifact does not carry"))

	t.Run("a good signature past the cap is never reached", func(t *testing.T) {
		t.Parallel()
		blobs := make([][]byte, 0, 10_000)
		for range 10_000 {
			blobs = append(blobs, junk)
		}
		blobs = append(blobs, good)

		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		err := verifyCollectionSignatures(
			context.Background(), fx.deps, testSignedCollection, verifyPayload(serverSignatureMeta(blobs...), tarPath))
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
	})

	t.Run("the same signature within the cap verifies", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		err := verifyCollectionSignatures(
			context.Background(), fx.deps, testSignedCollection, verifyPayload(serverSignatureMeta(good), tarPath))
		if err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
		}
	})
}

// TestSignaturesWithoutKeyringIsAHardError pins that declared signatures with no
// keyring are ErrKeyringRequired, exit 2 rather than a verdict; adding a keyring
// accepts the same requirements (the control).
func TestSignaturesWithoutKeyringIsAHardError(t *testing.T) {
	t.Parallel()
	root := testSignedCollection
	root.Signatures = []string{"https://example.invalid/acme-app.asc"}
	runtime := infra.New(&capturingPrinter{}, http.DefaultClient)

	t.Run("no keyring refuses the run", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{Signature: config.SignatureConfig{RequiredCount: "1"}}
		_, err := newVerifyContext(cfg, runtime, []collection{root})
		if !errors.Is(err, helpers.ErrKeyringRequired) {
			t.Fatalf("newVerifyContext() = %v, want errors.Is helpers.ErrKeyringRequired", err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitUsage {
			t.Fatalf("exit code = %d, want %d (a configuration error, never a verification verdict)", got, exitcode.ExitUsage)
		}
	})

	t.Run("a keyring accepts the same requirements", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{Signature: config.SignatureConfig{KeyringPath: writeTestKeyring(t), RequiredCount: "1"}}
		verify, err := newVerifyContext(cfg, runtime, []collection{root})
		if err != nil {
			t.Fatalf("newVerifyContext() error = %v, want nil", err)
		}
		if !verify.enabled() {
			t.Fatal("newVerifyContext() returned a disabled context for a configured keyring")
		}
	})
}

// TestAnsibleSignatureKeysAreWarnedOnce pins that newVerifyContext, the single
// emission point, warns once per call naming the ansible.cfg and its signature
// key names, never their values.
func TestAnsibleSignatureKeysAreWarnedOnce(t *testing.T) {
	t.Parallel()
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{
		AnsibleConfigPath:    "/etc/ansible/ansible.cfg",
		AnsibleSignatureKeys: []string{"gpg_keyring", "disable_gpg_verify"},
		Signature:            config.SignatureConfig{RequiredCount: "1"},
	}

	if _, err := newVerifyContext(cfg, runtime, nil); err != nil {
		t.Fatalf("newVerifyContext() error = %v, want nil", err)
	}
	if len(printer.warns) != 1 {
		t.Fatalf("warns = %v, want exactly one line naming the ansible.cfg signature keys", printer.warns)
	}
	line := printer.warns[0]
	for _, want := range []string{"/etc/ansible/ansible.cfg", "gpg_keyring", "disable_gpg_verify"} {
		if !strings.Contains(line, want) {
			t.Fatalf("warning %q does not name %q", line, want)
		}
	}

	if _, err := newVerifyContext(cfg, runtime, nil); err != nil {
		t.Fatalf("newVerifyContext() error = %v, want nil", err)
	}
	if len(printer.warns) != 2 {
		t.Fatalf("warns = %v, want one line per newVerifyContext call", printer.warns)
	}
}

// TestVerificationDisabledWarnsAboutDeclaredSources pins that verification
// switched off explicitly, with a keyring configured, proceeds with a warning
// naming the collection whose declared sources go unchecked.
func TestVerificationDisabledWarnsAboutDeclaredSources(t *testing.T) {
	t.Parallel()
	root := testSignedCollection
	root.Signatures = []string{"https://example.invalid/acme-app.asc"}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{Signature: config.SignatureConfig{
		KeyringPath:      writeTestKeyring(t),
		RequiredCount:    "1",
		DisableGPGVerify: true,
	}}

	verify, err := newVerifyContext(cfg, runtime, []collection{root})
	if err != nil {
		t.Fatalf("newVerifyContext() error = %v, want nil", err)
	}
	if verify.enabled() {
		t.Fatal("newVerifyContext() returned an enabled context while verification is disabled")
	}
	if !printer.hasWarnContaining(requirementKey(root)) {
		t.Fatalf("no warning naming %s; warns=%v", requirementKey(root), printer.warns)
	}
}

// TestServerSignatureBlobsIgnoresForeignShapes pins that a server entry of an
// unrecognized shape is skipped and not counted as offered, rather than failing
// the install, since a third-party server may send more than signatures.
func TestServerSignatureBlobsIgnoresForeignShapes(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("x", int(helpers.SignatureMaxSize)+1)
	meta := &types.GalaxyCollectionVersionInfo{Signatures: []any{
		"a bare string, not an object",
		map[string]any{"pubkey_fingerprint": "abc"},
		map[string]any{"signature": 7},
		map[string]any{"signature": ""},
		map[string]any{"signature": oversized},
		map[string]any{"signature": "-----BEGIN PGP SIGNATURE-----\nreal enough\n-----END PGP SIGNATURE-----"},
	}}

	blobs, offered := serverSignatureBlobs(meta)
	if len(blobs) != 1 {
		t.Fatalf("serverSignatureBlobs() returned %d blobs, want 1 (only the recognized shape)", len(blobs))
	}
	// offered counts entries that pass the shape filter, not the raw list
	// length: six entries were offered and only one is shape-valid, so this
	// must read 1, never 6.
	if offered != 1 {
		t.Fatalf("serverSignatureBlobs() offered = %d, want 1 (shape-valid entries, not the raw list length of 6)", offered)
	}
	if !strings.Contains(string(blobs[0].Data), "real enough") {
		t.Fatalf("serverSignatureBlobs() returned %q, want the recognized entry's own bytes", blobs[0].Data)
	}
	if blobs[0].Origin == "" {
		t.Fatal("serverSignatureBlobs() returned a blob with no origin")
	}
	if got, offered := serverSignatureBlobs(nil); got != nil || offered != 0 {
		t.Fatalf("serverSignatureBlobs(nil) = %v, offered %d, want nil, 0", got, offered)
	}
	if got, offered := serverSignatureBlobs(&types.GalaxyCollectionVersionInfo{}); got != nil || offered != 0 {
		t.Fatalf("serverSignatureBlobs(no signatures) = %v, offered %d, want nil, 0", got, offered)
	}
}

// TestVerifyContextSourcesAreDedupedInFileOrder pins what a requirements entry
// contributes: its own sources, in the order the file wrote them, with a
// repeat collapsed - and nothing at all for a collection that declared none.
func TestVerifyContextSourcesAreDedupedInFileOrder(t *testing.T) {
	t.Parallel()
	signed := testSignedCollection
	signed.Signatures = []string{"file:///b.asc", "file:///a.asc", "file:///b.asc"}
	unsigned := collection{Namespace: "acme", Name: "lib", Version: "1.0.0"}

	got := requirementSources([]collection{signed, unsigned})
	want := []string{"file:///b.asc", "file:///a.asc"}
	if fmt.Sprint(got[requirementKey(signed)]) != fmt.Sprint(want) {
		t.Fatalf("sources = %v, want %v", got[requirementKey(signed)], want)
	}
	if _, ok := got[requirementKey(unsigned)]; ok {
		t.Fatalf("sources carries a key for a collection declaring none: %v", got)
	}
}

// TestVacuousPassAdviceCanActuallyPass pins strictSpelling: the advice for a
// count of 0 is "+1", since "+0" fails under every outcome, while a real count
// and "all" are made strict by prefixing.
func TestVacuousPassAdviceCanActuallyPass(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name string
		want string
		spec signature.CountSpec
	}{
		{name: "count 0", spec: signature.CountSpec{}, want: "+1"},
		{name: "count 2", spec: signature.CountSpec{Count: 2}, want: "+2"},
		{name: "all", spec: signature.CountSpec{All: true}, want: "+all"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := strictSpelling(row.spec); got != row.want {
				t.Fatalf("strictSpelling(%s) = %q, want %q", row.name, got, row.want)
			}
		})
	}
}

// TestVacuousPassUnderCountZeroAdvisesAWritableSpelling pins end to end that a
// vacuous pass under count 0 advises "+1", never "+0", which cannot pass.
func TestVacuousPassUnderCountZeroAdvisesAWritableSpelling(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifact(t, false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "0", nil)

	if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
		t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
	}
	if !fx.printer.hasWarnContaining(`"+1"`) {
		t.Fatalf("the vacuous-pass warning does not advise a spelling that can pass; warns=%v", fx.printer.warns)
	}
	if fx.printer.hasWarnContaining(`"+0"`) {
		t.Fatalf("the vacuous-pass warning advises %q, which fails under every outcome; warns=%v", "+0", fx.printer.warns)
	}
}

// buildDistinctFileSources writes n temp files with distinct bytes -
// "candidate-<i>" each - and returns their file:// sources, so the sha dedupe
// nextBlob applies never collapses two of them into one gathered candidate.
func buildDistinctFileSources(t *testing.T, n int) []string {
	t.Helper()
	dir := t.TempDir()
	sources := make([]string, n)
	for i := range sources {
		path := filepath.Join(dir, fmt.Sprintf("sig-%d.asc", i))
		mustWriteFile(t, path, fmt.Appendf(nil, "candidate-%d", i))
		sources[i] = "file://" + path
	}

	return sources
}

// buildDistinctServerBlobs returns n server blobs with distinct bytes, so
// nextBlob's sha dedupe cannot collapse two of them into one candidate.
func buildDistinctServerBlobs(n int) [][]byte {
	blobs := make([][]byte, n)
	for i := range blobs {
		blobs[i] = fmt.Appendf(nil, "server-blob-%d", i)
	}

	return blobs
}

// drainGather pulls every blob next offers and counts it as file-sourced or
// server-sourced by its Origin, the two sides of gatherOne's gather.
func drainGather(t *testing.T, next signature.NextBlob, fileSources []string) (int, int) {
	t.Helper()
	known := make(map[string]struct{}, len(fileSources))
	for _, source := range fileSources {
		known[source] = struct{}{}
	}
	fileCount, serverCount := 0, 0
	for {
		blob, ok, err := next()
		if err != nil {
			t.Fatalf("nextBlob() pull error = %v, want nil", err)
		}
		if !ok {
			return fileCount, serverCount
		}
		if _, isFile := known[blob.Origin]; isFile {
			fileCount++
		} else {
			serverCount++
		}
	}
}

// gatherLimitRow is one row of gatherLimitRows: a declared-and-offered pair
// gatherLimit sees, and what its verdict must be.
type gatherLimitRow struct {
	name            string
	wantWarnSubstr  string
	declared        int
	offered         int
	wantWarns       int
	wantFileCount   int
	wantServerCount int
}

// gatherLimitRows is TestGatherWarnsWhenTheCapTruncatesTheCandidateSet's table.
// Rows whose server side alone passes the cap catch a post-truncation count.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var gatherLimitRows = []gatherLimitRow{
	{
		name: "64 declared, 2 offered", declared: 64, offered: 2, wantWarns: 1,
		wantWarnSubstr: "66 signature candidates exceed the limit of 64 (64 declared, 2 offered by the server); " +
			"the last 2 were not gathered",
		wantFileCount: 64, wantServerCount: 0,
	},
	{
		name: "63 declared, 2 offered", declared: 63, offered: 2, wantWarns: 1,
		wantWarnSubstr: "65 signature candidates exceed the limit of 64 (63 declared, 2 offered by the server); " +
			"the last 1 were not gathered",
		wantFileCount: 63, wantServerCount: 1,
	},
	// The control: one candidate under the cap, every blob is gathered and no
	// warning fires, so the harness can complete a silent gather.
	{name: "62 declared, 2 offered", declared: 62, offered: 2, wantWarns: 0, wantFileCount: 62, wantServerCount: 2},
	// The rows below push the SERVER side itself past the cap,
	// which none of the three rows above can: with the server side fixed
	// at 2, offered never crosses the cap on its own.
	{
		name: "0 declared, 200 offered", declared: 0, offered: 200, wantWarns: 1,
		wantWarnSubstr: "200 signature candidates exceed the limit of 64 (0 declared, 200 offered by the server); " +
			"the last 136 were not gathered",
		wantFileCount: 0, wantServerCount: 64,
	},
	{
		name: "0 declared, 65 offered", declared: 0, offered: 65, wantWarns: 1,
		wantWarnSubstr: "the last 1 were not gathered",
		wantFileCount:  0, wantServerCount: 64,
	},
	{
		name: "10 declared, 200 offered", declared: 10, offered: 200, wantWarns: 1,
		wantWarnSubstr: "210 signature candidates exceed the limit of 64 (10 declared, 200 offered by the server); " +
			"the last 146 were not gathered",
		wantFileCount: 10, wantServerCount: 54,
	},
	// The boundary control on the server side: the server alone fills the
	// cap exactly, nothing is dropped and no warning fires.
	{name: "0 declared, 64 offered", declared: 0, offered: 64, wantWarns: 0, wantFileCount: 0, wantServerCount: 64},
}

// TestGatherWarnsWhenTheCapTruncatesTheCandidateSet pins gatherLimit: declared
// plus server-offered candidates past helpers.MaxSignaturesPerCollection warn
// once, naming what was dropped, rather than silently dropping or refusing.
func TestGatherWarnsWhenTheCapTruncatesTheCandidateSet(t *testing.T) {
	t.Parallel()

	for _, row := range gatherLimitRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			sources := buildDistinctFileSources(t, row.declared)
			fx := newVerifyFixture(t, writeTestKeyring(t), "1", sources)
			meta := serverSignatureMeta(buildDistinctServerBlobs(row.offered)...)

			next := fx.deps.verify.nextBlob(context.Background(), fx.deps.runtime, testSignedCollection, meta)
			fileCount, serverCount := drainGather(t, next, sources)

			if fileCount != row.wantFileCount {
				t.Fatalf("gathered %d file candidates, want %d", fileCount, row.wantFileCount)
			}
			if serverCount != row.wantServerCount {
				t.Fatalf("gathered %d server candidates, want %d", serverCount, row.wantServerCount)
			}
			// The warning's text is checked as well as its count, so an offered
			// count taken after truncation fails here on its arithmetic.
			if len(fx.printer.warns) != row.wantWarns {
				t.Fatalf("warns = %v, want exactly %d", fx.printer.warns, row.wantWarns)
			}
			if row.wantWarnSubstr != "" && !fx.printer.hasWarnContaining(row.wantWarnSubstr) {
				t.Fatalf("warns do not name %q: %v", row.wantWarnSubstr, fx.printer.warns)
			}
		})
	}
}
