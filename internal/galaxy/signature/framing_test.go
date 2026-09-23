package signature

import (
	"bytes"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// framingAllocCeiling is what refusing an amplifying blob may cost, whichever
	// header shape carried it. The ungated library call allocates 4 GiB for ten such
	// bytes; this ceiling sits far below that and far above race-detector noise.
	framingAllocCeiling = 1 << 20 // 1 MiB

	// keyringFileMode is the mode the temporary keyrings below are written
	// with.
	keyringFileMode = 0o600
)

// framingCase is one row of TestCheckPacketFraming: the bytes to walk, the tag
// set to walk them under, and whether the walk must refuse them.
type framingCase struct {
	name       string
	data       []byte
	profile    packetProfile
	wantRefuse bool
}

// framingCases covers every refusing arm of checkPacketFraming but the packet
// ceiling, plus accepting rows that keep real key material and empty input clean.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var framingCases = []framingCase{
	{
		name:       "a v6 signature declaring 4 GiB of hashed subpackets",
		data:       synthesizedBlobs[amplifyingBlob],
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// The hashed length is honest and the unhashed one is not, so the walk
		// has to reach past the hashed subpackets to see it.
		name:       "a v6 signature declaring 4 GiB of unhashed subpackets",
		data:       []byte{0xc2, 0x0d, 0x06, 0x13, 0x01, 0x08, 0x00, 0x00, 0x00, 0x01, 0x2a, 0xff, 0xff, 0xff, 0xff},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// A packet body longer than the bytes behind it, whatever the packet
		// turns out to be.
		name:       "a packet declaring a body longer than the input",
		data:       []byte{0xc2, 0xff, 0xff, 0xff, 0xff, 0xff, 0x06},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// The same octets under v4 are a two-octet hashed length of 65535 with two
		// present: the bytes present bound it at every version.
		name:       "a v4 signature carrying the same octets",
		data:       []byte{0xc2, 0x08, 0x04, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// Not a packet header. Refused rather than ending the walk, since go-crypto
		// recovers past unreadable bytes and would parse what lies behind them unjudged.
		name:       "bytes that are not a packet header",
		data:       []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n"),
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// Old-format length type 3: the body runs to the end of input and no octet
		// names its length, so the walk cannot bound it while go-crypto keeps parsing.
		name:       "an old-format packet with an indeterminate length",
		data:       []byte{0x8b, 0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// A new-format partial length sizes one chunk and declares no total, so the
		// walk cannot find the packet's end; the tag plays no part in the refusal.
		name:       "a new-format packet with a partial length",
		data:       []byte{0xc2, 0xe1, 0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// The same partial length on tag 1: the refusal is the framing's, not a rule
		// about signatures.
		name:       "a new-format partial length off the signature tag",
		data:       []byte{0xc1, 0xe0, 0x00, 0x00},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// And the other unbounded spelling off the signature tag: one octet
		// naming tag 1 and length type 3, with a zero body behind it. Refused
		// for the same reason, from a header a byte long.
		name:       "an old-format indeterminate length off the signature tag",
		data:       []byte{0x87, 0x00},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// A marker packet, which is tag 10 in no set: it belongs to neither a
		// keyring nor a detached signature, and it is what a carrier packet
		// ahead of real material is written as.
		name:       "a marker packet in a keyring",
		data:       []byte{0xca, 0x03, 0x50, 0x47, 0x50},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// An empty public key packet: the parser fails it and consumes it, so
		// the walk has nothing against it in a file made of key packets.
		name:    "an empty public key packet in a keyring",
		data:    []byte{0xc6, 0x00},
		profile: keyringProfile(),
	},
	{
		// The secret key tag, one bit below the public key row: identical bytes but
		// for the tag, so the refusal is the tag rather than the empty body.
		name:       "an empty secret key packet in a keyring",
		data:       []byte{0xc5, 0x00},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// And its subkey spelling, which no exporter writes without the packet
		// above it - refused on its own terms all the same, since a rule that
		// covered only the primary is one a file steps out of by dropping it.
		name:       "an empty secret subkey packet in a keyring",
		data:       []byte{0xc7, 0x00},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// A v6 hashed length of 256 spelled in the low two of its four octets, which
		// pins the width read as four octets rather than two.
		name:       "a v6 signature declaring 256 bytes of hashed subpackets",
		data:       []byte{0xc2, 0x08, 0x06, 0x13, 0x01, 0x08, 0x00, 0x00, 0x01, 0x00},
		profile:    signatureBlobProfile(),
		wantRefuse: true,
	},
	{
		// A subpacket of zero length, one octet shorter than the refused
		// type-without-body shape: the walk accepts it, and go-crypto refuses it
		// before reading anything behind it.
		name:    "a signature subpacket of no length at all",
		data:    []byte{0xc2, 0x08, 0x04, 0x13, 0x01, 0x08, 0x00, 0x02, 0x00, 0x00},
		profile: signatureBlobProfile(),
	},
	{
		// The identical bytes in a detached signature, where a key packet has
		// no place. This row and the one above it differ in the set alone.
		name:       "the same packet in a signature blob",
		data:       []byte{0xc6, 0x00},
		profile:    signatureBlobProfile(),
		wantRefuse: true,
	},
	{
		// GnuPG's ring-trust packet, which a raw pubring.gpg carries and which
		// go-crypto never parses at all - it errors on the tag and consumes the
		// body, so the walk finds nothing unread.
		name:    "a ring-trust packet in a keyring",
		data:    []byte{0xcc, 0x02, 0x00, 0x00},
		profile: keyringProfile(),
	},
	{
		// A public key packet whose parse succeeds reading three bytes less than its
		// header declares: declared lengths alone accept it, leaving go-crypto's
		// reader inside the body.
		name: "a public key packet the parser under-reads",
		data: []byte{
			0xc6, 0x0f, 0x04, 0x00, 0x00, 0x00, 0x00, 0x01,
			0x00, 0x08, 0xff, 0x00, 0x08, 0x03, 0xaa, 0xbb, 0xcc,
		},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// A header cut off inside its length field: the walk can locate neither this
		// packet's end nor anything behind it.
		name:       "a header truncated inside its length field",
		data:       []byte{0xc2, 0xff, 0x00},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	// An empty input never enters the walk, so it is not a stream ending off a
	// packet boundary; this is the positive control for that arm.
	{name: "no bytes at all", data: nil, profile: keyringProfile()},
}

// TestCheckPacketFraming walks the gate's predicate row by row. A nil answer
// means every packet is admitted, backed by its bytes and read to its end, not
// that any of it is a signature.
func TestCheckPacketFraming(t *testing.T) {
	t.Parallel()

	// The positive control for the whole table, on real key material rather
	// than on a hand-built shape: a committed keyring walks clean, so a
	// refusing row is its own bytes and not a gate that refuses everything.
	if err := checkPacketFraming(readFixture(t, binaryFixture), keyringProfile()); err != nil {
		t.Fatalf("positive control: checkPacketFraming(%s) = %v, want nil", binaryFixture, err)
	}

	for _, tc := range framingCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkPacketFraming(tc.data, tc.profile)
			if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
				t.Fatalf("checkPacketFraming(%s) refused = %t (err %v), want refused = %t", tc.name, got, err, tc.wantRefuse)
			}
		})
	}
}

// packetCeilingCase is one row of TestPacketCeilingIsPerProfile: how many
// packets to build, the profile to walk them under, and whether that profile
// must refuse them.
type packetCeilingCase struct {
	name       string
	profile    packetProfile
	packets    int
	wantRefuse bool
}

// packetCeilingCases states each profile's ceiling as a boundary: exactly
// maxPackets is walked and one more is refused. The same 65 packets refused as
// a blob are accepted as a keyring, so the ceiling is the profile's.
func packetCeilingCases() []packetCeilingCase {
	return []packetCeilingCase{
		{name: "a blob at its ceiling", profile: signatureBlobProfile(), packets: signatureBlobMaxPackets},
		{name: "a blob one packet past it", profile: signatureBlobProfile(), packets: signatureBlobMaxPackets + 1, wantRefuse: true},
		{name: "the same packets in a keyring", profile: keyringProfile(), packets: signatureBlobMaxPackets + 1},
		{name: "a keyring at its ceiling", profile: keyringProfile(), packets: keyringMaxPackets},
		{name: "a keyring one packet past it", profile: keyringProfile(), packets: keyringMaxPackets + 1, wantRefuse: true},
	}
}

// TestPacketCeilingIsPerProfile pins how many packets each profile admits, over
// the cheapest packet a stream can buy, a two-byte empty signature. It counts
// verdicts rather than allocations, so it runs in parallel.
func TestPacketCeilingIsPerProfile(t *testing.T) {
	t.Parallel()

	// The whole message hand-spelled, and apart from the production format
	// string and the constant it names, so that a mutation to either cannot
	// reshape the expectation into agreeing with it.
	const wantBlob = "malformed OpenPGP packet framing: more than 64 packets across the whole input"

	for _, tc := range packetCeilingCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stream := repeat([]byte{0xc2, 0x00}, tc.packets*2)
			err := checkPacketFraming(stream, tc.profile)
			if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
				t.Fatalf("checkPacketFraming(%s, %d packets) refused = %t (err %v), want refused = %t",
					tc.name, tc.packets, got, err, tc.wantRefuse)
			}
			// Which arm refused it, asked on the one row whose ceiling is small
			// enough to spell: a stream refused as malformed by some other arm
			// would pass the assertion above and fail here.
			if tc.wantRefuse && tc.profile.maxPackets == signatureBlobMaxPackets && err.Error() != wantBlob {
				t.Fatalf("checkPacketFraming(%s) = %v, want %q", tc.name, err, wantBlob)
			}
		})
	}
}

const (
	// budgetBlockCopies is how many copies of the three-packet binary export each
	// block carries: 2049 packets fit keyringMaxPackets alone and overrun it as a pair.
	budgetBlockCopies = 683
	// budgetBlockPackets is what that comes to, hand-multiplied rather than
	// derived from the fixture, so a fixture that stopped being three packets
	// fails the count below instead of quietly restating itself.
	budgetBlockPackets = 2049
	// binaryFixturePackets is that three, named so the arithmetic above has
	// something to be checked against.
	binaryFixturePackets = 3
)

// TestArmorBlocksShareOnePacketBudget pins that the packet ceiling counts the
// whole keyring file rather than each armor block. Two blocks glued without a
// newline read as one, so the fixture's block count is asserted first.
func TestArmorBlocksShareOnePacketBudget(t *testing.T) {
	t.Parallel()

	unit := readFixture(t, binaryFixture)
	if got := countBoundedPackets(unit); got != binaryFixturePackets {
		t.Fatalf("%s holds %d packets, want %d: the arithmetic below is stated over that number",
			binaryFixture, got, binaryFixturePackets)
	}
	if budgetBlockCopies*binaryFixturePackets != budgetBlockPackets {
		t.Fatalf("%d copies of %d packets is not %d", budgetBlockCopies, binaryFixturePackets, budgetBlockPackets)
	}
	if budgetBlockPackets > keyringMaxPackets || 2*budgetBlockPackets <= keyringMaxPackets {
		t.Fatalf("a block of %d packets is not one that fits alone and overruns in a pair against a ceiling of %d",
			budgetBlockPackets, keyringMaxPackets)
	}

	block := armorEncode(t, openpgp.PublicKeyType, bytes.Repeat(unit, budgetBlockCopies))
	// The newline is what keeps the two blocks two: armor.Encode ends a block
	// without one, so concatenating them directly glues the end line of the first
	// to the opening line of the second.
	file := make([]byte, 0, 2*len(block)+2)
	file = append(file, block...)
	file = append(file, '\n')
	file = append(file, block...)
	file = append(file, '\n')

	if got := countArmorBlocks(file); got != 2 {
		t.Fatalf("the two-block fixture presents %d armor blocks, want 2", got)
	}

	entities, err := readEntities(block)
	if err != nil || len(entities) != budgetBlockCopies {
		t.Fatalf("positive control: readEntities(one block) = %d entities, %v, want %d and nil",
			len(entities), err, budgetBlockCopies)
	}

	_, err = readEntities(file)
	if !errors.Is(err, errMalformedSignaturePacket) {
		t.Fatalf("readEntities(two blocks of %d packets) = %v, want the malformed-packet refusal", budgetBlockPackets, err)
	}
}

// countBoundedPackets reports how many bounded packet frames data holds, stopping
// where the walk can no longer read one.
func countBoundedPackets(data []byte) int {
	packets := 0
	for len(data) > 0 {
		frame, status := readPacketFrame(data)
		if status != frameBounded || frame.bodyLen > int64(len(data)-frame.headerLen) {
			return packets
		}
		packets++
		data = data[int64(frame.headerLen)+frame.bodyLen:]
	}

	return packets
}

// countArmorBlocks reports how many opening lines data carries, cut the way
// readArmoredKeyRing cuts them.
func countArmorBlocks(data []byte) int {
	blocks := 0
	for off := 0; off < len(data); {
		line, next := nextLine(data, off)
		if isArmorBlockStart(line) {
			blocks++
		}
		off = next
	}

	return blocks
}

// blobProfileCase is one input reaching checkOne, and the whole refusal the blob
// profile owes it.
type blobProfileCase struct {
	name string
	want string
	data []byte
}

