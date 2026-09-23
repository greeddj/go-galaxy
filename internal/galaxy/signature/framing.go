package signature

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

const (
	// packetHeaderMSB is the bit every OpenPGP packet header octet sets. A
	// byte without it is not a packet header at all.
	packetHeaderMSB = 0x80
	// packetNewFormatBit distinguishes the new header format from the old one.
	packetNewFormatBit = 0x40
	// packetTagMask covers the six low bits a new-format header spends on the
	// tag, and the six the old format spends on the tag plus its length type.
	packetTagMask = 0x3f
	// oldFormatTagShift is how far an old-format tag sits above the two length
	// type bits below it.
	oldFormatTagShift = 2
	// oldFormatLengthTypeMask covers those two bits.
	oldFormatLengthTypeMask = 0x03

	// Three of the four old-format length types, each a body length spelled in
	// that many octets. The fourth, indeterminate, is the switch's default: it
	// names no length at all.
	oldFormatOneOctet  = 0
	oldFormatTwoOctet  = 1
	oldFormatFourOctet = 2

	// tagOctet is the single header octet carrying the format bit and the tag.
	// Every header length below is that octet plus its length field, so a
	// header length is written as the sum rather than as a number.
	tagOctet = 1
	// The length field widths, in octets. lenFieldFive is the new format's
	// five-octet form: a marker octet followed by four length octets.
	lenFieldOne  = 1
	lenFieldTwo  = 2
	lenFieldFour = 4
	lenFieldFive = 5

	// octetRange is how many values one octet holds, which is the multiplier
	// the new format's two-octet length is built with.
	octetRange = 256

	// newFormatOneOctetMax, newFormatPartialMin and newFormatFiveOctetMarker
	// split a new-format length's first octet: one octet, two octets, a partial
	// (chunked) length declaring no total, and the four-octet form.
	newFormatOneOctetMax     = 192
	newFormatPartialMin      = 224
	newFormatFiveOctetMarker = 255

	// packetTagSignature is the tag of a signature packet, in both header
	// formats.
	packetTagSignature = 2
	// The other tags the sets below name: the ones keyringPacketTags admits,
	// whose own doc comment holds why each of them belongs in a keyring, and the
	// two secretKeyPacketTags refuses.
	packetTagPrivateKey    = 5
	packetTagPublicKey     = 6
	packetTagPrivateSubkey = 7
	packetTagTrust         = 12
	packetTagUserID        = 13
	packetTagPublicSubkey  = 14
	packetTagUserAttribute = 17
	packetTagPadding       = 21

	// The three signature versions whose bodies carry subpacket area lengths:
	// v6 spells both in four octets, v4 and v5 in two. checkSignatureBodyFraming
	// leaves every other version alone.
	signatureVersionV4 = 4
	signatureVersionV5 = 5
	signatureVersionV6 = 6
	// signatureHashedLenOffset is where a signature body carries its hashed
	// subpacket length, after the version, type and two algorithm octets. It is
	// the same in every version; only the field's width changes.
	signatureHashedLenOffset = 4
	// subpacketLenSizeV6 and subpacketLenSizeV4 are those two widths, hashed
	// and unhashed alike. The second name is the v4 spelling of a width every
	// version but v6 uses.
	subpacketLenSizeV6 = 4
	subpacketLenSizeV4 = 2

	// subpacketOneOctetMax and subpacketFiveOctetMarker split a subpacket
	// length's first octet. They are not the packet-length boundaries: with no
	// partial form, every value from 192 to 254 is a two-octet length.
	subpacketOneOctetMax     = 192
	subpacketFiveOctetMarker = 255
	// subpacketLenFieldTwo and subpacketLenFieldFive are the two wider length
	// field widths, in octets, marker octet included.
	subpacketLenFieldTwo  = 2
	subpacketLenFieldFive = 5
	// subpacketTypeOctet is the one octet of a subpacket's contents that names
	// its type; the rest is the subpacket's own body.
	subpacketTypeOctet = 1
	// subpacketTypeMask strips the critical bit (RFC 4880 5.2.3.1, RFC 9580
	// 5.2.3.7) from that octet, so an embedded signature is recognized whichever
	// way its producer set the bit.
	subpacketTypeMask = 0x7f
	// embeddedSignatureSubpacketType is the subpacket type whose body is a
	// whole signature packet body of its own, which is what makes the walk
	// below recursive.
	embeddedSignatureSubpacketType = 32

	// maxEmbeddedSignatureDepth caps the walk's recursion, which is otherwise
	// input-chosen: at about ten bytes a level, keyringMaxSize admits millions of
	// levels. The one defined use, a subkey's cross-certification, is one deep.
	maxEmbeddedSignatureDepth = 4

	// framingIndeterminate and framingPartial name the two header shapes that
	// introduce a body this walk cannot bound, spelled as the refusal prints
	// them.
	framingIndeterminate = "an old-format indeterminate length"
	framingPartial       = "a new-format partial length"
)

