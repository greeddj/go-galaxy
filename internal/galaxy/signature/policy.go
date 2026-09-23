package signature

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// strictPrefix is the leading marker that makes a count spec strict. It is
	// a marker rather than a sign: the number it precedes is still a plain
	// non-negative count.
	strictPrefix = "+"

	// allSpelling is the one non-numeric count spec, and it is lower case
	// because that is the literal ansible's own grammar accepts.
	allSpelling = "all"

	// negativeAllSpelling is the value ansible's install-parser help claims
	// means every signature, though ansible's grammar accepts no negative number.
	// It is refused by name so the error can say to write allSpelling instead.
	negativeAllSpelling = "-1"
)

// CountSpec is a parsed required-valid-signature-count: Count signatures or All
// checked ones must verify. Bare N and all pass vacuously on an empty list, as
// in ansible-galaxy; keep that parity, since only Strict is meant to close it.
type CountSpec struct {
	// Count is the number of signatures that must verify. It is meaningful
	// only when All is false.
	Count int
	// All requires every signature checked to verify, in place of a count.
	All bool
	// Strict also requires that at least one signature verified. It is not a
	// rule about failures: a counted walk never checks past its Count-th
	// signer, so making every failure fatal is what All means instead.
	Strict bool
}

// ParseCountSpec parses ansible-core 2.21.2's grammar: an optional strictPrefix,
// then allSpelling or an ASCII decimal count. The whole value must match,
// untrimmed and case-sensitive, so nothing passes here that ansible refuses.
func ParseCountSpec(value string) (CountSpec, error) {
	if value == negativeAllSpelling {
		return CountSpec{}, fmt.Errorf("%w: %q does not mean every signature; write %q instead",
			helpers.ErrInvalidSignatureCount, value, allSpelling)
	}

	var spec CountSpec

	rest := value
	if after, found := strings.CutPrefix(rest, strictPrefix); found {
		spec.Strict = true
		rest = after
	}

	if rest == allSpelling {
		spec.All = true

		return spec, nil
	}

	if !isDecimalDigits(rest) {
		return CountSpec{}, invalidCountSpec(value)
	}

	count, err := strconv.Atoi(rest)
	if err != nil {
		// The only way a string of nothing but digits fails to convert is a
		// range error, so the message names the size rather than the shape.
		return CountSpec{}, fmt.Errorf("%w: %q is larger than a signature count can be", helpers.ErrInvalidSignatureCount, value)
	}
	spec.Count = count

	return spec, nil
}

// invalidCountSpec refuses a value outside the grammar, spelling out the
// accepted literals because the message is where an operator learns them.
func invalidCountSpec(value string) error {
	return fmt.Errorf("%w: %q; write a non-negative count or %q, optionally prefixed with %q for strict",
		helpers.ErrInvalidSignatureCount, value, allSpelling, strictPrefix)
}

// isDecimalDigits reports whether s is one or more ASCII decimal digits. It
// loops over bytes so a non-ASCII digit is a grammar refusal, not an Atoi one.
func isDecimalDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return true
}

// Policy is one run's signature verification policy. It holds no key material
// and nothing mutates it after NewPolicy, so every worker can read one Policy
// without synchronization.
type Policy struct {
	// Ignore is the set of statuses a failed signature may carry without
	// counting as a failure.
	Ignore StatusSet
	// KeyringPath is the keyring location exactly as this package received it;
	// nothing here expands or resolves it, since that belongs to the config
	// layer.
	KeyringPath string
	// Required is the parsed required-valid-signature-count spec.
	Required CountSpec
	// Disabled records that verification was switched off outright, which is
	// separate from no keyring having been configured.
	Disabled bool
}

// NewPolicy assembles a Policy from the raw configured values. It takes the
// keyring path, not a loaded Keyring, so Enabled needs no file read, and stores
// it unexpanded: resolving "~" belongs to the config layer.
func NewPolicy(keyringPath, requiredCount string, ignoreCodes []string, disabled bool) (Policy, error) {
	required, err := ParseCountSpec(requiredCount)
	if err != nil {
		return Policy{}, err
	}

	ignore, err := ParseStatusCodes(ignoreCodes)
	if err != nil {
		return Policy{}, err
	}

	return Policy{
		KeyringPath: keyringPath,
		Ignore:      ignore,
		Required:    required,
		Disabled:    disabled,
	}, nil
}

// Enabled reports whether this run verifies signatures at all: a keyring must
// be configured and verification must not be switched off.
func (p Policy) Enabled() bool {
	return VerificationEnabled(p.KeyringPath, p.Disabled)
}

// VerificationEnabled is Enabled over the raw configured values, for a caller
// deciding whether to build a Policy at all. Call it rather than copying the
// predicate: a drifted copy could silently switch verification off.
func VerificationEnabled(keyringPath string, disabled bool) bool {
	return keyringPath != "" && !disabled
}