// blobProfileCases are the refusals separating the blob profile from the
// keyring one, two by tag and one by ceiling, each message spelled apart from
// the production format strings so a change there cannot reshape it.
func blobProfileCases(t *testing.T) []blobProfileCase {
	t.Helper()

	// A real public key export offered as a detached signature, which is the
	// shape an operator produces by naming the wrong file.
	keyExport := readFixture(t, binaryFixture)
	// A key packet ahead of a signature that verifies, which is the shape an
	// attacker produces: under the keyring vocabulary the leading packet is
	// admitted and the signature behind it is still checked.
	prefixed := make([]byte, 0, len(keyExport)+len(readFixture(t, sigValidBinary)))
	prefixed = append(prefixed, keyExport...)
	prefixed = append(prefixed, readFixture(t, sigValidBinary)...)

	return []blobProfileCase{
		{
			name: "a public key export offered as a signature",
			data: keyExport,
			want: "malformed OpenPGP packet framing: a tag 6 packet has no place in this stream",
		},
		{
			name: "a key packet ahead of a signature that verifies",
			data: prefixed,
			want: "malformed OpenPGP packet framing: a tag 6 packet has no place in this stream",
		},
		{
			name: "65 empty signature packets",
			data: repeat([]byte{0xc2, 0x00}, 2*(signatureBlobMaxPackets+1)),
			want: "malformed OpenPGP packet framing: more than 64 packets across the whole input",
		},
	}
}

// TestCheckOnePassesTheBlobProfile pins that checkOne hands the gate the blob
// profile: a keyring profile would admit key packets into a blob and raise its
// ceiling, and no carrier measurement would notice.
func TestCheckOnePassesTheBlobProfile(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	// The positive control, through the identical call: a real signature over
	// this manifest still verifies, so a refusal below is the profile rather than
	// a path that refuses everything.
	if _, err := checkOne(manifest, readFixture(t, sigValidBinary), kr); err != nil {
		t.Fatalf("positive control: checkOne(%s) = %v, want nil", sigValidBinary, err)
	}

	for _, tc := range blobProfileCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := checkOne(manifest, tc.data, kr)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("checkOne(%s) = %v, want %q", tc.name, err, tc.want)
			}
		})
	}
}

// TestUnreadableHeaderNamesTheRemainingBytes pins the unreadable-header arm by
// its message rather than the shared sentinel; the control is the same bytes
// with the header MSB set, a user id packet that is accepted.
func TestUnreadableHeaderNamesTheRemainingBytes(t *testing.T) {
	t.Parallel()

	// The whole message hand-spelled, and apart from the production format
	// string it checks, so that a mutation to that string cannot reshape the
	// expectation into agreeing with it.
	const want = "malformed OpenPGP packet framing: 2 bytes are not a packet header this walk can read"

	if err := checkPacketFraming([]byte{0xcd, 0x00}, keyringProfile()); err != nil {
		t.Fatalf("positive control: checkPacketFraming(the same octets with the header MSB) = %v, want nil", err)
	}

	err := checkPacketFraming([]byte{0x4d, 0x00}, keyringProfile())
	if err == nil || err.Error() != want {
		t.Fatalf("checkPacketFraming(two bytes without the header MSB) = %v, want %q", err, want)
	}
}

// truncationCase is one input cut off inside a length field or a body, and the
// verdict the walk owes it.
type truncationCase struct {
	name       string
	data       []byte
	wantRefuse bool
}

// truncationCases has one row per guard on bytes being present. A header cut
// short is refused, since go-crypto recovers past it; a signature body cut short
// is accepted, since go-crypto fails those reads without allocating.
func truncationCases() []truncationCase {
	return []truncationCase{
		{name: "a tag octet without the header MSB", data: []byte{0x00}, wantRefuse: true},
		{name: "an old-format one-octet length field absent", data: []byte{0x8c}, wantRefuse: true},
		{name: "an old-format two-octet length field truncated", data: []byte{0x8d, 0x00}, wantRefuse: true},
		{name: "an old-format four-octet length field truncated", data: []byte{0x8e, 0x00, 0x00, 0x00}, wantRefuse: true},
		{name: "a new-format header with no length octet", data: []byte{0xc2}, wantRefuse: true},
		{name: "a new-format two-octet length field truncated", data: []byte{0xc2, 0xc0}, wantRefuse: true},
		{name: "a new-format five-octet length field truncated", data: []byte{0xc2, 0xff, 0x00}, wantRefuse: true},
		// Below here the header is whole and the body is what runs out, so each
		// row is a signature packet framed exactly around the bytes it holds.
		{name: "a signature packet with an empty body", data: []byte{0xc2, 0x00}},
		{name: "a signature body too short for its length prefix", data: []byte{0xc2, 0x03, 0x04, 0x13, 0x01}},
		{
			name: "a signature body with no unhashed length behind its hashed area",
			data: []byte{0xc2, 0x06, 0x04, 0x13, 0x01, 0x08, 0x00, 0x00},
		},
		{
			name: "a subpacket two-octet length field truncated",
			data: []byte{0xc2, 0x07, 0x04, 0x13, 0x01, 0x08, 0x00, 0x01, 0xc0},
		},
		{
			name: "a subpacket five-octet length field truncated",
			data: []byte{0xc2, 0x09, 0x04, 0x13, 0x01, 0x08, 0x00, 0x03, 0xff, 0x00, 0x00},
		},
		{
			name: "a subpacket declaring more than its area holds",
			data: []byte{0xc2, 0x08, 0x04, 0x13, 0x01, 0x08, 0x00, 0x02, 0x05, 0x02},
		},
	}
}

// TestTruncatedFramingVerdicts asserts what each truncation guard decides; no
// committed fixture or other hand-built case reaches most of them.
func TestTruncatedFramingVerdicts(t *testing.T) {
	t.Parallel()

	// Positive control: a whole signature packet of the rows' shape walks clean.
	whole := []byte{0xc2, 0x0b, 0x04, 0x13, 0x01, 0x08, 0x00, 0x03, 0x02, 0x04, 0x01, 0x00, 0x00}
	if err := checkPacketFraming(whole, signatureBlobProfile()); err != nil {
		t.Fatalf("positive control: checkPacketFraming(a whole signature packet) = %v, want nil", err)
	}

	for _, tc := range truncationCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkPacketFraming(tc.data, signatureBlobProfile())
			if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
				t.Fatalf("checkPacketFraming(%s) refused = %t (err %v), want refused = %t", tc.name, got, err, tc.wantRefuse)
			}
		})
	}
}

// fixtureGateCase is one committed fixture, the tag set the entry point that
// really reads it passes to the gate, and whether that gate refuses it as secret
// key material.
type fixtureGateCase struct {
	name       string
	profile    packetProfile
	wantSecret bool
}

// gatedFixtures is every committed fixture holding OpenPGP packets, under the
// profile its own entry point passes; under the other one it would assert nothing.
func gatedFixtures() []fixtureGateCase {
	return []fixtureGateCase{
		{name: sigValidArmored, profile: signatureBlobProfile()},
		{name: sigValidBinary, profile: signatureBlobProfile()},
		{name: sigSecondSigner, profile: signatureBlobProfile()},
		{name: sigOverManifestB, profile: signatureBlobProfile()},
		{name: sigOutsiderKey, profile: signatureBlobProfile()},
		{name: sigExpiredKey, profile: signatureBlobProfile()},
		{name: sigRevokedKey, profile: signatureBlobProfile()},
		{name: sigExpiredSig, profile: signatureBlobProfile()},
		{name: keyringFixture, profile: keyringProfile()},
		{name: outsiderKeyringFixture, profile: keyringProfile()},
		{name: armoredFixture, profile: keyringProfile()},
		{name: binaryFixture, profile: keyringProfile()},
		{name: twoKeyFixture, profile: keyringProfile()},
		{name: secondFixture, profile: keyringProfile()},
		{name: secretPublicFixture, profile: keyringProfile()},
		{name: secretArmoredFixture, profile: keyringProfile(), wantSecret: true},
		{name: secretBinaryFixture, profile: keyringProfile(), wantSecret: true},
		{name: subkeyFixture, profile: keyringProfile()},
	}
}

// TestCheckPacketFramingAcceptsEverySignatureFixture pins that every committed
// signature and public keyring walks clean under its entry point's profile, and
// that the two real gpg secret exports are refused as secret material.
func TestCheckPacketFramingAcceptsEverySignatureFixture(t *testing.T) {
	t.Parallel()

	// The armored fixtures are decoded first, since the gate runs on decoded
	// bytes and armor text carries no framing to walk.
	for _, tc := range gatedFixtures() {
		data := readFixture(t, tc.name)
		if bytes.Contains(data, []byte(armorBlockStart)) {
			decoded, _, err := decodeArmorBlock(data)
			if err != nil {
				t.Fatalf("decodeArmorBlock(%s) = %v, want nil", tc.name, err)
			}
			data = decoded
		}

		err := checkPacketFraming(data, tc.profile)
		if got := errors.Is(err, errSecretKeyPacket); got != tc.wantSecret {
			t.Fatalf("checkPacketFraming(%s) refused as secret material = %t (err %v), want %t",
				tc.name, got, err, tc.wantSecret)
		}
		if !tc.wantSecret && err != nil {
			t.Fatalf("checkPacketFraming(%s) = %v, want nil", tc.name, err)
		}
	}
}

// TestCheckOneRefusesAnAmplifyingPacketCheaply pins that checkOne refuses the
// ten-byte v6 blob the ungated parser spends 4 GiB on, allocating no more than
// framingAllocCeiling. Not parallel: runtime.MemStats is process-wide.
func TestCheckOneRefusesAnAmplifyingPacketCheaply(t *testing.T) {
	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	// The positive control, through the identical call: this keyring and this
	// manifest do verify a signature, so the refusal below is the blob's own
	// framing and not a path that refuses everything.
	if _, err := checkOne(manifest, readFixture(t, sigValidArmored), kr); err != nil {
		t.Fatalf("positive control: checkOne(%s) = %v, want nil", sigValidArmored, err)
	}

	blob := synthesizedBlobs[amplifyingBlob]

	var err error
	allocated := measureAlloc(func() {
		_, err = checkOne(manifest, blob, kr)
	})
	if allocated > framingAllocCeiling {
		t.Fatalf("checkOne(v6 amplification blob) allocated %d bytes, want at most %d", allocated, framingAllocCeiling)
	}
	// Which refusal produced that cheap outcome. It sits below the measurement
	// because the measurement is what the gate is for; a mutation that refused
	// this blob cheaply under some other error would fail here and pass above.
	if !errors.Is(err, errMalformedSignaturePacket) {
		t.Fatalf("checkOne(v6 amplification blob) = %v, want the malformed-packet refusal", err)
	}
}

// TestLoadKeyringRefusesAnAmplifyingPacketCheaply is the same measurement on the
// keyring path, binary and armored; both are refused either way, so the byte
// ceiling is the whole pin. Not parallel: runtime.MemStats is process-wide.
func TestLoadKeyringRefusesAnAmplifyingPacketCheaply(t *testing.T) {
	dir := t.TempDir()
	blob := synthesizedBlobs[amplifyingBlob]
	armored := armorEncode(t, openpgp.PublicKeyType, blob)

	// The control is a real keyring written into the same directory, so a
	// refusal below is the blob and not this directory or this test's writes.
	controlPath := filepath.Join(dir, "control.asc")
	// #nosec G703 -- dir is this test's own t.TempDir and the leaf is a
	// constant; no part of the path comes from outside this function.
	if err := os.WriteFile(controlPath, readFixture(t, armoredFixture), keyringFileMode); err != nil {
		t.Fatalf("write control keyring: %v", err)
	}
	if _, err := LoadKeyring(controlPath); err != nil {
		t.Fatalf("positive control: LoadKeyring(control.asc) = %v, want nil", err)
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "binary.gpg", data: blob},
		{name: "armored.asc", data: armored},
	} {
		path := filepath.Join(dir, tc.name)
		// #nosec G703 -- same t.TempDir and a leaf from this function's own
		// fixed table, as just above.
		if err := os.WriteFile(path, tc.data, keyringFileMode); err != nil {
			t.Fatalf("write %s: %v", tc.name, err)
		}

		var loadErr error
		allocated := measureAlloc(func() {
			_, loadErr = LoadKeyring(path)
		})
		if !errors.Is(loadErr, helpers.ErrKeyringUnreadable) {
			t.Fatalf("LoadKeyring(%s) error = %v, want the unreadable sentinel", tc.name, loadErr)
		}
		if allocated > framingAllocCeiling {
			t.Fatalf("LoadKeyring(%s) allocated %d bytes, want at most %d", tc.name, allocated, framingAllocCeiling)
		}
	}
}