// packetTagSet is a set of OpenPGP packet tags, one bit per tag.
type packetTagSet uint64

// has reports whether tag is in the set. Both header readers mask a tag to six
// bits, so it is always in [0,63]; a wider shift would read as absent anyway.
func (s packetTagSet) has(tag int) bool {
	return s&(packetTagSet(1)<<tag) != 0
}

// signatureBlobPacketTags is what a detached signature blob may hold: signature
// packets and nothing else. It is the set the blob path passes, which is the
// path whose input is not the operator's own.
const signatureBlobPacketTags = packetTagSet(1) << packetTagSignature

// keyringPacketTags allow-lists a keyring file: a transferable public key's
// packets plus GnuPG ring-trust (12) and RFC 9580 padding (21). The tag is
// input-chosen, so no deny-list; the marker packet (10) is in no set.
const keyringPacketTags = packetTagSet(1)<<packetTagSignature |
	packetTagSet(1)<<packetTagPublicKey |
	packetTagSet(1)<<packetTagTrust |
	packetTagSet(1)<<packetTagUserID |
	packetTagSet(1)<<packetTagPublicSubkey |
	packetTagSet(1)<<packetTagUserAttribute |
	packetTagSet(1)<<packetTagPadding

// secretKeyPacketTags (secret key, secret subkey) are refused at the header,
// never parsed: a GNU-dummy S2K parses short and a huge RSA modulus costs
// seconds of validation. Refusing them first lets loadKeyring name the fix.
const secretKeyPacketTags = packetTagSet(1)<<packetTagPrivateKey |
	packetTagSet(1)<<packetTagPrivateSubkey

const (
	// signatureBlobMaxPackets caps the packets (one per signer) in one detached
	// signature blob. It is unrelated to helpers.MaxSignaturesPerCollection, a
	// count of blobs, despite the equal value; a blob past it is refused whole.
	signatureBlobMaxPackets = 64

	// keyringMaxPackets caps a keyring file's packets across all its armor
	// blocks, each block also costing armorBlockPacketCost. It is sized for a
	// collection-publisher keyring; a file past it is refused whole.
	keyringMaxPackets = 4096

	// armorBlockPacketCost is what one armor block adds to its file's packet
	// budget, so the block count is bounded even for blocks decoding to no
	// packets: each still costs a decode and an openpgp.ReadKeyRing call.
	armorBlockPacketCost = 1
)

// packetProfile is what one kind of input may hold over its whole file: which
// tags (so which go-crypto parsers run) and how many packets (so how often). A
// new kind of input gets its own profile rather than widening one of these.
type packetProfile struct {
	tags       packetTagSet
	maxPackets int
}

// keyringProfile is the vocabulary of a keyring file, which is the operator's
// own: the packets a transferable key is made of, and a ceiling sized for a
// collection-publisher keyring.
func keyringProfile() packetProfile {
	return packetProfile{tags: keyringPacketTags, maxPackets: keyringMaxPackets}
}

// signatureBlobProfile is the vocabulary of a detached signature blob, which is
// the input this package does not trust: signature packets and nothing else, and
// a ceiling sized for the signers one blob can plausibly name.
func signatureBlobProfile() packetProfile {
	return packetProfile{tags: signatureBlobPacketTags, maxPackets: signatureBlobMaxPackets}
}

// errMalformedSignaturePacket is every framing refusal of this walk, each of
// which refuses some file go-crypto would read. It is absent from classify on
// purpose, so it lands on ERRSIG and no BADARMOR ignore entry covers it.
var errMalformedSignaturePacket = errors.New("malformed OpenPGP packet framing")

