package signature

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// keyringBlobEquivalents is how many signature blobs' worth of bytes a
	// keyring may occupy, room for many keys while bounding a hostile file. It
	// only coincides with helpers.MaxSignaturesPerCollection, which counts blobs.
	keyringBlobEquivalents = 64

	// keyringMaxSize caps the bytes LoadKeyring reads. Crossing it is refused,
	// never truncated, since a short read still parses and silently drops keys;
	// the refusal is ErrKeyringUnreadable, a config error, not a transport one.
	keyringMaxSize = helpers.SignatureMaxSize * keyringBlobEquivalents

	// keyboxMagicOffset is where a GnuPG keybox carries keyboxMagic: a keybox
	// opens with a header blob whose first eight bytes are a 4-byte blob
	// length, a 1-byte blob type, a 1-byte version and 2 bytes of flags.
	keyboxMagicOffset = 8

	// keyboxMagic is the four-byte magic identifying a GnuPG keybox container.
	keyboxMagic = "KBXf"

	// publicKeyArmorHeader opens an ASCII-armored OpenPGP public key block,
	// which is what `gpg --export --armor` writes.
	publicKeyArmorHeader = "-----BEGIN PGP PUBLIC KEY BLOCK-----"

	// privateKeyArmorHeader opens an armored private key block. It routes such a
	// file to the armored reader so it earns the secret-material refusal.
	// #nosec G101 -- this is the public delimiter line that introduces such a
	// block, not key material.
	privateKeyArmorHeader = "-----BEGIN PGP PRIVATE KEY BLOCK-----"

	// armorBlockStart is the prefix every armored OpenPGP block's opening line
	// carries, whatever the block's type.
	armorBlockStart = "-----BEGIN "

	// armorBlockStartMinLen mirrors the length test armor.Decode applies to a
	// candidate opening line: the prefix, the trailing "-----", and at least
	// one byte of type between them.
	armorBlockStartMinLen = len(armorBlockStart) + len("-----") + 1

	// armorHeaderMaxSize bounds one armor block's header section, because
	// armor.Decode's header parsing allocates quadratically in its length. Real
	// armor carries a few header lines, and the library already caps body lines.
	armorHeaderMaxSize = 4096
)

// errArmorBlockStartMidLine refuses a keyring whose armor opening prefix is not
// at a line start (exports glued without a newline): the cut would read one
// block and drop the keys behind it, and a mid-line split could merge blocks.
var errArmorBlockStartMidLine = errors.New("an armored block start does not begin its line")

// Keyring is the OpenPGP key material of one keyring file, together with the
// path it was read from. Nothing in this package mutates one after LoadKeyring
// returns it.
type Keyring struct {
	path     string
	entities openpgp.EntityList
}

// Path returns the file this keyring was read from. It is the value to name in
// an operator-facing message about the keyring, so a run that verified against
// the wrong file still says which file that was.
func (k *Keyring) Path() string {
	return k.path
}

// Len returns how many OpenPGP entities the keyring holds. It is never zero
// for a *Keyring LoadKeyring returned, which is what lets a caller treat a
// loaded keyring as usable without re-checking that it holds anything.
func (k *Keyring) Len() int {
	return len(k.entities)
}

// LoadKeyring reads path as an OpenPGP keyring, judging the format from the
// bytes, never the name. Every armored block is read, so concatenated exports
// work; a keybox, secret material or an empty or oversized file is refused.
func LoadKeyring(path string) (*Keyring, error) {
	return loadKeyring(path, keyringMaxSize)
}

