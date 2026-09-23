package solver

// Terms are signed statements over exact verSet values, so their arithmetic
// is exact and never needs the published universe. The sign matters: a
// negative-only accumulation leaves a package undetermined, not settled.

import (
	"slices"
)

// term pairs a package name with a signed exact version set.
type term struct {
	Package  string
	Set      verSet
	Positive bool
}

// Negate returns the logical negation of t. It only flips Positive; the
// underlying set is never touched (complementing a set is a distinct
// operation, used via the signed intersection below).
func (t term) Negate() term {
	return term{Package: t.Package, Set: t.Set, Positive: !t.Positive}
}

// accumSeed is the identity of signed intersection for pkg: "not (pkg in
// {})", the vacuously true statement every package's accumulation starts
// from.
func accumSeed(pkg string) term {
	return term{Package: pkg, Set: emptyVerSet(), Positive: false}
}

// termIntersect returns the conjunction of two terms on one package: P&P is
// P(a&b), P&N is P(a-b), N&P is P(b-a), N&N is N(a|b). The identity N({})
// returns the other term verbatim, keeping its display carriers.
func termIntersect(a, b term) term {
	if !a.Positive && a.Set.isEmpty() {
		return b
	}
	if !b.Positive && b.Set.isEmpty() {
		return a
	}
	switch {
	case a.Positive && b.Positive:
		return term{Package: a.Package, Set: a.Set.intersect(b.Set), Positive: true}
	case a.Positive:
		return term{Package: a.Package, Set: a.Set.difference(b.Set), Positive: true}
	case b.Positive:
		return term{Package: a.Package, Set: b.Set.difference(a.Set), Positive: true}
	default:
		return term{Package: a.Package, Set: a.Set.union(b.Set), Positive: false}
	}
}

// termSubset reports whether a entails b: P(a) entails P(b) iff a is a subset
// of b, P(a) entails N(b) iff they are disjoint, N(a) entails N(b) iff b is a
// subset of a, and a negative term never entails a positive one.
func termSubset(a, b term) bool {
	switch {
	case a.Positive && b.Positive:
		return a.Set.subsetOf(b.Set)
	case a.Positive:
		return a.Set.disjointFrom(b.Set)
	case b.Positive:
		return false
	default:
		return b.Set.subsetOf(a.Set)
	}
}

// relateAccum reports whether accum satisfies t, contradicts it (their
// conjunction is P({})) or is inconclusive. A negative accumulation never
// contradicts a negative term, which keeps dependency terms derivable.
func relateAccum(accum, t term) termRelation {
	if termSubset(accum, t) {
		return termSatisfied
	}
	if joint := termIntersect(accum, t); joint.Positive && joint.Set.isEmpty() {
		return termContradicted
	}
	return termInconclusive
}

// termPermits reports whether selecting v would keep t true.
func termPermits(t term, v Version) bool {
	return t.Set.contains(v) == t.Positive
}

// termIsTautological reports whether t is exactly N({}). A positive term never
// is: even P(full) asserts that the package is selected at all.
func termIsTautological(t term) bool {
	return !t.Positive && t.Set.isEmpty()
}

// sameVersionSet reports whether two sets denote the same set of versions.
// Exact sets make this total: canonical structural equality is set
// equality, with no representation caveats.
func sameVersionSet(a, b verSet) bool {
	return a.equalSet(b)
}

// sameTerm reports whether two terms are content-identical: same package,
// same polarity, same set.
func sameTerm(a, b term) bool {
	return a.Package == b.Package && a.Positive == b.Positive && sameVersionSet(a.Set, b.Set)
}

// compareVersionsDescending is the universe total order: semver precedence
// descending, then original string descending, so "the highest version" is
// unambiguous between equal-precedence spellings such as "1.0.0+build".
func compareVersionsDescending(a, b Version) int {
	if c := b.sv().Compare(a.sv()); c != 0 {
		return c
	}
	switch {
	case a.original > b.original:
		return -1
	case a.original < b.original:
		return 1
	default:
		return 0
	}
}

// buildUniverse deduplicates versions by original string and sorts the
// result into the universe total order (descending).
func buildUniverse(versions []Version) []Version {
	seen := make(map[string]bool, len(versions))
	out := make([]Version, 0, len(versions))
	for _, v := range versions {
		if seen[v.original] {
			continue
		}
		seen[v.original] = true
		out = append(out, v)
	}
	slices.SortFunc(out, compareVersionsDescending)
	return out
}

// packageUniverse holds one package's published version list once fetched.
// It plays no part in relating terms or resolving conflicts; only decision
// making and error reporting read it.
type packageUniverse struct {
	versions []Version
	fetched  bool
}

// newPackageUniverse returns an unfetched universe placeholder.
func newPackageUniverse() *packageUniverse {
	return &packageUniverse{}
}

// setVersions installs versions (already sorted into total order) as u's
// universe and marks it fetched.
func (u *packageUniverse) setVersions(versions []Version) {
	u.versions = versions
	u.fetched = true
}
