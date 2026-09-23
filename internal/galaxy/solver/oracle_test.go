package solver

import (
	"errors"
	"maps"
	"slices"
	"testing"
)

// oracleSeedCount is the seeds per package count TestOracleMembership
// enumerates. The suite is a completeness gate: the resolver must reject
// nothing the oracle can solve, so lowering the count weakens it.
const oracleSeedCount = 3000

// reachableClosure returns the packages reachable from the roots by following,
// from each present package, the dependency edges of its assigned version, to a
// fixpoint. A package absent from assign is never reached.
func (g generatedGraph) reachableClosure(assign map[string]string) map[string]bool {
	reach := make(map[string]bool, len(assign))
	stack := make([]string, 0, len(g.roots))
	for _, r := range g.roots {
		stack = append(stack, r.Package)
	}
	for len(stack) > 0 {
		pkg := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		v, present := assign[pkg]
		if !present || reach[pkg] {
			continue
		}
		reach[pkg] = true
		for dep := range g.deps[pkg+"@"+v] {
			stack = append(stack, dep)
		}
	}
	return reach
}

func (g generatedGraph) constraintsSatisfied(assign map[string]string) bool {
	for _, r := range g.roots {
		if v, ok := assign[r.Package]; !ok || !propCheck(v, r.Constraint) {
			return false
		}
	}
	for pkg, v := range assign {
		for dep, c := range g.deps[pkg+"@"+v] {
			if dv, ok := assign[dep]; !ok || !propCheck(dv, c) {
				return false
			}
		}
	}
	return true
}

// validResolution reports whether assign is a valid closed minimal
// resolution: every root and dependency present and satisfied, and the
// present set exactly the closure reachable from the roots.
func (g generatedGraph) validResolution(assign map[string]string) (Resolution, bool) {
	if !g.constraintsSatisfied(assign) {
		return nil, false
	}
	reach := g.reachableClosure(assign)
	if len(reach) != len(assign) {
		return nil, false
	}
	for pkg := range assign {
		if !reach[pkg] {
			return nil, false
		}
	}
	out := make(Resolution, len(assign))
	maps.Copy(out, assign)
	return out, true
}

// bruteForceResolutions enumerates every (version-or-absent) assignment over
// the graph's packages and returns each valid closed minimal resolution. Only
// for small graphs: the enumeration is (versions+1)^packages.
func (g generatedGraph) bruteForceResolutions() []Resolution {
	choices := make([][]string, len(g.pkgs))
	for i, pkg := range g.pkgs {
		choices[i] = append(slices.Clone(g.versions[pkg]), "")
	}
	var out []Resolution
	assign := make(map[string]string, len(g.pkgs))
	var rec func(i int)
	rec = func(i int) {
		if i == len(g.pkgs) {
			if r, ok := g.validResolution(assign); ok {
				out = append(out, r)
			}
			return
		}
		for _, v := range choices[i] {
			if v == "" {
				delete(assign, g.pkgs[i])
			} else {
				assign[g.pkgs[i]] = v
			}
			rec(i + 1)
		}
		delete(assign, g.pkgs[i])
	}
	rec(0)
	return out
}

func containsResolution(set []Resolution, r Resolution) bool {
	return slices.ContainsFunc(set, func(x Resolution) bool { return maps.Equal(x, r) })
}

// TestOracleMembership is the brute-force membership oracle over small graphs:
// the resolver's result must be a member of the set of valid closed minimal
// resolutions, and an empty valid set must coincide with a resolver conflict.
func TestOracleMembership(t *testing.T) {
	t.Parallel()
	for _, n := range []int{2, 3, 4} {
		for seed := range int64(oracleSeedCount) {
			g := generateGraph(seed, n, 3)
			valid := g.bruteForceResolutions()
			res, err := Solve(t.Context(), g.roots, g.provider())
			if err != nil {
				if _, ok := errors.AsType[*ConflictError](err); !ok {
					t.Fatalf("n=%d seed=%d: want *ConflictError, got %v", n, seed, err)
				}
				if len(valid) != 0 {
					t.Fatalf("n=%d seed=%d: resolver conflict but oracle has %d valid, e.g. %v", n, seed, len(valid), valid[0])
				}
				continue
			}
			if len(valid) == 0 {
				t.Fatalf("n=%d seed=%d: resolver produced %v but oracle found NO valid resolution", n, seed, res.Versions)
			}
			if !containsResolution(valid, res.Versions) {
				t.Fatalf("n=%d seed=%d: resolver result %v not a member of oracle's %d valid", n, seed, res.Versions, len(valid))
			}
		}
	}
}

