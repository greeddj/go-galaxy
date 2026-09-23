package signature

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// signatureArmorHeader is the opening line of an ASCII-armored detached
// signature. checkOne routes on the whole line, not the bare armor prefix, so
// the needle is exactly as selective as the reader behind it.
const signatureArmorHeader = "-----BEGIN PGP SIGNATURE-----"

// errNoSignatureData classifies a blob carrying no OpenPGP data, blank or an
// empty armor envelope, as NODATA: go-crypto answers an empty packet stream
// with ErrUnknownIssuer, which would read as an ignorable NO_PUBKEY.
var errNoSignatureData = errors.New("signature blob carries no OpenPGP data")

// Blob is one detached signature as it was gathered, together with the source
// it came from.
type Blob struct {
	// Origin names the source for failure messages and is rendered verbatim,
	// so every filler cuts credentials (userinfo, query) and caps its length.
	Origin string
	// Data is the raw signature, armored or binary. Which of the two it
	// carries is decided from these bytes, never from Origin.
	Data []byte
}

// Failure is one signature that did not verify, with a status this run does
// not tolerate. Err leads the fields for fieldalignment, not for emphasis.
type Failure struct {
	// Err is the underlying verification error, kept so a caller can inspect
	// it rather than parse a rendered message.
	Err error
	// Origin is the Blob.Origin of the signature that failed.
	Origin string
	// Status is the gpg status code the failure classifies as, drawn from the
	// eight status.go declares a verdict can carry.
	Status Status
}

// Result is one collection's verification outcome, fully populated on a pass
// or a negative verdict and partly populated when the caller's pull failed.
type Result struct {
	// Failures holds every signature that was checked, failed, and was not
	// ignored, in the order the pull yielded them.
	Failures []Failure
	// Verified counts DISTINCT verifying keys, not good blobs: a counted
	// policy stops at its count and one key signing twice counts once. The
	// manifest chain check runs only when it is non-zero.
	Verified int
	// VacuousPass reports a pass with nothing verified, which ansible's
	// non-strict verdict allows; the caller must warn about it on stderr, as
	// this package knows no collection to name.
	VacuousPass bool
}

// NextBlob yields one gathered signature at a time: a blob and true, false when
// the caller has no more, or a gather error Verify returns as it came. A pull
// rather than a slice keeps one blob resident instead of up to 64 MiB.
type NextBlob func() (Blob, bool, error)

// Verify checks next's blobs as detached signatures over manifest, following
// ansible-core's verify_file_signatures except that it walks the caller's order
// and counts distinct keys, not blobs. kr is never written, so workers share it.
func Verify(manifest []byte, next NextBlob, kr *Keyring, p Policy) (Result, error) {
	// A nil or empty keyring is a configuration error, never a verdict a
	// signature produced, so it fails closed before anything is checked.
	if kr == nil || kr.Len() == 0 {
		return Result{}, fmt.Errorf("%w: nothing to verify signatures against", helpers.ErrKeyringRequired)
	}

	walk, err := walkBlobs(manifest, next, kr, p)
	verified := len(walk.signers)
	result := Result{Failures: walk.failures, Verified: verified}
	if err != nil {
		return result, err
	}

	passed := p.verdict(verified, len(walk.failures), walk.gathered)
	result.VacuousPass = passed && verified == 0
	if passed {
		return result, nil
	}

	return result, verificationError(p, verified, walk.failures)
}

// blobWalk is what one pass over a caller's pull produced, before any clause of
// the policy has been applied to it.
type blobWalk struct {
	// failures holds every signature that failed with a status this run does
	// not tolerate, in the order the pull yielded them.
	failures []Failure
	// signers holds one entry per distinct key that verified a signature,
	// keyed as signerKey describes.
	signers []string
	// gathered is how many blobs the pull yielded before the walk stopped,
	// which is the term Policy.verdict's vacuous clause reads.
	gathered int
}

