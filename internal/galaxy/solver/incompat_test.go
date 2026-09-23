package solver

import "testing"

// TestIncompatStoreContentDedup pins that two content-identical
// incompatibilities (same terms: package, polarity, symbolic key) collapse
// to the same store entry, while a genuinely different one gets its own.
func TestIncompatStoreContentDedup(t *testing.T) {
	t.Parallel()
	store := newIncompatStore()

	a := &incompatibility{Terms: []term{
		{Package: testPkgFoo, Set: mustSet(t, "^1.0.0"), Positive: true},
		{Package: "bar", Set: mustSet(t, "^2.0.0"), Positive: false},
	}}
	idxA, added := store.add(a)
	if !added {
		t.Fatalf("first add should report added=true")
	}

	b := &incompatibility{Terms: []term{
		{Package: testPkgFoo, Set: mustSet(t, "^1.0.0"), Positive: true},
		{Package: "bar", Set: mustSet(t, "^2.0.0"), Positive: false},
	}}
	idxB, added := store.add(b)
	if added {
		t.Fatalf("content-identical incompatibility should not be added again")
	}
	if idxA != idxB {
		t.Fatalf("content-identical incompatibility returned a different index: %d vs %d", idxA, idxB)
	}
	if len(store.all) != 1 {
		t.Fatalf("store has %d entries, want 1 after a duplicate add", len(store.all))
	}

	c := &incompatibility{Terms: []term{
		{Package: testPkgFoo, Set: mustSet(t, "^1.0.0"), Positive: true},
		{Package: "bar", Set: mustSet(t, "^3.0.0"), Positive: false},
	}}
	idxC, added := store.add(c)
	if !added || idxC == idxA {
		t.Fatalf("a genuinely different incompatibility must get its own new entry")
	}
	if len(store.all) != 2 {
		t.Fatalf("store has %d entries, want 2", len(store.all))
	}
}

// TestIncompatStoreByPackageAppendOrder pins that byPackageNewestFirst
// returns indices in reverse append order.
func TestIncompatStoreByPackageAppendOrder(t *testing.T) {
	t.Parallel()
	store := newIncompatStore()
	indices := make([]int, 0, 3)
	for i := range 3 {
		idx, _ := store.add(&incompatibility{Terms: []term{
			{Package: testPkgFoo, Set: mustSet(t, []string{"^1.0.0", "^2.0.0", "^3.0.0"}[i]), Positive: true},
		}})
		indices = append(indices, idx)
	}
	got := store.byPackageNewestFirst(testPkgFoo)
	want := []int{indices[2], indices[1], indices[0]}
	if len(got) != len(want) {
		t.Fatalf("byPackageNewestFirst returned %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byPackageNewestFirst = %v, want %v", got, want)
		}
	}
}

// TestNormalizeTermsMergesDuplicatePackages pins that normalizeTerms merges
// multiple terms for the same package into their signed conjunction.
func TestNormalizeTermsMergesDuplicatePackages(t *testing.T) {
	t.Parallel()
	terms := []term{
		{Package: testPkgFoo, Set: mustSet(t, ">=1.0.0"), Positive: true},
		{Package: testPkgFoo, Set: mustSet(t, "<2.0.0"), Positive: true},
	}
	merged := normalizeTerms(terms)
	if len(merged) != 1 {
		t.Fatalf("normalizeTerms merged %d terms for one package, want 1", len(merged))
	}
	if merged[0].Package != testPkgFoo || !merged[0].Positive {
		t.Fatalf("merged term = %+v, want a single positive foo term", merged[0])
	}
	if !merged[0].Set.equalSet(mustSet(t, ">=1.0.0, <2.0.0")) {
		t.Fatalf("merged term's set = %q, want the exact conjunction >=1.0.0,<2.0.0", merged[0].Set.displayLabel())
	}
}

// TestNormalizeTermsMergeKeepsNegativeConjunction pins the signed merge's
// polarity honesty: two negative terms conjoin to the negation of their
// sets' union, never to a force-repolarized positive term.
func TestNormalizeTermsMergeKeepsNegativeConjunction(t *testing.T) {
	t.Parallel()
	terms := []term{
		{Package: testPkgFoo, Set: mustSet(t, "^1.0.0"), Positive: false},
		{Package: testPkgFoo, Set: mustSet(t, "^2.0.0"), Positive: false},
	}
	merged := normalizeTerms(terms)
	if len(merged) != 1 || merged[0].Positive {
		t.Fatalf("normalizeTerms = %+v, want a single negative foo term", merged)
	}
	if !merged[0].Set.equalSet(mustSet(t, "^1.0.0 || ^2.0.0")) {
		t.Fatalf("merged negative set = %q, want the union of both ranges", merged[0].Set.displayLabel())
	}
}

// TestNormalizeTermsDropsPositiveRoot pins that a positive root term is
// dropped once more than one term remains.
func TestNormalizeTermsDropsPositiveRoot(t *testing.T) {
	t.Parallel()
	terms := []term{
		{Package: rootPkg, Set: singletonVerSet(rootVersion), Positive: true},
		{Package: testPkgFoo, Set: mustSet(t, "^1.0.0"), Positive: false},
	}
	merged := normalizeTerms(terms)
	if len(merged) != 1 || merged[0].Package != testPkgFoo {
		t.Fatalf("normalizeTerms = %+v, want only the foo term (root dropped)", merged)
	}
}

// TestNormalizeTermsKeepsSoleRootTerm pins that a SINGLE positive root term
// is never dropped (it must survive to be recognized by isTerminal).
func TestNormalizeTermsKeepsSoleRootTerm(t *testing.T) {
	t.Parallel()
	terms := []term{{Package: rootPkg, Set: singletonVerSet(rootVersion), Positive: true}}
	merged := normalizeTerms(terms)
	if len(merged) != 1 || merged[0].Package != rootPkg {
		t.Fatalf("normalizeTerms = %+v, want the sole root term preserved", merged)
	}
}

// TestNormalizeTermsDropsTautologicalTerm pins that N({}) is dropped beside
// any other term, while a sole tautological term survives so a single-term
// incompatibility is never emptied.
func TestNormalizeTermsDropsTautologicalTerm(t *testing.T) {
	t.Parallel()
	tautology := term{Package: "bar", Set: emptyVerSet(), Positive: false}
	terms := []term{
		{Package: testPkgFoo, Set: mustSet(t, "^1.0.0"), Positive: true},
		tautology,
	}
	merged := normalizeTerms(terms)
	if len(merged) != 1 || merged[0].Package != testPkgFoo {
		t.Fatalf("normalizeTerms = %+v, want the tautological bar term dropped", merged)
	}
	sole := normalizeTerms([]term{tautology})
	if len(sole) != 1 {
		t.Fatalf("normalizeTerms emptied a single-term incompatibility: %+v", sole)
	}
}