// TestOracleRejectsInvalid proves the oracle's teeth: on a hand-worked graph
// with a single valid resolution it returns exactly that one, and its
// membership check rejects invalid claimed results.
func TestOracleRejectsInvalid(t *testing.T) {
	g := generatedGraph{
		versions: map[string][]string{"acme.foo": {"1.0.0", "2.0.0"}, "acme.bar": {"1.0.0"}},
		deps: map[string]map[string]string{
			"acme.foo@2.0.0": {"acme.bar": ">=5.0.0"},
			"acme.foo@1.0.0": {"acme.bar": "*"},
		},
		pkgs:  []string{"acme.bar", "acme.foo"},
		roots: []Requirement{{Package: "acme.foo", Constraint: ">=1.0.0"}},
	}
	valid := g.bruteForceResolutions()
	want := Resolution{"acme.foo": "1.0.0", "acme.bar": "1.0.0"}
	if len(valid) != 1 || !maps.Equal(valid[0], want) {
		t.Fatalf("want exactly [%v], got %v", want, valid)
	}
	for _, bad := range []Resolution{
		{"acme.foo": "2.0.0", "acme.bar": "1.0.0"}, // foo@2.0.0 needs bar>=5.0.0, unsatisfiable
		{"acme.foo": "1.0.0"},                      // dropped a required dependency
		{"acme.foo": "1.0.0", "acme.bar": "2.0.0"}, // bar version does not exist
	} {
		if containsResolution(valid, bad) {
			t.Fatalf("oracle wrongly accepted invalid result %v", bad)
		}
	}
}

// TestOracleAllowsMultipleValidMembers proves membership (not equality): a
// graph with two valid resolutions accepts both, and the resolver's own pick
// is one of them.
func TestOracleAllowsMultipleValidMembers(t *testing.T) {
	g := generatedGraph{
		versions: map[string][]string{"app": {"1.0.0"}, "lib": {"1.0.0", "1.2.0"}},
		deps:     map[string]map[string]string{"app@1.0.0": {"lib": "^1.0.0"}},
		pkgs:     []string{"app", "lib"},
		roots:    []Requirement{{Package: "app", Constraint: "*"}},
	}
	valid := g.bruteForceResolutions()
	if len(valid) != 2 ||
		!containsResolution(valid, Resolution{"app": "1.0.0", "lib": "1.0.0"}) ||
		!containsResolution(valid, Resolution{"app": "1.0.0", "lib": "1.2.0"}) {
		t.Fatalf("want both lib:1.0.0 and lib:1.2.0 valid, got %v", valid)
	}
	res, err := Solve(t.Context(), g.roots, g.provider())
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	if !containsResolution(valid, res.Versions) {
		t.Fatalf("resolver result %v not a member of %v", res.Versions, valid)
	}
}

// TestOracleEnforcesMinimality proves the corrected reachability logic: an
// isolated package nothing depends on must NOT appear in any valid resolution,
// and a resolution that includes it is rejected as non-minimal.
func TestOracleEnforcesMinimality(t *testing.T) {
	g := generatedGraph{
		versions: map[string][]string{"foo": {"1.0.0"}, "iso": {"1.0.0"}},
		deps:     map[string]map[string]string{},
		pkgs:     []string{"foo", "iso"},
		roots:    []Requirement{{Package: "foo", Constraint: "*"}},
	}
	valid := g.bruteForceResolutions()
	if len(valid) != 1 || !maps.Equal(valid[0], Resolution{"foo": "1.0.0"}) {
		t.Fatalf("want exactly [{foo:1.0.0}], got %v", valid)
	}
	if containsResolution(valid, Resolution{"foo": "1.0.0", "iso": "1.0.0"}) {
		t.Fatalf("oracle accepted a non-minimal resolution including an unreachable package")
	}
}
