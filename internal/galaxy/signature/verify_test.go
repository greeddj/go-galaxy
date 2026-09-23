package signature

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// The verification fixtures are committed gpg 2.5.21 output, public material
// only: expiry is baked into the expired fixtures' bytes, keyring.asc omits the
// outsider key, and synthesizedBlobs holds the shapes gpg does not write.
const (
	manifestAFixture = "manifest-a.json"
	manifestBFixture = "manifest-b.json"

	keyringFixture         = "keyring.asc"
	outsiderKeyringFixture = "keyring-outsider.asc"
	// subkeyFixture is the export carrying a signing subkey, and so the only
	// committed fixture holding an embedded signature subpacket.
	subkeyFixture = "signing-subkey.asc"

	sigValidArmored  = "sig-a-valid.asc"
	sigValidBinary   = "sig-a-valid.sig"
	sigSecondSigner  = "sig-a-signer2.asc"
	sigOverManifestB = "sig-b-valid.asc"
	sigOutsiderKey   = "sig-a-outsider.asc"
	sigExpiredKey    = "sig-a-expired.asc"
	sigRevokedKey    = "sig-a-revoked.asc"
	sigExpiredSig    = "sig-a-expsig.asc"
	sigFoldedArmor   = "sig-a-badarmor.asc"
	sigBadBase64     = "sig-a-badbase64.asc"

	// The four synthesized blobs, named unlike file names because
	// signatureBlobs resolves them out of synthesizedBlobs rather than reading
	// anything.
	noDataBlob        = "<no data>"
	emptyArmorBlob    = "<empty armor envelope>"
	emptyArmorCRCBlob = "<empty armor envelope with a checksum line>"
	amplifyingBlob    = "<v6 signature declaring 4 GiB of subpackets>"

	// deterministicRuns is how many times TestVerifyIsDeterministicAcrossRuns
	// repeats one call; repetition makes agreement a result rather than the
	// coincidence a map iteration could produce twice.
	deterministicRuns = 100
)

