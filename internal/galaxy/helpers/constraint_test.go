package helpers

import (
	"strings"
	"testing"
)

type canonicalConstraintCase struct {
	name string
	in   string
	want string
}

// canonicalConstraintCases pins CanonicalConstraint's rewrites, its match-all
// forms, its fixed points and the refusals it hands back as NormalizeConstraint
// left them; the rows are split so no table outgrows one function.
func canonicalConstraintCases() []canonicalConstraintCase {
	cases := []canonicalConstraintCase{
		{"space after operator", ">= 1.0", ">=1.0"},
		{"space between clauses", ">=1.0 <2.0", ">=1.0,<2.0"},
		{"comma and space between clauses", ">=1.0, <2.0", ">=1.0,<2.0"},
		{"padding around clauses", " >=1.0.0 ,<2.0.0 ", ">=1.0.0,<2.0.0"},
		{"hyphen range", "1.0 - 2.0", ">=1.0,<=2.0"},
		{"hyphen range of full versions", "1.0.0 - 1.9.0", ">=1.0.0,<=1.9.0"},
		{"=> alias", "=> 1.0", ">=1.0"},
		{"=< alias", "=<1.0", "<=1.0"},
		{"~> alias with space", "~> 1.2", "~1.2"},
		{"~> alias", "~>1.2.3", "~1.2.3"},
		{"double equals", "==1.0.0", "1.0.0"},
		{"equals with space", "= 1.0.0", "1.0.0"},
		{"equals with build metadata", "=1.2.3+build.7", "1.2.3+build.7"},
		{"spaced OR groups", "^1 || ^2", "^1||^2"},
		{"bare space between versions", "1.0 2.0", "1.0,2.0"},
		{"wildcard OR group", "* || 1.0", "*||1.0"},
		{"empty", "", ""},
		{"star", "*", ""},
		{"padded star", " * ", ""},
		{"lone equals star", "=*", "=*"},
		{"double equals star", "==*", "=*"},
		{"equals and spaced star", "= *", "=*"},
	}
	cases = append(cases, canonicalConstraintFixedPointCases()...)
	return append(cases, canonicalConstraintRefusalCases()...)
}

// canonicalConstraintFixedPointCases are spellings already canonical: the
// operand, "v", "x" and build metadata stay as written, and "1.0-2.0" is a
// prerelease, not a range.
func canonicalConstraintFixedPointCases() []canonicalConstraintCase {
	inputs := []string{
		">=10.0.0", ">=10.0.0,<12.0.0", "1.2.3", "~1.2", "^1.2", "1.x", "!=1.3.0",
		"v1.2.3", "x", "X", "1.0", "1.0-2.0", "1.2.3+build",
	}
	cases := make([]canonicalConstraintCase, 0, len(inputs))
	for _, in := range inputs {
		cases = append(cases, canonicalConstraintCase{"fixed point " + in, in, in})
	}
	return cases
}

// canonicalConstraintRefusalCases are inputs semver refuses, before or after
// the rewrite, which come back as NormalizeConstraint left them.
func canonicalConstraintRefusalCases() []canonicalConstraintCase {
	tooLong := strings.Repeat("1.0 - 2.0,", 42) + "1.0 - 2.0"
	return []canonicalConstraintCase{
		{"doubled operator", ">>= 1.0", ">>= 1.0"},
		{"double equals in a second group", "==1.0 || ==2.0", "=1.0 || ==2.0"},
		{"triple equals", "===1.0", "===1.0"},
		{"empty clause", ">=1.0,,<2.0", ">=1.0,,<2.0"},
		{"split operator", "> = 1.0", "> = 1.0"},
		{"dangling OR", ">=1.0 ||", ">=1.0 ||"},
		{"half-spaced hyphen", "1.0 -2.0", "1.0 -2.0"},
		{"ranges respelled past the length cap", tooLong, NormalizeConstraint(tooLong)},
	}
}

// TestCanonicalConstraint pins the canonical spelling of each table row.
func TestCanonicalConstraint(t *testing.T) {
	t.Parallel()
	for _, tc := range canonicalConstraintCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CanonicalConstraint(tc.in); got != tc.want {
				t.Errorf("CanonicalConstraint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCanonicalConstraintIsIdempotent pins that every canonical spelling maps
// to itself, and that an exact version, the constraint a git or url root
// carries, is already canonical, so their replay keys never move.
func TestCanonicalConstraintIsIdempotent(t *testing.T) {
	t.Parallel()
	for _, tc := range canonicalConstraintCases() {
		if got := CanonicalConstraint(tc.want); got != tc.want {
			t.Errorf("CanonicalConstraint(%q) = %q, want it unchanged", tc.want, got)
		}
	}
	for _, v := range []string{
		"1.2.3", "v1.2.3", "1.2.3-rc.1", "1.2.3+build", "0.0.0", "10.20.30", "1.0.0-alpha.beta", "1.2.3-0", "1.2.3+b.7",
	} {
		if !IsExactVersion(v) {
			t.Fatalf("IsExactVersion(%q) = false, want an exact version", v)
		}
		if got := CanonicalConstraint(v); got != v {
			t.Errorf("CanonicalConstraint(%q) = %q, want it unchanged", v, got)
		}
	}
}
