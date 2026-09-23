package solver

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// packageIsExactPin reports pkg's exact pin: before the universe fetch, a
// positive accumulation that is a singleton (it keeps the registry spelling
// the decision keys on); after it, one published candidate left.
func (s *solveState) packageIsExactPin(pkg string) (Version, bool) {
	p := s.ps.pkgState(pkg)
	u := s.uniFor(pkg)
	if u.fetched {
		only := Version{}
		count := 0
		for _, v := range u.versions {
			if termPermits(p.accum, v) {
				only = v
				count++
			}
		}
		if count == 1 {
			return only, true
		}
		return Version{}, false
	}
	if !p.accum.Positive {
		return Version{}, false
	}
	if v, ok := p.accum.Set.decidedVersion(); ok {
		return v, true
	}
	return Version{}, false
}

// versionPassesAccum reports whether v satisfies everything currently known
// about pkg: a single signed membership test against the accumulation,
// which is the exact conjunction of every assignment on record.
func (s *solveState) versionPassesAccum(pkg string, v Version) bool {
	return termPermits(s.ps.pkgState(pkg).accum, v)
}

// probeWorthTrying reports whether the provider.Highest probe may settle pkg
// without fetching its universe: pkg needs a positive requirement, since a
// negative-only accumulation never asserts selection.
func (s *solveState) probeWorthTrying(pkg string) bool {
	p := s.ps.pkgState(pkg)
	return len(p.indices) > 0 && p.accum.Positive
}

// candidatePackages returns, sorted by name, every undecided package with a
// positive accumulation, which flips positive on the first positive
// assignment and never back: the pool pickPackage chooses from.
func (s *solveState) candidatePackages() []string {
	names := make([]string, 0, len(s.ps.packages))
	for pkg, p := range s.ps.packages {
		if p.decisionIdx != -1 || len(p.indices) == 0 {
			continue
		}
		if p.accum.Positive {
			names = append(names, pkg)
		}
	}
	slices.Sort(names)
	return names
}

// pickPackage chooses the next package by the frozen priority: exact pins,
// then highest conflict count, then fetched packages with the fewest allowed
// candidates, then the rest, ties broken by candidatePackages' name order.
func (s *solveState) pickPackage() (string, bool) {
	names := s.candidatePackages()
	if len(names) == 0 {
		return "", false
	}

	for _, n := range names {
		if _, ok := s.packageIsExactPin(n); ok {
			return n, true
		}
	}

	if pkg, ok := s.pickByConflictCount(names); ok {
		return pkg, true
	}

	if pkg, ok := s.pickByFewestCandidates(names); ok {
		return pkg, true
	}

	return names[0], true
}

func (s *solveState) pickByConflictCount(names []string) (string, bool) {
	best := ""
	bestCount := 0
	for _, n := range names {
		c := s.conflictCounts[n]
		if c > bestCount {
			best = n
			bestCount = c
		}
	}
	return best, best != ""
}

func (s *solveState) pickByFewestCandidates(names []string) (string, bool) {
	best := ""
	bestCount := -1
	for _, n := range names {
		u := s.uniFor(n)
		if !u.fetched {
			continue
		}
		c := s.countAllowed(n, u)
		if bestCount == -1 || c < bestCount {
			best = n
			bestCount = c
		}
	}
	return best, best != ""
}

// countAllowed counts pkg's published versions its accumulation permits.
func (s *solveState) countAllowed(pkg string, u *packageUniverse) int {
	accum := s.ps.pkgState(pkg).accum
	count := 0
	for _, v := range u.versions {
		if termPermits(accum, v) {
			count++
		}
	}
	return count
}

// decisionOutcome carries makeDecision's early-exit result: a fast-path
// helper returns a non-nil outcome when the caller should return it as-is,
// or nil when the caller should fall through to decideFromAllowed instead.
type decisionOutcome struct {
	err  error
	pkg  string
	done bool
}

// makeDecision picks a package and decides it, via the exact-pin and probe
// fast paths when they apply. An empty pool means the solve is complete: a
// decided parent's requirement is positive, so it is pooled or decided.
func (s *solveState) makeDecision(ctx context.Context) (string, bool, error) {
	pkg, ok := s.pickPackage()
	if !ok {
		return "", true, nil
	}
	if out := s.tryFastDecide(ctx, pkg); out != nil {
		return out.pkg, out.done, out.err
	}
	return s.decideFromAllowed(ctx, pkg)
}

// tryFastDecide tries the exact-pin then the provider.Highest probe fast
// path, fetching the universe when neither can be confirmed cheaply; nil
// means fall through to decideFromAllowed.
func (s *solveState) tryFastDecide(ctx context.Context, pkg string) *decisionOutcome {
	u := s.uniFor(pkg)
	if vp, isPin := s.packageIsExactPin(pkg); isPin {
		return s.tryDecidePin(ctx, pkg, u, vp)
	}
	if !u.fetched && s.probeWorthTrying(pkg) {
		return s.tryDecideByProbe(ctx, pkg)
	}
	if !u.fetched {
		if err := s.ensureUniverse(ctx, pkg); err != nil {
			return &decisionOutcome{err: err}
		}
	}
	return nil
}

// tryDecidePin decides pkg at its pin vp when the universe is fetched or vp
// passes the accumulation, else fetches the universe so decideFromAllowed
// re-verifies it; nil means fall through.
func (s *solveState) tryDecidePin(ctx context.Context, pkg string, u *packageUniverse, vp Version) *decisionOutcome {
	if u.fetched || s.versionPassesAccum(pkg, vp) {
		pkg, done, err := s.decideVersion(ctx, pkg, vp)
		return &decisionOutcome{pkg: pkg, done: done, err: err}
	}
	if err := s.ensureUniverse(ctx, pkg); err != nil {
		return &decisionOutcome{err: err}
	}
	return nil
}