// walkBlobs pulls and checks blobs until the pull runs out, the required count
// is reached, or the pull reports an error, which it returns as it came.
func walkBlobs(manifest []byte, next NextBlob, kr *Keyring, p Policy) (blobWalk, error) {
	// Both slices start nil since the usual outcome is one signer and no
	// failure. signers is a slice, not a map, because
	// helpers.MaxSignaturesPerCollection bounds it at 64.
	var walk blobWalk

	for {
		blob, ok, err := next()
		if err != nil {
			return walk, err
		}
		if !ok {
			return walk, nil
		}
		walk.gathered++

		signer, checkErr := checkOne(manifest, blob.Data, kr)
		if checkErr != nil {
			walk.failures = recordFailure(walk.failures, blob.Origin, checkErr, p.Ignore)

			continue
		}

		walk.signers = recordSigner(walk.signers, signer)
		// An "all" policy checks every blob; a counted one stops here, which is
		// what makes a bare N mean "at least N" and leaves any blob after the
		// Nth distinct signer unexamined.
		if !p.Required.All && len(walk.signers) == p.Required.Count {
			return walk, nil
		}
	}
}

// recordSigner adds the key that verified a signature to signers, unless a
// signature from that same key already counted.
func recordSigner(signers []string, signer *openpgp.Entity) []string {
	key := signerKey(signer)
	if slices.Contains(signers, key) {
		return signers
	}

	return append(signers, key)
}

// recordFailure classifies one check failure and records it, unless its status
// is one this run tolerates - in which case it is invisible to both counters,
// which is what makes an ignored failure neither a success nor a failure.
func recordFailure(failures []Failure, origin string, err error, ignore StatusSet) []Failure {
	status := classify(err)
	if ignore.Ignores(status) {
		return failures
	}

	return append(failures, Failure{Origin: origin, Status: status, Err: err})
}

// signerKey identifies the primary key a signature verified against, so a
// subkey counts as its primary and two signatures by one key count once. No
// entity keys on "", so such signatures collapse into one signer.
func signerKey(signer *openpgp.Entity) string {
	if signer == nil || signer.PrimaryKey == nil {
		return ""
	}

	return string(signer.PrimaryKey.Fingerprint)
}

// verdict is ansible's three-clause decision, ordered: strict, then all, then
// the count. gathered is blobs pulled, not counters moved; verdictReason must
// mirror this order or a message names a clause that did not fire.
func (p Policy) verdict(verified, failed, gathered int) bool {
	if p.Required.Strict && verified == 0 {
		return false
	}
	if p.Required.All {
		return failed == 0
	}

	// Equality rather than >= is ansible's: the walk stops at the Count-th
	// signer, so only a Count of 0 with a signature verified can differ.
	return gathered == 0 || verified == p.Required.Count
}

// maxRenderedFailures bounds how many causes a negative verdict's message
// renders, one per distinct status a verdict can carry. It never bounds what
// errors.Is reaches, and the footer names hidden statuses, never origins.
const maxRenderedFailures = 8

// verdictError is a negative verdict: causes[0] is the headline, then every
// non-ignored Failure, none nil (verificationError alone builds it). Only Error
// caps the render, so what errors.Is matches never depends on gather order.
type verdictError struct {
	causes []error
	hidden []Status
}

// Error renders the headline and up to maxRenderedFailures failures, one per
// line, plus a footer counting the rest and naming their distinct statuses, so
// a BADSIG behind a wall of NO_PUBKEY decoys still shows.
func (e *verdictError) Error() string {
	shown := min(len(e.causes), maxRenderedFailures+1) // the headline occupies the first slot
	parts := make([]string, 0, shown+1)
	for _, cause := range e.causes[:shown] {
		parts = append(parts, cause.Error())
	}
	if notShown := len(e.causes) - shown; notShown > 0 {
		footer := fmt.Sprintf("showing the first %d of %d signature failures", maxRenderedFailures, len(e.causes)-1)
		if len(e.hidden) > 0 {
			names := make([]string, len(e.hidden))
			for i, status := range e.hidden {
				names[i] = string(status)
			}
			footer += fmt.Sprintf("; %d not shown, carrying: %s", notShown, strings.Join(names, ", "))
		}
		parts = append(parts, footer)
	}

	return strings.Join(parts, "\n")
}

// Unwrap returns every cause, rendered or not, uncopied as errors.Join does;
// nothing in this package writes causes after verificationError builds it.
func (e *verdictError) Unwrap() []error {
	return e.causes
}

