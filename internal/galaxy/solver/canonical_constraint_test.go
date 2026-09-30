package solver

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// respellOperatorRegexp finds an operator opening a clause, so a respelling
// can put a space between it and its version.
var respellOperatorRegexp = regexp.MustCompile(`(^|[\s,|])(!=|>=|=>|<=|=<|~>|==|>|<|~|\^|=)(v?[0-9xX*])`)

// respellings returns six spellings of c semver reads alike, duplicates kept:
// c itself, padded, spaced around operators, commas and ORs, and its clauses
// joined by ", ", a space and a tab.
func respellings(c string) []string {
	spaced := respellOperatorRegexp.ReplaceAllString(c, "$1$2 $3")
	spaced = strings.ReplaceAll(spaced, ",", " , ")
	spaced = strings.ReplaceAll(spaced, "||", " || ")
	return []string{
		c,
		"  " + c + "\t",
		spaced,
		strings.ReplaceAll(c, ",", ", "),
		strings.ReplaceAll(c, ",", " "),
		strings.ReplaceAll(c, ",", "\t"),
	}
}

// TestCanonicalConstraintPreservesMeaning holds the canonical spelling of
// every respelled differential constraint to the respelling's own acceptance,
// version set, singleton and Check answer on every probe.
func TestCanonicalConstraintPreservesMeaning(t *testing.T) {
	t.Parallel()
	probes := make([]Version, 0, len(differentialProbePool()))
	for _, raw := range differentialProbePool() {
		probes = append(probes, mustV(t, raw))
	}
	checked := 0
	for _, c := range differentialConstraintPool() {
		for _, raw := range respellings(c) {
			assertSameMeaning(t, raw, probes)
			checked++
		}
	}
	t.Logf("checked %d spellings of %d constraints", checked, len(differentialConstraintPool()))
}

// assertSameMeaning asserts CanonicalConstraint(raw) carries no whitespace,
// maps to itself, and reads as raw does to semver, newVerSet and Check.
func assertSameMeaning(t *testing.T, raw string, probes []Version) {
	t.Helper()
	canonical := helpers.CanonicalConstraint(raw)
	if strings.ContainsAny(canonical, " \t") {
		t.Fatalf("CanonicalConstraint(%q) = %q, want no whitespace", raw, canonical)
	}
	if again := helpers.CanonicalConstraint(canonical); again != canonical {
		t.Fatalf("CanonicalConstraint(%q) = %q, not idempotent on %q", canonical, again, raw)
	}
	assertSameVerSet(t, raw, canonical)
	for _, v := range probes {
		if propCheck(v.Original(), raw) != propCheck(v.Original(), canonical) {
			t.Fatalf("Check(%q) against %q and %q disagree", v.Original(), raw, canonical)
		}
	}
}

// assertSameVerSet asserts semver and newVerSet accept or refuse raw and its
// canonical spelling alike, and build one set with one singleton from both.
func assertSameVerSet(t *testing.T, raw, canonical string) {
	t.Helper()
	_, rawErr := semver.NewConstraint(helpers.NormalizeConstraint(raw))
	_, canonicalErr := semver.NewConstraint(helpers.NormalizeConstraint(canonical))
	if (rawErr == nil) != (canonicalErr == nil) {
		t.Fatalf("semver accepts %q: %v, but %q: %v", raw, rawErr, canonical, canonicalErr)
	}
	rawSet, rawErr := newVerSet(raw)
	canonicalSet, canonicalErr := newVerSet(canonical)
	if (rawErr == nil) != (canonicalErr == nil) ||
		errors.Is(rawErr, errNonIntervalConstraint) != errors.Is(canonicalErr, errNonIntervalConstraint) {
		t.Fatalf("newVerSet(%q) error = %v, newVerSet(%q) error = %v", raw, rawErr, canonical, canonicalErr)
	}
	if rawErr != nil {
		return
	}
	if !rawSet.equalSet(canonicalSet) {
		t.Fatalf("newVerSet(%q) and newVerSet(%q) are different sets", raw, canonical)
	}
	if rawSet.hasSingle != canonicalSet.hasSingle || rawSet.single.Original() != canonicalSet.single.Original() {
		t.Fatalf("singleton of %q = %q (%v), of %q = %q (%v)", raw, rawSet.single.Original(), rawSet.hasSingle,
			canonical, canonicalSet.single.Original(), canonicalSet.hasSingle)
	}
}
