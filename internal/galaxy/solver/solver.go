package solver

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// fuelLimit bounds the main loop: the algorithm terminates by construction,
// so exceeding it means a solver defect, not a large input.
//
//nolint:gochecknoglobals // deliberately a var, not a const, so in-package tests can lower it to exercise the fuel guard
var fuelLimit = 1_000_000

// errSolverBug is wrapped by every violated invariant of the algorithm's own
// bookkeeping, so a defect returns through the caller's error handling rather
// than panicking; only an init-time constant (mustNewVersion) may panic.
var errSolverBug = errors.New("solver: internal invariant violated")

// solveState is one Solve call's mutable state: incompatibility store, partial
// solution, per-package universes and decision bookkeeping. It is never shared
// across goroutines and never persisted between calls.
type solveState struct {
	provider       Provider
	store          *incompatStore
	ps             *partialSolution
	universes      map[string]*packageUniverse
	conflictCounts map[string]int
	depsAdded      map[string]bool
	rootDeps       map[string]Constraint
}

// Solve resolves reqs against p, or returns a *ConflictError when no selection
// exists; other errors are wrapped provider failures or errSolverBug. ctx is
// checked once per iteration, and a cancellation seen there returns ctx.Err().
func Solve(ctx context.Context, reqs []Requirement, p Provider) (*Result, error) {
	s := newSolveState(p, reqs)

	rootInc := &incompatibility{
		Terms: []term{{Package: rootPkg, Set: singletonVerSet(rootVersion), Positive: false}},
		Cause: causeRoot{},
	}
	s.store.add(rootInc)

	next := rootPkg
	for fuel := 0; ; fuel++ {
		if fuel > fuelLimit {
			return nil, fmt.Errorf("iteration limit exceeded: %w", errSolverBug)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.unitPropagation(next); err != nil {
			return nil, err
		}
		pkg, done, err := s.makeDecision(ctx)
		if err != nil {
			return nil, err
		}
		if done {
			return s.extractResult()
		}
		next = pkg
	}
}

// newSolveState builds a fresh solve state, pre-populating the synthetic
// root package's universe (the constant {0.0.0}) so the core never calls
// the provider for it.
func newSolveState(p Provider, reqs []Requirement) *solveState {
	rootDeps := make(map[string]Constraint, len(reqs))
	for _, r := range reqs {
		rootDeps[r.Package] = r.Constraint
	}

	s := &solveState{
		provider:       p,
		store:          newIncompatStore(),
		universes:      make(map[string]*packageUniverse),
		conflictCounts: make(map[string]int),
		depsAdded:      make(map[string]bool),
		rootDeps:       rootDeps,
	}
	s.ps = newPartialSolution()

	rootUni := newPackageUniverse()
	rootUni.setVersions([]Version{rootVersion})
	s.universes[rootPkg] = rootUni

	return s
}

// uniFor returns pkg's published-universe record, lazily creating an
// unfetched placeholder for a package that has never been touched yet.
func (s *solveState) uniFor(pkg string) *packageUniverse {
	u, ok := s.universes[pkg]
	if !ok {
		u = newPackageUniverse()
		s.universes[pkg] = u
	}
	return u
}

// ensureUniverse fetches pkg's published version list once, never for the
// synthetic root. Term arithmetic is exact without it, so only decision making
// and error-report cosmetics read the list.
func (s *solveState) ensureUniverse(ctx context.Context, pkg string) error {
	if pkg == rootPkg {
		return nil
	}
	u := s.uniFor(pkg)
	if u.fetched {
		return nil
	}
	versions, err := s.provider.Universe(ctx, pkg)
	if err != nil {
		return fmt.Errorf("fetching version universe for %s: %w", pkg, err)
	}
	u.setVersions(buildUniverse(versions))
	return nil
}

// extractResult builds the Result from the decisions reachable from root and
// fails with errSolverBug when an edge targets an undecided package, rather
// than returning an incomplete resolution that would under-install.
func (s *solveState) extractResult() (*Result, error) {
	decidedVer := make(map[string]Version, len(s.ps.packages))
	decidedDeps := make(map[string][]string, len(s.ps.packages))
	for pkg, p := range s.ps.packages {
		if p.decisionIdx == -1 {
			continue
		}
		decidedVer[pkg] = p.decisionVersion
		decidedDeps[pkg] = s.decidedDependencyNames(pkg, p.decisionVersion)
	}

	// Only the closure reachable from root is returned: a decision left from a
	// backtracked branch may have no path from any requirement, and including
	// it would install a package nothing depends on.
	reachable := make(map[string]bool, len(decidedVer))
	stack := []string{rootPkg}
	for len(stack) > 0 {
		pkg := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if reachable[pkg] {
			continue
		}
		reachable[pkg] = true
		stack = append(stack, decidedDeps[pkg]...)
	}

	versions := make(Resolution, len(reachable))
	graph := make(map[string][]string, len(reachable))
	for pkg := range reachable {
		if pkg == rootPkg {
			continue
		}
		v, ok := decidedVer[pkg]
		if !ok {
			return nil, fmt.Errorf("resolution reaches an undecided package %q: %w", pkg, errSolverBug)
		}
		versions[pkg] = v.Original()
		graph[pkg] = decidedDeps[pkg]
	}

	return &Result{Versions: versions, Graph: graph}, nil
}

// decidedDependencyNames returns the sorted dependency package names
// recorded by pkg@v's causeDependency incompatibilities.
func (s *solveState) decidedDependencyNames(pkg string, v Version) []string {
	names := make([]string, 0)
	for _, inc := range s.store.all {
		dep, ok := inc.Cause.(causeDependency)
		if !ok {
			continue
		}
		if dep.Parent != pkg || dep.ParentVersion.Original() != v.Original() {
			continue
		}
		names = append(names, dep.Dep)
	}
	slices.Sort(names)
	return names
}