// synthesizedBlobs holds blobs gpg does not write: nothing, empty armor with and
// without a checksum line, and a v6 header declaring 4 GiB of hashed subpackets.
//
//nolint:gochecknoglobals // a fixed table consumed by the tests, not mutable shared state
var synthesizedBlobs = map[string][]byte{
	noDataBlob:        nil,
	emptyArmorBlob:    []byte("-----BEGIN PGP SIGNATURE-----\n\n-----END PGP SIGNATURE-----\n"),
	emptyArmorCRCBlob: []byte("-----BEGIN PGP SIGNATURE-----\n\n=twTO\n-----END PGP SIGNATURE-----\n"),
	amplifyingBlob:    {0xc2, 0x08, 0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
}

// errUnanticipated stands in for an error shape no release of go-crypto has
// returned, which is the only input whose status classify chooses rather than
// maps.
var errUnanticipated = errors.New("an error no release of go-crypto has ever returned")

// errGatherFailed stands in for whatever a caller's own pull reports when a
// signature source cannot be fetched. Verify carries it back untouched, so its
// identity is all a test needs.
var errGatherFailed = errors.New("a signature source could not be fetched")

// readFixture reads one testdata file whole.
func readFixture(tb testing.TB, name string) []byte {
	tb.Helper()

	data, err := os.ReadFile(fixturePath(name))
	if err != nil {
		tb.Fatalf("read %s: %v", name, err)
	}

	return data
}

// loadTestKeyring loads a keyring fixture, failing the test if it does not
// load: every case below is about a verdict, so a keyring that could not be
// read has to stop the case rather than quietly shape its outcome.
func loadTestKeyring(t *testing.T, name string) *Keyring {
	t.Helper()

	kr, err := LoadKeyring(fixturePath(name))
	if err != nil {
		t.Fatalf("LoadKeyring(%s) = %v, want nil", name, err)
	}

	return kr
}

// signatureBlobs turns fixture and synthesized-blob names into blobs, naming
// each blob's origin after the name it came from so a Failure can be attributed
// to a row.
func signatureBlobs(t *testing.T, names ...string) []Blob {
	t.Helper()

	blobs := make([]Blob, 0, len(names))
	for _, name := range names {
		blobs = append(blobs, Blob{Origin: name, Data: blobBytes(t, name)})
	}

	return blobs
}

// blobBytes resolves one blob name: a synthesized shape if the table holds it,
// otherwise a committed fixture read whole.
func blobBytes(t *testing.T, name string) []byte {
	t.Helper()

	if data, ok := synthesizedBlobs[name]; ok {
		return data
	}

	return readFixture(t, name)
}

// blobSource turns a slice into the pull Verify takes and reports how many
// blobs were pulled, the only outside view of the loop's early stop.
func blobSource(blobs []Blob) (NextBlob, *int) {
	pulled := 0
	next := func() (Blob, bool, error) {
		if pulled >= len(blobs) {
			return Blob{}, false, nil
		}
		blob := blobs[pulled]
		pulled++

		return blob, true, nil
	}

	return next, &pulled
}

// verifyBlobs runs Verify over a slice, for the cases that care about the
// verdict rather than about how many blobs were pulled to reach it.
func verifyBlobs(manifest []byte, blobs []Blob, kr *Keyring, p Policy) (Result, error) {
	next, _ := blobSource(blobs)

	return Verify(manifest, next, kr, p)
}

// testPolicy builds a Policy from the spellings an operator would configure,
// through the same parser production uses.
func testPolicy(t *testing.T, required string, ignore []string) Policy {
	t.Helper()

	policy, err := NewPolicy(fixturePath(keyringFixture), required, ignore, false)
	if err != nil {
		t.Fatalf("NewPolicy(%q, %q) = %v, want nil", required, ignore, err)
	}

	return policy
}

// classifyCase is one row of TestClassifyMapsEachFailureToItsStatus: the
// fixture to check against manifest-a, and the status its failure must carry.
type classifyCase struct {
	name    string
	fixture string
	want    Status
}

// classifyCases drives every arm of classify from a real input; BADARMOR has
// two rows because the armor decoder reaches it by two routes.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var classifyCases = []classifyCase{
	{name: "a signature over a different manifest", fixture: sigOverManifestB, want: StatusBadSig},
	{name: "a signature by a key the keyring does not hold", fixture: sigOutsiderKey, want: StatusNoPubKey},
	{name: "a signature by an expired key", fixture: sigExpiredKey, want: StatusExpKeySig},
	{name: "a signature by a revoked key", fixture: sigRevokedKey, want: StatusRevKeySig},
	{name: "a signature that has itself expired", fixture: sigExpiredSig, want: StatusExpSig},
	{name: "armor whose line breaks are gone", fixture: sigFoldedArmor, want: StatusBadArmor},
	{name: "armor whose body is not base64", fixture: sigBadBase64, want: StatusBadArmor},
	{name: "a blob carrying nothing", fixture: noDataBlob, want: StatusNoData},
	// An empty envelope must reach the same status as an empty blob, so the shape
	// an attacker chooses gains nothing; both envelope layouts are rows.
	{name: "an empty armor envelope", fixture: emptyArmorBlob, want: StatusNoData},
	{name: "an empty armor envelope with a checksum line", fixture: emptyArmorCRCBlob, want: StatusNoData},
	// A key block handed over as a signature is a well-formed OpenPGP object of
	// the wrong kind: nothing about it is a signature that failed, which is
	// exactly the shape the vocabulary's ERRSIG covers.
	{name: "a blob that is not a signature at all", fixture: keyringFixture, want: StatusErrSig},
	// The framing gate refuses with ERRSIG, the status the ungated library call
	// reached after a 4 GiB allocation; ignore lists key on status, so it stays.
	{name: "a v6 signature declaring more subpackets than it holds", fixture: amplifyingBlob, want: StatusErrSig},
}

