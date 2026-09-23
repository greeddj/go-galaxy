package solver

import (
	"testing"
)

// constraintCase mirrors the collections package's constraintCase rows, so
// the same ground-truth semver semantics drive TestVerSetGroundTruthRows.
type constraintCase struct {
	name       string
	constraint string
	version    string
	wantMatch  bool
	wantErr    bool
}

// symbolicSemanticsCases are the pinned semver semantics rows: operators,
// x-ranges, tilde, caret, prerelease gating, the "==" rewrite and parse-error
// guards. newVerSet must agree with every row.
func symbolicSemanticsCases() []constraintCase {
	return []constraintCase{
		{name: "bare exact match", constraint: "1.2.3", version: "1.2.3", wantMatch: true},
		{name: "bare exact no-match", constraint: "1.2.3", version: "1.2.4", wantMatch: false},
		{name: "= exact match", constraint: "=1.2.3", version: "1.2.3", wantMatch: true},
		{name: "!= match", constraint: "!=1.2.3", version: "1.2.4", wantMatch: true},
		{name: "!= no-match", constraint: "!=1.2.3", version: "1.2.3", wantMatch: false},
		{name: ">= match at floor", constraint: ">=1.0.0", version: "1.0.0", wantMatch: true},
		{name: ">= no-match below floor", constraint: ">=1.0.0", version: "0.9.0", wantMatch: false},
		{name: "range match inside", constraint: ">=1.0.0,<2.0.0", version: "1.5.0", wantMatch: true},
		{name: "range no-match at ceiling", constraint: ">=1.0.0,<2.0.0", version: "2.0.0", wantMatch: false},
		{name: "* matches anything", constraint: "*", version: "0.0.1", wantMatch: true},
		{name: "empty matches anything", constraint: "", version: "9.9.9", wantMatch: true},
		{name: "1.x match", constraint: "1.x", version: "1.9.9", wantMatch: true},
		{name: "1.x no-match", constraint: "1.x", version: "2.0.0", wantMatch: false},
		{name: "~1.2.3 match", constraint: "~1.2.3", version: "1.2.9", wantMatch: true},
		{name: "~1.2.3 no-match", constraint: "~1.2.3", version: "1.3.0", wantMatch: false},
		{name: "^1.2.3 match", constraint: "^1.2.3", version: "1.9.9", wantMatch: true},
		{name: "^1.2.3 no-match", constraint: "^1.2.3", version: "2.0.0", wantMatch: false},
		{name: "^0.2.3 caret-zero-minor match", constraint: "^0.2.3", version: "0.2.9", wantMatch: true},
		{name: "^0.2.3 caret-zero-minor no-match", constraint: "^0.2.3", version: "0.3.0", wantMatch: false},
		{name: "^0.0.3 caret-zero-patch match", constraint: "^0.0.3", version: "0.0.3", wantMatch: true},
		{name: "^0.0.3 caret-zero-patch no-match", constraint: "^0.0.3", version: "0.0.4", wantMatch: false},
		{name: "prerelease excluded above range", constraint: ">=1.0.0", version: "2.0.0-rc1", wantMatch: false},
		{name: "prerelease included via -0 floor", constraint: ">=1.0.0-0", version: "1.0.0-rc1", wantMatch: true},
		{name: "bare prerelease exact match", constraint: "1.0.0-rc1", version: "1.0.0-rc1", wantMatch: true},
		{name: "bare prerelease exact no-match", constraint: "1.0.0-rc1", version: "1.0.0", wantMatch: false},
		{name: "== rewritten to = match", constraint: "==1.2.3", version: "1.2.3", wantMatch: true},
		{name: "==,!= match", constraint: "==1.0.0,!=1.0.5", version: "1.0.0", wantMatch: true},
		{name: "==,!= excluded", constraint: "==1.0.0,!=1.0.5", version: "1.0.5", wantMatch: false},
		{name: "=== stays a parse error", constraint: "===1.2.3", version: "1.2.3", wantErr: true},
		{name: ">== stays a parse error", constraint: ">==1.2.3", version: "1.2.3", wantErr: true},
	}
}

func mustV(t *testing.T, raw string) Version {
	t.Helper()
	v, err := NewVersion(raw)
	if err != nil {
		t.Fatalf("NewVersion(%q): %v", raw, err)
	}
	return v
}

// buildTestUniverse parses and orders raw version strings the same way the
// core does when a package's universe is fetched.
func buildTestUniverse(t *testing.T, raw ...string) []Version {
	t.Helper()
	versions := make([]Version, 0, len(raw))
	for _, r := range raw {
		versions = append(versions, mustV(t, r))
	}
	return buildUniverse(versions)
}

// TestUniverseTotalOrder pins the universe order: precedence descending, then
// original string descending, which separates equal-precedence spellings.
func TestUniverseTotalOrder(t *testing.T) {
	t.Parallel()
	got := buildTestUniverse(t, "1.0.0", "2.0.0", "1.0.0+build", "1.5.0")
	want := []string{"2.0.0", "1.5.0", "1.0.0+build", "1.0.0"}
	if len(got) != len(want) {
		t.Fatalf("buildUniverse length = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Original() != w {
			t.Fatalf("buildUniverse[%d] = %q, want %q (full: %v)", i, got[i].Original(), w, originals(got))
		}
	}
}

