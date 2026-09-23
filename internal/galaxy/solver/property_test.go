package solver

import (
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Masterminds/semver/v3"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const propertySeedCount = 500

// sharpConstraintPool is the constraint alphabet the generator draws from. It
// deliberately includes every sharp-edge form the resolver must handle: a "!="
// exclusion, the ">=X-0" prerelease floor, caret-zero ranges, and x-ranges.
func sharpConstraintPool() []string {
	return []string{"*", ">=1.0.0", ">=1.0.0-0", "!=1.5.0", "^0.2.0", "^0.0.3", "1.x", "1.2.x", "<2.0.0", ">=0.2.0"}
}

// sharpVersionPool includes a prerelease and several 0.y.z / 0.0.z versions so
// the sharp-edge constraints actually bite (prerelease exclusion, caret-zero
// boundaries).
func sharpVersionPool() []string {
	return []string{"0.0.3", "0.2.0", "0.2.5", "1.0.0-rc.1", "1.0.0", "1.2.0", "1.5.0", "2.0.0"}
}

type generatedGraph struct {
	versions map[string][]string
	deps     map[string]map[string]string
	pkgs     []string
	roots    []Requirement
}

// generateGraph builds a deterministic acyclic graph for seed: n packages of
// 1..maxVersions versions, dependencies only toward higher indices, and one
// root on the first package, so every machine reproduces the same corpus.
func generateGraph(seed int64, n, maxVersions int) generatedGraph {
	//nolint:gosec // G404: deterministic seeded PRNG for reproducible corpora, not security-sensitive
	rng := rand.New(rand.NewSource(seed))
	cpool := sharpConstraintPool()
	vpool := sharpVersionPool()
	g := generatedGraph{
		versions: make(map[string][]string, n),
		deps:     make(map[string]map[string]string, n),
		pkgs:     make([]string, n),
	}
	for i := range g.pkgs {
		g.pkgs[i] = fmt.Sprintf("gen.p%d", i)
	}
	for i, pkg := range g.pkgs {
		perm := rng.Perm(len(vpool))
		vs := make([]string, 0, maxVersions)
		for _, j := range perm[:1+rng.Intn(maxVersions)] {
			vs = append(vs, vpool[j])
		}
		slices.Sort(vs)
		g.versions[pkg] = vs
		for _, v := range vs {
			dm := make(map[string]string)
			for j := i + 1; j < n; j++ {
				if rng.Intn(3) == 0 {
					dm[g.pkgs[j]] = cpool[rng.Intn(len(cpool))]
				}
			}
			if len(dm) > 0 {
				g.deps[pkg+"@"+v] = dm
			}
		}
	}
	g.roots = []Requirement{{Package: g.pkgs[0], Constraint: cpool[rng.Intn(len(cpool))]}}
	return g
}

func (g generatedGraph) provider() *fakeProvider {
	p := newFakeProvider()
	for _, pkg := range g.pkgs {
		p.withVersions(pkg, g.versions[pkg]...)
		for _, v := range g.versions[pkg] {
			if dm := g.deps[pkg+"@"+v]; len(dm) > 0 {
				p.withDeps(pkg, v, dm)
			}
		}
	}
	return p
}

// propCheckConstraintMemo and propCheckVersionMemo cache immutable parses
// for propCheck, whose membership authority stays Masterminds Check.
//
//nolint:gochecknoglobals // immutable parse cache shared across parallel tests, not mutable logic state
var propCheckConstraintMemo sync.Map

//nolint:gochecknoglobals // immutable parse cache shared across parallel tests, not mutable logic state
var propCheckVersionMemo sync.Map

// propCheck is the independent membership authority: Masterminds Check over the
// same normalization the resolver's provider contract uses, never the
// resolver's own set algebra.
func propCheck(version, constraint string) bool {
	norm := helpers.NormalizeConstraint(constraint)
	if norm == "" {
		return true
	}
	c := memoConstraint(norm)
	if c == nil {
		return false
	}
	v := memoVersion(version)
	if v == nil {
		return false
	}
	return c.Check(v)
}

// memoConstraint returns norm's parsed constraint, caching it on first parse;
// nil means the parse failed, and failures are not cached.
func memoConstraint(norm string) *semver.Constraints {
	if cached, ok := propCheckConstraintMemo.Load(norm); ok {
		if c, isConstraint := cached.(*semver.Constraints); isConstraint {
			return c
		}
		return nil
	}
	parsed, err := semver.NewConstraint(norm)
	if err != nil {
		return nil
	}
	propCheckConstraintMemo.Store(norm, parsed)
	return parsed
}

// memoVersion is memoConstraint's version-string counterpart.
func memoVersion(raw string) *semver.Version {
	if cached, ok := propCheckVersionMemo.Load(raw); ok {
		if v, isVersion := cached.(*semver.Version); isVersion {
			return v
		}
		return nil
	}
	parsed, err := semver.NewVersion(raw)
	if err != nil {
		return nil
	}
	propCheckVersionMemo.Store(raw, parsed)
	return parsed
}

func (g generatedGraph) constraintViolation(res *Result) string {
	for _, r := range g.roots {
		v, ok := res.Versions[r.Package]
		if !ok {
			return fmt.Sprintf("root %s not resolved", r.Package)
		}
		if !propCheck(v, r.Constraint) {
			return fmt.Sprintf("root %s@%s violates %q", r.Package, v, r.Constraint)
		}
	}
	for pkg, v := range res.Versions {
		for dep, c := range g.deps[pkg+"@"+v] {
			dv, ok := res.Versions[dep]
			if !ok {
				return fmt.Sprintf("%s@%s requires %s which is unresolved", pkg, v, dep)
			}
			if !propCheck(dv, c) {
				return fmt.Sprintf("%s@%s: dependency %s@%s violates %q", pkg, v, dep, dv, c)
			}
		}
	}
	return ""
}

// TestPropertyResolutionSatisfiesConstraints pins that every resolution
// satisfies every constraint on a resolved package, checked by Masterminds,
// and that every failure is a *ConflictError, never an internal error.
func TestPropertyResolutionSatisfiesConstraints(t *testing.T) {
	t.Parallel()
	solved, conflicts := 0, 0
	for _, n := range []int{5, 6, 7, 8} {
		for seed := range int64(propertySeedCount) {
			g := generateGraph(seed, n, 3)
			res, err := Solve(t.Context(), g.roots, g.provider())
			if err != nil {
				if _, ok := errors.AsType[*ConflictError](err); !ok {
					t.Fatalf("n=%d seed=%d: want success or *ConflictError, got %v", n, seed, err)
				}
				conflicts++
				continue
			}
			if v := g.constraintViolation(res); v != "" {
				t.Fatalf("n=%d seed=%d: %s (result=%v)", n, seed, v, res.Versions)
			}
			solved++
		}
	}
	if solved == 0 || conflicts == 0 {
		t.Fatalf("degenerate corpus: solved=%d conflicts=%d", solved, conflicts)
	}
	t.Logf("property (i)/(ii): solved=%d conflicts=%d", solved, conflicts)
}

func countSharpEmissions(g generatedGraph, sharp map[string]func(string) bool, counts map[string]int) bool {
	for _, dm := range g.deps {
		for _, c := range dm {
			for label, pred := range sharp {
				if pred(c) {
					counts[label]++
				}
			}
		}
	}
	for _, vs := range g.versions {
		for _, v := range vs {
			if strings.Contains(v, "-") && !propCheck(v, ">=1.0.0") && propCheck(v, ">=1.0.0-0") {
				return true
			}
		}
	}
	return false
}

// TestPropertyExercisesSharpEdges proves the generator actually emits each
// mandatory sharp-edge constraint form across the corpus and publishes a
// prerelease that ">=1.0.0" excludes while ">=1.0.0-0" admits.
func TestPropertyExercisesSharpEdges(t *testing.T) {
	t.Parallel()
	sharp := map[string]func(string) bool{
		"!= exclusion": func(c string) bool { return strings.HasPrefix(c, "!=") },
		">=X-0 floor":  func(c string) bool { return strings.Contains(c, "-0") },
		"caret-zero":   func(c string) bool { return strings.HasPrefix(c, "^0.") },
		"x-range":      func(c string) bool { return strings.HasSuffix(c, ".x") },
	}
	counts := make(map[string]int)
	prereleaseBite := false
	for _, n := range []int{5, 6, 7, 8} {
		for seed := range int64(propertySeedCount) {
			if countSharpEmissions(generateGraph(seed, n, 3), sharp, counts) {
				prereleaseBite = true
			}
		}
	}
	for label := range sharp {
		if counts[label] == 0 {
			t.Fatalf("generator never emitted a %s constraint", label)
		}
	}
	if !prereleaseBite {
		t.Fatalf("generator never published a prerelease that >=1.0.0 excludes but >=1.0.0-0 admits")
	}
	t.Logf("sharp-edge emission counts: %v", counts)
}