// TestAmplifyingFramingsAreRefusedCheaply measures five header shapes and
// encodings of one amplifying v6 body through their own entry points, so no
// header rewrite reopens the allocation. Not parallel: MemStats is process-wide.
func TestAmplifyingFramingsAreRefusedCheaply(t *testing.T) {
	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	requireFramingFixturesStillWork(t, kr, manifest)

	// The amplifying v6 body every row carries: hashed subpacket length 0xffffffff.
	body := []byte{0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff}
	// 0xc2 0x08 is a declared one-octet length; 0xc2 0xe6 a partial length naming
	// no total; 0x8b the old format on tag 2 with an indeterminate length.
	declared := append([]byte{0xc2, 0x08}, body...)
	partial := append([]byte{0xc2, 0xe6}, body...)
	indeterminate := append([]byte{0x8b}, body...)

	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		leaf string
		data []byte
	}{
		{name: "a new-format declared length", leaf: "declared.gpg", data: declared},
		{name: "an old-format indeterminate length", leaf: "indeterminate.gpg", data: indeterminate},
		{name: "an armored public key block", leaf: "indeterminate.asc", data: armorEncode(t, openpgp.PublicKeyType, indeterminate)},
	} {
		path := filepath.Join(dir, tc.leaf)
		// #nosec G703 -- dir is this test's own t.TempDir and the leaf comes
		// from this function's own fixed table; no part of the path comes from
		// outside this function.
		if err := os.WriteFile(path, tc.data, keyringFileMode); err != nil {
			t.Fatalf("write %s: %v", tc.leaf, err)
		}

		var loadErr error
		allocated := measureAlloc(func() {
			_, loadErr = LoadKeyring(path)
		})
		if allocated > framingAllocCeiling {
			t.Fatalf("LoadKeyring(%s) allocated %d bytes, want at most %d", tc.name, allocated, framingAllocCeiling)
		}
		// Which refusal produced the cheap outcome, asked after the cost that is the
		// point of the row.
		if !errors.Is(loadErr, errMalformedSignaturePacket) {
			t.Fatalf("LoadKeyring(%s) error = %v, want the malformed-packet refusal", tc.name, loadErr)
		}
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "an armored signature block", data: armorEncode(t, openpgp.SignatureType, indeterminate)},
		{name: "a new-format partial length", data: partial},
	} {
		var checkErr error
		allocated := measureAlloc(func() {
			_, checkErr = checkOne(manifest, tc.data, kr)
		})
		if allocated > framingAllocCeiling {
			t.Fatalf("checkOne(%s) allocated %d bytes, want at most %d", tc.name, allocated, framingAllocCeiling)
		}
		if !errors.Is(checkErr, errMalformedSignaturePacket) {
			t.Fatalf("checkOne(%s) error = %v, want the malformed-packet refusal", tc.name, checkErr)
		}
	}
}

// TestNoPacketTagCarriesAnUnboundedFramingPastTheGate sweeps both unbounded
// framings with the amplifier behind them over every tag and both entry points,
// since the input writes the tag. Not parallel: MemStats is process-wide.
func TestNoPacketTagCarriesAnUnboundedFramingPastTheGate(t *testing.T) {
	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	// Positive control: real keyrings load and real signatures verify.
	requireFramingFixturesStillWork(t, kr, manifest)

	var overCeiling, wrongVerdict []unboundedFinding
	// Declared once rather than per iteration: it closes over the two
	// accumulators, and a closure rebuilt inside the loop would allocate one per
	// carrier for no gain.
	record := func(carrier unboundedCarrier, entry string, allocated uint64, err error) {
		finding := unboundedFinding{framing: carrier.framing, entry: entry, allocated: allocated, tag: carrier.tag}
		if allocated > framingAllocCeiling {
			overCeiling = append(overCeiling, finding)
		}
		if !errors.Is(err, errMalformedSignaturePacket) {
			wrongVerdict = append(wrongVerdict, finding)
		}
	}

	// One leaf, rewritten per carrier: LoadKeyring takes a path and every write
	// truncates, so the sweep costs one file rather than one per tag.
	path := filepath.Join(t.TempDir(), "carrier.gpg")
	for _, carrier := range unboundedCarriers() {
		// #nosec G703 -- the directory is this test's own t.TempDir and the leaf
		// is a constant; no part of the path comes from outside this function.
		if err := os.WriteFile(path, carrier.data, keyringFileMode); err != nil {
			t.Fatalf("write %s tag %d: %v", carrier.framing, carrier.tag, err)
		}

		var loadErr error
		allocated := measureAlloc(func() {
			_, loadErr = LoadKeyring(path)
		})
		record(carrier, "LoadKeyring", allocated, loadErr)

		var checkErr error
		allocated = measureAlloc(func() {
			_, checkErr = checkOne(manifest, carrier.data, kr)
		})
		record(carrier, "checkOne", allocated, checkErr)
	}

	if len(overCeiling) > 0 {
		t.Errorf("%d carriers allocated more than %d bytes: %+v", len(overCeiling), framingAllocCeiling, overCeiling)
	}
	// Which verdict came with the cheap outcome, asked separately from the cost.
	if len(wrongVerdict) > 0 {
		t.Errorf("%d carriers were not refused as malformed framing: %+v", len(wrongVerdict), wrongVerdict)
	}
}

// unboundedCarrier is one packet header declaring no total for its body, with
// the amplifying signature packet behind it - the whole point being that this
// walk cannot find where the carrier ends while go-crypto reads on regardless.
type unboundedCarrier struct {
	framing string
	data    []byte
	tag     int
}

// unboundedFinding is one carrier that got past the gate through one entry
// point, kept rather than fataled on so the failure can name every tag that did
// and what each of them cost.
type unboundedFinding struct {
	framing   string
	entry     string
	allocated uint64
	tag       int
}

// unboundedCarriers builds a new-format partial length on all 64 tags and an
// old-format indeterminate length on the 16 its tag field reaches, with octets
// hand-spelled so a mutated production constant cannot reshape them.
func unboundedCarriers() []unboundedCarrier {
	const (
		// The header MSB plus the new-format bit, leaving the six bits below it
		// for the tag; and the header MSB alone, where the tag sits two bits up
		// and length type 3 is the indeterminate one.
		newFormatLead      = 0xc0
		newFormatTagMax    = 63
		oldFormatLead      = 0x80
		oldFormatTagMax    = 15
		oldFormatTagShiftT = 2
		indeterminateType  = 0x03
		// A partial length sizing one one-byte chunk, that chunk, and the final
		// zero-length octet ending the chunk sequence.
		partialLead      = 0xe0
		newFormatCarrier = 4
		oldFormatCarrier = 1
	)

	// The ten bytes that allocate: a v6 signature declaring 0xffffffff bytes of
	// hashed subpackets.
	amplifier := []byte{0xc2, 0x08, 0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff}

	// The tags are counted in bytes rather than ints, since a tag is six bits of
	// one octet and the loop bound says so: the header octets below are then
	// assembled without a narrowing conversion for a linter to doubt.
	carriers := make([]unboundedCarrier, 0, newFormatTagMax+oldFormatTagMax+2)
	for tag := byte(0); tag <= newFormatTagMax; tag++ {
		// One allocation per carrier, sized once: each carrier owns its own
		// bytes, since they are written to a file and passed to checkOne
		// unchanged rather than consumed inside this loop.
		data := make([]byte, 0, newFormatCarrier+len(amplifier))
		data = append(data, newFormatLead|tag, partialLead, 0x00, 0x00)
		data = append(data, amplifier...)
		carriers = append(carriers, unboundedCarrier{framing: "new-format partial", data: data, tag: int(tag)})
	}
	for tag := byte(0); tag <= oldFormatTagMax; tag++ {
		// The indeterminate body runs to the end of the input, so here the
		// amplifier sits inside the carrier packet rather than behind it.
		data := make([]byte, 0, oldFormatCarrier+len(amplifier))
		data = append(data, oldFormatLead|tag<<oldFormatTagShiftT|indeterminateType)
		data = append(data, amplifier...)
		carriers = append(carriers, unboundedCarrier{framing: "old-format indeterminate", data: data, tag: int(tag)})
	}

	return carriers
}