// tryDecideByProbe decides pkg at the provider.Highest version when it
// passes the accumulation, else fetches the universe without deciding; nil
// means fall through to decideFromAllowed.
func (s *solveState) tryDecideByProbe(ctx context.Context, pkg string) *decisionOutcome {
	v, ok, err := s.provider.Highest(ctx, pkg)
	if err != nil {
		return &decisionOutcome{err: fmt.Errorf("probing highest version of %s: %w", pkg, err)}
	}
	if ok && s.versionPassesAccum(pkg, v) {
		decidedPkg, done, decErr := s.decideVersion(ctx, pkg, v)
		return &decisionOutcome{pkg: decidedPkg, done: done, err: decErr}
	}
	if err := s.ensureUniverse(ctx, pkg); err != nil {
		return &decisionOutcome{err: err}
	}
	return nil
}

// decideFromAllowed decides the highest allowed version, or records an
// unknown-package or no-versions incompatibility whose term is the exact
// region every assignment jointly permits, so the leaf names what is unmet.
func (s *solveState) decideFromAllowed(ctx context.Context, pkg string) (string, bool, error) {
	p := s.ps.pkgState(pkg)
	u := s.uniFor(pkg)

	for _, v := range u.versions {
		if termPermits(p.accum, v) {
			return s.decideVersion(ctx, pkg, v)
		}
	}

	if len(u.versions) == 0 {
		s.store.add(&incompatibility{
			Terms: []term{{Package: pkg, Set: fullVerSet(), Positive: true}},
			Cause: causeUnknownPackage{Package: pkg},
		})
		return pkg, false, nil
	}
	noVersionsTerm := term{Package: pkg, Set: positiveFormSet(p.accum), Positive: true}
	s.store.add(&incompatibility{Terms: []term{noVersionsTerm}, Cause: causeNoVersions{term: noVersionsTerm}})
	return pkg, false, nil
}

// positiveFormSet returns the set of versions accum actually permits, as a
// plain positive set: the accumulation's own set when it is positive, its
// complement otherwise.
func positiveFormSet(accum term) verSet {
	if accum.Positive {
		return accum.Set
	}
	return accum.Set.complement()
}

// decideVersion is PubGrub's DECIDE(v): add v's dependency incompatibilities,
// then record the decision unless one of them would already be satisfied
// against it (the conservative decision-time conflict check).
func (s *solveState) decideVersion(ctx context.Context, pkg string, v Version) (string, bool, error) {
	newIdx, err := s.dependencyIncompatibilities(ctx, pkg, v)
	if err != nil {
		return "", false, err
	}
	for _, idx := range newIdx {
		if relateTentative(s.store.all[idx], s.ps, pkg, v) == incSatisfied {
			return pkg, false, nil
		}
	}
	s.ps.decide(pkg, v)
	return pkg, false, nil
}

// relateTentative evaluates relate() as if pkg were decided at v, without
// mutating the partial solution: pkg's term by exact membership, every other
// term through relation().
func relateTentative(inc *incompatibility, ps *partialSolution, pkg string, v Version) incRelation {
	hasUnsat := false
	for _, t := range inc.Terms {
		var r termRelation
		if t.Package == pkg {
			if termPermits(t, v) {
				r = termSatisfied
			} else {
				r = termContradicted
			}
		} else {
			r = relation(t, ps)
		}
		switch r {
		case termContradicted:
			return incContradicted
		case termInconclusive:
			if hasUnsat {
				return incInconclusive
			}
			hasUnsat = true
		case termSatisfied:
			// Continue scanning the remaining terms.
		}
	}
	if !hasUnsat {
		return incSatisfied
	}
	return incAlmostSatisfied
}

// dependencyIncompatibilities adds pkg@v's dependency incompatibilities once
// per (pkg, v) and returns their indices. An empty-set constraint becomes the
// single term {parent@v}, since no stored term may be tautological.
func (s *solveState) dependencyIncompatibilities(ctx context.Context, pkg string, v Version) ([]int, error) {
	key := pkg + "\x00" + v.Original()
	if s.depsAdded[key] {
		return nil, nil
	}

	deps, err := s.dependenciesOf(ctx, pkg, v)
	if err != nil {
		return nil, err
	}
	s.depsAdded[key] = true

	names := slices.Sorted(maps.Keys(deps))

	parentTerm := term{Package: pkg, Set: singletonVerSet(v), Positive: true}
	added := make([]int, 0, len(names))
	for _, dep := range names {
		constraint := deps[dep]
		set, err := newVerSet(constraint)
		if err != nil {
			return nil, fmt.Errorf("invalid dependency constraint %q for %s -> %s: %w", constraint, pkg, dep, err)
		}
		terms := []term{parentTerm, {Package: dep, Set: set, Positive: false}}
		if set.isEmpty() {
			terms = []term{parentTerm}
		}
		inc := &incompatibility{
			Terms: terms,
			Cause: causeDependency{Parent: pkg, ParentVersion: v, Dep: dep, Constraint: constraint},
		}
		idx, _ := s.store.add(inc)
		added = append(added, idx)
	}
	return added, nil
}

// dependenciesOf returns pkg@v's dependencies: root's are the solve's root
// requirements, injected without a provider call.
func (s *solveState) dependenciesOf(ctx context.Context, pkg string, v Version) (map[string]Constraint, error) {
	if pkg == rootPkg {
		return s.rootDeps, nil
	}
	deps, err := s.provider.Dependencies(ctx, pkg, v)
	if err != nil {
		return nil, fmt.Errorf("fetching dependencies of %s@%s: %w", pkg, v.Original(), err)
	}
	return deps, nil
}
