package solver

import "fmt"

// assignment is one entry of the partial solution: a decision (CauseIndex
// -1) or a derivation whose CauseIndex points at the forcing incompatibility.
type assignment struct {
	term          term
	DecisionLevel int
	CauseIndex    int
	Index         int
}

// isDecision reports whether a is a decision rather than a derivation.
func (a *assignment) isDecision() bool {
	return a.CauseIndex == -1
}

// decisionVersionOf extracts the version a decision's singletonVerSet term
// carries; a term carrying none is an invariant violation, reported as an
// error wrapping errSolverBug.
func decisionVersionOf(t term) (Version, error) {
	v, ok := t.Set.decidedVersion()
	if !ok {
		return Version{}, fmt.Errorf("decision term for %q does not carry a singleton version: %w", t.Package, errSolverBug)
	}
	return v, nil
}

// packageAssignments is one package's bookkeeping: its assignment indices,
// its decision if any, and accum, the signed conjunction of all its terms,
// which is exact whether or not the package's universe was fetched.
type packageAssignments struct {
	decisionVersion Version
	indices         []int32
	accum           term
	decisionIdx     int32
}

// partialSolution is Pubgrub's ordered assignment list plus the per-package
// indices and signed accumulations that make propagation and decision
// making efficient.
type partialSolution struct {
	packages    map[string]*packageAssignments
	assignments []assignment
	decisions   int
}

func newPartialSolution() *partialSolution {
	return &partialSolution{
		packages: make(map[string]*packageAssignments),
	}
}

// currentLevel returns the current decision level: the number of real
// (non-root) decisions made so far.
func (ps *partialSolution) currentLevel() int {
	return ps.decisions
}

func (ps *partialSolution) pkgState(pkg string) *packageAssignments {
	p, ok := ps.packages[pkg]
	if !ok {
		p = &packageAssignments{decisionIdx: -1, accum: accumSeed(pkg)}
		ps.packages[pkg] = p
	}
	return p
}

// append adds one assignment and folds its term into the package's
// accumulation; it is the single mutation point decide and derive share.
func (ps *partialSolution) append(term term, level, causeIndex int) *assignment {
	idx := len(ps.assignments)
	ps.assignments = append(ps.assignments, assignment{
		term:          term,
		DecisionLevel: level,
		CauseIndex:    causeIndex,
		Index:         idx,
	})
	a := &ps.assignments[idx]
	p := ps.pkgState(term.Package)
	//nolint:gosec // G115: assignment indices are small, bounded by the fuel limit
	p.indices = append(p.indices, int32(idx))
	if causeIndex == -1 {
		p.decisionIdx = int32(idx) //nolint:gosec // G115: bounded by the fuel limit
	}
	p.accum = termIntersect(p.accum, term)
	return a
}

// decide records pkg@v as a decision. Root is always decided at level 0 and
// never advances the decision counter, so the first real decision is level 1.
func (ps *partialSolution) decide(pkg string, v Version) {
	level := ps.decisions
	if pkg != rootPkg {
		ps.decisions++
		level = ps.decisions
	}
	term := term{Package: pkg, Set: singletonVerSet(v), Positive: true}
	ps.append(term, level, -1)
	ps.pkgState(pkg).decisionVersion = v
}

// derive records term as a derivation caused by the incompatibility at
// causeIndex, at the current decision level.
func (ps *partialSolution) derive(term term, causeIndex int) *assignment {
	return ps.append(term, ps.decisions, causeIndex)
}

// hasEquivalentAssignment reports whether term's package already carries an
// identical assignment; re-deriving it could never change relation(), so
// skipping it is sound and stops a no-progress loop.
func (ps *partialSolution) hasEquivalentAssignment(term term) bool {
	p, ok := ps.packages[term.Package]
	if !ok {
		return false
	}
	for _, idx := range p.indices {
		if sameTerm(ps.assignments[idx].term, term) {
			return true
		}
	}
	return false
}

// rebuildPackageAssignments replays ps.assignments into fresh per-package
// bookkeeping for backtrackTo. On a decisionVersionOf failure it returns a
// nil map, so a map built from a prefix never reaches a caller.
func (ps *partialSolution) rebuildPackageAssignments() (map[string]*packageAssignments, error) {
	rebuilt := make(map[string]*packageAssignments, len(ps.packages))
	for i := range ps.assignments {
		a := &ps.assignments[i]
		p, ok := rebuilt[a.term.Package]
		if !ok {
			p = &packageAssignments{decisionIdx: -1, accum: accumSeed(a.term.Package)}
			rebuilt[a.term.Package] = p
		}
		p.indices = append(p.indices, int32(i))
		p.accum = termIntersect(p.accum, a.term)
		if a.CauseIndex == -1 {
			p.decisionIdx = int32(i)
			v, err := decisionVersionOf(a.term)
			if err != nil {
				return nil, err
			}
			p.decisionVersion = v
		}
	}
	return rebuilt, nil
}

// backtrackTo drops every assignment above level and rebuilds all package
// bookkeeping by replay, which is microseconds at Galaxy scale. On error the
// assignments are already truncated: the caller must abandon the solution.
func (ps *partialSolution) backtrackTo(level int) error {
	cut := len(ps.assignments)
	for cut > 0 && ps.assignments[cut-1].DecisionLevel > level {
		cut--
	}
	ps.assignments = ps.assignments[:cut]
	ps.decisions = level

	rebuilt, err := ps.rebuildPackageAssignments()
	if err != nil {
		return err
	}
	ps.packages = rebuilt
	return nil
}
