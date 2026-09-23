package signature

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// countSpecCase is one row of TestParseCountSpec: the configured value, the
// spec it must parse to, and - for a refusal - a phrase its message must carry
// on top of naming the offending value.
type countSpecCase struct {
	name            string
	value           string
	wantErrContains string
	want            CountSpec
	wantErr         bool
}

// countSpecCases is the grammar, spelled out; the accepted rows are the
// positive control, so a parser refusing everything fails the table.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var countSpecCases = []countSpecCase{
	{name: "bare count", value: "1", want: CountSpec{Count: 1}},
	// A floor of zero parses rather than being refused: it asks for no floor,
	// which is a policy an operator may write.
	{name: "zero is a floor of zero", value: "0", want: CountSpec{Count: 0}},
	{name: "multi digit count", value: "3", want: CountSpec{Count: 3}},
	{name: "all", value: "all", want: CountSpec{All: true}},
	{name: "strict count", value: "+1", want: CountSpec{Count: 1, Strict: true}},
	{name: "strict all", value: "+all", want: CountSpec{All: true, Strict: true}},
	// "+0" is accepted, and it is the row showing the two halves are
	// independent: the strict marker is not part of the number, so it survives
	// a floor of zero and still requires that some signature verified.
	{name: "strict zero", value: "+0", want: CountSpec{Count: 0, Strict: true}},
	// The generic refusal has to name what would have been accepted, since a
	// value this small offers nothing else to reason from.
	{name: "empty value", value: "", wantErr: true, wantErrContains: allSpelling},
	// ansible's install-parser help text claims -1 means every signature, so
	// operators arrive here having read it. The refusal carries its own message
	// naming the spelling that does mean it.
	{name: "negative one", value: "-1", wantErr: true, wantErrContains: "does not mean every signature"},
	{name: "decimal fraction", value: "1.5", wantErr: true},
	// "ALL" is refused rather than folded to lower case: the token is a literal
	// of ansible's grammar, and accepting a spelling ansible itself rejects
	// would let a value work here and fail under ansible-galaxy.
	{name: "upper case all", value: "ALL", wantErr: true},
	// The grammar covers the whole value, so an inner space is not absorbed...
	{name: "space after the strict marker", value: "+ 1", wantErr: true},
	// ...and neither is a surrounding one. A count is a single scalar, unlike
	// an element of the status code list, which is trimmed because a separator
	// leaves whitespace around it.
	{name: "leading space", value: " 1", wantErr: true},
	{name: "doubled strict marker", value: "++1", wantErr: true},
	{name: "count with a trailing word", value: "1all", wantErr: true},
	// Nothing but digits and still not a count: the conversion refuses it
	// rather than wrapping around into one.
	{name: "count past what an int holds", value: "99999999999999999999", wantErr: true, wantErrContains: "larger than"},
}

func TestParseCountSpec(t *testing.T) {
	t.Parallel()

	for _, tc := range countSpecCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseCountSpec(tc.value)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("ParseCountSpec(%q) = %v, want nil", tc.value, err)
				}
				if got != tc.want {
					t.Fatalf("ParseCountSpec(%q) = %+v, want %+v", tc.value, got, tc.want)
				}

				return
			}

			if !errors.Is(err, helpers.ErrInvalidSignatureCount) {
				t.Fatalf("ParseCountSpec(%q) error = %v, want the invalid-count sentinel", tc.value, err)
			}
			if !strings.Contains(err.Error(), strconv.Quote(tc.value)) {
				t.Fatalf("ParseCountSpec(%q) error does not name the offending value:\n%v", tc.value, err)
			}
			// Only this phrase tells a named refusal such as -1's apart from the
			// generic one, which carries the sentinel and the value as well.
			if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Fatalf("ParseCountSpec(%q) error does not carry %q:\n%v", tc.value, tc.wantErrContains, err)
			}
		})
	}
}