// loadKeyring is LoadKeyring with the size ceiling as a parameter, so that
// crossing it can be exercised without a fixture the size of the real one.
func loadKeyring(path string, limit int64) (*Keyring, error) {
	// #nosec G304 -- path is the keyring location the operator configured;
	// reading the file they named is the entire operation.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", helpers.ErrKeyringUnreadable, path, err)
	}
	defer func() { _ = f.Close() }()

	// Reading limit+1 is what makes the ceiling exact rather than off by one:
	// stopping at limit leaves a file of exactly limit bytes indistinguishable
	// from a longer one truncated there, so it would have to be refused too.
	data, err := io.ReadAll(&io.LimitedReader{R: f, N: limit + 1})
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", helpers.ErrKeyringUnreadable, path, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %q is larger than the %d bytes this tool reads", helpers.ErrKeyringUnreadable, path, limit)
	}

	if looksLikeKeybox(data) {
		return nil, fmt.Errorf("%w: %q", helpers.ErrKeyringIsKeybox, path)
	}

	entities, err := readEntities(data)
	if err != nil {
		if errors.Is(err, errSecretKeyPacket) {
			return nil, secretKeyMaterialError(path)
		}

		return nil, fmt.Errorf("%w: %q: %w", helpers.ErrKeyringUnreadable, path, err)
	}
	if len(entities) == 0 {
		return nil, fmt.Errorf("%w: %q holds no OpenPGP keys", helpers.ErrKeyringUnreadable, path)
	}
	if holdsSecretKey(entities) {
		// Unreachable from a file, since the packet walk refuses the secret-key
		// tags first; kept as a backstop for a reader that bypasses the walk.
		return nil, secretKeyMaterialError(path)
	}

	return &Keyring{path: path, entities: entities}, nil
}

// secretKeyMaterialError is the one refusal for a keyring holding secret
// material, whether the packet walk or holdsSecretKey found it, so the text and
// exit class stay one policy and errSecretKeyPacket stays errors.Is-reachable.
func secretKeyMaterialError(path string) error {
	return fmt.Errorf(
		"%w: %q holds %w; verifying needs only the public half, "+
			"so export that instead: gpg --export --armor > keyring.asc",
		helpers.ErrKeyringUnreadable, path, errSecretKeyPacket)
}

// readEntities reads data as key material. It routes on bytes.Contains of a key
// armor header, so armor behind a leading comment still reads as armor, and
// gates packet framing before any parse on both the armored and binary paths.
func readEntities(data []byte) (openpgp.EntityList, error) {
	if bytes.Contains(data, []byte(publicKeyArmorHeader)) || bytes.Contains(data, []byte(privateKeyArmorHeader)) {
		return readArmoredKeyRing(data)
	}
	if err := checkPacketFraming(data, keyringProfile()); err != nil {
		return nil, err
	}

	return openpgp.ReadKeyRing(bytes.NewReader(data))
}

// readArmoredKeyRing reads every armor block in data, where ReadArmoredKeyRing
// reads only the first. It cuts the bytes at line-start opening lines, since
// armor.Decode may read past a block's end, and budgets packets per file.
func readArmoredKeyRing(data []byte) (openpgp.EntityList, error) {
	if err := checkArmorBlockStartsAtLineStart(data); err != nil {
		return nil, err
	}

	var entities openpgp.EntityList

	// blockStart is where the current block begins, or -1 before the first; spent
	// is the file's packet budget used so far, armorBlockPacketCost included.
	blockStart := -1
	spent := 0
	readBlock := func(end int) error {
		if blockStart < 0 {
			return nil
		}
		block, packets, err := readKeyArmorBlock(data[blockStart:end], spent)
		if err != nil {
			return err
		}
		spent += packets
		entities = append(entities, block...)

		return nil
	}

	for off := 0; off < len(data); {
		line, next := nextLine(data, off)
		if isArmorBlockStart(line) {
			// The previous block ends where this one starts. Whatever sits
			// between its end line and here the decoder never reads, since it
			// stops at that end line.
			if err := readBlock(off); err != nil {
				return nil, err
			}
			blockStart = off
		}
		off = next
	}

	if err := readBlock(len(data)); err != nil {
		return nil, err
	}

	return entities, nil
}