// errSecretKeyPacket is wrapped beside errMalformedSignaturePacket by the
// secret-key arm, so loadKeyring renders one message for secret material
// whether this walk or holdsSecretKey found it.
var errSecretKeyPacket = errors.New("secret key material")

// errOversizedArmorHeader refuses an armor block whose header section no blank
// line ends within armorHeaderMaxSize bytes. It is ERRSIG, not BADARMOR, so an
// ignore entry meant for decoder failures does not cover this gate's refusal.
var errOversizedArmorHeader = errors.New("oversized OpenPGP armor header section")

// checkPacketFraming judges one packet stream before go-crypto parses it, since
// the parser sizes buffers from lengths the stream declares; every blob and
// keyring path must pass it. One refused packet refuses the whole file.
func checkPacketFraming(data []byte, profile packetProfile) error {
	_, err := walkPacketFraming(data, profile, 0)

	return err
}

// walkPacketFraming is checkPacketFraming with the packet budget carried across
// a file's armor blocks: spent is what the file has used so far, and the count
// returned is this stream's packets alone, without the caller's per-block charge.
func walkPacketFraming(data []byte, profile packetProfile, spent int) (int, error) {
	// One reader Reset per frame, so the drive costs no allocation per packet.
	var rd bytes.Reader

	// Counted here rather than derived afterwards: the ceiling has to refuse the
	// packet that would cross it before that packet is judged or driven, or it
	// would report a cost instead of bounding one.
	packets := 0

	for len(data) > 0 {
		frame, status := readPacketFrame(data)
		if err := profile.judgeHeader(frame, status, len(data)); err != nil {
			return packets, err
		}
		packets++
		if spent+packets > profile.maxPackets {
			// "The whole input", not one stream: the budget spans a keyring's
			// armor blocks, and the ceiling is the profile's own number.
			return packets, fmt.Errorf("%w: more than %d packets across the whole input",
				errMalformedSignaturePacket, profile.maxPackets)
		}
		if err := judgePacket(&rd, data, frame); err != nil {
			return packets, err
		}

		data = data[int64(frame.headerLen)+frame.bodyLen:]
	}

	return packets, nil
}

// armorBlockBudgetError refuses an armored keyring whose blocks alone exhaust
// the packet budget, which walkPacketFraming cannot see: a block decoding to no
// packets never enters its loop.
func armorBlockBudgetError(maxPackets int) error {
	return fmt.Errorf("%w: more than %d packets across the whole input, counting each armor block as one",
		errMalformedSignaturePacket, maxPackets)
}

// judgeHeader refuses unbounded and unreadable framings before the tag, which
// decides nothing for a body with no findable end, then secret keys ahead of the
// allow-list so loadKeyring can name the fix. Messages carry no input bytes.
func (p packetProfile) judgeHeader(frame packetFrame, status frameStatus, remaining int) error {
	switch {
	case status == frameUnbounded:
		return fmt.Errorf("%w: a tag %d packet carries %s, which declares no body length at all",
			errMalformedSignaturePacket, frame.tag, frame.framing)
	case status == frameUnreadable:
		// An empty input never enters the walk, and a walk that consumed the
		// last packet exactly leaves it by its own loop condition, so neither
		// reaches this arm.
		return fmt.Errorf("%w: %d bytes are not a packet header this walk can read",
			errMalformedSignaturePacket, remaining)
	case secretKeyPacketTags.has(frame.tag):
		return fmt.Errorf("%w: a tag %d packet carries %w",
			errMalformedSignaturePacket, frame.tag, errSecretKeyPacket)
	case !p.tags.has(frame.tag):
		return fmt.Errorf("%w: a tag %d packet has no place in this stream",
			errMalformedSignaturePacket, frame.tag)
	default:
		return nil
	}
}

// judgePacket checks a bounded packet's body is present, then a signature's
// subpacket lengths, then drives the parser. That order is load-bearing: the
// subpacket walk bounds Signature.parse's allocations and heads off its panic.
func judgePacket(rd *bytes.Reader, data []byte, frame packetFrame) error {
	body := data[frame.headerLen:]
	if frame.bodyLen > int64(len(body)) {
		return fmt.Errorf("%w: a packet declares a %d byte body with %d present",
			errMalformedSignaturePacket, frame.bodyLen, len(body))
	}
	if frame.tag == packetTagSignature {
		if err := checkSignatureBodyFraming(body[:frame.bodyLen], 0); err != nil {
			return err
		}
	}

	return driveParser(rd, data[:int64(frame.headerLen)+frame.bodyLen], frame.tag)
}