// requireFramingFixturesStillWork is the positive control for every measurement:
// committed keyrings load, the subkey export exercising the embedded-signature
// recursion, and both encodings of a real signature verify.
func requireFramingFixturesStillWork(t *testing.T, kr *Keyring, manifest []byte) {
	t.Helper()

	for _, name := range []string{binaryFixture, twoKeyFixture, subkeyFixture} {
		if _, err := LoadKeyring(fixturePath(name)); err != nil {
			t.Fatalf("positive control: LoadKeyring(%s) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{sigValidArmored, sigValidBinary} {
		if _, err := checkOne(manifest, readFixture(t, name), kr); err != nil {
			t.Fatalf("positive control: checkOne(%s) = %v, want nil", name, err)
		}
	}
}

// armorEncode wraps body in an ASCII-armored block of blockType; armored rows
// reach the gate only after a decode, since armor text carries no framing.
func armorEncode(t *testing.T, blockType string, body []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer, err := armor.Encode(&buf, blockType, nil)
	if err != nil {
		t.Fatalf("armor.Encode(%s) = %v, want nil", blockType, err)
	}
	if _, err = writer.Write(body); err != nil {
		t.Fatalf("write armored body: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close armored body: %v", err)
	}

	return buf.Bytes()
}

// measureAlloc reports how many bytes the Go heap handed out while f ran.
// TotalAlloc is process-wide, so its callers must not run in parallel.
func measureAlloc(f func()) uint64 {
	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc
}

const (
	// armorHeaderLineSize is the single header line the two measurements below
	// carry, just under a megabyte so the blob stays inside helpers.SignatureMaxSize.
	armorHeaderLineSize = 1<<20 - 128 // just under 1 MiB

	// armorHeaderAllocCeiling scales with the fixture because LoadKeyring reads the
	// whole file first, measured at 4.5 MB under the race detector; the defect
	// spends hundreds of times more.
	armorHeaderAllocCeiling = 16 * armorHeaderLineSize // 16 MiB

	// largestFixtureHeaderSection is the longest header section any committed
	// fixture carries, gpg's lone blank line; a fixture with real header lines must
	// restate it rather than silently eat armorHeaderMaxSize's headroom.
	largestFixtureHeaderSection = 1
)

// TestCheckOneRefusesAnOversizedArmorHeaderCheaply pins that the header bound
// spares checkOne armor.Decode's quadratic header loop, about N*N/200 bytes for
// an N-byte section. Not parallel: runtime.MemStats is process-wide.
func TestCheckOneRefusesAnOversizedArmorHeaderCheaply(t *testing.T) {
	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	// The positive control, through the identical call: a real armored signature
	// still decodes and still verifies, so the refusal below is this blob's own
	// header section and not a bound that refuses every armored blob.
	if _, err := checkOne(manifest, readFixture(t, sigValidArmored), kr); err != nil {
		t.Fatalf("positive control: checkOne(%s) = %v, want nil", sigValidArmored, err)
	}

	blob := oversizedArmorHeader(openpgp.SignatureType)

	var err error
	allocated := measureAlloc(func() {
		_, err = checkOne(manifest, blob, kr)
	})
	if allocated > armorHeaderAllocCeiling {
		t.Fatalf("checkOne(oversized armor header) allocated %d bytes, want at most %d", allocated, armorHeaderAllocCeiling)
	}
	// Which refusal produced that cheap outcome. It sits below the measurement
	// because the measurement is what the bound is for; a mutation that refused
	// this blob cheaply under some other error would fail here and pass above.
	if !errors.Is(err, errOversizedArmorHeader) {
		t.Fatalf("checkOne(oversized armor header) = %v, want the oversized-header refusal", err)
	}
}

// TestLoadKeyringRefusesAnOversizedArmorHeaderCheaply is the same measurement
// on the keyring path, where keyringMaxSize makes the quadratic cost worst.
// Not parallel: runtime.MemStats is process-wide.
func TestLoadKeyringRefusesAnOversizedArmorHeaderCheaply(t *testing.T) {
	dir := t.TempDir()

	// The control is a real keyring written into the same directory, so a refusal
	// below is the file's own header section and not this directory or this
	// test's writes.
	controlPath := filepath.Join(dir, "control.asc")
	// #nosec G703 -- dir is this test's own t.TempDir and the leaf is a
	// constant; no part of the path comes from outside this function.
	if err := os.WriteFile(controlPath, readFixture(t, armoredFixture), keyringFileMode); err != nil {
		t.Fatalf("write control keyring: %v", err)
	}
	if _, err := LoadKeyring(controlPath); err != nil {
		t.Fatalf("positive control: LoadKeyring(control.asc) = %v, want nil", err)
	}

	path := filepath.Join(dir, "oversized.asc")
	// #nosec G703 -- the same t.TempDir and another constant leaf, as just above.
	if err := os.WriteFile(path, oversizedArmorHeader(openpgp.PublicKeyType), keyringFileMode); err != nil {
		t.Fatalf("write oversized keyring: %v", err)
	}

	var err error
	allocated := measureAlloc(func() {
		_, err = LoadKeyring(path)
	})
	if allocated > armorHeaderAllocCeiling {
		t.Fatalf("LoadKeyring(oversized armor header) allocated %d bytes, want at most %d", allocated, armorHeaderAllocCeiling)
	}
	// Which refusal produced that cheap outcome, asked below the measurement for
	// the reason the test above gives.
	if !errors.Is(err, errOversizedArmorHeader) {
		t.Fatalf("LoadKeyring(oversized armor header) = %v, want the oversized-header refusal", err)
	}
}

// TestArmorHeaderBoundAcceptsEveryCommittedFixture pins that every file in
// testdata passes checkArmorHeaderSection, fixtures added later included, and
// reports the longest section as armorHeaderMaxSize's headroom.
func TestArmorHeaderBoundAcceptsEveryCommittedFixture(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(testdataDir)
	if err != nil {
		t.Fatalf("read %s: %v", testdataDir, err)
	}

	longest, longestIn, sections := 0, "", 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		data := readFixture(t, name)
		if err := checkArmorHeaderSection(data); err != nil {
			t.Errorf("checkArmorHeaderSection(%s) = %v, want nil", name, err)

			continue
		}

		fileLongest, fileSections := armorHeaderSections(t, name, data)
		sections += fileSections
		if fileLongest > longest {
			longest, longestIn = fileLongest, name
		}
	}

	// Without this the sweep passes just as well against a testdata directory
	// holding no armor at all, or against a walk that never recognized an opening
	// line: every file would be accepted for having no section to judge.
	if sections == 0 {
		t.Fatalf("the sweep measured no armor header section at all in %s", testdataDir)
	}
	if longest > largestFixtureHeaderSection {
		t.Errorf("the longest committed header section is %d bytes, in %s, want at most %d",
			longest, longestIn, largestFixtureHeaderSection)
	}
	t.Logf("%d committed header sections, longest %d bytes (in %s), against a bound of %d",
		sections, longest, longestIn, armorHeaderMaxSize)
}

// armorHeaderSections walks data as checkArmorHeaderSection does and reports
// the longest header section and the section count. A section end it cannot
// find is fatal, so the two walks cannot silently diverge.
func armorHeaderSections(t *testing.T, name string, data []byte) (int, int) {
	t.Helper()

	longest, sections := 0, 0
	for off := 0; off < len(data); {
		line, next := nextLine(data, off)
		off = next
		if !isArmorBlockStart(line) {
			continue
		}
		end, ok := armorHeaderSectionEnd(data, off)
		if !ok {
			t.Fatalf("armorHeaderSectionEnd(%s, %d) = _, false, want true", name, off)
		}
		sections++
		if end-off > longest {
			longest = end - off
		}
		off = end
	}

	return longest, sections
}

// oversizedArmorHeader builds an armor block whose header section is one line of
// armorHeaderLineSize bytes. The colon makes every chunk after the first a
// continuation armor.Decode appends, which is the copying measured.
func oversizedArmorHeader(blockType string) []byte {
	const header = "Comment: "

	opening := "-----BEGIN " + blockType + "-----\n"
	closing := "\n-----END " + blockType + "-----\n"

	// Sized once, since the whole point of the fixture is its length: the header
	// line, the blank line that ends the section, and the two delimiters.
	buf := bytes.NewBuffer(make([]byte, 0, len(opening)+len(header)+armorHeaderLineSize+len(closing)+1))
	buf.WriteString(opening)
	buf.WriteString(header)
	for range armorHeaderLineSize {
		buf.WriteByte('A')
	}
	buf.WriteString("\n")
	buf.WriteString(closing)

	return buf.Bytes()
}

const (
	// acceptedSectionFill is each section's header line length, hand-spelled under
	// armorHeaderMaxSize's 4096 and near the costliest packing per input byte.
	acceptedSectionFill = 4000

	// acceptedSectionAllocRatio bounds a blob of accepted sections as a multiple of
	// its length: measured at 23.9, far below a return to quadratic cost.
	acceptedSectionAllocRatio = 64
)

// TestArmorHeaderBoundLeavesALinearResidual pins that accepted sections cost
// linear in the blob: a colon-free line restarts armor.Decode's search, so one
// blob holds many sections. Not parallel: runtime.MemStats is process-wide.
func TestArmorHeaderBoundLeavesALinearResidual(t *testing.T) {
	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	blob, sections := acceptedSectionArmor(openpgp.SignatureType)
	if int64(len(blob)) > helpers.SignatureMaxSize {
		t.Fatalf("the blob is %d bytes, past the %d a source may hand over", len(blob), helpers.SignatureMaxSize)
	}
	// The acceptance half, and the anti-vacuity one: every section is inside the
	// bound, so the measurement below is the cost of walking them rather than the
	// cost of the gate refusing the first.
	if err := checkArmorHeaderSection(blob); err != nil {
		t.Fatalf("checkArmorHeaderSection(%d accepted sections) = %v, want nil", sections, err)
	}

	var err error
	allocated := measureAlloc(func() {
		_, err = checkOne(manifest, blob, kr)
	})
	ceiling := uint64(len(blob)) * acceptedSectionAllocRatio
	if allocated > ceiling {
		t.Fatalf("checkOne(%d accepted armor header sections) allocated %d bytes for %d bytes of input, want at most %d",
			sections, allocated, len(blob), ceiling)
	}
	// What the blob decoded to, asked below the measurement because the
	// measurement is the point: every section is abandoned at its colon-free line
	// and no block is ever read, so the bytes counted above are the walk itself.
	if !armorDecodeFoundNothing(err) {
		t.Fatalf("checkOne(%d accepted armor header sections) = %v, want the decoder to find no block", sections, err)
	}
	t.Logf("%d accepted sections in %d bytes allocated %d, %.1f times the input",
		sections, len(blob), allocated, float64(allocated)/float64(len(blob)))
}

// acceptedSectionArmor builds as many maximal accepted header sections as fit in
// helpers.SignatureMaxSize and reports the count; each ends in a colon-free line
// and a blank line, so the decoder abandons it and searches on.
func acceptedSectionArmor(blockType string) ([]byte, int) {
	var section bytes.Buffer

	section.WriteString("-----BEGIN " + blockType + "-----\n")
	section.WriteString("C: ")
	for range acceptedSectionFill {
		section.WriteByte('A')
	}
	// The header line's terminator, then a header line with no colon in it, then
	// the blank line ending the section.
	section.WriteString("\nx\n\n")

	sections := int(helpers.SignatureMaxSize) / section.Len()
	buf := bytes.NewBuffer(make([]byte, 0, sections*section.Len()))
	for range sections {
		buf.Write(section.Bytes())
	}

	return buf.Bytes(), sections
}

// armorSectionTermination is how armorSectionOfSpan ends the header section it
// builds: the way real armor ends one, or by running the input out inside it.
type armorSectionTermination int

const (
	// sectionEndsBlank ends the section with the blank line armorHeaderSectionEnd
	// is looking for, which is what gives the section a span of its own choosing.
	sectionEndsBlank armorSectionTermination = iota
	// sectionRunsOut ends the input with no blank line behind the opening line at
	// all, which is the shape a truncated or corrupt file hands the walk.
	sectionRunsOut
)

// armorSectionRunOutSpan is the run-out row's span, far below armorHeaderMaxSize
// so that running out of input, not the bound, decides the row.
const armorSectionRunOutSpan = 16

// armorSectionBoundaryCase is one row of TestArmorHeaderSectionEndBoundary: the
// section to build, whether checkArmorHeaderSection has to refuse it, and
// whether armor.Decode has to find no block at all in the same bytes.
type armorSectionBoundaryCase struct {
	name                   string
	span                   int
	term                   armorSectionTermination
	wantRefuse             bool
	wantDecodeFoundNothing bool
}

// armorSectionBoundaryCases is armorHeaderSectionEnd's predicate at its edges,
// one row per answer it gives there: the widest section it accepts, the
// narrowest it refuses, and a section whose input runs out inside the bound.
func armorSectionBoundaryCases() []armorSectionBoundaryCase {
	return []armorSectionBoundaryCase{
		{
			// The widest section armorHeaderSectionEnd accepts.
			name:       "a section ending exactly at the bound",
			span:       armorHeaderMaxSize,
			term:       sectionEndsBlank,
			wantRefuse: false,
		},
		{
			// The narrowest section refused, one byte wider than the row above.
			name:       "a section one byte past the bound",
			span:       armorHeaderMaxSize + 1,
			term:       sectionEndsBlank,
			wantRefuse: true,
		},
		{
			// Input running out inside the bound is accepted, leaving armor.Decode to
			// find no block.
			name:                   "a section the input runs out inside",
			span:                   armorSectionRunOutSpan,
			term:                   sectionRunsOut,
			wantDecodeFoundNothing: true,
		},
	}
}

// TestArmorHeaderSectionEndBoundary pins armorHeaderSectionEnd at its edges: a
// section exactly at armorHeaderMaxSize, one byte past it, and input running out
// inside it, which no other fixture here reaches.
func TestArmorHeaderSectionEndBoundary(t *testing.T) {
	t.Parallel()

	for _, tc := range armorSectionBoundaryCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			blob := armorSectionOfSpan(openpgp.SignatureType, tc.span, tc.term)
			// The generated section must span what the row asked for, or the
			// verdict below judges some other span.
			if _, start := nextLine(blob, 0); len(blob)-start != tc.span {
				t.Fatalf("the generated section spans %d bytes, want %d", len(blob)-start, tc.span)
			}

			err := checkArmorHeaderSection(blob)
			if got := errors.Is(err, errOversizedArmorHeader); got != tc.wantRefuse {
				t.Fatalf("checkArmorHeaderSection(a %d-byte section) refused = %t (err %v), want refused = %t", tc.span, got, err, tc.wantRefuse)
			}
			if !tc.wantDecodeFoundNothing {
				return
			}
			// What the decoder makes of the bytes the walk just accepted, which is the
			// io.EOF armorHeaderSectionEnd's doc comment leaves this shape to. It sits
			// below the verdict because the verdict is what the row exists for.
			if _, _, err := decodeArmorBlock(blob); !armorDecodeFoundNothing(err) {
				t.Fatalf("decodeArmorBlock(a %d-byte section running out of input) = %v, want no block found", tc.span, err)
			}
		})
	}
}

// armorSectionOfSpan builds an armor block whose header section spans exactly
// span bytes, ended per term. It keeps acceptedSectionArmor's one-character
// header name, since a longer name shifts armor.Decode's allocation size class.
func armorSectionOfSpan(blockType string, span int, term armorSectionTermination) []byte {
	const name = "C: "

	// What the section spends on its own shape rather than on fill: the header
	// name, the header line's terminator, and, where the section ends the way
	// armor does, the blank line ending it.
	overhead := len(name) + len("\n")
	if term == sectionEndsBlank {
		overhead += len("\n")
	}

	opening := "-----BEGIN " + blockType + "-----\n"

	// Sized once, since the fixture is its length: the opening line, the section
	// behind it, and nothing else.
	buf := bytes.NewBuffer(make([]byte, 0, len(opening)+span))
	buf.WriteString(opening)
	buf.WriteString(name)
	for range span - overhead {
		buf.WriteByte('A')
	}
	buf.WriteString("\n")
	if term == sectionEndsBlank {
		buf.WriteString("\n")
	}

	return buf.Bytes()
}

const (
	// committedPacketMeasurements is how many bounded frames the sweep finds across
	// testdata, every armor block included; asserted so a fixture silently dropping
	// out fails it, and restated whenever a packet-bearing fixture is added.
	committedPacketMeasurements = 53

	// subkeyFixtureSignatures counts the signing-subkey export's signatures; only
	// the subkey binding carries an embedded one, a control in both directions.
	subkeyFixtureSignatures = 2

	// driveAllocPerByte is twice the worst per-byte drive cost measured (59.0, the
	// packed-subpacket signature at 256 KiB), rounded up to a power of two.
	driveAllocPerByte = 128
	// driveAllocFloor covers the largest constant-size allocation a driven tag
	// makes: a public key's fixed MPI budget, just under 30 KiB.
	driveAllocFloor = 64 << 10

	// mpiDeclaredBits is the two-octet bit length the MPI probe declares, which
	// is the largest one that field can name.
	mpiDeclaredBits = 0xffff
	// mpiAllocFloor is the allocation that bit length names: one byte per eight
	// bits. Asserting it from below is what says the probe reached the MPI read
	// at all, rather than measuring a parse that failed before it.
	mpiAllocFloor = (mpiDeclaredBits + 7) / 8
	// mpiAllocCeiling is what the whole packet may then cost. Measured at 9560
	// bytes, so the ceiling holds most of a factor of seven while staying far
	// below what a four-octet bit length would reach.
	mpiAllocCeiling = 64 << 10
)

// TestDriveConsumesEveryCommittedPacketExactly pins the drive's premise: go-crypto
// reads every committed packet to exactly the end this walk computes, so a
// dependency bump that moves where the parser stops fails on real gpg output.
func TestDriveConsumesEveryCommittedPacketExactly(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(testdataDir)
	if err != nil {
		t.Fatalf("read %s: %v", testdataDir, err)
	}

	measured := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		data := readFixture(t, name)
		if !bytes.Contains(data, []byte(armorBlockStart)) {
			measured += driveEveryPacket(t, name, data)

			continue
		}
		// Every block, cut as readArmoredKeyRing cuts them. Two fixtures are damaged
		// armor on purpose, so an undecodable block is skipped and the total guards it.
		for off := 0; off < len(data); {
			line, next := nextLine(data, off)
			start := off
			off = next
			if !isArmorBlockStart(line) {
				continue
			}
			decoded, _, decodeErr := decodeArmorBlock(data[start:])
			if decodeErr != nil {
				t.Logf("%s: a block did not decode (%v), so it carries no packets to measure", name, decodeErr)

				continue
			}
			measured += driveEveryPacket(t, name, decoded)
		}
	}

	if measured != committedPacketMeasurements {
		t.Fatalf("the sweep measured %d packets, want %d", measured, committedPacketMeasurements)
	}
	t.Logf("%d committed packets, every one read to its declared end", measured)
}

