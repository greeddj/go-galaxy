package signature

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Fixtures under testdata are committed gpg 2.5.21 exports, never built at test
// time. secret.gpg and secret.asc hold a discardable key trusted by nothing, and
// two-keys.asc is public.asc and second.asc catted, each ending in a newline.
const (
	testdataDir = "testdata"

	armoredFixture = "public.asc"
	binaryFixture  = "public.gpg"
	keyboxFixture  = "pubring.kbx"
	garbageFixture = "garbage.bin"
	emptyFixture   = "empty.gpg"
	secondFixture  = "second.asc"
	twoKeyFixture  = "two-keys.asc"
	// secretBinaryFixture and secretArmoredFixture are the two encodings of
	// one secret key export, so the refusal is shown to be this tool's policy
	// rather than a property of whichever reader the bytes reach.
	secretBinaryFixture  = "secret.gpg"
	secretArmoredFixture = "secret.asc"
	secretPublicFixture  = "secret-public.asc"
	// absentFixture names a file testdata deliberately does not hold.
	absentFixture = "no-such-keyring.asc"

	// signatureArmorBlock is a well-formed armor block that is not key
	// material. Its body is never decoded: openpgp.ReadArmoredKeyRing refuses
	// the block on its type before reading any of it.
	signatureArmorBlock = "-----BEGIN PGP SIGNATURE-----\n\naGVsbG8=\n-----END PGP SIGNATURE-----\n"

	// ceilingHeadroom is how far above the armored fixture's own length the
	// ceiling test sets its limit, so the control file fits and the padded one
	// does not.
	ceilingHeadroom = 16
	// ceilingFiller is how many bytes the ceiling test appends after the armor
	// block's end line; its margin over ceilingHeadroom keeps a truncated read
	// inside the filler, where the truncation would still parse.
	ceilingFiller = 1024
)

func fixturePath(name string) string {
	return filepath.Join(testdataDir, name)
}

// requirePositiveControl fails the test unless the armored fixture loads from
// the same testdata directory, so a refusal assertion cannot pass against a
// loader that refuses everything or a directory the test cannot read.
func requirePositiveControl(t *testing.T) {
	t.Helper()

	kr, err := LoadKeyring(fixturePath(armoredFixture))
	if err != nil {
		t.Fatalf("positive control: LoadKeyring(%s) = %v, want nil", armoredFixture, err)
	}
	if kr.Len() == 0 {
		t.Fatalf("positive control: LoadKeyring(%s) Len() = 0, want > 0", armoredFixture)
	}
}

// keyFingerprints returns the hex primary-key fingerprints a keyring holds, so
// a case can compare which keys were read rather than only how many.
func keyFingerprints(kr *Keyring) []string {
	out := make([]string, 0, kr.Len())
	for _, entity := range kr.entities {
		out = append(out, hex.EncodeToString(entity.PrimaryKey.Fingerprint))
	}

	return out
}

// loadFingerprint loads a fixture that must hold exactly one key and returns
// that key's fingerprint.
func loadFingerprint(t *testing.T, name string) string {
	t.Helper()

	kr, err := LoadKeyring(fixturePath(name))
	if err != nil {
		t.Fatalf("LoadKeyring(%s) = %v, want nil", name, err)
	}
	fprs := keyFingerprints(kr)
	if len(fprs) != 1 {
		t.Fatalf("LoadKeyring(%s) holds %d keys, want exactly 1", name, len(fprs))
	}

	return fprs[0]
}

func TestLoadKeyringAcceptsArmoredExport(t *testing.T) {
	t.Parallel()

	path := fixturePath(armoredFixture)

	kr, err := LoadKeyring(path)
	if err != nil {
		t.Fatalf("LoadKeyring(%s) = %v, want nil", armoredFixture, err)
	}
	if kr.Len() == 0 {
		t.Fatalf("LoadKeyring(%s) Len() = 0, want > 0", armoredFixture)
	}
	if kr.Path() != path {
		t.Fatalf("Path() = %q, want %q", kr.Path(), path)
	}
}