// driveParser runs packet.Read over one packet and refuses any unread residual:
// a short successful parse would cut the next packet at an input-chosen offset.
// The parse error is ignored on purpose; the residual decides, for every tag.
func driveParser(rd *bytes.Reader, segment []byte, tag int) error {
	rd.Reset(segment)
	_, _ = packet.Read(rd)
	if unread := rd.Len(); unread != 0 {
		// Numbers computed from the input's shape, never its bytes, so nothing
		// needs sanitizing; they still disclose the refused packet's size.
		return fmt.Errorf("%w: the parser left %d of a tag %d packet's %d bytes unread",
			errMalformedSignaturePacket, unread, tag, len(segment))
	}

	return nil
}

// frameStatus is what readPacketFrame made of the octets at the front of the
// walk's remaining bytes.
type frameStatus int

const (
	// frameUnreadable means the octets are not a header this walk can read, so
	// there is nothing here to judge and nothing behind it to reach - which is
	// why the walk refuses the stream rather than ending on it.
	frameUnreadable frameStatus = iota
	// frameBounded means the header declares how long its body is, which is the
	// only state whose bodyLen and headerLen mean anything.
	frameBounded
	// frameUnbounded means the header is one go-crypto reads and parses a body
	// behind while declaring no total for it, so this walk can find neither the
	// end of that body nor whatever follows it. Only tag and framing are set.
	frameUnbounded
)

// packetFrame is one packet header as the walk read it. framing is set only
// with frameUnbounded, naming the shape that left the body unbounded; the other
// two states leave it empty.
type packetFrame struct {
	framing   string
	bodyLen   int64
	headerLen int
	tag       int
}

// readPacketFrame reads the packet header at the front of data, reporting which
// of the three frame states it landed in.
func readPacketFrame(data []byte) (packetFrame, frameStatus) {
	first := data[0]
	if first&packetHeaderMSB == 0 {
		return packetFrame{}, frameUnreadable
	}
	if first&packetNewFormatBit == 0 {
		return oldFormatFrame(data)
	}

	return newFormatFrame(data)
}

// oldFormatFrame reads an old-format header, whose two lowest bits name how
// many octets spell the body length.
func oldFormatFrame(data []byte) (packetFrame, frameStatus) {
	tag := int((data[0] & packetTagMask) >> oldFormatTagShift)

	switch data[0] & oldFormatLengthTypeMask {
	case oldFormatOneOctet:
		if len(data) < tagOctet+lenFieldOne {
			return packetFrame{}, frameUnreadable
		}

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldOne, bodyLen: int64(data[tagOctet])}, frameBounded
	case oldFormatTwoOctet:
		if len(data) < tagOctet+lenFieldTwo {
			return packetFrame{}, frameUnreadable
		}
		bodyLen := int64(binary.BigEndian.Uint16(data[tagOctet : tagOctet+lenFieldTwo]))

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldTwo, bodyLen: bodyLen}, frameBounded
	case oldFormatFourOctet:
		if len(data) < tagOctet+lenFieldFour {
			return packetFrame{}, frameUnreadable
		}
		bodyLen := int64(binary.BigEndian.Uint32(data[tagOctet : tagOctet+lenFieldFour]))

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldFour, bodyLen: bodyLen}, frameBounded
	default:
		// The indeterminate length type: the body runs to the end of the input,
		// so the header declares no total this walk could compare anything
		// against - and go-crypto hands that same body to the parser anyway.
		return packetFrame{tag: tag, framing: framingIndeterminate}, frameUnbounded
	}
}

