package solver

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"testing"
)

// scaledGraphFanout is the k-ary fanout of buildScaledGraph's tree. One
// parent per child means no second parent can contradict a safe constraint,
// so the tree is solvable by construction and its edge count linear in n.
const scaledGraphFanout = 3

// safeConstraint draws from pool a constraint admitting at least one of
// childVersions (judged by propCheck), else "*", so benchmark graphs use
// real constraint forms while staying solvable.
func safeConstraint(rng *rand.Rand, pool, childVersions []string) string {
	candidates := make([]string, 0, len(pool))
	for _, c := range pool {
		for _, v := range childVersions {
			if propCheck(v, c) {
				candidates = append(candidates, c)
				break
			}
		}
	}
	if len(candidates) == 0 {
		return "*"
	}
	return candidates[rng.Intn(len(candidates))]
}

// buildScaledGraph builds a deterministic, solvable n-package k-ary tree
// over the sharp version and constraint pools, with 1-3 versions per package;
// the bounded fanout keeps construction and solving linear in n.
func buildScaledGraph(seed int64, n int) ([]Requirement, *fakeProvider) {
	//nolint:gosec // G404: deterministic seeded PRNG for a reproducible benchmark corpus, not security-sensitive
	rng := rand.New(rand.NewSource(seed))
	cpool := sharpConstraintPool()
	vpool := sharpVersionPool()
	p := newFakeProvider()

	pkgs := make([]string, n)
	versions := make([][]string, n)
	for i := range pkgs {
		pkgs[i] = fmt.Sprintf("bench.p%d", i)
		perm := rng.Perm(len(vpool))
		vs := make([]string, 1+rng.Intn(3))
		for j := range vs {
			vs[j] = vpool[perm[j]]
		}
		versions[i] = vs
		p.withVersions(pkgs[i], vs...)
	}

	for i, pkg := range pkgs {
		first := i*scaledGraphFanout + 1
		children := make([]int, 0, scaledGraphFanout)
		for c := first; c < first+scaledGraphFanout && c < n; c++ {
			children = append(children, c)
		}
		if len(children) == 0 {
			continue
		}
		for _, v := range versions[i] {
			deps := make(map[string]string, len(children))
			for _, c := range children {
				deps[pkgs[c]] = safeConstraint(rng, cpool, versions[c])
			}
			p.withDeps(pkg, v, deps)
		}
	}

	return []Requirement{{Package: pkgs[0], Constraint: "*"}}, p
}

// BenchmarkSolve measures Solve over the in-memory fakeProvider as the
// package count scales from a typical requirements.yml to a stress size;
// each graph is built outside the timed loop.
func BenchmarkSolve(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			reqs, p := buildScaledGraph(int64(n), n)
			for b.Loop() {
				res, err := Solve(context.Background(), reqs, p)
				if err != nil {
					b.Fatalf("Solve: %v", err)
				}
				_ = res
			}
		})
	}
}

// galaxyShapeRoots and galaxyShapeHubs size the "galaxy shape" benchmark:
// many top-level requirements converging on a few shared hub dependencies.
const (
	galaxyShapeRoots = 60
	galaxyShapeHubs  = 5
)

// buildGalaxyShapeGraph builds a deterministic, solvable shallow forest:
// each root needs two hubs via "*", so convergent edges never conflict, and
// hubs form a chain constrained through safeConstraint.
func buildGalaxyShapeGraph() ([]Requirement, *fakeProvider) {
	//nolint:gosec // G404: deterministic seeded PRNG for a reproducible benchmark corpus, not security-sensitive
	rng := rand.New(rand.NewSource(1))
	cpool := sharpConstraintPool()
	vpool := sharpVersionPool()
	p := newFakeProvider()

	hubs := make([]string, galaxyShapeHubs)
	for i := range hubs {
		hubs[i] = fmt.Sprintf("galaxy.hub%d", i)
		p.withVersions(hubs[i], vpool...)
	}
	for i := 0; i+1 < galaxyShapeHubs; i++ {
		next := hubs[i+1]
		for _, v := range vpool {
			p.withDeps(hubs[i], v, map[string]string{next: safeConstraint(rng, cpool, vpool)})
		}
	}

	reqs := make([]Requirement, galaxyShapeRoots)
	for i := range reqs {
		root := fmt.Sprintf("galaxy.root%d", i)
		p.withVersions(root, "1.0.0")
		deps := make(map[string]string, 2)
		for range 2 {
			deps[hubs[rng.Intn(len(hubs))]] = "*"
		}
		p.withDeps(root, "1.0.0", deps)
		reqs[i] = Requirement{Package: root, Constraint: "*"}
	}
	return reqs, p
}

// BenchmarkSolveGalaxyShape measures Solve time over the realistic Galaxy
// topology: many independently requested top-level collections converging
// on a small shared set of hub dependencies.
func BenchmarkSolveGalaxyShape(b *testing.B) {
	reqs, p := buildGalaxyShapeGraph()
	for b.Loop() {
		res, err := Solve(context.Background(), reqs, p)
		if err != nil {
			b.Fatalf("Solve: %v", err)
		}
		_ = res
	}
}

// deepBacktrackChainLength is the chain length for BenchmarkSolveDeepBacktrack.
const deepBacktrackChainLength = 200

// buildDeepBacktrackGraph builds a conflict-dense chain whose terminal
// package publishes only 1.0.0 while every higher version needs its
// successor >=2.0.0, forcing conflict resolution through every link.
func buildDeepBacktrackGraph(n int) ([]Requirement, *fakeProvider) {
	p := newFakeProvider()
	pkgs := make([]string, n)
	for i := range pkgs {
		pkgs[i] = fmt.Sprintf("bench.chain%d", i)
	}
	for i, pkg := range pkgs {
		if i == n-1 {
			p.withVersions(pkg, "1.0.0")
			continue
		}
		p.withVersions(pkg, "1.0.0", "2.0.0", "3.0.0")
		next := pkgs[i+1]
		p.withDeps(pkg, "2.0.0", map[string]string{next: ">=2.0.0"})
		p.withDeps(pkg, "3.0.0", map[string]string{next: ">=2.0.0"})
	}
	return []Requirement{{Package: pkgs[0], Constraint: "*"}}, p
}

// BenchmarkSolveDeepBacktrack measures Solve on the conflict-dense chain;
// the outcome is incidental, the backtracking path is what is timed.
func BenchmarkSolveDeepBacktrack(b *testing.B) {
	reqs, p := buildDeepBacktrackGraph(deepBacktrackChainLength)
	for b.Loop() {
		res, _ := Solve(context.Background(), reqs, p)
		_ = res
	}
}