// TestLoadKeyringAcceptsBinaryKeyring pins the binary reader: the binary and
// armored exports of one key must load to the same non-zero entity count.
func TestLoadKeyringAcceptsBinaryKeyring(t *testing.T) {
	t.Parallel()

	binary, err := LoadKeyring(fixturePath(binaryFixture))
	if err != nil {
		t.Fatalf("LoadKeyring(%s) = %v, want nil", binaryFixture, err)
	}
	if binary.Len() == 0 {
		t.Fatalf("LoadKeyring(%s) Len() = 0, want > 0", binaryFixture)
	}

	armored, err := LoadKeyring(fixturePath(armoredFixture))
	if err != nil {
		t.Fatalf("LoadKeyring(%s) = %v, want nil", armoredFixture, err)
	}
	if binary.Len() != armored.Len() {
		t.Fatalf("Len() = %d for %s, %d for %s, want equal", binary.Len(), binaryFixture, armored.Len(), armoredFixture)
	}
}

// TestLoadKeyringReadsEveryArmorBlock pins that a catted multi-key keyring loads
// every block's key, where openpgp.ReadArmoredKeyRing alone would silently
// return the first block's keys with a nil error.
func TestLoadKeyringReadsEveryArmorBlock(t *testing.T) {
	t.Parallel()

	first := loadFingerprint(t, armoredFixture)
	second := loadFingerprint(t, secondFixture)
	// The two blocks have to be different keys, or a count cannot tell "read
	// both blocks" from "read the first block twice" - and would stop telling
	// them apart entirely if the library ever deduplicated by fingerprint.
	if first == second {
		t.Fatalf("%s and %s carry the same key %s, so this case proves nothing", armoredFixture, secondFixture, first)
	}

	kr, err := LoadKeyring(fixturePath(twoKeyFixture))
	if err != nil {
		t.Fatalf("LoadKeyring(%s) = %v, want nil", twoKeyFixture, err)
	}
	if kr.Len() != 2 {
		t.Fatalf("LoadKeyring(%s) Len() = %d, want 2", twoKeyFixture, kr.Len())
	}

	got := keyFingerprints(kr)
	// The count alone would pass on a two-keys.asc rebuilt from two copies of
	// one export; the fingerprints are what catch that fixture defect.
	if !slices.Contains(got, first) || !slices.Contains(got, second) {
		t.Fatalf("LoadKeyring(%s) holds %v, want both %s and %s", twoKeyFixture, got, first, second)
	}
}

// TestLoadKeyringRefusesSecretKeyMaterial pins that a keyring holding a private
// key is refused in both encodings, with a message naming the problem and the
// public export to make instead.
func TestLoadKeyringRefusesSecretKeyMaterial(t *testing.T) {
	t.Parallel()
	requirePositiveControl(t)

	// Same-key control: the public half of the key below loads, so a refusal is
	// the secret material rather than this key being unreadable.
	if _, err := LoadKeyring(fixturePath(secretPublicFixture)); err != nil {
		t.Fatalf("positive control: LoadKeyring(%s) = %v, want nil", secretPublicFixture, err)
	}

	for _, name := range []string{secretBinaryFixture, secretArmoredFixture} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadKeyring(fixturePath(name))
			if !errors.Is(err, helpers.ErrKeyringUnreadable) {
				t.Fatalf("LoadKeyring(%s) error = %v, want the unreadable sentinel", name, err)
			}
			// The message must name secret material: an armored secret key sent to
			// the binary reader would be refused as malformed packet framing instead.
			if !strings.Contains(err.Error(), "secret key material") {
				t.Fatalf("LoadKeyring(%s) error does not name the problem:\n%v", name, err)
			}
			if !strings.Contains(err.Error(), "--export --armor") {
				t.Fatalf("LoadKeyring(%s) error does not name the remedy:\n%v", name, err)
			}
		})
	}
}

