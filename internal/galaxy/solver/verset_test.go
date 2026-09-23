package solver

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
)

// assertCanonicalVerSet fails unless both sublines of s are canonical: sorted,
// nonempty, disjoint, non-abutting. Set equality rests on this, so a violation
// is a bug even when membership still answers right.
func assertCanonicalVerSet(t *testing.T, s verSet) {
	t.Helper()
	assertCanonicalPieces(t, "rel", s.rel, cmpRel)
	assertCanonicalPieces(t, "pre", s.pre, cmpPre)
}

func assertCanonicalPieces[B any](t *testing.T, subline string, pieces []piece[B], cmp func(B, B) int) {
	t.Helper()
	for i, p := range pieces {
		if !p.hiInf && cmp(p.lo, p.hi) >= 0 {
			t.Fatalf("%s piece %d is empty or inverted: %+v", subline, i, p)
		}
		if i == 0 {
			continue
		}
		if prev := pieces[i-1]; prev.hiInf || cmp(prev.hi, p.lo) >= 0 {
			t.Fatalf("%s pieces %d and %d overlap, abut, or are unsorted: %+v then %+v", subline, i-1, i, prev, p)
		}
	}
}

// algebraLawSeedCount is the number of random (A, B) pairs the law suite
// draws; small enough to stay sub-second, large enough that every operator
// pair combination in the pool gets exercised many times over.
const algebraLawSeedCount = 400

// TestVerSetAlgebraLaws property-checks the set algebra on random pool-built
// pairs: canonical results, double complement, De Morgan by structural
// equality, excluded middle, and pointwise agreement with membership.
func TestVerSetAlgebraLaws(t *testing.T) {
	t.Parallel()
	pool := differentialConstraintPool()
	sets := make([]verSet, 0, len(pool))
	for _, c := range pool {
		s, err := newVerSet(c)
		if err != nil {
			t.Fatalf("newVerSet(%q): %v", c, err)
		}
		sets = append(sets, s)
	}
	probes := make([]Version, 0, len(differentialProbePool()))
	for _, raw := range differentialProbePool() {
		probes = append(probes, mustV(t, raw))
	}

	//nolint:gosec // G404: deterministic seeded PRNG for a reproducible corpus, not security-sensitive
	rng := rand.New(rand.NewSource(1))
	for range algebraLawSeedCount {
		a := sets[rng.Intn(len(sets))]
		b := sets[rng.Intn(len(sets))]
		checkAlgebraLaws(t, a, b, probes)
	}
}

func checkAlgebraLaws(t *testing.T, a, b verSet, probes []Version) {
	t.Helper()
	inter, uni, diff, compA := a.intersect(b), a.union(b), a.difference(b), a.complement()
	for _, s := range []verSet{inter, uni, diff, compA} {
		assertCanonicalVerSet(t, s)
	}
	checkStructuralLaws(t, a, b, inter, uni, compA)
	if a.subsetOf(b) != diff.isEmpty() {
		t.Fatalf("subsetOf disagrees with difference emptiness for %q vs %q", a.displayLabel(), b.displayLabel())
	}
	if a.disjointFrom(b) != inter.isEmpty() {
		t.Fatalf("disjointFrom disagrees with intersection emptiness for %q vs %q", a.displayLabel(), b.displayLabel())
	}
	checkPointwiseOps(t, a, b, inter, uni, diff, compA, probes)
}

func checkPointwiseOps(t *testing.T, a, b, inter, uni, diff, compA verSet, probes []Version) {
	t.Helper()
	for _, v := range probes {
		inA, inB := a.contains(v), b.contains(v)
		if inter.contains(v) != (inA && inB) || uni.contains(v) != (inA || inB) ||
			diff.contains(v) != (inA && !inB) || compA.contains(v) == inA {
			t.Fatalf("pointwise mismatch at %q for %q vs %q", v.Original(), a.displayLabel(), b.displayLabel())
		}
	}
}

