package solver

import (
	"testing"
)

// newTestState builds a bare solveState with no requirements, for tests that
// drive relation()/propagation machinery directly rather than through Solve.
func newTestState(p Provider) *solveState {
	return newSolveState(p, nil)
}

func mustSet(t *testing.T, raw string) verSet {
	t.Helper()
	set, err := newVerSet(raw)
	if err != nil {
		t.Fatalf("newVerSet(%q): %v", raw, err)
	}
	return set
}

// TestRelationOnDecision covers the decided-package reading: a decision
// folds a positive singleton into the accumulation, so membership of the
// decided version is exact for both polarities.
func TestRelationOnDecision(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider().withVersions("foo", "1.0.0", "2.0.0"))
	s.ps.decide("foo", mustV(t, "1.0.0"))

	positiveMatching := term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}
	if got := relation(positiveMatching, s.ps); got != termSatisfied {
		t.Fatalf("positive matching term = %v, want termSatisfied", got)
	}
	positiveNonMatching := term{Package: "foo", Set: mustSet(t, "^2.0.0"), Positive: true}
	if got := relation(positiveNonMatching, s.ps); got != termContradicted {
		t.Fatalf("positive non-matching term = %v, want termContradicted", got)
	}
	negativeMatching := term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: false}
	if got := relation(negativeMatching, s.ps); got != termContradicted {
		t.Fatalf("negative matching term = %v, want termContradicted", got)
	}
	negativeNonMatching := term{Package: "foo", Set: mustSet(t, "^2.0.0"), Positive: false}
	if got := relation(negativeNonMatching, s.ps); got != termSatisfied {
		t.Fatalf("negative non-matching term = %v, want termSatisfied", got)
	}
}

// TestRelationNoAssignments pins that a package with no assignments relates
// inconclusive to any term: the vacuous seed never satisfies or contradicts.
func TestRelationNoAssignments(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	term := term{Package: "untouched", Set: fullVerSet(), Positive: true}
	if got := relation(term, s.ps); got != termInconclusive {
		t.Fatalf("relation on an untouched package = %v, want termInconclusive", got)
	}
}

// TestRelationExactWithoutUniverse pins that, with the universe never
// fetched, subset, disjointness and partial overlap are judged exactly, two
// disjoint positive ranges included as an outright contradiction.
func TestRelationExactWithoutUniverse(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	s.ps.derive(term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}, 0)

	subsetTerm := term{Package: "foo", Set: mustSet(t, ">=1.0.0"), Positive: true}
	if got := relation(subsetTerm, s.ps); got != termSatisfied {
		t.Fatalf("accumulation subset of term = %v, want termSatisfied", got)
	}
	disjointTerm := term{Package: "foo", Set: mustSet(t, "^2.0.0"), Positive: true}
	if got := relation(disjointTerm, s.ps); got != termContradicted {
		t.Fatalf("accumulation disjoint from term = %v, want termContradicted", got)
	}
	inconclusiveTerm := term{Package: "foo", Set: mustSet(t, "=1.0.0"), Positive: true}
	if got := relation(inconclusiveTerm, s.ps); got != termInconclusive {
		t.Fatalf("partial overlap = %v, want termInconclusive", got)
	}
	oppositePolarity := term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: false}
	if got := relation(oppositePolarity, s.ps); got != termContradicted {
		t.Fatalf("same set, opposite polarity = %v, want termContradicted", got)
	}
}