// TestHoldsSecretKeyReportsMaterialInEitherPlace pins holdsSecretKey on
// hand-built entities, since the packet walk keeps every file from reaching it;
// the false rows are same-shape controls without private material.
func TestHoldsSecretKeyReportsMaterialInEitherPlace(t *testing.T) {
	t.Parallel()

	public := &packet.PublicKey{}
	private := &packet.PrivateKey{}

	for _, tc := range []struct {
		name     string
		entities openpgp.EntityList
		want     bool
	}{
		{name: "no entities at all", entities: nil},
		{name: "a public primary with a public subkey", entities: openpgp.EntityList{{
			PrimaryKey: public,
			Subkeys:    []openpgp.Subkey{{PublicKey: public}},
		}}},
		{name: "a private primary", entities: openpgp.EntityList{{
			PrimaryKey: public,
			PrivateKey: private,
		}}, want: true},
		{name: "a public primary with a private subkey", entities: openpgp.EntityList{{
			PrimaryKey: public,
			Subkeys:    []openpgp.Subkey{{PublicKey: public}, {PublicKey: public, PrivateKey: private}},
		}}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := holdsSecretKey(tc.entities); got != tc.want {
				t.Fatalf("holdsSecretKey(%s) = %t, want %t", tc.name, got, tc.want)
			}
		})
	}
}

// TestLoadKeyringRefusesADirectory pins that a read failing after a successful
// open (a directory, portably) is ErrKeyringUnreadable naming the read error,
// not a readable file reported as holding no keys.
func TestLoadKeyringRefusesADirectory(t *testing.T) {
	t.Parallel()
	requirePositiveControl(t)

	dir := filepath.Join(t.TempDir(), "keyring.d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("make the directory: %v", err)
	}

	_, err := LoadKeyring(dir)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("LoadKeyring(a directory) = %v, want the read failure to be named", err)
	}
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("LoadKeyring(a directory) error = %v, want the unreadable sentinel", err)
	}
}

// TestArmoredKeyRingRefusesANonFinalBlock pins that a bad block anywhere but
// last stops the whole file rather than being skipped; the control is the key
// export alone, so the refusal is the leading block's.
func TestArmoredKeyRingRefusesANonFinalBlock(t *testing.T) {
	t.Parallel()

	armored := readFixture(t, armoredFixture)
	if _, err := readEntities(armored); err != nil {
		t.Fatalf("positive control: readEntities(%s) = %v, want nil", armoredFixture, err)
	}

	// Each leader ends with its own newline, and the block count below is what
	// says so: a leader glued to the key block's opening line would present one
	// block rather than two, and the file would then be measuring nothing.
	for _, tc := range []struct {
		name   string
		want   string
		leader []byte
	}{
		{
			name:   "a signature block ahead of the key block",
			leader: readFixture(t, sigValidArmored),
			want:   "openpgp: invalid argument: expected public or private key block, got: PGP SIGNATURE",
		},
		{
			// An opening line with nothing behind it: the decoder runs out of
			// input in its header section and reports io.EOF.
			name:   "an opening line with no block behind it",
			leader: []byte("-----BEGIN NOT REALLY AN ARMOR BLOCK-----\n"),
			want:   "openpgp: invalid argument: no armored data found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			file := make([]byte, 0, len(tc.leader)+len(armored))
			file = append(file, tc.leader...)
			file = append(file, armored...)
			if got := countArmorBlocks(file); got != 2 {
				t.Fatalf("the fixture presents %d armor blocks, want 2", got)
			}

			entities, err := readEntities(file)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("readEntities(%s) = %d entities, %v, want %q", tc.name, len(entities), err, tc.want)
			}
		})
	}
}

// TestLoadKeyringRefusesNonKeyArmorBlock pins that a block which is not key
// material stops the load and names its type rather than being passed over, so
// the loader never quietly uses part of a file.
func TestLoadKeyringRefusesNonKeyArmorBlock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	armored, err := os.ReadFile(fixturePath(armoredFixture))
	if err != nil {
		t.Fatalf("read %s: %v", armoredFixture, err)
	}

	controlPath := filepath.Join(dir, "control.asc")
	// #nosec G703 -- dir is this test's own t.TempDir and the leaf is a
	// constant; no part of either path comes from outside this function.
	if writeErr := os.WriteFile(controlPath, armored, 0o600); writeErr != nil {
		t.Fatalf("write control fixture: %v", writeErr)
	}

	mixed := make([]byte, 0, len(armored)+len(signatureArmorBlock))
	mixed = append(mixed, armored...)
	mixed = append(mixed, signatureArmorBlock...)
	mixedPath := filepath.Join(dir, "mixed.asc")
	// #nosec G703 -- same t.TempDir and a constant leaf, as just above.
	if writeErr := os.WriteFile(mixedPath, mixed, 0o600); writeErr != nil {
		t.Fatalf("write mixed fixture: %v", writeErr)
	}

	// The control is the same key block from the same directory, so the
	// refusal below is the appended block being refused, not this directory or
	// this copy of the export.
	if _, ctlErr := LoadKeyring(controlPath); ctlErr != nil {
		t.Fatalf("positive control: LoadKeyring(control.asc) = %v, want nil", ctlErr)
	}

	_, err = LoadKeyring(mixedPath)
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("LoadKeyring(mixed.asc) error = %v, want the unreadable sentinel", err)
	}
	// Naming the block type is what makes the refusal actionable: the operator
	// learns which appended object the file has to lose.
	if !strings.Contains(err.Error(), "PGP SIGNATURE") {
		t.Fatalf("LoadKeyring(mixed.asc) error does not name the offending block type: %v", err)
	}
}

