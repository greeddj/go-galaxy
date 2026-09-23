package solver

import (
	"slices"
)

// termRelation is the three-valued result of relating a single term to the
// partial solution.
type termRelation uint8

const (
	termInconclusive termRelation = iota
	termSatisfied
	termContradicted
)

// incRelation is the four-valued result of relating a whole incompatibility
// to the partial solution.
type incRelation uint8

const (
	incInconclusive incRelation = iota
	incSatisfied
	incAlmostSatisfied
	incContradicted
)

// relation relates term to the partial solution from its exact signed
// accumulation, never a published universe. A package with no assignment is
// inconclusive: its vacuous N({}) seed must not satisfy a negative term.
func relation(term term, ps *partialSolution) termRelation {
	p, ok := ps.packages[term.Package]
	if !ok || len(p.indices) == 0 {
		return termInconclusive
	}
	return relateAccum(p.accum, term)
}

// relate relates inc to the partial solution; when inc is almost satisfied
// (one inconclusive term, the rest satisfied) that term is returned too.
func relate(inc *incompatibility, ps *partialSolution) (incRelation, term) {
	var unsat term
	hasUnsat := false
	for _, t := range inc.Terms {
		switch relation(t, ps) {
		case termContradicted:
			return incContradicted, term{}
		case termInconclusive:
			if hasUnsat {
				return incInconclusive, term{}
			}
			unsat = t
			hasUnsat = true
		case termSatisfied:
			// Continue scanning the remaining terms.
		}
	}
	if !hasUnsat {
		return incSatisfied, term{}
	}
	return incAlmostSatisfied, unsat
}

// unitPropagation derives assignments from pkg's incompatibilities until
// none remain, resolving conflicts as found. It pops changed packages in name
// order and does no I/O: the provider is reached only from decision making.
func (s *solveState) unitPropagation(pkg string) error {
	changed := map[string]bool{pkg: true}
	for len(changed) > 0 {
		p := popSmallest(changed)
		if _, err := s.propagatePackage(p, changed); err != nil {
			return err
		}
	}
	return nil
}

// propagatePackage scans p's incompatibilities newest first. On a satisfied
// one it resolves the conflict and returns conflicted = true, since the
// backtrack left the rest of this scan stale.
func (s *solveState) propagatePackage(p string, changed map[string]bool) (bool, error) {
	for _, idx := range s.store.byPackageNewestFirst(p) {
		inc := s.store.all[idx]
		rel, unsat := relate(inc, s.ps)
		switch rel {
		case incSatisfied:
			if err := s.resolveAndDerive(idx, changed); err != nil {
				return false, err
			}
			return true, nil
		case incAlmostSatisfied:
			s.deriveOnce(unsat.Negate(), idx, changed)
		case incContradicted, incInconclusive:
			// Nothing to derive from this incompatibility right now.
		}
	}
	return false, nil
}

// deriveOnce derives term and marks its package changed unless an identical
// assignment already exists, a sound skip that guards against re-derivation.
func (s *solveState) deriveOnce(term term, causeIdx int, changed map[string]bool) {
	if s.ps.hasEquivalentAssignment(term) {
		return
	}
	s.ps.derive(term, causeIdx)
	changed[term.Package] = true
}

// resolveAndDerive resolves the conflict at idx and derives the negation of
// the almost-satisfied term of the returned root cause. Any other relation is
// a defect, ended as a clean failure (TestNonConvergingConflictIsCleanFailure).
func (s *solveState) resolveAndDerive(idx int, changed map[string]bool) error {
	rootIdx, rootCause, err := s.resolveConflict(idx)
	if err != nil {
		return err
	}
	rel, t := relate(rootCause, s.ps)
	if rel != incAlmostSatisfied {
		return s.buildConflictError(rootCause)
	}
	clear(changed)
	s.deriveOnce(t.Negate(), rootIdx, changed)
	return nil
}

// popSmallest removes and returns the lexicographically smallest key from
// set, giving unit propagation's changed-set processing a deterministic,
// total-order pop sequence.
func popSmallest(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	smallest := slices.Min(keys)
	delete(set, smallest)
	return smallest
}