// TestClassifyMapsEachFailureToItsStatus pins the status vocabulary against
// real gpg output, so a go-crypto release that reshapes a failure fails here
// instead of silently collapsing it into ERRSIG.
func TestClassifyMapsEachFailureToItsStatus(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	// The positive control for every row below: this keyring and this manifest
	// do verify a signature, so a row that reports a failure is reporting its
	// own fixture rather than a setup that could never have succeeded.
	if _, err := checkOne(manifest, readFixture(t, sigValidArmored), kr); err != nil {
		t.Fatalf("positive control: checkOne(%s) = %v, want nil", sigValidArmored, err)
	}

	for _, tc := range classifyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := checkOne(manifest, blobBytes(t, tc.fixture), kr)
			if err == nil {
				t.Fatalf("checkOne(%s) = nil, want a failure to classify", tc.name)
			}
			if got := classify(err); got != tc.want {
				t.Fatalf("classify(%s) = %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}

// TestEmptyPacketStreamWouldReportNoPubKey pins why errNoSignatureData exists:
// go-crypto answers an empty packet stream as NO_PUBKEY, an ignorable status,
// so an empty source must get checkOne's own NODATA verdict first.
func TestEmptyPacketStreamWouldReportNoPubKey(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	// The binary encoding, since this call takes a packet stream rather than an
	// armor envelope.
	signer, err := openpgp.CheckDetachedSignature(kr.entities,
		bytes.NewReader(manifest), bytes.NewReader(readFixture(t, sigValidBinary)), nil)
	if err != nil || signer == nil {
		t.Fatalf("positive control: CheckDetachedSignature(%s) = %v, %v, want an entity and nil", sigValidBinary, signer, err)
	}

	_, err = openpgp.CheckDetachedSignature(kr.entities, bytes.NewReader(manifest), bytes.NewReader(nil), nil)
	if got := classify(err); got != StatusNoPubKey {
		t.Fatalf("CheckDetachedSignature(an empty packet stream) = %v, classified %s, want %s", err, got, StatusNoPubKey)
	}
}

// TestCheckOneRefusesAnArmorBlockOfTheWrongKind pins checkOne's block type
// check: the header sniff searches the whole blob but the decode reads only the
// first block, so a key export with a signature appended is refused as ERRSIG.
func TestCheckOneRefusesAnArmorBlockOfTheWrongKind(t *testing.T) {
	t.Parallel()

	// The whole message hand-spelled, and apart from the production string it
	// checks, so that a mutation to that string cannot reshape the expectation
	// into agreeing with it.
	const want = "openpgp: invalid argument: expected 'PGP SIGNATURE', got: PGP PUBLIC KEY BLOCK"

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	signature := readFixture(t, sigValidArmored)
	if _, err := checkOne(manifest, signature, kr); err != nil {
		t.Fatalf("positive control: checkOne(%s) = %v, want nil", sigValidArmored, err)
	}

	blob := make([]byte, 0, len(readFixture(t, armoredFixture))+len(signature))
	blob = append(blob, readFixture(t, armoredFixture)...)
	blob = append(blob, signature...)

	_, err := checkOne(manifest, blob, kr)
	if err == nil || err.Error() != want {
		t.Fatalf("checkOne(a key block with a signature behind it) = %v, want %q", err, want)
	}
	// The status is asked separately, because what an operator's ignore list
	// covers is decided by it rather than by the message: a well-formed OpenPGP
	// object of the wrong kind is not a signature that failed.
	if got := classify(err); got != StatusErrSig {
		t.Fatalf("classify(a key block with a signature behind it) = %s, want %s", got, StatusErrSig)
	}
}

// verifyCase is one row of the semantic matrix: the configured policy, the
// blobs gathered, and everything the Result must say about them.
type verifyCase struct {
	name         string
	required     string
	ignore       []string
	blobs        []string
	wantVerified int
	wantFailures int
	wantPass     bool
	wantVacuous  bool
}

// verifyCases spells out ansible's decision function one cell at a time; the
// zero-blob passes are frozen parity, reported through Result.VacuousPass.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var verifyCases = []verifyCase{
	{name: "count of one with nothing gathered", required: "1", wantPass: true, wantVacuous: true},
	{name: "strict count of one with nothing gathered", required: "+1"},
	{name: "all with nothing gathered", required: "all", wantPass: true, wantVacuous: true},
	{name: "strict all with nothing gathered", required: "+all"},
	{
		name: "count of one with one valid signature", required: "1",
		blobs: []string{sigValidArmored}, wantVerified: 1, wantPass: true,
	},
	{
		name: "count of one with one invalid signature", required: "1",
		blobs: []string{sigOverManifestB}, wantFailures: 1,
	},
	// The tolerance rule: a failure below the required count is recorded and
	// reported, and the run still passes because the count was reached anyway.
	{
		name: "count of two tolerates a failure below the count", required: "2",
		blobs:        []string{sigValidArmored, sigOverManifestB, sigSecondSigner},
		wantVerified: 2, wantFailures: 1, wantPass: true,
	},
	// Reordered, the count is reached before the invalid blob, which is then never
	// checked or recorded: the row that makes the early stop observable.
	{
		name: "count of two stops at the second signer", required: "2",
		blobs:        []string{sigValidArmored, sigSecondSigner, sigOverManifestB},
		wantVerified: 2, wantPass: true,
	},
	// One signature must not stand in for a second signer: the blob list crosses a
	// trust boundary, so a count of blobs would pass the three replay rows below.
	{
		name: "count of two refuses one signature supplied twice", required: "2",
		blobs:        []string{sigValidArmored, sigValidArmored},
		wantVerified: 1,
	},
	{
		name: "count of three refuses one signature supplied three times", required: "3",
		blobs:        []string{sigValidArmored, sigValidArmored, sigValidArmored},
		wantVerified: 1,
	},
	// The same key in its two encodings: not byte-identical, so a replay check
	// that hashed the blob would pass this while one key still signed both.
	{
		name: "count of two refuses one key under both encodings", required: "2",
		blobs:        []string{sigValidArmored, sigValidBinary},
		wantVerified: 1,
	},
	// The positive control for the replay rows: two distinct signers satisfy a
	// count of two on the same policy and keyring.
	{
		name: "count of two accepts two distinct signers", required: "2",
		blobs:        []string{sigValidArmored, sigSecondSigner},
		wantVerified: 2, wantPass: true,
	},
	{
		name: "all refuses one failure beside a valid signature", required: "all",
		blobs: []string{sigValidArmored, sigOverManifestB}, wantVerified: 1, wantFailures: 1,
	},
	{
		name: "all tolerates a failure whose status is ignored", required: "all",
		ignore: []string{string(StatusNoPubKey)},
		blobs:  []string{sigValidArmored, sigOutsiderKey}, wantVerified: 1, wantPass: true,
	},
	// An ignored failure moves neither counter, so under "all" it is a vacuous pass
	// with a blob in hand: the one a caller could not derive from an empty list.
	{
		name: "all with only an ignored failure", required: "all",
		ignore: []string{string(StatusNoPubKey)},
		blobs:  []string{sigOutsiderKey}, wantPass: true, wantVacuous: true,
	},
	// Under a counted policy the same blobs refuse, as in ansible: the vacuous
	// clause asks whether a signature was gathered, and an ignored one was.
	{
		name: "count of one with a single ignored failure", required: "1",
		ignore: []string{string(StatusNoPubKey)}, blobs: []string{sigOutsiderKey},
	},
	{
		name: "strict count of one with a single ignored failure", required: "+1",
		ignore: []string{string(StatusNoPubKey)}, blobs: []string{sigOutsiderKey},
	},
	// A floor of zero refuses a signature that verified, as ansible does, which is
	// why the count clause is an equality rather than an "at least".
	{name: "count of zero with nothing gathered", required: "0", wantPass: true, wantVacuous: true},
	{
		name: "count of zero refuses a signature that verified", required: "0",
		blobs: []string{sigValidArmored}, wantVerified: 1,
	},
	// Every damaged fixture tolerated by its own status, which proves each refusal
	// row's fixture was actually checked and classified.
	{
		name: "all tolerates every status it was told to", required: "all",
		ignore: []string{
			string(StatusExpKeySig), string(StatusRevKeySig), string(StatusExpSig),
			string(StatusBadArmor), string(StatusNoData),
		},
		blobs: []string{
			sigExpiredKey, sigRevokedKey, sigExpiredSig, sigFoldedArmor, sigBadBase64, noDataBlob,
		},
		wantPass: true, wantVacuous: true,
	},
}

// TestVerifySemanticMatrix pins ansible-core 2.21.2's verify_file_signatures
// cell by cell, including its last clause `(not detached_signatures) or
// (require_count == successful)`, where an ignored blob still counts as gathered.
func TestVerifySemanticMatrix(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	for _, tc := range verifyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			policy := testPolicy(t, tc.required, tc.ignore)
			blobs := signatureBlobs(t, tc.blobs...)

			result, err := verifyBlobs(manifest, blobs, kr, policy)
			passed := err == nil
			if passed != tc.wantPass {
				t.Fatalf("Verify(%s) passed = %t, want %t (err: %v)", tc.name, passed, tc.wantPass, err)
			}
			if !passed && !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
				t.Fatalf("Verify(%s) error = %v, want the verification-failed sentinel", tc.name, err)
			}
			if result.Verified != tc.wantVerified {
				t.Fatalf("Verify(%s) Verified = %d, want %d", tc.name, result.Verified, tc.wantVerified)
			}
			if len(result.Failures) != tc.wantFailures {
				t.Fatalf("Verify(%s) recorded %d failures, want %d", tc.name, len(result.Failures), tc.wantFailures)
			}
			if result.VacuousPass != tc.wantVacuous {
				t.Fatalf("Verify(%s) VacuousPass = %t, want %t", tc.name, result.VacuousPass, tc.wantVacuous)
			}
		})
	}
}