// newFormatFrame reads a new-format header, whose length is spelled in one, two
// or five octets - or is partial, and declares no total at all.
func newFormatFrame(data []byte) (packetFrame, frameStatus) {
	if len(data) < tagOctet+lenFieldOne {
		return packetFrame{}, frameUnreadable
	}
	tag := int(data[0] & packetTagMask)
	lead := data[tagOctet]

	switch {
	case lead < newFormatOneOctetMax:
		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldOne, bodyLen: int64(lead)}, frameBounded
	case lead < newFormatPartialMin:
		if len(data) < tagOctet+lenFieldTwo {
			return packetFrame{}, frameUnreadable
		}
		// RFC 9580's two-octet form, which encodes lengths from 192 upwards.
		bodyLen := int64(lead-newFormatOneOctetMax)*octetRange + int64(data[tagOctet+lenFieldOne]) + newFormatOneOctetMax

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldTwo, bodyLen: bodyLen}, frameBounded
	case lead < newFormatFiveOctetMarker:
		// A partial length sizes one chunk, not the body, so no octet names
		// the total - yet go-crypto parses the chunked body all the same.
		return packetFrame{tag: tag, framing: framingPartial}, frameUnbounded
	default:
		if len(data) < tagOctet+lenFieldFive {
			return packetFrame{}, frameUnreadable
		}
		bodyLen := int64(binary.BigEndian.Uint32(data[tagOctet+lenFieldOne : tagOctet+lenFieldFive]))

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldFive, bodyLen: bodyLen}, frameBounded
	}
}

// checkSignatureBodyFraming refuses a v4, v5 or v6 signature body whose
// subpacket area lengths exceed its bytes, at every embedding depth. Other
// versions are admitted: a v3 body carries its creation time at that offset.
func checkSignatureBodyFraming(body []byte, depth int) error {
	if depth > maxEmbeddedSignatureDepth {
		// The numbers are this walk's own, so the message carries no part of
		// the input.
		return fmt.Errorf("%w: a signature nests embedded signatures more than %d deep",
			errMalformedSignaturePacket, maxEmbeddedSignatureDepth)
	}
	if len(body) == 0 {
		return nil
	}
	lenSize, judged := subpacketLenSize(body[0])
	if !judged {
		return nil
	}
	prefixLen := signatureHashedLenOffset + lenSize
	if len(body) < prefixLen {
		return nil
	}

	rest := body[prefixLen:]
	hashed := subpacketAreaLen(body[signatureHashedLenOffset:], lenSize)
	if hashed > int64(len(rest)) {
		return subpacketOverrun("hashed", hashed, len(rest))
	}
	if err := walkSignatureSubpackets(rest[:hashed], depth); err != nil {
		return err
	}

	// The unhashed length sits immediately behind the hashed subpackets. A body
	// too short to carry it is left to go-crypto, and what that costs is one of
	// the shapes TestAdmittedShapesAllocateNothing measures.
	rest = rest[hashed:]
	if len(rest) < lenSize {
		return nil
	}
	unhashed := subpacketAreaLen(rest, lenSize)
	rest = rest[lenSize:]
	if unhashed > int64(len(rest)) {
		return subpacketOverrun("unhashed", unhashed, len(rest))
	}

	return walkSignatureSubpackets(rest[:unhashed], depth)
}

// subpacketLenSize reports how wide a version's subpacket area lengths are, and
// false for a version whose octets at that offset are some other field.
func subpacketLenSize(version byte) (int, bool) {
	switch version {
	case signatureVersionV6:
		return subpacketLenSizeV6, true
	case signatureVersionV4, signatureVersionV5:
		return subpacketLenSizeV4, true
	default:
		return 0, false
	}
}

// subpacketAreaLen reads a hashed or unhashed subpacket area length of lenSize
// octets from the front of b, which the caller has already checked holds at
// least that many.
func subpacketAreaLen(b []byte, lenSize int) int64 {
	if lenSize == subpacketLenSizeV6 {
		return int64(binary.BigEndian.Uint32(b[:subpacketLenSizeV6]))
	}

	return int64(binary.BigEndian.Uint16(b[:subpacketLenSizeV4]))
}