// statusCodesCase is one row of TestParseStatusCodes: the configured values,
// the codes the resulting set must hold, and - for a refusal - a phrase its
// message must carry.
type statusCodesCase struct {
	name            string
	wantErrContains string
	values          []string
	want            []Status
	wantErr         bool
}

// statusCodesCases covers the whole vocabulary, both normalizations and every
// refused shape, with the accepted rows as the refusals' positive control.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var statusCodesCases = []statusCodesCase{
	{name: "every code in the vocabulary", values: statusCodeStrings(), want: slices.Clone(statusCodes)},
	{name: "lower case", values: []string{"badsig"}, want: []Status{StatusBadSig}},
	{name: "mixed case", values: []string{"BadSig"}, want: []Status{StatusBadSig}},
	{name: "surrounding whitespace", values: []string{"  BADSIG\t"}, want: []Status{StatusBadSig}},
	// A repeated code is not a refusal: a set holds one of each, and asking
	// twice asks for what asking once did.
	{name: "repeated code collapses", values: []string{"BADSIG", "badsig"}, want: []Status{StatusBadSig}},
	{name: "no codes configured", values: nil, want: nil},
	{name: "unknown name", values: []string{"BADSIGG"}, wantErr: true, wantErrContains: `"BADSIGG"`},
	// The list is refused whole rather than kept up to the bad element, so a
	// typo cannot leave a partly applied ignore set behind.
	{name: "unknown name after a known one", values: []string{"BADSIG", "NOPE"}, wantErr: true, wantErrContains: `"NOPE"`},
	{name: "empty element", values: []string{""}, wantErr: true, wantErrContains: `""`},
	// The message quotes what the operator wrote rather than the trimmed form,
	// which is the only version they can search their configuration for.
	{name: "element that is only whitespace", values: []string{"  "}, wantErr: true, wantErrContains: `"  "`},
}

func TestParseStatusCodes(t *testing.T) {
	t.Parallel()

	for _, tc := range statusCodesCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseStatusCodes(tc.values)
			if tc.wantErr {
				// A silently skipped unknown code would leave the run failing on
				// the very status the operator meant to tolerate.
				if !errors.Is(err, helpers.ErrUnknownSignatureStatusCode) {
					t.Fatalf("ParseStatusCodes(%q) error = %v, want the unknown-code sentinel", tc.values, err)
				}
				if !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("ParseStatusCodes(%q) does not name the offending value %s:\n%v", tc.values, tc.wantErrContains, err)
				}
				// The accepted list is part of the contract; the vocabulary's
				// last code stands in for it, since the list is rendered whole.
				if !strings.Contains(err.Error(), string(StatusFailure)) {
					t.Fatalf("ParseStatusCodes(%q) error does not list the accepted codes:\n%v", tc.values, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseStatusCodes(%q) = %v, want nil", tc.values, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseStatusCodes(%q) holds %d codes, want %d", tc.values, len(got), len(tc.want))
			}
			for _, code := range tc.want {
				// The set is read directly rather than through Ignores, which
				// answers for a synonym too and so could not tell a code that
				// was parsed from one merely equivalent to a parsed one.
				if _, ok := got[code]; !ok {
					t.Fatalf("ParseStatusCodes(%q) does not hold %s", tc.values, code)
				}
			}
		})
	}
}

// ignoresCase is one row of TestStatusSetIgnores. A nil configured slice builds
// a nil StatusSet, which is the value a run with no ignore list carries; an
// empty one builds an allocated but empty set. Both must tolerate nothing.
type ignoresCase struct {
	name       string
	code       Status
	configured []Status
	want       bool
}