// TestVerifyReportsTheFixtureItRefused shows the outsider and other-manifest
// fixtures verify once the missing key or manifest is supplied, so their matrix
// statuses are verdicts about context rather than about broken blobs.
func TestVerifyReportsTheFixtureItRefused(t *testing.T) {
	t.Parallel()

	manifestA := readFixture(t, manifestAFixture)
	manifestB := readFixture(t, manifestBFixture)

	outsiderKeyring := loadTestKeyring(t, outsiderKeyringFixture)
	if _, err := checkOne(manifestA, readFixture(t, sigOutsiderKey), outsiderKeyring); err != nil {
		t.Fatalf("checkOne(%s) against the keyring holding its key = %v, want nil", sigOutsiderKey, err)
	}

	kr := loadTestKeyring(t, keyringFixture)
	if _, err := checkOne(manifestB, readFixture(t, sigOverManifestB), kr); err != nil {
		t.Fatalf("checkOne(%s) against the manifest it signs = %v, want nil", sigOverManifestB, err)
	}
}

// TestVerifyAcceptsBothSignatureEncodings pins the binary branch: the binary
// signature verifies through Verify but fails the armored entry point.
func TestVerifyAcceptsBothSignatureEncodings(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	policy := testPolicy(t, "1", nil)

	for _, name := range []string{sigValidArmored, sigValidBinary} {
		result, err := verifyBlobs(manifest, signatureBlobs(t, name), kr, policy)
		if err != nil {
			t.Fatalf("Verify(%s) = %v, want nil", name, err)
		}
		if result.Verified != 1 {
			t.Fatalf("Verify(%s) Verified = %d, want 1", name, result.Verified)
		}
	}

	binary := readFixture(t, sigValidBinary)
	_, err := openpgp.CheckArmoredDetachedSignature(kr.entities, bytes.NewReader(manifest), bytes.NewReader(binary), nil)
	if err == nil {
		t.Fatalf("openpgp.CheckArmoredDetachedSignature(%s) = nil, want a failure: the sniff would then be pointless", sigValidBinary)
	}
}