// TestRelationNegativeAccumulation pins that a package known only through
// negative facts never satisfies a positive term nor contradicts a negative
// one, which keeps its dependency terms derivable rather than settled.
func TestRelationNegativeAccumulation(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	s.ps.derive(term{Package: "foo", Set: mustSet(t, "^2.0.0"), Positive: false}, 0)

	positiveElsewhere := term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}
	if got := relation(positiveElsewhere, s.ps); got != termInconclusive {
		t.Fatalf("negative accumulation vs positive term = %v, want termInconclusive", got)
	}
	negativeWider := term{Package: "foo", Set: mustSet(t, ">=2.0.0"), Positive: false}
	if got := relation(negativeWider, s.ps); got != termInconclusive {
		t.Fatalf("negative accumulation vs wider negative term = %v, want termInconclusive", got)
	}
	negativeNarrower := term{Package: "foo", Set: mustSet(t, "=2.0.0"), Positive: false}
	if got := relation(negativeNarrower, s.ps); got != termSatisfied {
		t.Fatalf("negative accumulation vs narrower negative term = %v, want termSatisfied", got)
	}
	negativeFull := term{Package: "foo", Set: fullVerSet(), Positive: false}
	if got := relation(negativeFull, s.ps); got != termInconclusive {
		t.Fatalf("negative accumulation vs not-anything term = %v, want termInconclusive", got)
	}
}

// TestRelateAlmostSatisfied covers relate()'s aggregate logic: all terms but
// one satisfied, the remaining one inconclusive, is ALMOST_SATISFIED with
// that term surfaced.
func TestRelateAlmostSatisfied(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	s.ps.decide(rootPkg, rootVersion)
	inc := &incompatibility{
		Terms: []term{
			{Package: rootPkg, Set: singletonVerSet(rootVersion), Positive: true},
			{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: false},
		},
	}
	rel, unsat := relate(inc, s.ps)
	if rel != incAlmostSatisfied {
		t.Fatalf("relate = %v, want incAlmostSatisfied", rel)
	}
	if unsat.Package != "foo" {
		t.Fatalf("unsat term package = %q, want foo", unsat.Package)
	}
}

// TestUnitPropagationNewestToOldest pins that a package's incompatibilities
// are scanned newest first: of two that both derive, the newest one's
// derivation is recorded first.
func TestUnitPropagationNewestToOldest(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	s.ps.decide(rootPkg, rootVersion)

	oldInc := &incompatibility{Terms: []term{
		{Package: rootPkg, Set: singletonVerSet(rootVersion), Positive: true},
		{Package: "foo", Set: mustSet(t, ">=2.5.0"), Positive: false},
	}}
	oldIdx, _ := s.store.add(oldInc)

	newInc := &incompatibility{Terms: []term{
		{Package: rootPkg, Set: singletonVerSet(rootVersion), Positive: true},
		{Package: "foo", Set: mustSet(t, "^2.0.0"), Positive: false},
	}}
	newIdx, _ := s.store.add(newInc)

	if err := s.unitPropagation(rootPkg); err != nil {
		t.Fatalf("unitPropagation: %v", err)
	}

	// Both are almost satisfied with different foo constraints, so two
	// derivations land, the one caused by newInc first.
	p := s.ps.pkgState("foo")
	if len(p.indices) != 2 {
		t.Fatalf("expected two derivations for foo, got %d", len(p.indices))
	}
	first := s.ps.assignments[p.indices[0]]
	second := s.ps.assignments[p.indices[1]]
	if first.CauseIndex != newIdx {
		t.Fatalf("first derivation's cause = %d, want the newest incompatibility (%d)", first.CauseIndex, newIdx)
	}
	if second.CauseIndex != oldIdx {
		t.Fatalf("second derivation's cause = %d, want the oldest incompatibility (%d)", second.CauseIndex, oldIdx)
	}
}

// TestPopSmallestOrder pins changed's deterministic pop order: ascending
// package name, regardless of insertion order.
func TestPopSmallestOrder(t *testing.T) {
	t.Parallel()
	changed := map[string]bool{"zebra": true, "alpha": true, "middle": true}
	var order []string
	for len(changed) > 0 {
		order = append(order, popSmallest(changed))
	}
	want := []string{"alpha", "middle", "zebra"}
	for i, w := range want {
		if order[i] != w {
			t.Fatalf("pop order = %v, want %v", order, want)
		}
	}
}