// driveEveryPacket drives the parser over each bounded frame of data, failing
// the test for any packet not read to its end, and returns the count. It stops
// at the first unbounded frame, since several fixtures are not packets at all.
func driveEveryPacket(t *testing.T, name string, data []byte) int {
	t.Helper()

	measured := 0
	for len(data) > 0 {
		frame, status := readPacketFrame(data)
		if status != frameBounded {
			return measured
		}
		body := data[frame.headerLen:]
		if frame.bodyLen > int64(len(body)) {
			return measured
		}

		segment := data[:int64(frame.headerLen)+frame.bodyLen]
		reader := bytes.NewReader(segment)
		_, parseErr := packet.Read(reader)
		if unread := reader.Len(); unread != 0 {
			t.Errorf("%s: a tag %d packet left %d of %d bytes unread (parse error %v)",
				name, frame.tag, unread, len(segment), parseErr)
		}
		measured++
		data = body[frame.bodyLen:]
	}

	return measured
}

// TestV5ParsingStaysDisabled pins packet.V5Disabled, which go-crypto sets unless
// built with -tags v5: driveParser's cost argument relies on v5 signatures and v5
// secret key counters being refused before any length is read.
func TestV5ParsingStaysDisabled(t *testing.T) {
	t.Parallel()

	if !packet.V5Disabled {
		t.Fatalf("packet.V5Disabled = false, want true: two entries of driveParser's own enumeration assume it")
	}
}

// TestMPIStaysBoundedByItsTwoOctetLength pins that an MPI allocation is sized by
// its two-octet bit length, keeping tags 6 and 14 constant-cost; the floor shows
// the read happened. Not parallel: runtime.MemStats is process-wide.
func TestMPIStaysBoundedByItsTwoOctetLength(t *testing.T) {
	// A public key packet body: version 4, a creation time, RSA, and then a
	// modulus MPI declaring the largest bit length its two octets can name. The
	// bytes it names are not present, so the read fails after the allocation.
	segment := newFormatPacket(packetTagPublicKey, []byte{0x04, 0x00, 0x00, 0x00, 0x00, 0x01, 0xff, 0xff})

	var err error
	allocated := measureAlloc(func() {
		err = checkPacketFraming(segment, keyringProfile())
	})
	if allocated > mpiAllocCeiling {
		t.Fatalf("a %d-bit MPI allocated %d bytes, want at most %d", mpiDeclaredBits, allocated, mpiAllocCeiling)
	}
	if allocated < mpiAllocFloor {
		t.Fatalf("a %d-bit MPI allocated %d bytes, want at least the %d it names: the probe never reached the read",
			mpiDeclaredBits, allocated, mpiAllocFloor)
	}
	// What the gate made of it, below the measurement because the measurement is
	// the point: the parse fails and go-crypto consumes the rest, so nothing is
	// left unread and the walk has nothing to refuse.
	if err != nil {
		t.Fatalf("checkPacketFraming(a %d-bit MPI) = %v, want nil", mpiDeclaredBits, err)
	}
	t.Logf("a %d-bit MPI cost %d bytes over a %d-byte segment", mpiDeclaredBits, allocated, len(segment))
}

// admittedTagBodies are four bodies built to fail the parse, since the drive
// judges where the reader stopped, not what it made of the packet. The fourth's
// zeros keep it from over-declaring when walked as a signature body.
func admittedTagBodies() [][]byte {
	return [][]byte{
		nil,
		{0x00},
		make([]byte, 64<<10),
		{0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff},
	}
}

// admittedKeyringTags is every tag keyringPacketTags admits, hand-spelled and
// named apart from the production constant so that a tag leaving that set cannot
// quietly leave this sweep with it.
func admittedKeyringTags() []int {
	return []int{2, 6, 12, 13, 14, 17, 21}
}

// TestEveryAdmittedTagIsReadToItsEnd pins that a failed parse of every admitted
// keyring tag still consumes its frame, or the drive would refuse real key
// material; the under-read carrier is the control that the residual check works.
func TestEveryAdmittedTagIsReadToItsEnd(t *testing.T) {
	t.Parallel()

	if err := checkPacketFraming(underReadCarrier(t), keyringProfile()); !errors.Is(err, errMalformedSignaturePacket) {
		t.Fatalf("positive control: checkPacketFraming(the under-read carrier) = %v, want the malformed-packet refusal", err)
	}

	for _, tag := range admittedKeyringTags() {
		// The hand-spelled list and the production set are each other's check: a
		// tag listed here that the profile no longer admits would be refused by
		// the allow-list, and the row would measure that instead.
		if !keyringPacketTags.has(tag) {
			t.Fatalf("tag %d is not in keyringPacketTags, so it is not one of the tags this test is about", tag)
		}
		for i, body := range admittedTagBodies() {
			segment := newFormatPacket(tag, body)
			if err := checkPacketFraming(segment, keyringProfile()); err != nil {
				t.Errorf("tag %d body %d (%d bytes): checkPacketFraming = %v, want nil", tag, i, len(body), err)
			}
		}
	}
}

// driveTagCase is one driven tag and a body built to cost the drive as much as
// that tag's parser can be made to spend at the given length.
type driveTagCase struct {
	name string
	body []byte
	tag  int
}

// driveTagCases builds each admitted tag's costliest body at n bytes; tags with
// no structure of their own get zeros. A tag missing here is a parser nobody
// priced.
func driveTagCases(n int) []driveTagCase {
	// A public key prefix go-crypto parses: version 4, a creation time, RSA, and
	// a modulus declaring the largest bit length two octets can name, which is
	// the costliest a key packet gets.
	wideMPI := []byte{0x04, 0x00, 0x00, 0x00, 0x00, 0x01, 0xff, 0xff}
	// A v4 signature whose hashed area is packed with the smallest subpackets
	// this walk accepts, so the parser builds one record per three input bytes.
	signature := packedSignatureBody(n)
	// A user attribute area declaring one image subpacket that fills the body,
	// which is that parser's own costliest shape at this length.
	attribute := padTo([]byte{0xff, lowOctet(n >> 24), lowOctet(n >> 16), lowOctet(n >> 8), lowOctet(n), 0x01}, n)

	return []driveTagCase{
		{name: "a signature packed with subpackets", tag: packetTagSignature, body: signature},
		{name: "a public key naming a 65535-bit MPI", tag: packetTagPublicKey, body: padTo(wideMPI, n)},
		{name: "a public subkey naming a 65535-bit MPI", tag: packetTagPublicSubkey, body: padTo(wideMPI, n)},
		{name: "a ring-trust packet", tag: packetTagTrust, body: padTo(nil, n)},
		{name: "a user id", tag: packetTagUserID, body: padTo(nil, n)},
		{name: "a user attribute declaring one image subpacket", tag: packetTagUserAttribute, body: attribute},
		{name: "a padding packet", tag: packetTagPadding, body: padTo(nil, n)},
	}
}

// driveTagSizes spans two orders of magnitude so a per-packet cost that is
// really quadratic, or keyed on a size class, shows as a climbing ratio.
func driveTagSizes() []int {
	return []int{1 << 10, 8 << 10, 64 << 10, 256 << 10}
}

// TestDriveAllocationStaysLinearInTheSegment pins that no driven tag allocates
// out of proportion to its segment, which a go-crypto bump could change. Not
// parallel: runtime.MemStats is process-wide.
func TestDriveAllocationStaysLinearInTheSegment(t *testing.T) {
	for _, size := range driveTagSizes() {
		for _, tc := range driveTagCases(size) {
			segment := newFormatPacket(tc.tag, tc.body)
			// The fixture ahead of the measurement: a row whose frame did not
			// read as a bounded packet of its own tag would never reach the
			// drive, and would measure the walk refusing it instead.
			frame, status := readPacketFrame(segment)
			if status != frameBounded || frame.tag != tc.tag || frame.bodyLen != int64(len(tc.body)) {
				t.Fatalf("%s at %d: readPacketFrame = tag %d, body %d, status %d, want tag %d, body %d, bounded",
					tc.name, size, frame.tag, frame.bodyLen, status, tc.tag, len(tc.body))
			}

			allocated := measureAlloc(func() {
				_ = checkPacketFraming(segment, keyringProfile())
			})
			ceiling := uint64(driveAllocFloor) + uint64(len(segment))*driveAllocPerByte
			if allocated > ceiling {
				t.Errorf("%s at %d: tag %d allocated %d bytes over %d, want at most %d",
					tc.name, size, tc.tag, allocated, len(segment), ceiling)
			}
			t.Logf("%-40s tag %2d: %9d bytes over %7d, %.1f times the segment",
				tc.name, tc.tag, allocated, len(segment), float64(allocated)/float64(len(segment)))
		}
	}
}

// TestAllowedTagsAreTheHandSpelledSet states the blob, keyring and secret tag
// sets and both packet ceilings as literals, so a change is a deliberate edit,
// and checks that tags past 63 read as absent from a packetTagSet.
func TestAllowedTagsAreTheHandSpelledSet(t *testing.T) {
	t.Parallel()

	const tagSpace = 64

	blobTags := []int{2}
	keyTags := []int{2, 6, 12, 13, 14, 17, 21}
	secretTags := []int{5, 7}

	for tag := range tagSpace {
		assertTagMembership(t, tag, blobTags, keyTags, secretTags)
	}

	for _, tag := range []int{tagSpace, tagSpace + packetTagSignature, 255} {
		if keyringPacketTags.has(tag) {
			t.Errorf("keyringPacketTags.has(%d) = true, want false: a tag outside the space read as a member", tag)
		}
	}

	// The ceilings are the profiles' other term, and they are hand-spelled here
	// for the same reason the sets are. A profile is not a set: two callers with
	// the same vocabulary and different ceilings are two profiles.
	for _, tc := range []struct {
		name    string
		profile packetProfile
		want    int
	}{
		{name: "signatureBlobProfile", profile: signatureBlobProfile(), want: 64},
		{name: "keyringProfile", profile: keyringProfile(), want: 4096},
	} {
		if tc.profile.maxPackets != tc.want {
			t.Errorf("%s().maxPackets = %d, want %d", tc.name, tc.profile.maxPackets, tc.want)
		}
	}
}

// assertTagMembership checks one tag against all three sets, and that no tag is
// both admitted into a keyring and refused as secret material.
func assertTagMembership(t *testing.T, tag int, blobTags, keyTags, secretTags []int) {
	t.Helper()

	if got := keyringPacketTags.has(tag); got != slices.Contains(keyTags, tag) {
		t.Errorf("keyringPacketTags.has(%d) = %t, want %t", tag, got, !got)
	}
	if got := signatureBlobPacketTags.has(tag); got != slices.Contains(blobTags, tag) {
		t.Errorf("signatureBlobPacketTags.has(%d) = %t, want %t", tag, got, !got)
	}
	if got := secretKeyPacketTags.has(tag); got != slices.Contains(secretTags, tag) {
		t.Errorf("secretKeyPacketTags.has(%d) = %t, want %t", tag, got, !got)
	}
	// The two allow-lists and the secret set are disjoint by construction: an
	// admitted secret tag would be refused by an arm nobody reordered, which is a
	// state no message would explain.
	if keyringPacketTags.has(tag) && secretKeyPacketTags.has(tag) {
		t.Errorf("tag %d is both admitted into a keyring and refused as secret material", tag)
	}
}

// framedShape is one of the bounded header shapes a packet length can be spelled
// in, together with the body length that shape carries here.
type framedShape struct {
	name   string
	header []byte
	body   int
}

// framedShapes spells all six bounded length encodings by hand on the ring-trust
// tag, which the parser consumes to its declared end whatever the body, leaving
// the header shape as the only variable.
func framedShapes() []framedShape {
	return []framedShape{
		{name: "an old-format one-octet length", header: []byte{0xb0, 0x0a}, body: 10},
		{name: "an old-format two-octet length", header: []byte{0xb1, 0x01, 0x2c}, body: 300},
		{name: "an old-format four-octet length", header: []byte{0xb2, 0x00, 0x00, 0x01, 0x90}, body: 400},
		{name: "a new-format one-octet length", header: []byte{0xcc, 0x0a}, body: 10},
		{name: "a new-format two-octet length", header: []byte{0xcc, 0xc0, 0x08}, body: 200},
		{name: "a new-format five-octet length", header: []byte{0xcc, 0xff, 0x00, 0x00, 0x01, 0xf4}, body: 500},
	}
}

// TestFrameBoundsAgreeWithTheParser pins that readPacketFrame and go-crypto
// agree on where a packet ends for every length encoding, since the drive judges
// the parser's residual inside this walk's boundary.
func TestFrameBoundsAgreeWithTheParser(t *testing.T) {
	t.Parallel()

	for _, shape := range framedShapes() {
		// Four bytes nothing should touch, so a parser reading past the declared
		// body has somewhere to read into.
		data := append(append([]byte{}, shape.header...), padTo(nil, shape.body+len(shape.header))[len(shape.header):]...)
		data = append(data, 0xde, 0xad, 0xbe, 0xef)

		frame, status := readPacketFrame(data)
		if status != frameBounded || frame.bodyLen != int64(shape.body) {
			t.Errorf("%s: readPacketFrame = body %d, status %d, want body %d, bounded", shape.name, frame.bodyLen, status, shape.body)

			continue
		}

		reader := bytes.NewReader(data)
		if _, err := packet.Read(reader); err == nil {
			t.Errorf("%s: packet.Read = nil error, want the unknown-tag error a trust packet produces", shape.name)

			continue
		}
		consumed := len(data) - reader.Len()
		if want := int64(frame.headerLen) + frame.bodyLen; int64(consumed) != want {
			t.Errorf("%s: the parser took %d bytes, the walk computed %d", shape.name, consumed, want)
		}
	}
}