// TestVerifyRefusesWithoutKeyMaterial pins the fail-closed guard: no keyring is
// a configuration error, not a verification verdict, and never reaches the
// library, where a nil keyring would panic.
func TestVerifyRefusesWithoutKeyMaterial(t *testing.T) {
	t.Parallel()

	manifest := readFixture(t, manifestAFixture)
	blobs := signatureBlobs(t, sigValidArmored)
	policy := testPolicy(t, "1", nil)

	// The same call with key material passes, so the refusal below is the
	// missing keyring and not this manifest or this blob.
	if _, err := verifyBlobs(manifest, blobs, loadTestKeyring(t, keyringFixture), policy); err != nil {
		t.Fatalf("positive control: Verify with a keyring = %v, want nil", err)
	}

	_, err := verifyBlobs(manifest, blobs, nil, policy)
	if !errors.Is(err, helpers.ErrKeyringRequired) {
		t.Fatalf("Verify with no keyring = %v, want the keyring-required sentinel", err)
	}
	if errors.Is(err, helpers.ErrSignatureVerificationFailed) {
		t.Fatalf("Verify with no keyring reports a verification verdict: %v", err)
	}
}

// clauseCase is one row of TestVerifyErrorNamesTheClauseThatFired.
type clauseCase struct {
	name     string
	required string
	want     string
	blobs    []string
}

// clauseCases covers all three refusal clauses, with phrases written out so a
// reworded production message fails here.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var clauseCases = []clauseCase{
	{name: "strict with nothing verified", required: "+1", blobs: []string{sigOverManifestB}, want: "no valid signature"},
	{
		name: "all with a failure", required: "all",
		blobs: []string{sigValidArmored, sigOverManifestB}, want: "some signatures failed",
	},
	{
		name: "count not reached", required: "2",
		blobs: []string{sigValidArmored, sigOverManifestB}, want: "fewer valid signatures than required: got 1, need 2",
	},
}

// TestVerifyErrorNamesTheClauseThatFired pins that a refusal names its clause
// and every failure behind it: the clauses share one exit code, so the message
// is the only place their different remedies survive.
func TestVerifyErrorNamesTheClauseThatFired(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	for _, tc := range clauseCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			policy := testPolicy(t, tc.required, nil)
			_, err := verifyBlobs(manifest, signatureBlobs(t, tc.blobs...), kr, policy)
			if err == nil {
				t.Fatalf("Verify(%s) = nil, want a refusal", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Verify(%s) error does not name the clause %q:\n%v", tc.name, tc.want, err)
			}
			// Causes are joined, not rendered, so go-crypto's own error stays reachable;
			// errors.AsType is asked directly, as classify's helper would hide a bad join.
			if _, ok := errors.AsType[pgperrors.SignatureError](err); !ok {
				t.Fatalf("Verify(%s) error does not carry the joined cause:\n%v", tc.name, err)
			}
			if !strings.Contains(err.Error(), sigOverManifestB) {
				t.Fatalf("Verify(%s) error does not name the failing origin:\n%v", tc.name, err)
			}
		})
	}
}

