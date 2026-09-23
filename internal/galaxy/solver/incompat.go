package solver

import (
	"hash/fnv"
	"slices"
)

// cause is the closed sum of reasons an incompatibility exists: every
// variant but causeConflict records an external fact, causeConflict the
// derivation-graph edge conflict resolution produced.
type cause interface {
	isCause()
}

// causeRoot is the initial incompatibility "not root in {0.0.0}".
type causeRoot struct{}

func (causeRoot) isCause() {}

// causeDependency records "Parent@ParentVersion depends on Dep Constraint",
// which produces the two-term incompatibility
// {Parent in {ParentVersion}, not Dep in Constraint}.
type causeDependency struct {
	ParentVersion Version
	Parent        string
	Dep           string
	Constraint    string
}

func (causeDependency) isCause() {}

// causeNoVersions records that decision making found an empty candidate set
// for term's package.
type causeNoVersions struct {
	term term
}

func (causeNoVersions) isCause() {}

// causeUnknownPackage records that the provider reported zero published
// versions for Package.
type causeUnknownPackage struct {
	Package string
}

func (causeUnknownPackage) isCause() {}

// causeConflict is a derived incompatibility's derivation-graph edge: Left
// is the conflicting incompatibility, Right the satisfier's cause, merged by
// one generalized-resolution step.
type causeConflict struct {
	Left  *incompatibility
	Right *incompatibility
}

func (causeConflict) isCause() {}

// incompatibility is a set of terms that cannot all be true in a solution.
// Terms is normalized: at most one term per package, sorted by package
// ascending.
type incompatibility struct {
	Cause cause
	Terms []term
}

// isTerminal reports whether inc's terms are empty, or a single positive
// term referring to the root package - the terminal condition that proves
// no solution exists.
func (inc *incompatibility) isTerminal() bool {
	if len(inc.Terms) == 0 {
		return true
	}
	return len(inc.Terms) == 1 && inc.Terms[0].Positive && inc.Terms[0].Package == rootPkg
}

// termForPackage returns inc's term naming pkg, if any. Incompatibilities
// are normalized to at most one term per package, so this is unambiguous.
func (inc *incompatibility) termForPackage(pkg string) (term, bool) {
	for _, t := range inc.Terms {
		if t.Package == pkg {
			return t, true
		}
	}
	return term{}, false
}

// normalizeTerms merges each package's terms by signed conjunction and sorts
// by package; with two or more left it drops positive root terms (only root
// is selected unconditionally) and then tautological terms.
func normalizeTerms(terms []term) []term {
	byPkg := make(map[string][]term, len(terms))
	order := make([]string, 0, len(terms))
	for _, t := range terms {
		if _, ok := byPkg[t.Package]; !ok {
			order = append(order, t.Package)
		}
		byPkg[t.Package] = append(byPkg[t.Package], t)
	}

	merged := make([]term, 0, len(order))
	for _, pkg := range order {
		merged = append(merged, mergeTermGroup(byPkg[pkg]))
	}

	slices.SortFunc(merged, comparePackageAsc)
	if len(merged) > 1 {
		merged = dropPositiveRoot(merged)
	}
	if len(merged) > 1 {
		merged = dropTautological(merged)
	}
	return merged
}

// dropTautological removes N({}) terms, which are always true: left in a
// learned clause they keep it non-unit after a backjump. A term list that is
// all tautological is returned unchanged rather than emptied.
func dropTautological(terms []term) []term {
	out := terms[:0:0]
	for _, t := range terms {
		if termIsTautological(t) {
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return terms
	}
	return out
}

// mergeTermGroup collapses one package's terms into their signed
// conjunction, the termIntersect fold, keeping a negative result negative
// rather than forcing a polarity.
func mergeTermGroup(group []term) term {
	if len(group) == 1 {
		return group[0]
	}
	merged := group[0]
	for _, t := range group[1:] {
		merged = termIntersect(merged, t)
	}
	return merged
}

// comparePackageAsc orders terms by package name ascending, the canonical
// incompatibility term order normalizeTerms produces.
func comparePackageAsc(a, b term) int {
	switch {
	case a.Package < b.Package:
		return -1
	case a.Package > b.Package:
		return 1
	default:
		return 0
	}
}

// dropPositiveRoot removes any positive term naming the root package from
// terms - sound only once more than one term remains (see normalizeTerms'
// doc comment).
func dropPositiveRoot(terms []term) []term {
	out := make([]term, 0, len(terms))
	for _, t := range terms {
		if t.Positive && t.Package == rootPkg {
			continue
		}
		out = append(out, t)
	}
	return out
}

// incompatStore is the append-only, content-deduplicated incompatibility
// store, indexed per package in append order, which unit propagation's
// newest-first scan relies on.
type incompatStore struct {
	byPkg   map[string][]int
	content map[uint64][]int
	all     []*incompatibility
}

func newIncompatStore() *incompatStore {
	return &incompatStore{
		byPkg:   make(map[string][]int),
		content: make(map[uint64][]int),
	}
}

// add inserts inc unless a stored incompatibility has identical normalized
// terms, returning that one's index instead. The hash only prefilters;
// incompatEqual decides.
func (s *incompatStore) add(inc *incompatibility) (int, bool) {
	h := hashIncompat(inc)
	for _, cand := range s.content[h] {
		if incompatEqual(s.all[cand], inc) {
			return cand, false
		}
	}
	idx := len(s.all)
	s.all = append(s.all, inc)
	s.content[h] = append(s.content[h], idx)
	seenPkg := make(map[string]bool, len(inc.Terms))
	for _, t := range inc.Terms {
		if seenPkg[t.Package] {
			continue
		}
		seenPkg[t.Package] = true
		s.byPkg[t.Package] = append(s.byPkg[t.Package], idx)
	}
	return idx, true
}

// byPackageNewestFirst returns the indices of incompatibilities naming pkg,
// newest (most recently added) first - the scan order unit propagation uses.
func (s *incompatStore) byPackageNewestFirst(pkg string) []int {
	indices := s.byPkg[pkg]
	out := make([]int, len(indices))
	for i, idx := range indices {
		out[len(indices)-1-i] = idx
	}
	return out
}

// incompatEqual reports whether two incompatibilities have identical term
// lists; both are normalized into package order, so positional comparison
// is sound.
func incompatEqual(a, b *incompatibility) bool {
	if len(a.Terms) != len(b.Terms) {
		return false
	}
	for i := range a.Terms {
		if !sameTerm(a.Terms[i], b.Terms[i]) {
			return false
		}
	}
	return true
}

// hashIncompat computes the prefilter hash over an incompatibility's terms;
// a collision costs only an extra incompatEqual, never a dropped entry.
func hashIncompat(inc *incompatibility) uint64 {
	h := fnv.New64a()
	for _, t := range inc.Terms {
		_, _ = h.Write([]byte(t.Package))
		_, _ = h.Write([]byte{0})
		if t.Positive {
			_, _ = h.Write([]byte{1})
		} else {
			_, _ = h.Write([]byte{0})
		}
		t.Set.writeCanonical(h)
		_, _ = h.Write([]byte{0xff})
	}
	return h.Sum64()
}