func TestLoadKeyringRejectsKeybox(t *testing.T) {
	t.Parallel()
	requirePositiveControl(t)

	_, err := LoadKeyring(fixturePath(keyboxFixture))
	if !errors.Is(err, helpers.ErrKeyringIsKeybox) {
		t.Fatalf("LoadKeyring(%s) error = %v, want the keybox sentinel", keyboxFixture, err)
	}
	// Naming the export command is why this sentinel is separate from
	// ErrKeyringUnreadable; its wording lives in internal/galaxy/helpers.
	if !strings.Contains(err.Error(), "--export --armor") {
		t.Fatalf("keybox error does not name the export command: %v", err)
	}
}

func TestLoadKeyringRejectsGarbage(t *testing.T) {
	t.Parallel()
	requirePositiveControl(t)

	// A misroute to the keybox branch returns ErrKeyringIsKeybox alone, so this
	// one assertion also covers "not a keybox".
	_, err := LoadKeyring(fixturePath(garbageFixture))
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("LoadKeyring(%s) error = %v, want the unreadable sentinel", garbageFixture, err)
	}
}

// TestLoadKeyringRejectsEmptyKeyring pins the refusal of a keyring holding no
// keys, which openpgp.ReadKeyRing reports as an empty list and a nil error.
func TestLoadKeyringRejectsEmptyKeyring(t *testing.T) {
	t.Parallel()
	requirePositiveControl(t)

	_, err := LoadKeyring(fixturePath(emptyFixture))
	if err == nil {
		t.Fatalf("LoadKeyring(%s) = nil error, want the unreadable sentinel", emptyFixture)
	}
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("LoadKeyring(%s) error = %v, want the unreadable sentinel", emptyFixture, err)
	}
}

func TestLoadKeyringRejectsMissingFile(t *testing.T) {
	t.Parallel()
	requirePositiveControl(t)

	_, err := LoadKeyring(fixturePath(absentFixture))
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("LoadKeyring(%s) error = %v, want the unreadable sentinel", absentFixture, err)
	}
	// The open failure is wrapped with %w rather than rendered, so a caller can
	// still tell an absent keyring from an unparseable one without matching on
	// message text.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadKeyring(%s) error = %v, want fs.ErrNotExist to stay reachable", absentFixture, err)
	}
}

// TestLoadKeyringRefusesFileOverCeiling pins the size ceiling via loadKeyring's
// limit: the filler sits after the end line, so a truncating read would still
// parse and only the refusal rejects the file.
func TestLoadKeyringRefusesFileOverCeiling(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	armored, err := os.ReadFile(fixturePath(armoredFixture))
	if err != nil {
		t.Fatalf("read %s: %v", armoredFixture, err)
	}
	limit := int64(len(armored)) + ceilingHeadroom

	controlPath := filepath.Join(dir, "control.asc")
	// #nosec G703 -- dir is this test's own t.TempDir and the leaf is a
	// constant; no part of either path comes from outside this function.
	if writeErr := os.WriteFile(controlPath, armored, 0o600); writeErr != nil {
		t.Fatalf("write control fixture: %v", writeErr)
	}

	padded := make([]byte, 0, len(armored)+ceilingFiller)
	padded = append(padded, armored...)
	padded = append(padded, bytes.Repeat([]byte("x"), ceilingFiller)...)
	paddedPath := filepath.Join(dir, "padded.asc")
	// #nosec G703 -- same t.TempDir and a constant leaf, as just above.
	if writeErr := os.WriteFile(paddedPath, padded, 0o600); writeErr != nil {
		t.Fatalf("write padded fixture: %v", writeErr)
	}

	// The control shares the directory and the limit with the refusal below, so
	// a refusal cannot be the limit refusing everything.
	if _, ctlErr := loadKeyring(controlPath, limit); ctlErr != nil {
		t.Fatalf("positive control: loadKeyring at limit %d = %v, want nil", limit, ctlErr)
	}

	_, err = loadKeyring(paddedPath, limit)
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("loadKeyring(padded) error = %v, want the unreadable sentinel", err)
	}
}