// TestVerifyIsDeterministicAcrossRuns pins the walk in the caller's order where
// ansible iterates a set: three failures around one success must report in the
// same order on every run, each over a fresh source.
func TestVerifyIsDeterministicAcrossRuns(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	policy := testPolicy(t, "all", nil)
	blobs := signatureBlobs(t, sigOutsiderKey, sigOverManifestB, sigValidArmored, sigExpiredKey)

	want := []string{
		sigOutsiderKey + " " + string(StatusNoPubKey),
		sigOverManifestB + " " + string(StatusBadSig),
		sigExpiredKey + " " + string(StatusExpKeySig),
	}

	for run := range deterministicRuns {
		result, err := verifyBlobs(manifest, blobs, kr, policy)
		if err == nil {
			t.Fatalf("run %d: Verify = nil, want a refusal", run)
		}
		got := make([]string, 0, len(result.Failures))
		for _, failure := range result.Failures {
			got = append(got, failure.Origin+" "+string(failure.Status))
		}
		if len(got) != len(want) {
			t.Fatalf("run %d: Verify reported %d failures %v, want %d", run, len(got), got, len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("run %d: failure %d is %q, want %q", run, i, got[i], want[i])
			}
		}
		if result.Verified != 1 {
			t.Fatalf("run %d: Verified = %d, want 1", run, result.Verified)
		}
	}
}

// TestClassifyNeverAnswersOutsideTheVocabulary pins that classify produces only
// the eight statuses a verdict can carry, and that maxRenderedFailures equals
// that count, so a new producible status must move the render cap too.
func TestClassifyNeverAnswersOutsideTheVocabulary(t *testing.T) {
	t.Parallel()

	producible := map[Status]struct{}{
		StatusBadSig: {}, StatusErrSig: {}, StatusNoPubKey: {}, StatusExpKeySig: {},
		StatusRevKeySig: {}, StatusExpSig: {}, StatusNoData: {}, StatusBadArmor: {},
	}
	if len(producible) != maxRenderedFailures {
		t.Fatalf("len(producible) = %d, want maxRenderedFailures (%d): the render cap is derived from this vocabulary's size",
			len(producible), maxRenderedFailures)
	}

	errs := []error{
		errNoSignatureData,
		errMalformedSignaturePacket,
		pgperrors.ErrUnknownIssuer,
		pgperrors.ErrKeyRevoked,
		pgperrors.ErrKeyExpired,
		pgperrors.ErrSignatureExpired,
		pgperrors.SignatureError("EdDSA verification failure"),
		errUnanticipated,
	}
	for _, err := range errs {
		if _, ok := producible[classify(err)]; !ok {
			t.Fatalf("classify(%v) = %s, which is outside the eight statuses a verdict may carry", err, classify(err))
		}
	}
}

// TestVerifyStrictDoesNotMakeAFailureFatal pins that "+1" only adds "at least
// one verified": two failures beside a success pass, and only "all" refuses
// the same blobs.
func TestVerifyStrictDoesNotMakeAFailureFatal(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	// The success is last, so both failures are checked and recorded before the
	// count is reached; with it first the loop would stop and never see them.
	blobs := signatureBlobs(t, sigOverManifestB, sigOutsiderKey, sigValidArmored)

	result, err := verifyBlobs(manifest, blobs, kr, testPolicy(t, "+1", nil))
	if err != nil {
		t.Fatalf("Verify(+1 with two failures beside a success) = %v, want nil", err)
	}
	if len(result.Failures) != 2 {
		t.Fatalf("Verify(+1) recorded %d failures, want 2 checked and tolerated", len(result.Failures))
	}
	if result.Verified != 1 {
		t.Fatalf("Verify(+1) Verified = %d, want 1", result.Verified)
	}

	// The same blobs under the spelling that does make a failure fatal.
	if _, err = verifyBlobs(manifest, blobs, kr, testPolicy(t, "all", nil)); err == nil {
		t.Fatalf("Verify(all with two failures beside a success) = nil, want a refusal")
	}
}

// TestVerifyNamesAFloorOfZeroInItsOwnWords pins the count-of-zero refusal's own
// wording, since "fewer valid signatures than required: got 1, need 0" would
// describe the opposite of what happened.
func TestVerifyNamesAFloorOfZeroInItsOwnWords(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	// The control: a floor of zero with nothing gathered passes, so the refusal
	// below is the signature that verified and not the spelling itself.
	if _, err := verifyBlobs(manifest, nil, kr, testPolicy(t, "0", nil)); err != nil {
		t.Fatalf("positive control: Verify(0 with nothing gathered) = %v, want nil", err)
	}

	_, err := verifyBlobs(manifest, signatureBlobs(t, sigValidArmored), kr, testPolicy(t, "0", nil))
	if err == nil {
		t.Fatalf("Verify(0 with a signature that verified) = nil, want a refusal")
	}
	const want = "a required count of 0 is satisfied only while nothing verifies; 1 did"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Verify(0) error does not carry %q:\n%v", want, err)
	}
	if strings.Contains(err.Error(), "fewer valid signatures") {
		t.Fatalf("Verify(0) error still reads as a shortfall:\n%v", err)
	}
}