// readKeyArmorBlock decodes one armored block, gates its packet framing and
// reads its keys, returning what it cost the file's packet budget. The block's
// own cost is charged first, so a block over budget is never decoded.
func readKeyArmorBlock(data []byte, spent int) (openpgp.EntityList, int, error) {
	profile := keyringProfile()
	spent += armorBlockPacketCost
	if spent > profile.maxPackets {
		return nil, 0, armorBlockBudgetError(profile.maxPackets)
	}

	decoded, blockType, err := decodeArmorBlock(data)
	if armorDecodeFoundNothing(err) {
		return nil, 0, pgperrors.InvalidArgumentError("no armored data found")
	}
	if err != nil {
		return nil, 0, err
	}
	if blockType != openpgp.PublicKeyType && blockType != openpgp.PrivateKeyType {
		return nil, 0, pgperrors.InvalidArgumentError("expected public or private key block, got: " + blockType)
	}
	packets, err := walkPacketFraming(decoded, profile, spent)
	if err != nil {
		return nil, 0, err
	}

	entities, err := openpgp.ReadKeyRing(bytes.NewReader(decoded))

	return entities, packets + armorBlockPacketCost, err
}

// nextLine returns the line of data beginning at off, without its terminator,
// together with the offset the following line begins at. A final line carrying
// no terminator is returned whole, and its follower is the end of data.
func nextLine(data []byte, off int) ([]byte, int) {
	if i := bytes.IndexByte(data[off:], '\n'); i >= 0 {
		return data[off : off+i], off + i + 1
	}

	return data[off:], len(data)
}

// checkArmorBlockStartsAtLineStart refuses data carrying armorBlockStart after
// anything but whitespace on its line, judging every occurrence on each line,
// since that is the one shape the line-start cut cannot see.
func checkArmorBlockStartsAtLineStart(data []byte) error {
	needle := []byte(armorBlockStart)

	for off := 0; off < len(data); {
		line, next := nextLine(data, off)
		for at := 0; ; {
			i := bytes.Index(line[at:], needle)
			if i < 0 {
				break
			}
			i += at
			if len(bytes.TrimSpace(line[:i])) != 0 {
				// The offset is this walk's own arithmetic and the prefix is a
				// package constant, so the message carries no part of the input.
				return fmt.Errorf("%w: %q at byte %d; separate concatenated key exports with a newline",
					errArmorBlockStartMidLine, armorBlockStart, off+i)
			}
			at = i + len(needle)
		}
		off = next
	}

	return nil
}

// isArmorBlockStart reports whether line opens an armor block. It mirrors
// armor.Decode's own test so every block the decoder sees is a cut point, and
// trimming covers the carriage return of a CRLF file.
func isArmorBlockStart(line []byte) bool {
	line = bytes.TrimSpace(line)

	return len(line) >= armorBlockStartMinLen && bytes.HasPrefix(line, []byte(armorBlockStart))
}

// holdsSecretKey reports whether any entity carries private key material in its
// primary key or a subkey. It is a backstop no file reaches past the packet
// walk, so TestHoldsSecretKeyReportsMaterialInEitherPlace drives it directly.
func holdsSecretKey(entities openpgp.EntityList) bool {
	for _, entity := range entities {
		if entity.PrivateKey != nil {
			return true
		}
		for i := range entity.Subkeys {
			if entity.Subkeys[i].PrivateKey != nil {
				return true
			}
		}
	}

	return false
}

// looksLikeKeybox reports whether b opens with a GnuPG keybox header blob. The
// magic counts only at keyboxMagicOffset: a file merely containing "KBXf" is not
// a keybox, and refusing it as one would name a remedy that does not apply.
func looksLikeKeybox(b []byte) bool {
	end := keyboxMagicOffset + len(keyboxMagic)

	return len(b) >= end && string(b[keyboxMagicOffset:end]) == keyboxMagic
}