// ignoresCases pairs every synonym direction with a negative on the same
// mechanism, so an Ignores answering true for everything fails.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var ignoresCases = []ignoresCase{
	{name: "exact match", configured: []Status{StatusBadSig}, code: StatusBadSig, want: true},
	{name: "expired key under gpg's other name", configured: []Status{StatusKeyExpired}, code: StatusExpKeySig, want: true},
	{name: "expired key under the emitted name", configured: []Status{StatusExpKeySig}, code: StatusKeyExpired, want: true},
	{name: "revoked key under gpg's other name", configured: []Status{StatusKeyRevoked}, code: StatusRevKeySig, want: true},
	{name: "revoked key under the emitted name", configured: []Status{StatusRevKeySig}, code: StatusKeyRevoked, want: true},
	{name: "unrelated code is not ignored", configured: []Status{StatusBadSig}, code: StatusErrSig, want: false},
	// The two synonym pairs are separate conditions: tolerating an expired key
	// must not tolerate a revoked one.
	{name: "one synonym pair does not cover the other", configured: []Status{StatusKeyExpired}, code: StatusRevKeySig, want: false},
	{name: "empty set", configured: []Status{}, code: StatusBadSig, want: false},
	{name: "nil set", configured: nil, code: StatusBadSig, want: false},
}

func TestStatusSetIgnores(t *testing.T) {
	t.Parallel()

	for _, tc := range ignoresCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var set StatusSet
			if tc.configured != nil {
				set = statusSet(tc.configured...)
			}

			// The synonym rows fail if Ignores stops consulting statusSynonyms:
			// an operator names one of gpg's two names for a condition and the
			// verdict carries the other.
			if got := set.Ignores(tc.code); got != tc.want {
				t.Fatalf("StatusSet%v.Ignores(%s) = %t, want %t", tc.configured, tc.code, got, tc.want)
			}
		})
	}
}

// wantVocabulary is ansible's sixteen GPG_ERROR_MAP keys in statusCodes' order,
// hand-written rather than built from the constants to catch a wire rename.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var wantVocabulary = []Status{
	"BADSIG",
	"ERRSIG",
	"NO_PUBKEY",
	"EXPKEYSIG",
	"REVKEYSIG",
	"EXPSIG",
	"NODATA",
	"BADARMOR",
	"KEYEXPIRED",
	"KEYREVOKED",
	"MISSING_PASSPHRASE",
	"BAD_PASSPHRASE",
	"NO_SECKEY",
	"UNEXPECTED",
	"ERROR",
	"FAILURE",
}

// TestStatusVocabularyIsTheFrozenSixteen pins statusCodes, the list
// ParseStatusCodes accepts from: a code missing from it or stale in it would
// show up nowhere else.
func TestStatusVocabularyIsTheFrozenSixteen(t *testing.T) {
	t.Parallel()

	if !slices.Equal(statusCodes, wantVocabulary) {
		t.Fatalf("statusCodes = %v, want %v", statusCodes, wantVocabulary)
	}
}

// TestStatusSynonymsAreBidirectional pins the synonym table Ignores consults:
// both directions of both pairs, each naming a code in the vocabulary.
func TestStatusSynonymsAreBidirectional(t *testing.T) {
	t.Parallel()

	want := map[Status]Status{
		"KEYEXPIRED": "EXPKEYSIG",
		"EXPKEYSIG":  "KEYEXPIRED",
		"KEYREVOKED": "REVKEYSIG",
		"REVKEYSIG":  "KEYREVOKED",
	}
	if !maps.Equal(statusSynonyms, want) {
		t.Fatalf("statusSynonyms = %v, want %v", statusSynonyms, want)
	}
	for code := range statusSynonyms {
		if !slices.Contains(statusCodes, code) {
			t.Fatalf("statusSynonyms names %s, which is not in the vocabulary", code)
		}
	}
}

// policyEnabledCase is one row of the Enabled truth table.
type policyEnabledCase struct {
	name        string
	keyringPath string
	disabled    bool
	want        bool
}

// policyEnabledCases is that table in full: both inputs, both values each.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var policyEnabledCases = []policyEnabledCase{
	{name: "keyring configured and verification on", keyringPath: "keys.asc", disabled: false, want: true},
	{name: "keyring configured and verification off", keyringPath: "keys.asc", disabled: true, want: false},
	{name: "no keyring and verification on", keyringPath: "", disabled: false, want: false},
	{name: "no keyring and verification off", keyringPath: "", disabled: true, want: false},
}