// TestVerifyStopsPullingOnceTheCountIsMet pins that the pull stops once the
// count is reached, so a caller fetching inside it never fetches past the
// count; the third blob would fail if it were checked.
func TestVerifyStopsPullingOnceTheCountIsMet(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	blobs := signatureBlobs(t, sigValidArmored, sigSecondSigner, sigOverManifestB)

	next, pulled := blobSource(blobs)
	result, err := Verify(manifest, next, kr, testPolicy(t, "2", nil))
	if err != nil {
		t.Fatalf("Verify(count of two) = %v, want nil", err)
	}
	if result.Verified != 2 {
		t.Fatalf("Verify(count of two) Verified = %d, want 2", result.Verified)
	}
	if *pulled != 2 {
		t.Fatalf("Verify(count of two) pulled %d blobs, want 2 of the 3 supplied", *pulled)
	}
}

// TestVerifySurfacesAGatherErrorWhereItSits pins that a source that cannot be
// fetched fails at its position, with everything checked before it reported,
// and is never a verification verdict.
func TestVerifySurfacesAGatherErrorWhereItSits(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	policy := testPolicy(t, "2", nil)
	good := signatureBlobs(t, sigValidArmored)[0]

	// One blob checked, then a source that could not be fetched.
	pulls := 0
	next := func() (Blob, bool, error) {
		pulls++
		if pulls == 1 {
			return good, true, nil
		}

		return Blob{}, false, errGatherFailed
	}

	result, err := Verify(manifest, next, kr, policy)
	if !errors.Is(err, errGatherFailed) {
		t.Fatalf("Verify with a failing source = %v, want the gather error", err)
	}
	if errors.Is(err, helpers.ErrSignatureVerificationFailed) {
		t.Fatalf("Verify reports a gather failure as a verification verdict: %v", err)
	}
	if result.Verified != 1 {
		t.Fatalf("Verify with a failing source Verified = %d, want the 1 checked before it", result.Verified)
	}

	// The same failure on the first pull: nothing was checked, and the count
	// clause never gets to answer for a list that was never gathered.
	first := func() (Blob, bool, error) { return Blob{}, false, errGatherFailed }
	result, err = Verify(manifest, first, kr, policy)
	if !errors.Is(err, errGatherFailed) {
		t.Fatalf("Verify with a source failing at once = %v, want the gather error", err)
	}
	if result.Verified != 0 {
		t.Fatalf("Verify with a source failing at once Verified = %d, want 0", result.Verified)
	}
}

// errRenderCapSentinel is the Err of buildRenderCapFailures' last failure, so
// errors.Is can prove a cause past the render cap stayed reachable.
var errRenderCapSentinel = errors.New("verify_test: a cause past the render cap")

// errGenericRenderCapCause is Err for every failure buildRenderCapFailures
// builds except the last one: a single fixed value is enough, since only the
// last failure's own Err is ever asked about by errors.Is.
var errGenericRenderCapCause = errors.New("verify_test: a signature that failed to verify")

// buildRenderCapFailures builds n failures with distinct origins; callers pass
// literal counts, not maxRenderedFailures, so the fixture cannot move with the
// cap it probes.
func buildRenderCapFailures(n int) []Failure {
	failures := make([]Failure, n)
	for i := range failures {
		failures[i] = Failure{Origin: fmt.Sprintf("origin-%d", i), Status: StatusBadSig, Err: errGenericRenderCapCause}
	}
	failures[n-1].Err = errRenderCapSentinel

	return failures
}