// TestUniverseDedup pins deduplication by exact original string.
func TestUniverseDedup(t *testing.T) {
	t.Parallel()
	got := buildTestUniverse(t, "1.0.0", "1.0.0", "2.0.0")
	if len(got) != 2 {
		t.Fatalf("buildUniverse length = %d, want 2 (dedup by original string): %v", len(got), originals(got))
	}
}

func originals(vs []Version) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Original()
	}
	return out
}

// ---- signed term algebra ----------------------------------------------------

// termAlgebraSets is the small set pool the signed-algebra tests draw
// sign/set combinations from; the pairs cover subset, disjoint, and
// partial-overlap relationships.
func termAlgebraSets(t *testing.T) []verSet {
	t.Helper()
	out := make([]verSet, 0, 6)
	for _, c := range []string{"^1.0.0", ">=1.0.0", "^2.0.0", "=1.5.0", ">=1.5.0,<2.5.0", "*"} {
		out = append(out, mustSet(t, c))
	}
	return out
}

// termAlgebraProbes is the probe pool the pointwise checks evaluate on.
func termAlgebraProbes(t *testing.T) []Version {
	t.Helper()
	raws := []string{"0.9.0", "1.0.0", "1.0.0-rc.1", "1.5.0", "1.9.9", "2.0.0", "2.4.0", "2.5.0", "3.0.0"}
	out := make([]Version, 0, len(raws))
	for _, r := range raws {
		out = append(out, mustV(t, r))
	}
	return out
}

// TestTermIntersectPointwise pins the signed conjunction table: for every
// sign/set pair and probe, the conjunction permits v exactly when both terms
// do.
func TestTermIntersectPointwise(t *testing.T) {
	t.Parallel()
	sets := termAlgebraSets(t)
	probes := termAlgebraProbes(t)
	for _, sa := range sets {
		for _, sb := range sets {
			for _, pa := range []bool{true, false} {
				for _, pb := range []bool{true, false} {
					a := term{Package: "foo", Set: sa, Positive: pa}
					b := term{Package: "foo", Set: sb, Positive: pb}
					joint := termIntersect(a, b)
					for _, v := range probes {
						want := termPermits(a, v) && termPermits(b, v)
						if got := termPermits(joint, v); got != want {
							t.Fatalf("termIntersect(%v %q, %v %q) at %q = %v, want %v",
								pa, sa.displayLabel(), pb, sb.displayLabel(), v.Original(), got, want)
						}
					}
				}
			}
		}
	}
}

// TestTermIntersectIdentitySeed pins that folding a term over the N({}) seed
// returns it verbatim, so an "=X" assignment keeps its pinned version.
func TestTermIntersectIdentitySeed(t *testing.T) {
	t.Parallel()
	pinned := term{Package: "foo", Set: mustSet(t, "=1.2.3"), Positive: true}
	folded := termIntersect(accumSeed("foo"), pinned)
	if !sameTerm(folded, pinned) {
		t.Fatalf("identity fold changed the term: %+v", folded)
	}
	if v, ok := folded.Set.decidedVersion(); !ok || v.Original() != "1.2.3" {
		t.Fatalf("identity fold dropped the pinned version: (%q, %v)", v.Original(), ok)
	}
}

// TestTermSubsetTable pins the four entailment rows directly, including the
// two sign asymmetries: a negative term never entails a positive one, and
// negative-negative entailment inverts the subset direction.
func TestTermSubsetTable(t *testing.T) {
	t.Parallel()
	narrow, wide := mustSet(t, "^1.0.0"), mustSet(t, ">=1.0.0")
	disjoint := mustSet(t, "^2.0.0")

	pNarrow := term{Package: "foo", Set: narrow, Positive: true}
	pWide := term{Package: "foo", Set: wide, Positive: true}
	nNarrow := term{Package: "foo", Set: narrow, Positive: false}
	nWide := term{Package: "foo", Set: wide, Positive: false}
	nDisjoint := term{Package: "foo", Set: disjoint, Positive: false}

	if !termSubset(pNarrow, pWide) || termSubset(pWide, pNarrow) {
		t.Fatalf("positive-positive entailment must follow set inclusion")
	}
	if !termSubset(pNarrow, nDisjoint) || termSubset(pNarrow, nNarrow) {
		t.Fatalf("positive-negative entailment must follow disjointness")
	}
	if termSubset(nWide, pWide) {
		t.Fatalf("a negative term must never entail a positive one")
	}
	if !termSubset(nWide, nNarrow) || termSubset(nNarrow, nWide) {
		t.Fatalf("negative-negative entailment must invert set inclusion")
	}
}

// TestTermIsTautological pins the tautology definition: exactly N({}); a
// positive term is never tautological, even over the full set.
func TestTermIsTautological(t *testing.T) {
	t.Parallel()
	if !termIsTautological(term{Package: "foo", Set: emptyVerSet(), Positive: false}) {
		t.Fatalf("N({}) must be tautological")
	}
	if termIsTautological(term{Package: "foo", Set: fullVerSet(), Positive: true}) {
		t.Fatalf("P(full) asserts selection and must not be tautological")
	}
	if termIsTautological(term{Package: "foo", Set: emptyVerSet(), Positive: true}) {
		t.Fatalf("P({}) is unsatisfiable, not tautological")
	}
	if termIsTautological(term{Package: "foo", Set: fullVerSet(), Positive: false}) {
		t.Fatalf("N(full) forbids selection and must not be tautological")
	}
}