// TestLooksLikeKeybox pins that the magic is identified at its offset rather
// than searched for. Its offset-8 and offset-zero rows are the discriminating
// pair: the same four bytes are a keybox at one offset and not at the other.
func TestLooksLikeKeybox(t *testing.T) {
	t.Parallel()

	keybox, err := os.ReadFile(fixturePath(keyboxFixture))
	if err != nil {
		t.Fatalf("read %s: %v", keyboxFixture, err)
	}
	armored, err := os.ReadFile(fixturePath(armoredFixture))
	if err != nil {
		t.Fatalf("read %s: %v", armoredFixture, err)
	}
	binary, err := os.ReadFile(fixturePath(binaryFixture))
	if err != nil {
		t.Fatalf("read %s: %v", binaryFixture, err)
	}

	tests := []struct {
		name  string
		input []byte
		want  bool
	}{
		{name: "real keybox fixture", input: keybox, want: true},
		{name: "magic at its offset behind arbitrary bytes", input: []byte("01234567KBXf"), want: true},
		{name: "magic at offset zero", input: []byte("KBXf01234567"), want: false},
		{name: "armored export", input: armored, want: false},
		{name: "binary export", input: binary, want: false},
		{name: "shorter than the header prefix", input: []byte("0123456KBX"), want: false},
		{name: "empty", input: nil, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := looksLikeKeybox(tc.input); got != tc.want {
				t.Fatalf("looksLikeKeybox(%s) = %t, want %t", tc.name, got, tc.want)
			}
		})
	}
}

// gluedBlockCount is how many armor blocks the glued case builds; a third block
// separates reading only the first block from reading one per opening line.
const gluedBlockCount = 3

// TestGluedArmorBlocksAreRefused pins that armor blocks glued without a newline,
// which the cut sees as one block, are refused rather than loading one key;
// catted gpg exports and the same blocks newline-separated both still load.
func TestGluedArmorBlocksAreRefused(t *testing.T) {
	t.Parallel()

	requireCattedKeyringStillLoads(t)

	glued, separated := gluedArmorBlocks(t)
	// The fixture ahead of its verdict, and the defect itself in one number: the
	// cut finds one opening line in a file that carries three blocks.
	if got := countArmorBlocks(glued); got != 1 {
		t.Fatalf("the glued fixture presents %d armor blocks, want 1: it is not the shape this refusal is about", got)
	}

	dir := t.TempDir()
	kr, err := LoadKeyring(writeKeyringFile(t, dir, "separated.asc", separated))
	if err != nil || kr.Len() != gluedBlockCount {
		t.Fatalf("positive control: LoadKeyring(%d separated blocks) = %v holding %d keys, want nil and %d",
			gluedBlockCount, err, keyringLen(kr), gluedBlockCount)
	}

	kr, err = LoadKeyring(writeKeyringFile(t, dir, "glued.asc", glued))
	if !errors.Is(err, errArmorBlockStartMidLine) {
		t.Fatalf("LoadKeyring(%d glued blocks) = %d keys, %v, want the mid-line refusal",
			gluedBlockCount, keyringLen(kr), err)
	}
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("LoadKeyring(%d glued blocks) error = %v, want the unreadable sentinel", gluedBlockCount, err)
	}
	// The remedy, which is the whole reason a refusal is worth more here than a
	// smarter cut: the operator has to separate the exports they concatenated.
	if !strings.Contains(err.Error(), "separate concatenated key exports with a newline") {
		t.Fatalf("LoadKeyring(%d glued blocks) error does not name the remedy:\n%v", gluedBlockCount, err)
	}
}