// TestEmbeddedSignatureRecursionReachesCommittedMaterial uses the depth cap as
// an oracle: exactly one of the subkey fixture's two signatures is refused when
// walked from the cap, so the recursion descends into real material.
func TestEmbeddedSignatureRecursionReachesCommittedMaterial(t *testing.T) {
	t.Parallel()

	decoded, _, err := decodeArmorBlock(readFixture(t, subkeyFixture))
	if err != nil {
		t.Fatalf("decodeArmorBlock(%s) = %v, want nil", subkeyFixture, err)
	}

	bodies := signaturePacketBodies(decoded)
	if len(bodies) != subkeyFixtureSignatures {
		t.Fatalf("%s holds %d signature packets, want %d", subkeyFixture, len(bodies), subkeyFixtureSignatures)
	}

	descended := 0
	for i, body := range bodies {
		// The acceptance half: real material walks clean, embedded signature and
		// all, or the refusal below would say nothing.
		if walkErr := checkSignatureBodyFraming(body, 0); walkErr != nil {
			t.Fatalf("checkSignatureBodyFraming(%s signature %d) = %v, want nil", subkeyFixture, i, walkErr)
		}
		if errors.Is(checkSignatureBodyFraming(body, maxEmbeddedSignatureDepth), errMalformedSignaturePacket) {
			descended++
		}
	}

	if descended != 1 {
		t.Fatalf("the walk descended into %d of %s's %d signatures, want exactly 1", descended, subkeyFixture, len(bodies))
	}
}

// TestEmbeddedSignatureDepthIsCapped pins the cap as a boundary: a chain exactly
// maxEmbeddedSignatureDepth deep is walked, one level more is refused.
func TestEmbeddedSignatureDepthIsCapped(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		levels     int
		wantRefuse bool
	}{
		{name: "a chain as deep as the cap allows", levels: maxEmbeddedSignatureDepth},
		{name: "a chain one level deeper", levels: maxEmbeddedSignatureDepth + 1, wantRefuse: true},
	} {
		data := nestedEmbeddedCarrier(tc.levels)
		err := checkPacketFraming(data, signatureBlobProfile())
		if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
			t.Errorf("%s (%d bytes) refused = %t (err %v), want refused = %t", tc.name, len(data), got, err, tc.wantRefuse)
		}
	}
}

// signatureVersionCase is one signature packet differing from its neighbors in
// its version octet alone, and the verdict that octet earns it.
type signatureVersionCase struct {
	name       string
	body       []byte
	wantRefuse bool
}

// signatureVersionCases pins which versions have their lengths judged: v4, v5
// and v6 over-declare and are refused, while v3 (a real certification signature
// whose creation time sits at that offset), v0 and v7 are accepted.
func signatureVersionCases() []signatureVersionCase {
	return []signatureVersionCase{
		{
			name: "a v3 certification signature",
			body: []byte{
				0x03, 0x05, 0x10, 0x68, 0x9f, 0x3c, 0x21,
				0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
				0x01, 0x08, 0xaa, 0xbb, 0x00, 0x08, 0xff,
			},
		},
		{name: "a v4 signature over-declaring its hashed area", body: []byte{0x04, 0x13, 0x01, 0x08, 0xff, 0xff}, wantRefuse: true},
		{name: "a v5 signature over-declaring its hashed area", body: []byte{0x05, 0x13, 0x01, 0x08, 0xff, 0xff}, wantRefuse: true},
		{
			name:       "a v6 signature over-declaring its hashed area",
			body:       []byte{0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
			wantRefuse: true,
		},
		{name: "a version 0 signature", body: []byte{0x00, 0x13, 0x01, 0x08, 0xff, 0xff}},
		{name: "a version 7 signature", body: []byte{0x07, 0x13, 0x01, 0x08, 0xff, 0xff}},
	}
}

// TestSignatureVersionDecidesWhetherLengthsAreJudged pins that lengths are
// judged only for v4, v6 and v5 (in case the V5Disabled default moves): other
// versions have no length at that offset, and judging v3 refused real keyrings.
func TestSignatureVersionDecidesWhetherLengthsAreJudged(t *testing.T) {
	t.Parallel()

	for _, tc := range signatureVersionCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data := newFormatPacket(packetTagSignature, tc.body)
			err := checkPacketFraming(data, signatureBlobProfile())
			if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
				t.Fatalf("checkPacketFraming(%s) refused = %t (err %v), want refused = %t", tc.name, got, err, tc.wantRefuse)
			}
		})
	}
}

// admittedShapeAllocCeiling bounds one admitted shape's walk and drive. It sits
// between the 1800 bytes the costliest shape measured and the control's 10112.
const admittedShapeAllocCeiling = 4 << 10

// admittedShapes are the inputs the walk accepts because go-crypto has nothing
// data-derived left to allocate from them: the accepting rows of the truncation
// and version tables, plus the zero-length subpacket.
func admittedShapes() []framingCase {
	shapes := make([]framingCase, 0, len(truncationCases())+len(signatureVersionCases())+1)
	for _, tc := range truncationCases() {
		if !tc.wantRefuse {
			shapes = append(shapes, framingCase{name: tc.name, data: tc.data, profile: signatureBlobProfile()})
		}
	}
	for _, tc := range signatureVersionCases() {
		if !tc.wantRefuse {
			shapes = append(shapes, framingCase{
				name:    tc.name,
				data:    newFormatPacket(packetTagSignature, tc.body),
				profile: signatureBlobProfile(),
			})
		}
	}

	return append(shapes, framingCase{
		name:    "a signature subpacket of no length at all",
		data:    []byte{0xc2, 0x08, 0x04, 0x13, 0x01, 0x08, 0x00, 0x02, 0x00, 0x00},
		profile: signatureBlobProfile(),
	})
}

// TestAdmittedShapesAllocateNothing pins that shapes the walk admits cost the
// parser nothing; the control, an admitted 65535-bit MPI, shows the measurement
// sees allocation. Not parallel: runtime.MemStats is process-wide.
func TestAdmittedShapesAllocateNothing(t *testing.T) {
	// Control: a v4 signature with a creation time subpacket and an RSA MPI of
	// 65535 bits, an allocation the parser makes from bytes this walk admits.
	control := newFormatPacket(packetTagSignature, []byte{
		0x04, 0x13, 0x01, 0x08, 0x00, 0x06,
		0x05, 0x02, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0xaa, 0xbb, 0xff, 0xff,
	})
	var controlErr error
	controlAlloc := measureAlloc(func() {
		controlErr = checkPacketFraming(control, signatureBlobProfile())
	})
	if controlErr != nil {
		t.Fatalf("positive control: checkPacketFraming(a v4 signature declaring a 65535-bit MPI) = %v, want nil", controlErr)
	}
	if controlAlloc <= admittedShapeAllocCeiling {
		t.Fatalf("positive control: an admitted 65535-bit MPI allocated %d bytes, want more than the %d ceiling: "+
			"this measurement cannot see an allocation", controlAlloc, admittedShapeAllocCeiling)
	}

	var worst uint64
	worstName := "none"
	for _, shape := range admittedShapes() {
		var err error
		allocated := measureAlloc(func() {
			err = checkPacketFraming(shape.data, shape.profile)
		})
		if err != nil {
			t.Errorf("checkPacketFraming(%s) = %v, want nil: this row is not one of the admitted shapes", shape.name, err)

			continue
		}
		if allocated > worst {
			worst, worstName = allocated, shape.name
		}
		if allocated > admittedShapeAllocCeiling {
			t.Errorf("checkPacketFraming(%s) allocated %d bytes over %d, want at most %d",
				shape.name, allocated, len(shape.data), admittedShapeAllocCeiling)
		}
	}
	t.Logf("the costliest of %d admitted shapes (%s) allocated %d bytes, against the control's %d",
		len(admittedShapes()), worstName, worst, controlAlloc)
}

// emptySubpacketPacket is the twelve-byte reproducer that panics go-crypto's
// parseSignatureSubpacket: a hashed subpacket with a type octet and no body.
//
//nolint:gochecknoglobals // a fixed fixture consumed by the tests, not mutable shared state
var emptySubpacketPacket = []byte{0xc2, 0x0a, 0x04, 0x13, 0x01, 0x08, 0x00, 0x02, 0x01, 0x04, 0x00, 0x00}

// emptySubpacketControl is that packet with one octet of subpacket body, the
// shortest subpacket the walk accepts.
//
//nolint:gochecknoglobals // a fixed fixture consumed by the tests, not mutable shared state
var emptySubpacketControl = []byte{0xc2, 0x0b, 0x04, 0x13, 0x01, 0x08, 0x00, 0x03, 0x02, 0x04, 0x01, 0x00, 0x00}

// TestEmptySubpacketBodyIsRefused pins the refusal that prevents a go-crypto
// panic; its control is the same packet with one octet of body, which must pass
// both the walk and the drive.
func TestEmptySubpacketBodyIsRefused(t *testing.T) {
	t.Parallel()

	// The whole message hand-spelled, and apart from the production format
	// string it checks, so that a mutation to that string cannot reshape the
	// expectation into agreeing with it.
	const want = "malformed OpenPGP packet framing: a signature subpacket carries a type octet and no body"

	if err := checkPacketFraming(emptySubpacketControl, signatureBlobProfile()); err != nil {
		t.Fatalf("positive control: checkPacketFraming(a one-octet subpacket body) = %v, want nil", err)
	}

	err := checkPacketFraming(emptySubpacketPacket, signatureBlobProfile())
	if err == nil || err.Error() != want {
		t.Fatalf("checkPacketFraming(a subpacket with no body) = %v, want %q", err, want)
	}
}

// subpacketFormCase is one subpacket area whose first subpacket spells its
// length in one of the three forms, and the verdict the area earns.
type subpacketFormCase struct {
	name       string
	area       []byte
	wantRefuse bool
}

// subpacketFormCases puts one subpacket of each length form ahead of a refused
// embedded signature, so only exact length arithmetic lands on it and refuses;
// the last row swaps in a well-formed subpacket as the control.
func subpacketFormCases() []subpacketFormCase {
	// An embedded signature subpacket whose inner body over-declares its hashed
	// area, and a well-formed subpacket of the shortest length the walk accepts.
	refusing := []byte{0x0a, 0x20, 0x04, 0x13, 0x01, 0x08, 0xff, 0xff, 0x00, 0x00, 0x00}
	accepting := []byte{0x02, 0x0b, 0x09}

	return []subpacketFormCase{
		{name: "a one-octet subpacket length", area: subpacketFormArea([]byte{0x0a}, 10, refusing), wantRefuse: true},
		{name: "a two-octet subpacket length", area: subpacketFormArea([]byte{0xc0, 0x22}, 226, refusing), wantRefuse: true},
		{
			name:       "a five-octet subpacket length",
			area:       subpacketFormArea([]byte{0xff, 0x00, 0x00, 0x01, 0x2c}, 300, refusing),
			wantRefuse: true,
		},
		{name: "the same area ending on a subpacket with a body", area: subpacketFormArea([]byte{0x0a}, 10, accepting)},
	}
}

// subpacketFormArea builds a subpacket area: a hand-spelled length field, that
// many octets of a non-embedded type and 0xff fill, then tail. 226 and 300 are
// the shortest lengths no other form's arithmetic yields from the same octets.
func subpacketFormArea(lengthField []byte, contents int, tail []byte) []byte {
	area := make([]byte, 0, len(lengthField)+contents+len(tail))
	area = append(area, lengthField...)
	area = append(area, 0x0b)
	for range contents - 1 {
		area = append(area, 0xff)
	}

	return append(area, tail...)
}

// TestSubpacketLengthFormsDecideTheVerdict covers readSubpacketLength's two- and
// five-octet forms, which no committed subpacket or other case here reaches.
func TestSubpacketLengthFormsDecideTheVerdict(t *testing.T) {
	t.Parallel()

	for _, tc := range subpacketFormCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body := make([]byte, 0, 6+len(tc.area))
			body = append(body, 0x04, 0x13, 0x01, 0x08, lowOctet(len(tc.area)>>8), lowOctet(len(tc.area)))
			body = append(body, tc.area...)

			err := checkPacketFraming(newFormatPacket(packetTagSignature, body), signatureBlobProfile())
			if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
				t.Fatalf("checkPacketFraming(%s) refused = %t (err %v), want refused = %t", tc.name, got, err, tc.wantRefuse)
			}
		})
	}
}

// refusedByTheGate reports whether err is one of the gate's two carrier verdicts:
// a secret key packet through LoadKeyring carries errSecretKeyPacket without the
// framing sentinel, so its message can name the export to make.
func refusedByTheGate(err error) bool {
	return errors.Is(err, errMalformedSignaturePacket) || errors.Is(err, errSecretKeyPacket)
}

// carrierFinding is one carrier that got past the gate through one entry point,
// kept rather than fataled on so the failure can name every one that did and
// what each of them cost.
type carrierFinding struct {
	carrier   string
	entry     string
	allocated uint64
}

// framingCarrier is one input built to reach go-crypto's data-derived allocation
// through a gate that judges declared lengths alone.
type framingCarrier struct {
	name string
	data []byte
}

