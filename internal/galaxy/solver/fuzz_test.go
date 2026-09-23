package solver

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// fuzzMaxPackages bounds the package count a fuzz input can decode to: large
// enough to let the fuzzer build a real multi-hop dependency chain, small
// enough that a single corpus entry never dominates the fuzzing budget.
const fuzzMaxPackages = 6

// fuzzDecodeGraph decodes data into a small acyclic graph over the sharp
// version and constraint pools generateGraph uses. It is total: reads wrap
// around data, so no byte slice, nil included, can crash the decoder.
func fuzzDecodeGraph(data []byte) generatedGraph {
	pos := 0
	next := func() byte {
		if len(data) == 0 {
			return 0
		}
		b := data[pos%len(data)]
		pos++
		return b
	}

	vpool := sharpVersionPool()
	cpool := sharpConstraintPool()

	n := 1 + int(next())%fuzzMaxPackages
	maxVersions := 1 + int(next())%len(vpool)

	g := generatedGraph{
		versions: make(map[string][]string, n),
		deps:     make(map[string]map[string]string, n),
		pkgs:     make([]string, n),
	}
	for i := range g.pkgs {
		g.pkgs[i] = fmt.Sprintf("gen.p%d", i)
	}

	for i, pkg := range g.pkgs {
		vc := 1 + int(next())%maxVersions
		start := int(next()) % len(vpool)
		vs := make([]string, 0, vc)
		for k := 0; k < len(vpool) && len(vs) < vc; k++ {
			vs = append(vs, vpool[(start+k)%len(vpool)])
		}
		slices.Sort(vs)
		g.versions[pkg] = vs

		for _, v := range vs {
			dm := make(map[string]string)
			for j := i + 1; j < n; j++ {
				if int(next())%3 == 0 {
					dm[g.pkgs[j]] = cpool[int(next())%len(cpool)]
				}
			}
			if len(dm) > 0 {
				g.deps[pkg+"@"+v] = dm
			}
		}
	}

	g.roots = []Requirement{{Package: g.pkgs[0], Constraint: cpool[int(next())%len(cpool)]}}
	return g
}

// fuzzOracleCap bounds the assignment space a fuzz iteration brute-forces:
// the worst case of 9^6 would starve the fuzzing budget, so only smaller
// decoded graphs get the oracle check.
const fuzzOracleCap = 4096

// FuzzSolve pins over the fuzzer's corpus that a resolution satisfies every
// constraint (checked by propCheck), a failure is a *ConflictError and never
// errSolverBug, and under fuzzOracleCap the oracle agrees in both directions.
func FuzzSolve(f *testing.F) {
	for _, seed := range fuzzSeedCorpus() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		g := fuzzDecodeGraph(data)
		res, err := Solve(t.Context(), g.roots, g.provider())
		if err != nil {
			checkFuzzConflict(t, g, err)
			return
		}
		if v := g.constraintViolation(res); v != "" {
			t.Fatalf("%s (result=%v, roots=%v)", v, res.Versions, g.roots)
		}
		if g.enumerationSpace() <= fuzzOracleCap {
			if valid := g.bruteForceResolutions(); !containsResolution(valid, res.Versions) {
				t.Fatalf("resolution %v is not a member of the oracle's %d valid resolutions (roots=%v)",
					res.Versions, len(valid), g.roots)
			}
		}
	})
}

// checkFuzzConflict asserts a failed solve is a *ConflictError, never
// errSolverBug, and, under fuzzOracleCap, that the oracle finds nothing
// solvable either.
func checkFuzzConflict(t *testing.T, g generatedGraph, err error) {
	t.Helper()
	if errors.Is(err, errSolverBug) {
		t.Fatalf("internal-bug error on a fuzzed input: %v (roots=%v)", err, g.roots)
	}
	if _, ok := errors.AsType[*ConflictError](err); !ok {
		t.Fatalf("want success or *ConflictError, got %v (roots=%v)", err, g.roots)
	}
	if g.enumerationSpace() <= fuzzOracleCap {
		if valid := g.bruteForceResolutions(); len(valid) != 0 {
			t.Fatalf("false rejection: resolver conflicted but the oracle has %d valid resolutions, e.g. %v (roots=%v)",
				len(valid), valid[0], g.roots)
		}
	}
}

// enumerationSpace returns the size of g's brute-force assignment space,
// the product over packages of (published versions + 1 for absence).
func (g generatedGraph) enumerationSpace() int {
	space := 1
	for _, pkg := range g.pkgs {
		space *= len(g.versions[pkg]) + 1
	}
	return space
}

// fuzzSeedCorpus returns FuzzSolve's seeds: empty and zero-byte inputs for
// the decoder's wraparound path, plus byte sequences decoding to a diamond,
// a forced transitive backtrack, and a wildcard term that must stay visible.
func fuzzSeedCorpus() [][]byte {
	return [][]byte{
		{},
		{0},
		// Diamond: gen.p0 depends on both gen.p1 and gen.p2, which both
		// depend on gen.p3 - two independent paths converging on one target.
		{3, 0, 0, 0, 0, 0, 0, 0, 1, 0, 1, 1, 0, 0, 0, 2, 0, 0, 0, 4, 0},
		// Transitive backtrack: gen.p0@1.2.0 needs a gen.p1 version that is
		// not published, so the solver must learn "not gen.p0@1.2.0".
		{2, 1, 1, 4, 1, 0, 0, 0, 1, 1, 0, 0, 1, 0, 2, 0},
		// Wildcard invisibility: gen.p0@1.5.0 reaches an unsatisfiable
		// gen.p2 through a "*" dependency on gen.p1; that term must stay
		// visible so the solver backtracks to gen.p0@1.2.0 instead of failing.
		{3, 1, 1, 5, 1, 0, 0, 1, 0, 0, 1, 1, 0, 4, 0, 5, 1, 0, 2, 1, 0, 0, 0},
	}
}