// requireCattedKeyringStillLoads is the first of that test's two controls, and
// the one about the shape rather than about the blocks: two gpg exports catted,
// each ending with the newline gpg writes, still load as two keys.
func requireCattedKeyringStillLoads(t *testing.T) {
	t.Helper()

	kr, err := LoadKeyring(fixturePath(twoKeyFixture))
	if err != nil || kr.Len() != 2 {
		t.Fatalf("positive control: LoadKeyring(%s) = %v holding %d keys, want nil and 2",
			twoKeyFixture, err, keyringLen(kr))
	}
}

// gluedArmorBlocks returns gluedBlockCount copies of one armored export glued
// directly, then the same copies newline-separated as their control.
func gluedArmorBlocks(t *testing.T) ([]byte, []byte) {
	t.Helper()

	block := armorEncode(t, openpgp.PublicKeyType, readFixture(t, binaryFixture))
	glued := make([]byte, 0, gluedBlockCount*len(block))
	separated := make([]byte, 0, gluedBlockCount*(len(block)+1))
	for range gluedBlockCount {
		glued = append(glued, block...)
		separated = append(separated, block...)
		separated = append(separated, '\n')
	}

	return glued, separated
}

// writeKeyringFile writes one keyring into dir under leaf and returns its path.
func writeKeyringFile(t *testing.T, dir, leaf string, data []byte) string {
	t.Helper()

	path := filepath.Join(dir, leaf)
	// #nosec G703 -- dir is the caller's own t.TempDir and leaf is a constant at
	// every call site; no part of the path comes from outside this package.
	if err := os.WriteFile(path, data, keyringFileMode); err != nil {
		t.Fatalf("write %s: %v", leaf, err)
	}

	return path
}

// keyringLen reports how many keys a keyring holds, and zero for the nil one a
// refusal returns, so a failure message can name both outcomes in one line.
func keyringLen(kr *Keyring) int {
	if kr == nil {
		return 0
	}

	return kr.Len()
}

// TestEveryCommittedFixtureOpensItsArmorAtALineStart sweeps every testdata file
// through checkArmorBlockStartsAtLineStart, so a fixture added later is covered
// too and the refusal is shown never to reject committed armor.
func TestEveryCommittedFixtureOpensItsArmorAtALineStart(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(testdataDir)
	if err != nil {
		t.Fatalf("read %s: %v", testdataDir, err)
	}

	carrying := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		data := readFixture(t, name)
		if checkErr := checkArmorBlockStartsAtLineStart(data); checkErr != nil {
			t.Errorf("checkArmorBlockStartsAtLineStart(%s) = %v, want nil", name, checkErr)

			continue
		}
		if bytes.Contains(data, []byte(armorBlockStart)) {
			carrying++
		}
	}

	// Without this the sweep passes just as well against a testdata directory
	// holding no armor at all.
	if carrying == 0 {
		t.Fatalf("no committed fixture carries an armor block start, so the sweep measured nothing")
	}
	requireDamagedArmorFixturesOpenAtALineStart(t)

	// The positive control for the sweep itself: the check does refuse
	// something, so "every fixture passed" is not "the check never fires".
	glued, _ := gluedArmorBlocks(t)
	if checkErr := checkArmorBlockStartsAtLineStart(glued); !errors.Is(checkErr, errArmorBlockStartMidLine) {
		t.Fatalf("positive control: checkArmorBlockStartsAtLineStart(glued blocks) = %v, want the mid-line refusal", checkErr)
	}
	t.Logf("%d of the %d committed fixtures carry an armor block start, every one of them at a line start", carrying, len(entries))
}

// requireDamagedArmorFixturesOpenAtALineStart pins that the two deliberately
// damaged fixtures (folded body, bad base64) still open their armor at a line
// start, so their damage is not the shape the mid-line refusal is about.
func requireDamagedArmorFixturesOpenAtALineStart(t *testing.T) {
	t.Helper()

	for _, name := range []string{sigFoldedArmor, sigBadBase64} {
		data := readFixture(t, name)
		if !bytes.Contains(data, []byte(armorBlockStart)) {
			t.Fatalf("%s carries no armor block start, so it is not the fixture this row is about", name)
		}
		if checkErr := checkArmorBlockStartsAtLineStart(data); checkErr != nil {
			t.Errorf("checkArmorBlockStartsAtLineStart(%s) = %v, want nil: its damage is inside the body", name, checkErr)
		}
	}
}