// walkSignatureSubpackets walks one area and recurses into embedded signatures.
// Unwalkable shapes are admitted; a type octet with no body is refused, since
// go-crypto panics on an empty exportable-certification subpacket.
func walkSignatureSubpackets(subs []byte, depth int) error {
	for len(subs) > 0 {
		length, rest, ok := readSubpacketLength(subs)
		if !ok || length > int64(len(rest)) {
			return nil
		}
		contents := rest[:length]
		subs = rest[length:]
		if len(contents) < subpacketTypeOctet {
			// Admitted, and this guard is what keeps contents[0] below from panicking.
			return nil
		}
		if len(contents) == subpacketTypeOctet {
			// The numbers are this walk's own, so the message carries no part of
			// the input.
			return fmt.Errorf("%w: a signature subpacket carries a type octet and no body",
				errMalformedSignaturePacket)
		}
		if contents[0]&subpacketTypeMask != embeddedSignatureSubpacketType {
			continue
		}
		if err := checkSignatureBodyFraming(contents[subpacketTypeOctet:], depth+1); err != nil {
			return err
		}
	}

	return nil
}

// readSubpacketLength reads the length field at the front of non-empty subs and
// returns it with the bytes behind it; ok is false for a field not carried whole.
func readSubpacketLength(subs []byte) (int64, []byte, bool) {
	switch {
	case subs[0] < subpacketOneOctetMax:
		return int64(subs[0]), subs[1:], true
	case subs[0] < subpacketFiveOctetMarker:
		if len(subs) < subpacketLenFieldTwo {
			return 0, nil, false
		}
		length := int64(subs[0]-subpacketOneOctetMax)*octetRange + int64(subs[1]) + subpacketOneOctetMax

		return length, subs[subpacketLenFieldTwo:], true
	default:
		if len(subs) < subpacketLenFieldFive {
			return 0, nil, false
		}

		return int64(binary.BigEndian.Uint32(subs[1:subpacketLenFieldFive])), subs[subpacketLenFieldFive:], true
	}
}

// subpacketOverrun refuses an over-declared subpacket area, naming both numbers
// so a truncated blob can be told from a fabricated length.
func subpacketOverrun(which string, declared int64, available int) error {
	return fmt.Errorf("%w: a signature declares %d bytes of %s subpackets with %d present",
		errMalformedSignaturePacket, declared, which, available)
}

// decodeArmorBlock decodes the first armor block in data, returning its bytes
// and declared type. go-crypto's armored entry points parse straight after the
// decode, so decoding here is what lets checkPacketFraming run in between.
func decodeArmorBlock(data []byte) ([]byte, string, error) {
	if err := checkArmorHeaderSection(data); err != nil {
		return nil, "", err
	}

	block, err := armor.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}

	// Armor is base64, so the decoded body is at most three quarters of the
	// armored text; the extra bytes.MinRead is the headroom ReadFrom wants
	// available before it stops, so the buffer is sized once and never grown.
	buf := bytes.NewBuffer(make([]byte, 0, len(data)*3/4+bytes.MinRead))
	if _, err := buf.ReadFrom(block.Body); err != nil {
		return nil, "", err
	}

	return buf.Bytes(), block.Type, nil
}

// checkArmorHeaderSection bounds the header section behind every armor opening
// line, not only the first, since armor.Decode abandons a block at a colon-free
// header line and resumes. It fails closed on lines the decoder would skip.
func checkArmorHeaderSection(data []byte) error {
	for off := 0; off < len(data); {
		line, next := nextLine(data, off)
		off = next
		if !isArmorBlockStart(line) {
			continue
		}

		end, ok := armorHeaderSectionEnd(data, off)
		if !ok {
			// The message carries no part of the input: the numbers are this
			// package's own, so the refusal needs no sanitizing on its way out.
			return fmt.Errorf("%w: no blank line ends one within %d bytes",
				errOversizedArmorHeader, armorHeaderMaxSize)
		}
		off = end
	}

	return nil
}

// armorHeaderSectionEnd returns the offset past the blank line ending the
// section at off, or false past armorHeaderMaxSize. Lines are judged whole; input
// ending inside the bound is accepted and left to armor.Decode.
func armorHeaderSectionEnd(data []byte, off int) (int, bool) {
	start := off
	for off < len(data) {
		line, next := nextLine(data, off)
		if next-start > armorHeaderMaxSize {
			return 0, false
		}
		if len(bytes.TrimSpace(line)) == 0 {
			return next, true
		}
		off = next
	}

	return len(data), true
}

// armorDecodeFoundNothing reports whether err is armor.Decode's io.EOF for input
// with no armor block; bytes.Buffer.ReadFrom never returns io.EOF.
func armorDecodeFoundNothing(err error) bool {
	return errors.Is(err, io.EOF)
}