// hiddenStatuses returns the distinct statuses, in first-seen order, of the
// failures past maxRenderedFailures, or nil when none is hidden. Deduping keeps
// a rare status from drowning among many failures of one status.
func hiddenStatuses(failures []Failure) []Status {
	if len(failures) <= maxRenderedFailures {
		return nil
	}
	var hidden []Status
	for _, failure := range failures[maxRenderedFailures:] {
		if !slices.Contains(hidden, failure.Status) {
			hidden = append(hidden, failure.Status)
		}
	}

	return hidden
}

// verificationError builds a negative verdict's *verdictError, rendering each
// cause with its status and quoted origin, since the library's message names
// neither and the origin is a value this package never validates.
func verificationError(p Policy, verified int, failures []Failure) error {
	causes := make([]error, 0, len(failures)+1)
	causes = append(causes, fmt.Errorf("%w: %s", helpers.ErrSignatureVerificationFailed, verdictReason(p, verified)))
	for i := range failures {
		causes = append(causes, fmt.Errorf("%s from %q: %w", failures[i].Status, failures[i].Origin, failures[i].Err))
	}

	return &verdictError{causes: causes, hidden: hiddenStatuses(failures)}
}

// verdictReason names the clause of Policy.verdict that refused the run, in
// that function's order. A count of 0 gets its own wording, since it fails
// because a signature verified rather than because too few did.
func verdictReason(p Policy, verified int) string {
	switch {
	case p.Required.Strict && verified == 0:
		return "no valid signature"
	case p.Required.All:
		return "some signatures failed"
	case p.Required.Count == 0:
		return fmt.Sprintf("a required count of 0 is satisfied only while nothing verifies; %d did", verified)
	default:
		return fmt.Sprintf("fewer valid signatures than required: got %d, need %d", verified, p.Required.Count)
	}
}

// checkOne verifies one blob against manifest, returning the verifying entity.
// Armor is detected from the bytes and decoded here, so every path passes
// checkPacketFraming before openpgp.CheckDetachedSignature parses a packet.
func checkOne(manifest, blob []byte, kr *Keyring) (*openpgp.Entity, error) {
	// Cheap pre-check: whitespace alone is as empty as no bytes at all. The
	// deciding emptiness check runs after the armor envelope below.
	if len(bytes.TrimSpace(blob)) == 0 {
		return nil, errNoSignatureData
	}

	packets := blob
	if bytes.Contains(blob, []byte(signatureArmorHeader)) {
		decoded, blockType, err := decodeArmorBlock(blob)
		if err != nil {
			return nil, err
		}
		if blockType != openpgp.SignatureType {
			return nil, pgperrors.InvalidArgumentError("expected '" + openpgp.SignatureType + "', got: " + blockType)
		}
		packets = decoded
	}

	// A well-formed empty armor envelope decodes to zero bytes; without this
	// check it would reach the library and come back as an ignorable NO_PUBKEY.
	if len(packets) == 0 {
		return nil, errNoSignatureData
	}
	if err := checkPacketFraming(packets, signatureBlobProfile()); err != nil {
		return nil, err
	}

	return openpgp.CheckDetachedSignature(kr.entities, bytes.NewReader(manifest), bytes.NewReader(packets), nil)
}

// classify maps a verification failure onto the status vocabulary the ignore
// set is configured against. An unrecognized error becomes StatusErrSig, so a
// new go-crypto error shape makes a signature fail, never verify.
func classify(err error) Status {
	var corruptBase64 base64.CorruptInputError

	switch {
	case errors.Is(err, errNoSignatureData):
		return StatusNoData
	case errors.Is(err, pgperrors.ErrUnknownIssuer):
		return StatusNoPubKey
	case errors.Is(err, pgperrors.ErrKeyRevoked):
		return StatusRevKeySig
	case errors.Is(err, pgperrors.ErrKeyExpired):
		return StatusExpKeySig
	case errors.Is(err, pgperrors.ErrSignatureExpired):
		return StatusExpSig
	case errors.Is(err, armor.ArmorCorrupt), errors.As(err, &corruptBase64):
		return StatusBadArmor
	case isSignatureError(err):
		return StatusBadSig
	default:
		return StatusErrSig
	}
}

// isSignatureError reports whether err is go-crypto's SignatureError (BADSIG),
// tested by type because its text carries the failing algorithm. classify
// tests it after the sentinel arms, so a sentinel match always wins.
func isSignatureError(err error) bool {
	_, ok := errors.AsType[pgperrors.SignatureError](err)

	return ok
}