// framingCarriers builds every input measured to reach go-crypto's data-derived
// allocation past a gate judging declared lengths alone, the original amplifying
// blob included so a change reopening the oldest hole is caught too.
func framingCarriers(tb testing.TB) []framingCarrier {
	tb.Helper()

	return []framingCarrier{
		{
			name: "a marker packet carrying the amplifier",
			data: []byte{0xca, 0x0d, 0x50, 0x47, 0x50, 0xc2, 0x08, 0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
		},
		{name: "a public key packet under-reading the amplifier", data: underReadCarrier(tb)},
		{name: "a secret key packet under-reading the amplifier", data: dummyS2KCarrier()},
		{
			name: "a v4 signature embedding a v6 one",
			data: []byte{0xc2, 0x10, 0x04, 0x00, 0x01, 0x08, 0x00, 0x0a, 0x09, 0x20, 0x06, 0x19, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
		},
		{name: "a v6 signature declaring 4 GiB", data: synthesizedBlobs[amplifyingBlob]},
		{name: "a chain of embedded signatures", data: nestedEmbeddedCarrier(maxEmbeddedSignatureDepth + 1)},
		{name: "a v4 signature declaring 65535 bytes", data: []byte{0xc2, 0x08, 0x04, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff}},
	}
}

// TestEveryCarrierIsRefusedCheaplyAtBothEntryPoints measures every carrier as a
// keyring and as a signature, binary and armored, collecting every escape before
// failing. Not parallel: runtime.MemStats is process-wide.
func TestEveryCarrierIsRefusedCheaplyAtBothEntryPoints(t *testing.T) {
	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	// Positive control: real keyrings load and real signatures verify.
	requireFramingFixturesStillWork(t, kr, manifest)

	var overCeiling, wrongVerdict []carrierFinding
	// Declared once rather than per iteration: it closes over the two
	// accumulators, and a closure rebuilt inside the loop would allocate one per
	// carrier for no gain.
	record := func(carrier, entry string, allocated uint64, err error) {
		finding := carrierFinding{carrier: carrier, entry: entry, allocated: allocated}
		if allocated > framingAllocCeiling {
			overCeiling = append(overCeiling, finding)
		}
		if !refusedByTheGate(err) {
			wrongVerdict = append(wrongVerdict, finding)
		}
	}

	// One leaf, rewritten per row: LoadKeyring takes a path and every write
	// truncates, so the table costs one file rather than one per carrier.
	path := filepath.Join(t.TempDir(), "carrier.gpg")
	for _, carrier := range framingCarriers(t) {
		for _, enc := range []struct {
			entry string
			data  []byte
		}{
			{entry: "LoadKeyring binary", data: carrier.data},
			{entry: "LoadKeyring armored", data: armorEncode(t, openpgp.PublicKeyType, carrier.data)},
		} {
			// #nosec G703 -- the directory is this test's own t.TempDir and the
			// leaf is a constant; no part of the path comes from outside this
			// function.
			if err := os.WriteFile(path, enc.data, keyringFileMode); err != nil {
				t.Fatalf("write %s as %s: %v", carrier.name, enc.entry, err)
			}

			var loadErr error
			allocated := measureAlloc(func() {
				_, loadErr = LoadKeyring(path)
			})
			record(carrier.name, enc.entry, allocated, loadErr)
		}

		for _, enc := range []struct {
			entry string
			data  []byte
		}{
			{entry: "checkOne binary", data: carrier.data},
			{entry: "checkOne armored", data: armorEncode(t, openpgp.SignatureType, carrier.data)},
		} {
			var checkErr error
			allocated := measureAlloc(func() {
				_, checkErr = checkOne(manifest, enc.data, kr)
			})
			record(carrier.name, enc.entry, allocated, checkErr)
		}
	}

	if len(overCeiling) > 0 {
		t.Errorf("%d carriers allocated more than %d bytes: %+v", len(overCeiling), framingAllocCeiling, overCeiling)
	}
	// Which verdict came with the cheap outcome, asked separately; it also catches
	// carriers whose unguarded cost is a wrong verdict rather than an allocation.
	if len(wrongVerdict) > 0 {
		t.Errorf("%d carriers were not refused as malformed framing: %+v", len(wrongVerdict), wrongVerdict)
	}
}

// TestUnboundedFramingIsJudgedBeforeTheTag pins judgeHeader's arm order: a
// partial length on an unadmitted tag is refused as unbounded framing, since the
// tag of a packet whose end cannot be found decides nothing.
func TestUnboundedFramingIsJudgedBeforeTheTag(t *testing.T) {
	t.Parallel()

	// The whole message hand-spelled, and apart from the production format string
	// it checks, so that a mutation to that string cannot reshape the expectation
	// into agreeing with it.
	const want = "malformed OpenPGP packet framing: a tag 1 packet carries " +
		"a new-format partial length, which declares no body length at all"

	err := checkPacketFraming([]byte{0xc1, 0xe0, 0x00, 0x00}, keyringProfile())
	if err == nil || err.Error() != want {
		t.Fatalf("checkPacketFraming(a partial length on tag 1) = %v, want %q", err, want)
	}
}

const (
	// secretKeyHalfBits sizes the CPU carrier's two factors: a 65520-bit modulus,
	// the costliest RSA secret key an MPI's two-octet bit length allows.
	secretKeyHalfBits = 32760
	// secretKeyProofHalfBits is the size the parse cost is actually measured at,
	// where it is a fifth of a second rather than twelve of them.
	secretKeyProofHalfBits = 8192

	// secretKeyRefusalCeiling bounds refusing the CPU carrier: measured at a few
	// hundred microseconds against about 12s when the parser saw it.
	secretKeyRefusalCeiling = 2 * time.Second
	// secretKeyParseFloor is what the smaller key's parse must exceed for the proof
	// to have reached key validation; measured at about 200ms.
	secretKeyParseFloor = 10 * time.Millisecond

	// dummyS2KBodyLen is how many octets of that carrier's body the parse reads
	// before it returns at the GNU-dummy S2K: a v4 key prefix, two 8-bit MPIs,
	// the s2k usage octet, a cipher, and the six-octet dummy s2k.
	dummyS2KBodyLen = 20
)

// TestSecretKeyPacketsAreRefusedAtTheGate pins that tags 5 and 7 are refused at
// the header: a dummy-S2K parse succeeds leaving its body unread, and RSA key
// validation costs time superlinear in MPI size. Not parallel: it measures time.
func TestSecretKeyPacketsAreRefusedAtTheGate(t *testing.T) {
	// Positive control: the public half of the same key walks clean, so a refusal
	// below is the secret-key tag.
	control := newFormatPacket(packetTagPublicKey, rsaPublicKeyBody(secretKeyProofHalfBits))
	if err := checkPacketFraming(control, keyringProfile()); err != nil {
		t.Fatalf("positive control: checkPacketFraming(a public key packet of the same key) = %v, want nil", err)
	}

	carrier := requireDummyS2KDesync(t)

	// The CPU carrier is first because it is the row that can fail on the time
	// rather than on the message, and only a row reached at all can do that.
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "an RSA secret key of the widest MPIs one can carry", data: rsaSecretKeyPacket(t, secretKeyHalfBits)},
		{name: "a secret key packet parsing to a dummy S2K", data: carrier},
		{name: "the same body on the secret subkey tag", data: newFormatPacket(packetTagPrivateSubkey, carrier[6:])},
	} {
		path := filepath.Join(dir, "secret.gpg")
		// #nosec G703 -- dir is this test's own t.TempDir and the leaf is a
		// constant; no part of the path comes from outside this function.
		if err := os.WriteFile(path, tc.data, keyringFileMode); err != nil {
			t.Fatalf("write %s: %v", tc.name, err)
		}

		start := time.Now()
		_, err := LoadKeyring(path)
		elapsed := time.Since(start)
		if elapsed > secretKeyRefusalCeiling {
			t.Fatalf("LoadKeyring(%s) took %v, want at most %v", tc.name, elapsed, secretKeyRefusalCeiling)
		}
		if err == nil || !strings.Contains(err.Error(), "secret key material") {
			t.Fatalf("LoadKeyring(%s) = %v, want the secret-material message", tc.name, err)
		}
		if !errors.Is(err, helpers.ErrKeyringUnreadable) {
			t.Fatalf("LoadKeyring(%s) error = %v, want the unreadable sentinel", tc.name, err)
		}
		t.Logf("%-48s %6d bytes refused in %v", tc.name, len(tc.data), elapsed)
	}

	// What the refusal saves: the parse of a smaller secret key packet must take
	// real time, or the ceiling above proves nothing.
	proof := rsaSecretKeyPacket(t, secretKeyProofHalfBits)
	start := time.Now()
	_, _ = packet.Read(bytes.NewReader(proof))
	spent := time.Since(start)
	if spent < secretKeyParseFloor {
		t.Fatalf("parsing a %d-byte secret key packet took %v, want at least %v: the proof never reached key validation",
			len(proof), spent, secretKeyParseFloor)
	}
	t.Logf("the parser spent %v on one %d-byte secret key packet, which is what the refusal above no longer pays", spent, len(proof))
}

// requireDummyS2KDesync returns the dummy-S2K carrier after checking it still
// parses with a nil error and leaves bytes unread, which is the desync the
// secret-key arm answers.
func requireDummyS2KDesync(t *testing.T) []byte {
	t.Helper()

	carrier := dummyS2KCarrier()
	reader := bytes.NewReader(carrier)
	_, parseErr := packet.Read(reader)
	if parseErr != nil || reader.Len() == 0 {
		t.Fatalf("the dummy-S2K carrier parsed with error %v leaving %d of %d bytes unread, want a nil error and a residual",
			parseErr, reader.Len(), len(carrier))
	}
	t.Logf("the parser read the dummy-S2K carrier with a nil error and left %d of its %d bytes unread",
		reader.Len(), len(carrier))

	return carrier
}

// dummyS2KCarrier builds a tag 5 packet whose parse returns at a GNU-dummy S2K,
// leaving the rest of its declared body, the amplifying packet, unread.
func dummyS2KCarrier() []byte {
	amplifier := synthesizedBlobs[amplifyingBlob]
	body := make([]byte, 0, dummyS2KBodyLen+len(amplifier))
	body = append(body,
		0x04, 0x00, 0x00, 0x00, 0x00, 0x01,
		0x00, 0x08, 0xff,
		0x00, 0x08, 0x03,
		0xfe,
		0x09,
		0x65, 0x02, 'G', 'N', 'U', 0x01,
	)

	return newFormatPacket(packetTagPrivateKey, append(body, amplifier...))
}

// rsaPublicKeyBody builds the public half of the key rsaSecretKeyPacket builds:
// a v4 key packet body carrying a modulus of two halfBits-bit factors and an
// exponent.
func rsaPublicKeyBody(halfBits int) []byte {
	p, q := patternFactor(halfBits, 251), patternFactor(halfBits, 241)

	body := []byte{0x04, 0x00, 0x00, 0x00, 0x00, 0x01}
	body = append(body, encodeMPI(new(big.Int).Mul(p, q))...)

	return append(body, encodeMPI(big.NewInt(65537))...)
}

// rsaSecretKeyPacket builds an unencrypted RSA secret key packet of two
// halfBits-bit pattern factors; rsa.Validate precomputes before rejecting them,
// so only their size matters and the timing stays reproducible.
func rsaSecretKeyPacket(t *testing.T, halfBits int) []byte {
	t.Helper()

	p, q := patternFactor(halfBits, 251), patternFactor(halfBits, 241)

	body := rsaPublicKeyBody(halfBits)
	// The s2k usage octet naming no encryption, so the secret MPIs behind it are
	// read in the clear and the 16-bit checksum is the only thing between them
	// and parsePrivateKey.
	body = append(body, 0x00)

	// A one-octet private exponent keeps the packet at the size of its two
	// factors, which is what the timing scales with.
	secret := encodeMPI(big.NewInt(3))
	secret = append(secret, encodeMPI(p)...)
	secret = append(secret, encodeMPI(q)...)
	var sum uint16
	for _, b := range secret {
		sum += uint16(b)
	}
	secret = append(secret, lowOctet(int(sum>>8)), lowOctet(int(sum)))

	return newFormatPacket(packetTagPrivateKey, append(body, secret...))
}

// patternFactor builds one halfBits-bit odd number from a repeating pattern of
// period bytes, with the top bit set so it is exactly that wide.
func patternFactor(halfBits, period int) *big.Int {
	buf := make([]byte, halfBits/8)
	for i := range buf {
		buf[i] = lowOctet(i%period) | 0x01
	}
	buf[0] |= 0x80

	return new(big.Int).SetBytes(buf)
}

// encodeMPI renders n as an OpenPGP multiprecision integer: a two-octet bit
// length and then the bytes.
func encodeMPI(n *big.Int) []byte {
	b := n.Bytes()
	bits := n.BitLen()
	out := make([]byte, 0, 2+len(b))
	out = append(out, lowOctet(bits>>8), lowOctet(bits))

	return append(out, b...)
}

// underReadCarrier re-frames a committed public key body to declare ten bytes
// more than go-crypto reads, with the amplifying packet in those bytes.
func underReadCarrier(tb testing.TB) []byte {
	tb.Helper()

	frame, status := readPacketFrame(readFixture(tb, binaryFixture))
	if status != frameBounded || frame.tag != packetTagPublicKey {
		tb.Fatalf("%s does not open with a bounded public key packet: tag %d, status %d", binaryFixture, frame.tag, status)
	}
	body := readFixture(tb, binaryFixture)[frame.headerLen:][:frame.bodyLen]

	payload := make([]byte, 0, len(body)+len(synthesizedBlobs[amplifyingBlob]))
	payload = append(payload, body...)
	payload = append(payload, synthesizedBlobs[amplifyingBlob]...)

	return newFormatPacket(packetTagPublicKey, payload)
}