func checkStructuralLaws(t *testing.T, a, b, inter, uni, compA verSet) {
	t.Helper()
	if !compA.complement().equalSet(a) {
		t.Fatalf("double complement is not the identity for %q", a.displayLabel())
	}
	if !inter.complement().equalSet(compA.union(b.complement())) ||
		!uni.complement().equalSet(compA.intersect(b.complement())) {
		t.Fatalf("De Morgan violated for %q and %q", a.displayLabel(), b.displayLabel())
	}
	if !a.union(compA).isFull() || !a.intersect(compA).isEmpty() {
		t.Fatalf("excluded middle violated for %q", a.displayLabel())
	}
}

// TestVerSetCanonicalEncodingIdentity pins that equal sets built from
// different spellings encode identically and unequal sets do not.
func TestVerSetCanonicalEncodingIdentity(t *testing.T) {
	t.Parallel()
	equalPairs := [][2]string{
		{"^1.2.3", ">=1.2.3, <2.0.0"},
		{"1.x", ">=1.0.0, <2.0.0"},
		{"~1.2.3", ">=1.2.3, <1.3.0"},
		{"=1.2.3", "1.2.3"},
		{"1.2.3 - 2.3.4", ">=1.2.3, <=2.3.4"},
	}
	for _, pair := range equalPairs {
		a, errA := newVerSet(pair[0])
		b, errB := newVerSet(pair[1])
		if errA != nil || errB != nil {
			t.Fatalf("newVerSet(%q, %q): %v, %v", pair[0], pair[1], errA, errB)
		}
		if !a.equalSet(b) {
			t.Fatalf("%q and %q should denote the same set", pair[0], pair[1])
		}
		if encodeVerSet(a) != encodeVerSet(b) {
			t.Fatalf("equal sets %q and %q encode differently", pair[0], pair[1])
		}
	}
	a, errA := newVerSet("^1.2.3")
	b, errB := newVerSet("^1.2.4")
	if errA != nil || errB != nil {
		t.Fatalf("newVerSet: %v, %v", errA, errB)
	}
	if a.equalSet(b) || encodeVerSet(a) == encodeVerSet(b) {
		t.Fatalf("distinct sets compare or encode as equal")
	}
}

func encodeVerSet(s verSet) string {
	var b strings.Builder
	s.writeCanonical(&b)
	return b.String()
}

// TestVerSetBoundArithmetic pins the bound constructions: nothing in the probe
// pool sorts strictly between a bound and its succPre or succRel successor,
// and preFloor is minimal within its row.
func TestVerSetBoundArithmetic(t *testing.T) {
	t.Parallel()
	var pres []*semver.Version
	for _, raw := range differentialProbePool() {
		v := mustV(t, raw).sv()
		if v.Prerelease() != "" {
			pres = append(pres, stripMeta(v))
		}
	}
	checkSuccPreAdjacency(t, pres)
	floor := preFloor(relBound{major: 1, minor: 2, patch: 3})
	for _, u := range pres {
		if u.Major() == 1 && u.Minor() == 2 && u.Patch() == 3 && cmpPre(u, floor) < 0 {
			t.Fatalf("preFloor(1.2.3) = %s is not minimal: %s sorts below it", floor, u)
		}
	}
	if next, ok := succRel(relBound{major: 1, minor: 2, patch: 3}); !ok || next != (relBound{major: 1, minor: 2, patch: 4}) {
		t.Fatalf("succRel(1.2.3) = %+v, %v", next, ok)
	}
}

func checkSuccPreAdjacency(t *testing.T, pres []*semver.Version) {
	t.Helper()
	for _, w := range pres {
		succ := succPre(w)
		if cmpPre(w, succ) >= 0 {
			t.Fatalf("succPre(%s) = %s does not sort above its input", w, succ)
		}
		for _, u := range pres {
			if cmpPre(w, u) < 0 && cmpPre(u, succ) < 0 {
				t.Fatalf("%s sorts strictly between %s and succPre = %s", u, w, succ)
			}
		}
	}
}
