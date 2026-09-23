package signature

import (
	"fmt"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Status is one gpg status code as ansible's GPG_ERROR_MAP spells it. The
// sixteen constants are that map's key set (ansible-core 2.21.2), grouped by
// whether a verdict can carry the code, names a synonym of one, or is inert.
type Status string

// The eight status codes a verdict from this tool can carry, one per class of
// OpenPGP verification failure; the ignore set is configured against these,
// so a classifier must not produce any code outside them.
const (
	StatusBadSig    Status = "BADSIG"
	StatusErrSig    Status = "ERRSIG"
	StatusNoPubKey  Status = "NO_PUBKEY"
	StatusExpKeySig Status = "EXPKEYSIG"
	StatusRevKeySig Status = "REVKEYSIG"
	StatusExpSig    Status = "EXPSIG"
	StatusNoData    Status = "NODATA"
	StatusBadArmor  Status = "BADARMOR"
)

// The two status codes that are gpg's second name for one of the eight
// (KEYEXPIRED for EXPKEYSIG, KEYREVOKED for REVKEYSIG): no verdict carries
// one, but statusSynonyms makes configuring either spelling cover both.
const (
	StatusKeyExpired Status = "KEYEXPIRED"
	StatusKeyRevoked Status = "KEYREVOKED"
)

// The six status codes accepted and inert: three concern secret keys, which
// LoadKeyring refuses, and three report on a gpg process, which this tool never
// runs. They are accepted so an ansible-shaped ignore list naming one parses.
const (
	StatusMissingPassphrase Status = "MISSING_PASSPHRASE"
	StatusBadPassphrase     Status = "BAD_PASSPHRASE"
	StatusNoSecKey          Status = "NO_SECKEY"
	StatusUnexpected        Status = "UNEXPECTED"
	StatusError             Status = "ERROR"
	StatusFailure           Status = "FAILURE"
)

// statusCodes is the single membership table, in the order a refusal lists
// it; a Status constant left out of it is refused as unknown.
//
//nolint:gochecknoglobals // a fixed, immutable vocabulary, not mutable shared state.
var statusCodes = []Status{
	StatusBadSig,
	StatusErrSig,
	StatusNoPubKey,
	StatusExpKeySig,
	StatusRevKeySig,
	StatusExpSig,
	StatusNoData,
	StatusBadArmor,
	StatusKeyExpired,
	StatusKeyRevoked,
	StatusMissingPassphrase,
	StatusBadPassphrase,
	StatusNoSecKey,
	StatusUnexpected,
	StatusError,
	StatusFailure,
}

// statusSynonyms maps each code gpg spells two ways to its other spelling,
// both ways, so ignoring either name covers a verdict carrying the other.
//
//nolint:gochecknoglobals // a fixed, immutable lookup table, not mutable shared state.
var statusSynonyms = map[Status]Status{
	StatusKeyExpired: StatusExpKeySig,
	StatusExpKeySig:  StatusKeyExpired,
	StatusKeyRevoked: StatusRevKeySig,
	StatusRevKeySig:  StatusKeyRevoked,
}

// StatusSet is a set of status codes an operator configured this run to
// tolerate. Its zero value is usable and tolerates nothing, which is what a run
// that configured no ignore list carries.
type StatusSet map[Status]struct{}

// Ignores reports whether s tolerates code, consulting statusSynonyms so that
// configuring either spelling of a condition covers both. See that table for
// why a membership test on its own would silently under-deliver.
func (s StatusSet) Ignores(code Status) bool {
	if _, ok := s[code]; ok {
		return true
	}

	synonym, ok := statusSynonyms[code]
	if !ok {
		return false
	}
	_, ok = s[synonym]

	return ok
}

// ParseStatusCodes turns configured status code names, trimmed and
// upper-cased, into a StatusSet. Anything outside statusCodes, an empty
// element included, is refused naming the list: a typo would ignore nothing.
func ParseStatusCodes(values []string) (StatusSet, error) {
	set := make(StatusSet, len(values))
	for _, value := range values {
		code := Status(strings.ToUpper(strings.TrimSpace(value)))
		if !slices.Contains(statusCodes, code) {
			return nil, fmt.Errorf("%w: %q; accepted codes are %s",
				helpers.ErrUnknownSignatureStatusCode, value, statusCodeList())
		}
		set[code] = struct{}{}
	}

	return set, nil
}

// statusCodeList renders the vocabulary for a refusal message. It allocates per
// call rather than being precomputed, because the only caller is a refusal path
// that ends the run.
func statusCodeList() string {
	names := make([]string, len(statusCodes))
	for i, code := range statusCodes {
		names[i] = string(code)
	}

	return strings.Join(names, ", ")
}
