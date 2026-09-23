package solver

import (
	"fmt"
)

// resolveConflict resolves the conflict in the incompatibility at startIdx,
// backtracks, and returns the (index, incompatibility) unit propagation
// continues from, or a *ConflictError when no solution can exist.
func (s *solveState) resolveConflict(startIdx int) (int, *incompatibility, error) {
	inc := s.store.all[startIdx]
	curIdx := startIdx

	// Counted once per call, for the packages of the original conflicting
	// incompatibility only, not for the derived ones the loop generalizes to.
	for _, t := range inc.Terms {
		s.conflictCounts[t.Package]++
	}

	incChanged := false
	// A defensive cap: resolution terminates by construction once it reaches
	// root or a backjump point, so correct input never comes near this bound.
	for guard := range 10_000 {
		_ = guard
		if inc.isTerminal() {
			return 0, nil, s.buildConflictError(inc)
		}

		satisfier, term := s.earliestSatisfier(inc)
		if satisfier == nil {
			// Signed exact terms guarantee a satisfier for a genuinely
			// satisfied incompatibility (the empty prefix satisfies no term),
			// so its absence is a defect, not a proof.
			return 0, nil, fmt.Errorf("no satisfier found for a satisfied incompatibility: %w", errSolverBug)
		}
		prevLevel := s.prevSatisfierLevel(inc, satisfier)

		if shouldBackjump(satisfier, prevLevel) {
			return s.backjump(inc, curIdx, incChanged, prevLevel)
		}

		inc = s.mergeWithSatisfierCause(inc, satisfier, term)
		incChanged = true
	}
	return 0, nil, s.buildConflictError(inc)
}

// shouldBackjump is the reference PubGrub rule: backjump when the satisfier
// is a decision or sits at a different level than its previous satisfier.
// Signed exact terms need no special case for no-versions leaves.
func shouldBackjump(satisfier *assignment, prevLevel int) bool {
	return satisfier.isDecision() || prevLevel != satisfier.DecisionLevel
}

// prevSatisfierLevel returns the decision level of the earliest assignment
// before satisfier that, with satisfier pinned in, still satisfies inc, or 0
// when satisfier alone suffices.
func (s *solveState) prevSatisfierLevel(inc *incompatibility, satisfier *assignment) int {
	prevSatisfier := s.earliestSatisfierBefore(inc, satisfier)
	if prevSatisfier == nil {
		return 0
	}
	return prevSatisfier.DecisionLevel
}

// backjump stores inc when this resolveConflict call derived it, backtracks
// to prevLevel, and returns the (index, incompatibility) pair unit
// propagation continues from.
func (s *solveState) backjump(inc *incompatibility, curIdx int, incChanged bool, prevLevel int) (int, *incompatibility, error) {
	if incChanged {
		idx, _ := s.store.add(inc)
		curIdx = idx
	}
	if err := s.ps.backtrackTo(prevLevel); err != nil {
		return 0, nil, err
	}
	return curIdx, inc, nil
}

// mergeWithSatisfierCause is one generalized-resolution step: it merges inc
// with the satisfier's cause minus the satisfier's package, adding the
// partial-satisfier correction term when the satisfier alone falls short.
func (s *solveState) mergeWithSatisfierCause(inc *incompatibility, satisfier *assignment, satisfierTerm term) *incompatibility {
	cause := s.store.all[satisfier.CauseIndex]
	prior := mergeTermsExcluding(inc, cause, satisfier.term.Package)
	if !termSubset(satisfier.term, satisfierTerm) {
		prior = append(prior, negatedDifferenceTerm(satisfier.term, satisfierTerm))
	}

	return &incompatibility{
		Terms: normalizeTerms(prior),
		Cause: causeConflict{Left: inc, Right: cause},
	}
}

// mergeTermsExcluding collects the terms of a and b except those naming
// exclude: the "priorCause" step of generalized resolution, left for
// normalizeTerms to merge duplicate packages and drop redundant root terms.
func mergeTermsExcluding(a, b *incompatibility, exclude string) []term {
	collected := make([]term, 0, len(a.Terms)+len(b.Terms))
	for _, t := range a.Terms {
		if t.Package != exclude {
			collected = append(collected, t)
		}
	}
	for _, t := range b.Terms {
		if t.Package != exclude {
			collected = append(collected, t)
		}
	}
	return collected
}

// negatedDifferenceTerm returns "not (satisfierTerm minus incTerm)", the
// partial-satisfier correction term, computed as the negated signed
// conjunction of satisfierTerm with incTerm's negation.
func negatedDifferenceTerm(satisfierTerm, incTerm term) term {
	return termIntersect(satisfierTerm, incTerm.Negate()).Negate()
}

// computeFirstSatisfied maps each package of inc to the first index below
// limit whose prefix accumulation satisfies inc's term for it; -1 means the
// non-nil seed alone satisfies it, and an unsatisfied package is absent.
func (s *solveState) computeFirstSatisfied(inc *incompatibility, seed *term, limit int) map[string]int {
	firstIdx := make(map[string]int, len(inc.Terms))
	done := make(map[string]bool, len(inc.Terms))
	running := make(map[string]term, len(inc.Terms))
	for _, t := range inc.Terms {
		acc := accumSeed(t.Package)
		if seed != nil && seed.Package == t.Package {
			acc = termIntersect(acc, *seed)
			if termSubset(acc, t) {
				firstIdx[t.Package] = -1
				done[t.Package] = true
			}
		}
		running[t.Package] = acc
	}

	for i := range limit {
		a := &s.ps.assignments[i]
		t, ok := inc.termForPackage(a.term.Package)
		if !ok || done[t.Package] {
			continue
		}
		acc := termIntersect(running[t.Package], a.term)
		running[t.Package] = acc
		if termSubset(acc, t) {
			firstIdx[t.Package] = i
			done[t.Package] = true
		}
	}
	return firstIdx
}

// earliestSatisfier finds the earliest assignment such that the partial
// solution up to and including it satisfies inc, plus inc's term for the
// same package.
func (s *solveState) earliestSatisfier(inc *incompatibility) (*assignment, term) {
	firstIdx := s.computeFirstSatisfied(inc, nil, len(s.ps.assignments))
	maxIdx := -1
	maxPkg := ""
	for _, t := range inc.Terms {
		idx, ok := firstIdx[t.Package]
		if !ok {
			idx = -1
		}
		if idx > maxIdx {
			maxIdx = idx
			maxPkg = t.Package
		}
	}
	term, _ := inc.termForPackage(maxPkg)
	if maxIdx < 0 {
		return nil, term
	}
	return &s.ps.assignments[maxIdx], term
}

// earliestSatisfierBefore finds the earliest assignment before satisfier
// whose prefix, with satisfier pinned in, satisfies inc, or nil when
// satisfier alone suffices.
func (s *solveState) earliestSatisfierBefore(inc *incompatibility, satisfier *assignment) *assignment {
	if satisfier == nil {
		return nil
	}
	firstIdx := s.computeFirstSatisfied(inc, &satisfier.term, satisfier.Index)
	maxIdx := -1
	for _, t := range inc.Terms {
		idx, ok := firstIdx[t.Package]
		if !ok {
			idx = -1
		}
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	if maxIdx < 0 {
		return nil
	}
	return &s.ps.assignments[maxIdx]
}