// nestedEmbeddedCarrier builds a signature packet nesting levels of embedded
// signature, each the single hashed subpacket of the level above.
func nestedEmbeddedCarrier(levels int) []byte {
	body := []byte{0x04, 0x13, 0x01, 0x08, 0x00, 0x00, 0x00, 0x00}

	for range levels {
		// A subpacket: a one-octet length covering the type octet and the body,
		// then the embedded signature type, then that body. The lengths stay
		// inside one octet for every depth this builds.
		sub := make([]byte, 0, 2+len(body))
		sub = append(sub, lowOctet(1+len(body)), 0x20)
		sub = append(sub, body...)

		// A v4 signature whose hashed area is that subpacket and whose unhashed
		// area is empty.
		wrapped := make([]byte, 0, 6+len(sub)+2)
		wrapped = append(wrapped, 0x04, 0x13, 0x01, 0x08, lowOctet(len(sub)>>8), lowOctet(len(sub)))
		wrapped = append(wrapped, sub...)
		wrapped = append(wrapped, 0x00, 0x00)
		body = wrapped
	}

	return newFormatPacket(packetTagSignature, body)
}

// signaturePacketBodies returns the body of every signature packet in a walked
// packet stream, in stream order, stopping where the walk can no longer read a
// bounded frame.
func signaturePacketBodies(data []byte) [][]byte {
	var bodies [][]byte

	for len(data) > 0 {
		frame, status := readPacketFrame(data)
		if status != frameBounded {
			return bodies
		}
		body := data[frame.headerLen:]
		if frame.bodyLen > int64(len(body)) {
			return bodies
		}
		if frame.tag == packetTagSignature {
			bodies = append(bodies, body[:frame.bodyLen])
		}
		data = body[frame.bodyLen:]
	}

	return bodies
}

// newFormatPacket frames body as a new-format packet of tag with a five-octet
// length, hand-assembled so a mutated production constant cannot reshape it.
func newFormatPacket(tag int, body []byte) []byte {
	const (
		newFormatLead      = 0xc0
		fiveOctetMarker    = 0xff
		fiveOctetHeaderLen = 6
	)

	n := len(body)
	out := make([]byte, 0, fiveOctetHeaderLen+n)
	out = append(out, lowOctet(newFormatLead|tag), fiveOctetMarker,
		lowOctet(n>>24), lowOctet(n>>16), lowOctet(n>>8), lowOctet(n))

	return append(out, body...)
}

// lowOctet is the low eight bits of n, which is how the hand-built fixtures here
// spell a length every one of them keeps below 256 by construction. The mask is
// what says so to a reader and to the integer-conversion analyzer alike.
func lowOctet(n int) byte {
	return byte(n & 0xff)
}

// repeat tiles unit until the result is at least n bytes long, which is how the
// adversarial bodies above are packed with the smallest record a parser will
// build one struct for.
func repeat(unit []byte, n int) []byte {
	out := make([]byte, 0, n+len(unit))
	for len(out) < n {
		out = append(out, unit...)
	}

	return out
}

// padTo returns prefix followed by enough zero bytes to reach n, which is how a
// body that has to parse is given a length worth measuring.
func padTo(prefix []byte, n int) []byte {
	out := make([]byte, n)
	copy(out, prefix)

	return out
}

// packedSignatureBody builds a v6 signature body of about n bytes packed with
// the smallest accepted subpackets; v6's four-octet length keeps sizes above
// 65535 from wrapping the declared hashed length.
func packedSignatureBody(n int) []byte {
	const (
		prefixLen   = 8
		unhashedLen = 4
	)

	hashed := repeat([]byte{0x02, 0x0b, 0x09}, n-prefixLen-unhashedLen)

	body := make([]byte, 0, prefixLen+len(hashed)+unhashedLen)
	body = append(body, 0x06, 0x13, 0x01, 0x08,
		lowOctet(len(hashed)>>24), lowOctet(len(hashed)>>16), lowOctet(len(hashed)>>8), lowOctet(len(hashed)))
	body = append(body, hashed...)
	body = append(body, 0x00, 0x00, 0x00, 0x00)

	return body
}

const (
	// fuzzMaxInput is the largest input the fuzz oracle judges; every hole the gate
	// closes was reached with under a hundred bytes.
	fuzzMaxInput = 1 << 16

	// fuzzAllocPerByte is the power of two above the worst measured per-byte cost,
	// 966.5 (64 ten-byte keys each naming a 0xffff-bit MPI). It is one constant on
	// purpose: a packet term would put the gate's design into the oracle.
	fuzzAllocPerByte = 1024
	// fuzzAllocFloor is what an input of no length may cost, absorbing both entry
	// points' fixed cost so the per-byte ratio need not.
	fuzzAllocFloor = 1 << 20
)

// fuzzCeiling is what either entry point may allocate for data's own length.
func fuzzCeiling(data []byte) uint64 {
	return fuzzAllocFloor + uint64(len(data))*fuzzAllocPerByte
}

// allocDelta is measureAlloc without the leading GC: TotalAlloc is exact either
// way, and the fuzzer cannot afford a collection per execution.
func allocDelta(f func()) uint64 {
	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc
}

// FuzzSignatureParsingIsBoundedByItsInput pins that readEntities and checkOne
// allocate in proportion to their input, naming no gate internal. A legitimate
// input over the ceiling moves the constant, never a special case.
func FuzzSignatureParsingIsBoundedByItsInput(f *testing.F) {
	kr, err := LoadKeyring(fixturePath(keyringFixture))
	if err != nil {
		f.Fatalf("LoadKeyring(%s) = %v, want nil", keyringFixture, err)
	}
	manifest := readFixture(f, manifestAFixture)

	for _, seed := range fuzzSeeds(f) {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzMaxInput {
			t.Skip("larger than the input this ceiling is stated for")
		}
		ceiling := fuzzCeiling(data)

		if allocated := allocDelta(func() { _, _ = readEntities(data) }); allocated > ceiling {
			t.Fatalf("readEntities(%d bytes) allocated %d bytes, want at most %d", len(data), allocated, ceiling)
		}
		if allocated := allocDelta(func() { _, _ = checkOne(manifest, data, kr) }); allocated > ceiling {
			t.Fatalf("checkOne(%d bytes) allocated %d bytes, want at most %d", len(data), allocated, ceiling)
		}
	})
}

// fuzzSeeds collects every committed fixture, each decoded armor body (the gate
// runs on decoded bytes), every carrier, and three reproducers.
func fuzzSeeds(f *testing.F) [][]byte {
	f.Helper()

	entries, err := os.ReadDir(testdataDir)
	if err != nil {
		f.Fatalf("read %s: %v", testdataDir, err)
	}

	seeds := make([][]byte, 0, len(entries)*2)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data := readFixture(f, entry.Name())
		seeds = append(seeds, data)
		if decoded, _, decodeErr := decodeArmorBlock(data); decodeErr == nil {
			seeds = append(seeds, decoded)
		}
	}

	for _, carrier := range framingCarriers(f) {
		seeds = append(seeds, carrier.data)
	}

	// Reproducers: the subpacket panic, the amplifier behind a non-header octet, and
	// a short stream of empty signature packets.
	seeds = append(seeds, emptySubpacketPacket)
	seeds = append(seeds, append([]byte{0x00}, synthesizedBlobs[amplifyingBlob]...))
	seeds = append(seeds, repeat([]byte{0xc2, 0x00}, 2*(signatureBlobMaxPackets+1)))

	return seeds
}

const (
	// emptyBlocksAtCeiling is how many empty armor blocks fit ahead of the binary
	// export at exactly the packet ceiling: every block, the export's included,
	// costs one packet, and the export adds its own three.
	emptyBlocksAtCeiling = keyringMaxPackets - binaryFixturePackets - armorBlockPacketCost

	// emptyBlockAllocCeiling bounds refusing a file of empty blocks: measured at
	// 20.5 MB under the race detector, against 70.5 MB when blocks were uncharged.
	emptyBlockAllocCeiling = 32 << 20

	// emptyBlockAbuseCount is how many empty blocks that measurement carries. It
	// is far past the ceiling on purpose: what it measures is that the refusal
	// arrives at the ceiling rather than at the end of the file.
	emptyBlockAbuseCount = 20000
)

// armorBlockBudgetCase is one file of empty armor blocks, optionally ending on
// a real key export, and the verdict the packet budget owes it.
type armorBlockBudgetCase struct {
	name         string
	wantMessage  string
	empties      int
	wantEntities int
	withKey      bool
	wantRefuse   bool
}

// armorBlockBudgetCases pins the block charge as a boundary at both arms that
// report it: an export behind empty blocks at the ceiling and one past it, and
// empty blocks alone, which walkPacketFraming never sees.
func armorBlockBudgetCases() []armorBlockBudgetCase {
	return []armorBlockBudgetCase{
		{name: "a key export at the ceiling", empties: emptyBlocksAtCeiling, withKey: true, wantEntities: 1},
		{name: "one empty block more", empties: emptyBlocksAtCeiling + 1, withKey: true, wantRefuse: true,
			wantMessage: "malformed OpenPGP packet framing: more than 4096 packets across the whole input"},
		{name: "empty blocks alone at the ceiling", empties: keyringMaxPackets},
		{name: "one empty block past it", empties: keyringMaxPackets + 1, wantRefuse: true,
			wantMessage: "malformed OpenPGP packet framing: more than 4096 packets across the whole input, " +
				"counting each armor block as one"},
	}
}

// TestArmorBlocksAreChargedAgainstThePacketBudget pins that each armor block
// costs its file one packet of budget, so the block count is bounded too;
// refusals are matched by their whole hand-spelled message.
func TestArmorBlocksAreChargedAgainstThePacketBudget(t *testing.T) {
	t.Parallel()

	empty := armorEncode(t, openpgp.PublicKeyType, nil)
	export := armorEncode(t, openpgp.PublicKeyType, readFixture(t, binaryFixture))

	for _, tc := range armorBlockBudgetCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			blocks := tc.empties
			file := make([]byte, 0, blocks*(len(empty)+1)+len(export)+1)
			for range blocks {
				file = append(file, empty...)
				file = append(file, '\n')
			}
			if tc.withKey {
				file = append(file, export...)
				file = append(file, '\n')
				blocks++
			}
			// The fixture ahead of its verdict: every block has to present its own
			// opening line, or the file is measuring the cut rather than the charge.
			if got := countArmorBlocks(file); got != blocks {
				t.Fatalf("the fixture presents %d armor blocks, want %d", got, blocks)
			}

			entities, err := readEntities(file)
			if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
				t.Fatalf("readEntities(%s) = %d entities, %v, want refused = %t", tc.name, len(entities), err, tc.wantRefuse)
			}
			if tc.wantRefuse {
				if err.Error() != tc.wantMessage {
					t.Fatalf("readEntities(%s) = %v, want %q", tc.name, err, tc.wantMessage)
				}

				return
			}
			if len(entities) != tc.wantEntities {
				t.Fatalf("readEntities(%s) = %d entities, want %d", tc.name, len(entities), tc.wantEntities)
			}
		})
	}
}

// TestEmptyArmorBlocksAreRefusedCheaply pins that the block charge is taken
// before decoding, so 20000 empty blocks are refused cheaply at the ceiling.
// Not parallel: runtime.MemStats is process-wide.
func TestEmptyArmorBlocksAreRefusedCheaply(t *testing.T) {
	empty := armorEncode(t, openpgp.PublicKeyType, nil)
	export := armorEncode(t, openpgp.PublicKeyType, readFixture(t, binaryFixture))

	file := make([]byte, 0, emptyBlockAbuseCount*(len(empty)+1)+len(export)+1)
	for range emptyBlockAbuseCount {
		file = append(file, empty...)
		file = append(file, '\n')
	}
	file = append(file, export...)
	file = append(file, '\n')

	dir := t.TempDir()
	controlPath := filepath.Join(dir, "control.asc")
	// #nosec G703 -- dir is this test's own t.TempDir and the leaf is a
	// constant; no part of either path comes from outside this function.
	if err := os.WriteFile(controlPath, export, keyringFileMode); err != nil {
		t.Fatalf("write control keyring: %v", err)
	}
	if _, err := LoadKeyring(controlPath); err != nil {
		t.Fatalf("positive control: LoadKeyring(the export alone) = %v, want nil", err)
	}

	path := filepath.Join(dir, "empties.asc")
	// #nosec G703 -- same t.TempDir and a constant leaf, as just above.
	if err := os.WriteFile(path, file, keyringFileMode); err != nil {
		t.Fatalf("write the empty-block keyring: %v", err)
	}

	var loadErr error
	allocated := measureAlloc(func() {
		_, loadErr = LoadKeyring(path)
	})
	if allocated > emptyBlockAllocCeiling {
		t.Fatalf("LoadKeyring(%d empty blocks) allocated %d bytes, want at most %d",
			emptyBlockAbuseCount, allocated, emptyBlockAllocCeiling)
	}
	// Which refusal produced that cheap outcome, asked below the measurement for
	// the reason every measurement here gives.
	if !errors.Is(loadErr, errMalformedSignaturePacket) {
		t.Fatalf("LoadKeyring(%d empty blocks) = %v, want the malformed-packet refusal", emptyBlockAbuseCount, loadErr)
	}
	t.Logf("%d empty blocks in %d bytes were refused in %d bytes of allocation", emptyBlockAbuseCount, len(file), allocated)
}