// TestVerificationErrorBoundsWhatItRendersAndNotWhatItMatches pins that
// maxRenderedFailures bounds only verdictError's message, while every cause
// stays reachable through errors.Is; the counts 9 and 8 are literals.
func TestVerificationErrorBoundsWhatItRendersAndNotWhatItMatches(t *testing.T) {
	t.Parallel()

	policy := testPolicy(t, "1", nil)

	t.Run("past the cap", func(t *testing.T) {
		t.Parallel()

		failures := buildRenderCapFailures(9)
		err := verificationError(policy, 0, failures)
		lastOrigin := failures[len(failures)-1].Origin

		if strings.Contains(err.Error(), lastOrigin) {
			t.Fatalf("verificationError() rendered the origin past the cap (%s):\n%v", lastOrigin, err)
		}

		const wantFooter = "showing the first 8 of 9 signature failures"
		if !strings.Contains(err.Error(), wantFooter) {
			t.Fatalf("verificationError() does not carry %q:\n%v", wantFooter, err)
		}

		if !errors.Is(err, errRenderCapSentinel) {
			t.Fatalf("verificationError() does not reach the cause past the render cap through errors.Is:\n%v", err)
		}
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("verificationError() does not reach helpers.ErrSignatureVerificationFailed:\n%v", err)
		}
	})

	// The positive control: exactly at the cap nothing is hidden and no footer is
	// rendered, so the row above withheld its ninth origin because of the cap.
	t.Run("at the cap", func(t *testing.T) {
		t.Parallel()

		failures := buildRenderCapFailures(8)
		err := verificationError(policy, 0, failures)

		for _, failure := range failures {
			if !strings.Contains(err.Error(), failure.Origin) {
				t.Fatalf("verificationError() at the cap does not render origin %s:\n%v", failure.Origin, err)
			}
		}
		if strings.Contains(err.Error(), "showing the first") {
			t.Fatalf("verificationError() at the cap rendered a footer, though nothing was hidden:\n%v", err)
		}
	})

	// Eight NO_PUBKEY decoys ahead of a BADSIG fill the cap, and Result is dropped
	// on this error path, so the footer is the only place the hidden BADSIG can
	// still reach an operator.
	t.Run("footer names a status the cap hid, not one it rendered", func(t *testing.T) {
		t.Parallel()
		checkFooterNamesAHiddenStatus(t, policy)
	})
}

// checkFooterNamesAHiddenStatus is the render-cap test's third subtest, without
// t.Helper() so a failure is reported at its own assertion line.
//
//nolint:thelper // deliberately no t.Helper(): see the paragraph above.
func checkFooterNamesAHiddenStatus(t *testing.T, policy Policy) {
	statuses := append(repeatedStatuses(StatusNoPubKey, 8), StatusBadSig)
	failures := buildStatusFailures(statuses)
	hiddenOrigin := failures[len(failures)-1].Origin

	err := verificationError(policy, 0, failures)
	lines := strings.Split(err.Error(), "\n")
	footer := lines[len(lines)-1]

	if !strings.Contains(footer, string(StatusBadSig)) {
		t.Fatalf("footer does not name the hidden status BADSIG: %q", footer)
	}
	if strings.Contains(footer, string(StatusNoPubKey)) {
		t.Fatalf("footer names a status that was actually rendered: %q", footer)
	}

	const wantFooterPrefix = "showing the first 8 of 9 signature failures"
	if !strings.Contains(footer, wantFooterPrefix) {
		t.Fatalf("footer does not carry %q: %q", wantFooterPrefix, footer)
	}
	if strings.Contains(err.Error(), hiddenOrigin) {
		t.Fatalf("verificationError() rendered the hidden failure's own origin (%s):\n%v", hiddenOrigin, err)
	}

	// The positive control: when NO_PUBKEY is itself hidden the footer names it, so
	// its absence above means it was rendered, not that no status is ever named.
	controlErr := verificationError(policy, 0, buildStatusFailures(repeatedStatuses(StatusNoPubKey, 9)))
	controlLines := strings.Split(controlErr.Error(), "\n")
	controlFooter := controlLines[len(controlLines)-1]
	if !strings.Contains(controlFooter, string(StatusNoPubKey)) {
		t.Fatalf("control footer does not name NO_PUBKEY: %q", controlFooter)
	}
	if strings.Contains(controlFooter, string(StatusBadSig)) {
		t.Fatalf("control footer names a status this fixture never carried: %q", controlFooter)
	}

	// A status shared by several hidden failures is named once.
	dupStatuses := append(repeatedStatuses(StatusNoPubKey, 8), StatusBadSig, StatusBadSig)
	dupErr := verificationError(policy, 0, buildStatusFailures(dupStatuses))
	dupLines := strings.Split(dupErr.Error(), "\n")
	dupFooter := dupLines[len(dupLines)-1]
	if got := strings.Count(dupFooter, string(StatusBadSig)); got != 1 {
		t.Fatalf("footer names BADSIG %d times, want exactly 1 (the dedupe): %q", got, dupFooter)
	}
}

// repeatedStatuses returns n copies of status, for composing a
// buildStatusFailures fixture out of one run of a status followed by another.
func repeatedStatuses(status Status, n int) []Status {
	statuses := make([]Status, n)
	for i := range statuses {
		statuses[i] = status
	}

	return statuses
}

// buildStatusFailures builds one Failure per element of statuses, each with
// its own origin, so a render cap's footer can be observed by which STATUSES
// - not only which origins - made it past the cap.
func buildStatusFailures(statuses []Status) []Failure {
	failures := make([]Failure, len(statuses))
	for i, status := range statuses {
		failures[i] = Failure{Origin: fmt.Sprintf("origin-%d", i), Status: status, Err: errGenericRenderCapCause}
	}

	return failures
}