func TestPolicyEnabled(t *testing.T) {
	t.Parallel()

	for _, tc := range policyEnabledCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			policy, err := NewPolicy(tc.keyringPath, "1", nil, tc.disabled)
			if err != nil {
				t.Fatalf("NewPolicy(%q, disabled=%t) = %v, want nil", tc.keyringPath, tc.disabled, err)
			}
			if got := policy.Enabled(); got != tc.want {
				t.Fatalf("Policy{KeyringPath: %q, Disabled: %t}.Enabled() = %t, want %t", tc.keyringPath, tc.disabled, got, tc.want)
			}
		})
	}
}

// TestNewPolicyAssemblesWithoutExpandingThePath pins the assembly end to end,
// including that the keyring path is stored as configured, "~" and all.
func TestNewPolicyAssemblesWithoutExpandingThePath(t *testing.T) {
	t.Parallel()

	const configured = "~/keys/team.asc"

	policy, err := NewPolicy(configured, "+2", []string{"keyexpired"}, false)
	if err != nil {
		t.Fatalf("NewPolicy = %v, want nil", err)
	}
	if policy.KeyringPath != configured {
		t.Fatalf("KeyringPath = %q, want %q unexpanded", policy.KeyringPath, configured)
	}
	if want := (CountSpec{Count: 2, Strict: true}); policy.Required != want {
		t.Fatalf("Required = %+v, want %+v", policy.Required, want)
	}
	// The ignore set arrives through the same normalization and synonym rules
	// pinned above, so this asserts they survived assembly rather than
	// re-testing them: lower case in, and the code a verdict would carry out.
	if !policy.Ignore.Ignores(StatusExpKeySig) {
		t.Fatalf("Ignore does not tolerate %s after being configured with its other name", StatusExpKeySig)
	}
	if !policy.Enabled() {
		t.Fatalf("Enabled() = false, want true for a configured keyring with verification on")
	}
}

// newPolicyRefusalCase is one row of TestNewPolicyRefusesAnInvalidValue: the
// two parsed inputs and the sentinel the assembly must fail with, or nil for
// the control row.
type newPolicyRefusalCase struct {
	wantErr       error
	name          string
	requiredCount string
	ignoreCodes   []string
}

// newPolicyRefusalCases carries its own positive control: one row whose inputs
// are both good, so a refusal cannot be NewPolicy refusing everything.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var newPolicyRefusalCases = []newPolicyRefusalCase{
	{name: "both values accepted", requiredCount: "all", ignoreCodes: []string{"BADSIG"}, wantErr: nil},
	{name: "count outside the grammar", requiredCount: "-1", ignoreCodes: []string{"BADSIG"}, wantErr: helpers.ErrInvalidSignatureCount},
	{name: "code outside the vocabulary", requiredCount: "1", ignoreCodes: []string{"NOPE"}, wantErr: helpers.ErrUnknownSignatureStatusCode},
}

func TestNewPolicyRefusesAnInvalidValue(t *testing.T) {
	t.Parallel()

	for _, tc := range newPolicyRefusalCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewPolicy("keys.asc", tc.requiredCount, tc.ignoreCodes, false)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("NewPolicy(%q, %q) = %v, want nil", tc.requiredCount, tc.ignoreCodes, err)
				}

				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("NewPolicy(%q, %q) error = %v, want %v", tc.requiredCount, tc.ignoreCodes, err, tc.wantErr)
			}
		})
	}
}

// statusSet builds a StatusSet from codes, which the production parser only
// ever produces from strings.
func statusSet(codes ...Status) StatusSet {
	set := make(StatusSet, len(codes))
	for _, code := range codes {
		set[code] = struct{}{}
	}

	return set
}

// statusCodeStrings renders the vocabulary as the strings an operator would
// configure, so the "every code" row exercises all sixteen without spelling
// them a third time.
func statusCodeStrings() []string {
	values := make([]string, len(statusCodes))
	for i, code := range statusCodes {
		values[i] = string(code)
	}

	return values
}
