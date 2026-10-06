package solver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// These are this file's fixture literals, named for goconst: a generic
// package, dependers whose names sort around it, and its versions.
const (
	prefPkgFoo     = "acme.foo"
	prefPkgAlpha   = "acme.alpha"
	prefPkgBeta    = "acme.beta"
	prefPkgNew     = "acme.new"
	prefPkgZed     = "acme.zed"
	prefPkgZeta    = "acme.zeta"
	prefVersion150 = "1.5.0"
	prefVersion200 = "2.0.0"
	prefVersion250 = "2.5.0"
)

// errPreferredLookup is what the error-path double's Preferred returns, so a
// test can match it through the core's wrapping.
var errPreferredLookup = errors.New("preferred lookup failed")

// preferringProvider is fakeProvider plus the Preferrer extension: preferred
// is what Preferred answers per package, failing what it returns as an error;
// Confirm refuses what unconfirmed holds and fails what confirmFailing does.
type preferringProvider struct {
	*fakeProvider

	preferred      map[string]string
	failing        map[string]error
	unconfirmed    map[string]bool
	confirmFailing map[string]error
	preferredCalls map[string]int
	confirmCalls   map[string]int
}

func newPreferringProvider(p *fakeProvider) *preferringProvider {
	return &preferringProvider{
		fakeProvider:   p,
		preferred:      make(map[string]string),
		failing:        make(map[string]error),
		unconfirmed:    make(map[string]bool),
		confirmFailing: make(map[string]error),
		preferredCalls: make(map[string]int),
		confirmCalls:   make(map[string]int),
	}
}

// Confirm counts the question and answers it from unconfirmed and
// confirmFailing, confirming every other preference.
func (p *preferringProvider) Confirm(_ context.Context, pkg string, _ Version) (bool, error) {
	p.mu.Lock()
	p.confirmCalls[pkg]++
	p.mu.Unlock()
	if err, ok := p.confirmFailing[pkg]; ok {
		return false, err
	}
	return !p.unconfirmed[pkg], nil
}

func (p *preferringProvider) Preferred(_ context.Context, pkg string) (Version, bool, error) {
	p.mu.Lock()
	p.preferredCalls[pkg]++
	p.mu.Unlock()

	if err, ok := p.failing[pkg]; ok {
		return Version{}, false, err
	}
	raw, ok := p.preferred[pkg]
	if !ok {
		return Version{}, false, nil
	}
	v, err := NewVersion(raw)
	if err != nil {
		return Version{}, false, fmt.Errorf("preferringProvider: bad preference %q for %s: %w", raw, pkg, err)
	}
	return v, true, nil
}

// prefer registers version as pkg's preference.
func (p *preferringProvider) prefer(pkg, version string) *preferringProvider {
	p.preferred[pkg] = version
	return p
}

// preferAll registers every preference in prefs.
func (p *preferringProvider) preferAll(prefs map[string]string) *preferringProvider {
	maps.Copy(p.preferred, prefs)
	return p
}

// TestPreferredVersionDecidedOverAHigherOne pins the preferred step: a
// preference the accumulation allows is decided over higher allowed versions,
// before the Highest probe and with no universe fetched.
func TestPreferredVersionDecidedOverAHigherOne(t *testing.T) {
	t.Parallel()
	reqs := []Requirement{{Package: prefPkgFoo, Constraint: ">=1.0.0"}}
	build := func() *fakeProvider {
		return newFakeProvider().withVersions(prefPkgFoo, testVersion100, prefVersion150, prefVersion200)
	}

	plain, err := Solve(t.Context(), reqs, build())
	if err != nil {
		t.Fatalf("control: Solve without a preference: %v", err)
	}
	if got := plain.Versions[prefPkgFoo]; got != prefVersion200 {
		t.Fatalf("control: Versions[%s] = %q, want 2.0.0 without a preference", prefPkgFoo, got)
	}

	p := newPreferringProvider(build()).prefer(prefPkgFoo, prefVersion150)
	res, err := Solve(t.Context(), reqs, p)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if got := res.Versions[prefPkgFoo]; got != prefVersion150 {
		t.Fatalf("Versions[%s] = %q, want 1.5.0, the preference over 2.0.0", prefPkgFoo, got)
	}
	if h, u := p.highestCalls[prefPkgFoo], p.universeCalls[prefPkgFoo]; h != 0 || u != 0 {
		t.Fatalf("Highest asked %d times and Universe %d, want 0 and 0: the preference decides first", h, u)
	}
	if got := p.preferredCalls[prefPkgFoo]; got != 1 {
		t.Fatalf("Preferred asked %d times for %s, want 1", got, prefPkgFoo)
	}
}

// TestPreferredVersionOutsideTheConstraintGivesWay pins that a preference the
// root constraint excludes is passed over for the highest allowed version.
func TestPreferredVersionOutsideTheConstraintGivesWay(t *testing.T) {
	t.Parallel()
	p := newPreferringProvider(newFakeProvider().
		withVersions(prefPkgFoo, testVersion100, prefVersion150, prefVersion200, prefVersion250)).
		prefer(prefPkgFoo, prefVersion150)

	res, err := Solve(t.Context(), []Requirement{{Package: prefPkgFoo, Constraint: ">=2.0.0"}}, p)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if got := res.Versions[prefPkgFoo]; got != prefVersion250 {
		t.Fatalf("Versions[%s] = %q, want 2.5.0, the highest version >=2.0.0 allows", prefPkgFoo, got)
	}
	if got := p.preferredCalls[prefPkgFoo]; got != 1 {
		t.Fatalf("Preferred asked %d times for %s, want 1", got, prefPkgFoo)
	}
}

// TestNoPreferenceDecidesTheHighest pins ok false: a package the provider
// has no preference for is decided at its highest allowed version.
func TestNoPreferenceDecidesTheHighest(t *testing.T) {
	t.Parallel()
	p := newPreferringProvider(newFakeProvider().
		withVersions(prefPkgFoo, testVersion100, prefVersion150, prefVersion200).
		withVersions(prefPkgBeta, testVersion100, prefVersion200)).
		prefer(prefPkgBeta, testVersion100)

	res, err := Solve(t.Context(), []Requirement{{Package: prefPkgFoo, Constraint: "*"}}, p)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if !maps.Equal(res.Versions, Resolution{prefPkgFoo: prefVersion200}) {
		t.Fatalf("Versions = %v, want exactly {%s: 2.0.0}", res.Versions, prefPkgFoo)
	}
	if !maps.Equal(p.preferredCalls, map[string]int{prefPkgFoo: 1}) {
		t.Fatalf("Preferred calls = %v, want only %s asked, once", p.preferredCalls, prefPkgFoo)
	}
}

// TestPreferredErrorEndsTheSolve pins that a Preferred error ends the solve
// wrapped, naming the package, before any other provider call for it.
func TestPreferredErrorEndsTheSolve(t *testing.T) {
	t.Parallel()
	p := newPreferringProvider(newFakeProvider().withVersions(prefPkgFoo, testVersion100, prefVersion200))
	p.failing[prefPkgFoo] = errPreferredLookup

	res, err := Solve(t.Context(), []Requirement{{Package: prefPkgFoo, Constraint: ">=1.0.0"}}, p)
	if res != nil || !errors.Is(err, errPreferredLookup) {
		t.Fatalf("Solve = (%v, %v), want no result and an error wrapping errPreferredLookup", res, err)
	}
	if !strings.Contains(err.Error(), "preferred version of "+prefPkgFoo) {
		t.Fatalf("error = %q, want it to name the preferred version of %s", err, prefPkgFoo)
	}
	if _, isConflict := errors.AsType[*ConflictError](err); isConflict || errors.Is(err, errSolverBug) {
		t.Fatalf("error = %v, want a provider failure, neither a *ConflictError nor errSolverBug", err)
	}
	if h, u, d := p.totalHighestCalls(), p.totalUniverseCalls(), p.totalDepsCalls(); h+u+d != 0 {
		t.Fatalf("after the error: Highest %d, Universe %d, Dependencies %d calls, want none", h, u, d)
	}
}

// TestExactRootPinWinsOverAPreference pins the step order: an exact root pin
// is decided before any preference is asked for its package.
func TestExactRootPinWinsOverAPreference(t *testing.T) {
	t.Parallel()
	p := newPreferringProvider(newFakeProvider().withVersions(prefPkgFoo, testVersion100, prefVersion200)).
		prefer(prefPkgFoo, prefVersion200)

	res, err := Solve(t.Context(), []Requirement{{Package: prefPkgFoo, Constraint: testVersion100}}, p)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if got := res.Versions[prefPkgFoo]; got != testVersion100 {
		t.Fatalf("Versions[%s] = %q, want 1.0.0, the root's exact pin", prefPkgFoo, got)
	}
	if got := p.preferredCalls[prefPkgFoo]; got != 0 {
		t.Fatalf("Preferred asked %d times for the pinned %s, want 0", got, prefPkgFoo)
	}
}

// backjumpedPreferenceGraph builds a graph whose only valid resolution moves
// acme.alpha off 1.0.0: acme.beta, picked after it, needs acme.alpha >=2.0.0.
func backjumpedPreferenceGraph() generatedGraph {
	return generatedGraph{
		versions: map[string][]string{prefPkgAlpha: {testVersion100, prefVersion200}, prefPkgBeta: {testVersion100}},
		deps:     map[string]map[string]string{prefPkgBeta + "@" + testVersion100: {prefPkgAlpha: ">=2.0.0"}},
		pkgs:     []string{prefPkgAlpha, prefPkgBeta},
		roots:    []Requirement{{Package: prefPkgAlpha, Constraint: "*"}, {Package: prefPkgBeta, Constraint: "*"}},
	}
}

// declinedPreferenceGraph builds a graph where acme.zeta@1.0.0 needs
// acme.beta <2.0.0, so preferring acme.beta 2.0.0, which sorts first and is
// decided first, declines a preference for acme.zeta 1.0.0.
func declinedPreferenceGraph() generatedGraph {
	return generatedGraph{
		versions: map[string][]string{prefPkgBeta: {testVersion100, prefVersion200}, prefPkgZeta: {testVersion100, prefVersion200}},
		deps:     map[string]map[string]string{prefPkgZeta + "@" + testVersion100: {prefPkgBeta: "<2.0.0"}},
		pkgs:     []string{prefPkgBeta, prefPkgZeta},
		roots:    []Requirement{{Package: prefPkgBeta, Constraint: "*"}, {Package: prefPkgZeta, Constraint: "*"}},
	}
}

// TestPreferredVersionGivesWayToAConflict pins that a preference conflicting
// with a dependency, another package's or its own, is tried first, then given
// up for a valid resolution: after a backjump, or when its decision is declined.
func TestPreferredVersionGivesWayToAConflict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		want  Resolution
		prefs map[string]string
		name  string
		tried string
		g     generatedGraph
	}{
		{
			name: "backjumped", g: backjumpedPreferenceGraph(), prefs: map[string]string{prefPkgAlpha: testVersion100},
			tried: prefPkgAlpha + "@" + testVersion100,
			want:  Resolution{prefPkgAlpha: prefVersion200, prefPkgBeta: testVersion100},
		},
		{
			name: "declined", g: declinedPreferenceGraph(),
			prefs: map[string]string{prefPkgBeta: prefVersion200, prefPkgZeta: testVersion100},
			tried: prefPkgZeta + "@" + testVersion100,
			want:  Resolution{prefPkgBeta: prefVersion200, prefPkgZeta: prefVersion200},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newPreferringProvider(tc.g.provider()).preferAll(tc.prefs)
			res, err := Solve(t.Context(), tc.g.roots, p)
			if err != nil {
				t.Fatalf("Solve: %v", err)
			}
			if !maps.Equal(res.Versions, tc.want) {
				t.Fatalf("Versions = %v, want %v", res.Versions, tc.want)
			}
			if !containsResolution(tc.g.bruteForceResolutions(), res.Versions) {
				t.Fatalf("Versions = %v is not a valid resolution", res.Versions)
			}
			if got := p.depsCalls[tc.tried]; got != 1 {
				t.Fatalf("Dependencies of %s asked %d times, want 1: the preference is tried first", tc.tried, got)
			}
		})
	}
}

// TestPreferredAskedOncePerPackage pins the memo: each package is asked once
// per Solve, its answer reused when it is decided again, and the synthetic
// root is never asked.
func TestPreferredAskedOncePerPackage(t *testing.T) {
	t.Parallel()
	g := backjumpedPreferenceGraph()
	p := newPreferringProvider(g.provider()).prefer(prefPkgAlpha, testVersion100)
	if _, err := Solve(t.Context(), g.roots, p); err != nil {
		t.Fatalf("Solve: %v", err)
	}

	// Highest runs only after a preferred step that decided nothing: acme.beta
	// went through two, and acme.alpha one more after the backjump undid it.
	if a, b := p.highestCalls[prefPkgAlpha], p.highestCalls[prefPkgBeta]; a != 1 || b != 2 {
		t.Fatalf("Highest calls: %s %d, %s %d, want 1 and 2", prefPkgAlpha, a, prefPkgBeta, b)
	}
	want := map[string]int{prefPkgAlpha: 1, prefPkgBeta: 1}
	if !maps.Equal(p.preferredCalls, want) {
		t.Fatalf("Preferred calls = %v, want %v and never the root", p.preferredCalls, want)
	}
}

// TestProviderWithoutPreferrerIsUnchanged pins that the suite's fakeProvider
// lacks the extension, and that one answering ok false everywhere solves a
// generated corpus with the same results and the same provider calls.
func TestProviderWithoutPreferrerIsUnchanged(t *testing.T) {
	t.Parallel()
	if _, ok := any(newFakeProvider()).(Preferrer); ok {
		t.Fatal("fakeProvider implements Preferrer, so no test covers a provider without it")
	}
	// Five versions per package as well: no brute force bounds this corpus.
	for _, maxVersions := range []int{3, 5} {
		for _, n := range []int{2, 4, 6, 8} {
			for seed := range int64(propertySeedCount) {
				label := fmt.Sprintf("n=%d maxVersions=%d seed=%d", n, maxVersions, seed)
				requireUnchangedWithoutPreferences(t, label, generateGraph(seed, n, maxVersions))
			}
		}
	}
}

// requireUnchangedWithoutPreferences solves g with the plain fakeProvider and
// with a Preferrer that has no preference, and fails t unless the two agree.
func requireUnchangedWithoutPreferences(t *testing.T, label string, g generatedGraph) {
	t.Helper()
	plain := g.provider()
	resPlain, errPlain := Solve(t.Context(), g.roots, plain)
	asked := newPreferringProvider(g.provider())
	resAsked, errAsked := Solve(t.Context(), g.roots, asked)
	requireSameSolve(t, label, resPlain, resAsked, errPlain, errAsked)
	requireSameCalls(t, label, plain, asked.fakeProvider)
}

// requireSameSolve fails t unless two solves of one graph agree: the same
// versions and graph, or the same rendered conflict proof.
func requireSameSolve(t *testing.T, label string, a, b *Result, errA, errB error) {
	t.Helper()
	if errA != nil || errB != nil {
		ceA, okA := errors.AsType[*ConflictError](errA)
		ceB, okB := errors.AsType[*ConflictError](errB)
		if !okA || !okB || !slices.Equal(ceA.ProofLines(), ceB.ProofLines()) {
			t.Fatalf("%s: errors differ: %v vs %v", label, errA, errB)
		}
		return
	}
	if !maps.Equal(a.Versions, b.Versions) || !maps.EqualFunc(a.Graph, b.Graph, slices.Equal[[]string]) {
		t.Fatalf("%s: results differ: %v vs %v", label, a.Versions, b.Versions)
	}
}

// requireSameCalls fails t unless two providers saw the same Highest,
// Universe and Dependencies calls, counted per package or version.
func requireSameCalls(t *testing.T, label string, a, b *fakeProvider) {
	t.Helper()
	if !maps.Equal(a.highestCalls, b.highestCalls) || !maps.Equal(a.universeCalls, b.universeCalls) ||
		!maps.Equal(a.depsCalls, b.depsCalls) {
		t.Fatalf("%s: provider calls differ: Highest %v vs %v, Universe %v vs %v, Dependencies %v vs %v", label,
			a.highestCalls, b.highestCalls, a.universeCalls, b.universeCalls, a.depsCalls, b.depsCalls)
	}
}

// randomPreferences prefers one published version for about half of g's
// packages, drawn from a stream apart from generateGraph's.
func randomPreferences(g generatedGraph, seed int64) map[string]string {
	//nolint:gosec // G404: deterministic seeded PRNG for reproducible corpora, not security-sensitive
	rng := rand.New(rand.NewSource(^seed))
	prefs := make(map[string]string, len(g.pkgs))
	for _, pkg := range g.pkgs {
		if rng.Intn(2) == 0 {
			vs := g.versions[pkg]
			prefs[pkg] = vs[rng.Intn(len(vs))]
		}
	}
	return prefs
}

// freePreference reports whether preferring pkg@v conflicts with nothing else
// in g: v is published, every root and dependency constraint on pkg admits
// it, and pkg@v depends on nothing, so no decision of it can be declined.
func (g generatedGraph) freePreference(pkg, v string) bool {
	if !slices.Contains(g.versions[pkg], v) || len(g.deps[pkg+"@"+v]) > 0 {
		return false
	}
	for _, r := range g.roots {
		if r.Package == pkg && !propCheck(v, r.Constraint) {
			return false
		}
	}
	for _, dm := range g.deps {
		if c, ok := dm[pkg]; ok && !propCheck(v, c) {
			return false
		}
	}
	return true
}

// rootedPreferences returns, when the valid s agrees with every preference
// for a package it holds, each preferred package the roots reach through
// preferred packages of s alone, which the pick order keeps; else nil.
func (g generatedGraph) rootedPreferences(s Resolution, prefs map[string]string) []string {
	for pkg, v := range s {
		if want, ok := prefs[pkg]; ok && want != v {
			return nil
		}
	}
	var rooted []string
	seen := make(map[string]bool, len(s))
	stack := make([]string, 0, len(g.roots))
	for _, r := range g.roots {
		stack = append(stack, r.Package)
	}
	for len(stack) > 0 {
		pkg := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, preferred := prefs[pkg]; !preferred || seen[pkg] {
			continue
		}
		seen[pkg] = true
		rooted = append(rooted, pkg)
		for dep := range g.deps[pkg+"@"+s[pkg]] {
			stack = append(stack, dep)
		}
	}
	return rooted
}

// preferenceTally counts the preference outcomes a corpus produced: a free
// or a rooted preference held where the plain solve picks another version,
// and a resolved package left off its preference.
type preferenceTally struct {
	freeHeldOverPlain   int
	rootedHeldOverPlain int
	gaveWay             int
}

// check fails t when a free preference whose package res resolves is not
// what res holds, and tallies the outcomes against the plain resolution.
func (tally *preferenceTally) check(t *testing.T, label string, g generatedGraph, prefs map[string]string, res, plain Resolution) {
	t.Helper()
	for _, pkg := range g.pkgs {
		v, preferred := prefs[pkg]
		got, resolved := res[pkg]
		if !preferred || !resolved {
			continue
		}
		free := g.freePreference(pkg, v)
		if got != v {
			if free {
				t.Fatalf("%s: %s resolved to %s, want its free preference %s (result=%v)", label, pkg, got, v, res)
			}
			tally.gaveWay++
			continue
		}
		if plainV, ok := plain[pkg]; free && ok && plainV != v {
			tally.freeHeldOverPlain++
		}
	}
}

// checkRooted fails t when res moves a preference that rootedPreferences
// keeps for some resolution of valid, and tallies the preferences so kept
// that the plain resolution does not hold.
func (tally *preferenceTally) checkRooted(
	t *testing.T, label string, g generatedGraph, prefs map[string]string, res, plain Resolution, valid []Resolution,
) {
	t.Helper()
	held := make(map[string]bool)
	for _, s := range valid {
		for _, pkg := range g.rootedPreferences(s, prefs) {
			if res[pkg] != prefs[pkg] {
				t.Fatalf("%s: %s resolved to %q, want its preference %s, which %v keeps from the roots (result=%v)",
					label, pkg, res[pkg], prefs[pkg], s, res)
			}
			held[pkg] = true
		}
	}
	for pkg := range held {
		if plain[pkg] != prefs[pkg] {
			tally.rootedHeldOverPlain++
		}
	}
}

// require fails t when the corpus never exercised every outcome.
func (tally *preferenceTally) require(t *testing.T) {
	t.Helper()
	if tally.freeHeldOverPlain == 0 || tally.rootedHeldOverPlain == 0 || tally.gaveWay == 0 {
		t.Fatalf("degenerate corpus: freeHeldOverPlain=%d rootedHeldOverPlain=%d gaveWay=%d",
			tally.freeHeldOverPlain, tally.rootedHeldOverPlain, tally.gaveWay)
	}
	t.Logf("preferences: freeHeldOverPlain=%d rootedHeldOverPlain=%d gaveWay=%d",
		tally.freeHeldOverPlain, tally.rootedHeldOverPlain, tally.gaveWay)
}

// lockLikeStream offsets lockLikePreferences' seeds past every seed
// generateGraph and randomPreferences draw from.
const lockLikeStream = 1 << 32

// lockLikePreferences prefers, for about half of g's packages, the version
// res holds, or a random published one for a package res does not hold, as
// a lockfile left by an earlier resolution would.
func lockLikePreferences(g generatedGraph, res Resolution, seed int64) map[string]string {
	//nolint:gosec // G404: deterministic seeded PRNG for reproducible corpora, not security-sensitive
	rng := rand.New(rand.NewSource(seed + lockLikeStream))
	prefs := make(map[string]string, len(g.pkgs))
	for _, pkg := range g.pkgs {
		if rng.Intn(2) != 0 {
			continue
		}
		if v, ok := res[pkg]; ok {
			prefs[pkg] = v
			continue
		}
		vs := g.versions[pkg]
		prefs[pkg] = vs[rng.Intn(len(vs))]
	}
	return prefs
}

// solvePreferring solves g, which has a valid resolution, preferring prefs,
// and fails t unless the result satisfies every constraint.
func solvePreferring(t *testing.T, label string, g generatedGraph, prefs map[string]string) Resolution {
	t.Helper()
	res, err := Solve(t.Context(), g.roots, newPreferringProvider(g.provider()).preferAll(prefs))
	if err != nil {
		t.Fatalf("%s: preferring %v failed on a solvable graph: %v", label, prefs, err)
	}
	if v := g.constraintViolation(res); v != "" {
		t.Fatalf("%s: preferring %v: %s (result=%v)", label, prefs, v, res.Versions)
	}
	return res.Versions
}

// requireFreePreferencesValid fails t when a free preference whose package
// res resolves belongs to no valid resolution, which would make the honored
// check of preferenceTally a claim the oracle does not back.
func requireFreePreferencesValid(
	t *testing.T, label string, g generatedGraph, prefs map[string]string, res Resolution, valid []Resolution,
) {
	t.Helper()
	for _, pkg := range g.pkgs {
		v, preferred := prefs[pkg]
		if _, resolved := res[pkg]; !preferred || !resolved || !g.freePreference(pkg, v) {
			continue
		}
		if !slices.ContainsFunc(valid, func(r Resolution) bool { return r[pkg] == v }) {
			t.Fatalf("%s: free preference %s@%s belongs to no valid resolution", label, pkg, v)
		}
	}
}

// TestOracleMembershipWithPreferences holds results under random preferences to
// the oracle, free ones honored. Its rooted check passes under either pick order:
// a regression guard. TestOracleKeepsALockfileWhenARootIsAdded pins the order.
func TestOracleMembershipWithPreferences(t *testing.T) {
	t.Parallel()
	var tally preferenceTally
	for _, n := range []int{2, 3, 4} {
		for seed := range int64(oracleSeedCount) {
			g := generateGraph(seed, n, 3)
			prefs := randomPreferences(g, seed)
			label := fmt.Sprintf("n=%d seed=%d prefs=%v", n, seed, prefs)
			valid := g.bruteForceResolutions()
			res, err := Solve(t.Context(), g.roots, newPreferringProvider(g.provider()).preferAll(prefs))
			if err != nil {
				if _, ok := errors.AsType[*ConflictError](err); !ok || len(valid) != 0 {
					t.Fatalf("%s: Solve error %v, oracle has %d valid", label, err, len(valid))
				}
				continue
			}
			if !containsResolution(valid, res.Versions) {
				t.Fatalf("%s: result %v not a member of the oracle's %d valid", label, res.Versions, len(valid))
			}
			plain, err := Solve(t.Context(), g.roots, g.provider())
			if err != nil {
				t.Fatalf("%s: the plain solve failed where the preferred one resolved: %v", label, err)
			}
			requireFreePreferencesValid(t, label, g, prefs, res.Versions, valid)
			tally.check(t, label, g, prefs, res.Versions, plain.Versions)
			tally.checkRooted(t, label, g, prefs, res.Versions, plain.Versions, valid)
		}
	}
	tally.require(t)
}

// TestPropertyWithPreferences runs the property corpus under random preferences
// and a lockfile of each result. Its rooted check passes under either pick order:
// a regression guard. TestOracleKeepsALockfileWhenARootIsAdded pins the order.
func TestPropertyWithPreferences(t *testing.T) {
	t.Parallel()
	var tally preferenceTally
	for _, n := range []int{5, 6, 7, 8} {
		for seed := range int64(propertySeedCount) {
			g := generateGraph(seed, n, 3)
			prefs := randomPreferences(g, seed)
			label := fmt.Sprintf("n=%d seed=%d prefs=%v", n, seed, prefs)
			plain, plainErr := Solve(t.Context(), g.roots, g.provider())
			res, err := Solve(t.Context(), g.roots, newPreferringProvider(g.provider()).preferAll(prefs))
			if (err == nil) != (plainErr == nil) {
				t.Fatalf("%s: with preferences err=%v, without err=%v", label, err, plainErr)
			}
			if err != nil {
				if _, ok := errors.AsType[*ConflictError](err); !ok {
					t.Fatalf("%s: want success or *ConflictError, got %v", label, err)
				}
				continue
			}
			if v := g.constraintViolation(res); v != "" {
				t.Fatalf("%s: %s (result=%v)", label, v, res.Versions)
			}
			tally.check(t, label, g, prefs, res.Versions, plain.Versions)
			locked := lockLikePreferences(g, res.Versions, seed)
			label = fmt.Sprintf("%s earlier=%v locked=%v", label, res.Versions, locked)
			again := solvePreferring(t, label, g, locked)
			tally.check(t, label, g, locked, again, plain.Versions)
			tally.checkRooted(t, label, g, locked, again, plain.Versions, []Resolution{res.Versions})
		}
	}
	tally.require(t)
}

// TestOracleReproducesAPreferredResolution pins what a caller keeping a whole
// earlier result relies on: preferring every package of a valid resolution at
// its version returns exactly that resolution, for each valid one in turn.
func TestOracleReproducesAPreferredResolution(t *testing.T) {
	t.Parallel()
	differed := 0
	for _, n := range []int{2, 3, 4} {
		for seed := range int64(oracleSeedCount) {
			g := generateGraph(seed, n, 3)
			valid := g.bruteForceResolutions()
			if len(valid) == 0 {
				continue
			}
			plain, err := Solve(t.Context(), g.roots, g.provider())
			if err != nil {
				t.Fatalf("n=%d seed=%d: the plain solve failed on a solvable graph: %v", n, seed, err)
			}
			for _, want := range valid {
				res, err := Solve(t.Context(), g.roots, newPreferringProvider(g.provider()).preferAll(want))
				if err != nil || !maps.Equal(res.Versions, want) {
					t.Fatalf("n=%d seed=%d: preferring %v gave (%v, %v), want it unchanged", n, seed, want, res, err)
				}
				if !maps.Equal(want, plain.Versions) {
					differed++
				}
			}
		}
	}
	if differed == 0 {
		t.Fatal("degenerate corpus: every preferred resolution was the plain solve's own")
	}
	t.Logf("preferred resolutions reproduced away from the plain solve: %d", differed)
}

// lockfileSeedCount is the seeds per package count of the added-root oracle,
// which brute-forces two graphs per seed: a third of oracleSeedCount still holds
// dozens of graphs where picking an unpreferred package first moves a preference.
const lockfileSeedCount = 1000

// TestOracleKeepsALockfileWhenARootIsAdded pins what a lockfile relies on:
// after a root on gen.p1 is added, preferring a valid resolution of the graph
// before, whole or lockLikePreferences of it, keeps what rootedPreferences does.
func TestOracleKeepsALockfileWhenARootIsAdded(t *testing.T) {
	t.Parallel()
	var tally preferenceTally
	for _, n := range []int{2, 3, 4} {
		for seed := range int64(lockfileSeedCount) {
			g := generateGraph(seed, n, 3)
			added := g
			added.roots = append(slices.Clone(g.roots), Requirement{Package: g.pkgs[1], Constraint: "*"})
			valid := added.bruteForceResolutions()
			if len(valid) == 0 {
				continue
			}
			plain, err := Solve(t.Context(), added.roots, added.provider())
			if err != nil {
				t.Fatalf("n=%d seed=%d: the plain solve failed on a solvable graph: %v", n, seed, err)
			}
			for i, earlier := range g.bruteForceResolutions() {
				for _, locked := range []map[string]string{earlier, lockLikePreferences(added, earlier, seed+int64(i))} {
					label := fmt.Sprintf("n=%d seed=%d earlier=%v locked=%v", n, seed, earlier, locked)
					res := solvePreferring(t, label, added, locked)
					if !containsResolution(valid, res) {
						t.Fatalf("%s: result %v not a member of the oracle's %d valid", label, res, len(valid))
					}
					tally.checkRooted(t, label, added, locked, res, plain.Versions, valid)
				}
			}
		}
	}
	if tally.rootedHeldOverPlain == 0 {
		t.Fatal("degenerate corpus: every kept preference was the plain solve's own")
	}
	t.Logf("locked preferences kept away from the plain solve: %d", tally.rootedHeldOverPlain)
}

// unpreferredFirstGraph builds two roots: acme.new, sorting first, whose
// 2.0.0 needs acme.zed >=2.0.0, and acme.zed; oldNeeds is what acme.new
// 1.0.0 needs of acme.zed, nothing when empty.
func unpreferredFirstGraph(oldNeeds string) generatedGraph {
	deps := map[string]map[string]string{prefPkgNew + "@" + prefVersion200: {prefPkgZed: ">=2.0.0"}}
	if oldNeeds != "" {
		deps[prefPkgNew+"@"+testVersion100] = map[string]string{prefPkgZed: oldNeeds}
	}
	return generatedGraph{
		versions: map[string][]string{prefPkgNew: {testVersion100, prefVersion200}, prefPkgZed: {testVersion100, prefVersion200}},
		deps:     deps,
		pkgs:     []string{prefPkgNew, prefPkgZed},
		roots:    []Requirement{{Package: prefPkgNew, Constraint: "*"}, {Package: prefPkgZed, Constraint: "*"}},
	}
}

// TestPreferredPackagePickedBeforeAnUnpreferredOne pins the pick order: acme.zed,
// preferred at 1.0.0, is decided before acme.new, which sorts first, so its
// highest moves the preference only when no acme.new fits acme.zed 1.0.0.
func TestPreferredPackagePickedBeforeAnUnpreferredOne(t *testing.T) {
	t.Parallel()
	cases := []struct {
		want Resolution
		name string
		g    generatedGraph
	}{
		{name: "kept", g: unpreferredFirstGraph(""), want: Resolution{prefPkgNew: testVersion100, prefPkgZed: testVersion100}},
		{name: "moved", g: unpreferredFirstGraph(">=2.0.0"), want: Resolution{prefPkgNew: prefVersion200, prefPkgZed: prefVersion200}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plain, err := Solve(t.Context(), tc.g.roots, tc.g.provider())
			if err != nil || !maps.Equal(plain.Versions, Resolution{prefPkgNew: prefVersion200, prefPkgZed: prefVersion200}) {
				t.Fatalf("control: Solve without a preference = (%v, %v), want both at 2.0.0", plain, err)
			}
			p := newPreferringProvider(tc.g.provider()).prefer(prefPkgZed, testVersion100)
			res, err := Solve(t.Context(), tc.g.roots, p)
			if err != nil || !maps.Equal(res.Versions, tc.want) {
				t.Fatalf("Solve = (%v, %v), want %v", res, err, tc.want)
			}
			if !containsResolution(tc.g.bruteForceResolutions(), res.Versions) {
				t.Fatalf("Versions = %v is not a valid resolution", res.Versions)
			}
			if got := p.depsCalls[prefPkgZed+"@"+testVersion100]; got != 1 {
				t.Fatalf("Dependencies of %s@1.0.0 asked %d times, want 1: the preferred package is decided first", prefPkgZed, got)
			}
			if want := map[string]int{prefPkgNew: 1, prefPkgZed: 1}; !maps.Equal(p.preferredCalls, want) {
				t.Fatalf("Preferred calls = %v, want %v", p.preferredCalls, want)
			}
		})
	}
}

// requirePick fails t unless pickPackage picks want without an error.
func requirePick(t *testing.T, s *solveState, want string) {
	t.Helper()
	pkg, ok, err := s.pickPackage(t.Context())
	if err != nil || !ok || pkg != want {
		t.Fatalf("pickPackage = (%q, %v, %v), want (%q, true, nil)", pkg, ok, err, want)
	}
}

// TestPickPackagePrefersAfterExactPins pins where pickPackage weighs a
// preference: after an exact pin, ahead of a higher conflict count, the first
// in name order that accum allows, asking each candidate up to it once.
func TestPickPackagePrefersAfterExactPins(t *testing.T) {
	t.Parallel()
	p := newPreferringProvider(newFakeProvider()).preferAll(map[string]string{
		prefPkgBeta: prefVersion250, prefPkgFoo: prefVersion150, prefPkgZeta: prefVersion150,
	})
	s := newTestState(p)
	s.ps.decide(rootPkg, rootVersion)
	for _, pkg := range []string{prefPkgAlpha, prefPkgBeta, prefPkgFoo, prefPkgZeta} {
		s.ps.derive(term{Package: pkg, Set: mustSet(t, "^1.0.0"), Positive: true}, mustDummyCause(t, s))
	}
	s.conflictCounts[prefPkgAlpha] = 3

	requirePick(t, s, prefPkgFoo)
	requirePick(t, s, prefPkgFoo)
	asked := map[string]int{prefPkgAlpha: 1, prefPkgBeta: 1, prefPkgFoo: 1}
	if !maps.Equal(p.preferredCalls, asked) {
		t.Fatalf("Preferred calls = %v, want %v: the candidates in name order up to the pick, once", p.preferredCalls, asked)
	}

	s.ps.derive(term{Package: prefPkgZed, Set: mustSet(t, "=1.0.0"), Positive: true}, mustDummyCause(t, s))
	requirePick(t, s, prefPkgZed)
	if !maps.Equal(p.preferredCalls, asked) {
		t.Fatalf("Preferred calls = %v, want %v: an exact pin is picked without asking", p.preferredCalls, asked)
	}
}

// TestPickPackageReturnsAPreferredError pins that a Preferred error met while
// picking ends the pick, wrapped and naming the package.
func TestPickPackageReturnsAPreferredError(t *testing.T) {
	t.Parallel()
	p := newPreferringProvider(newFakeProvider()).prefer(prefPkgZeta, prefVersion150)
	p.failing[prefPkgBeta] = errPreferredLookup
	s := newTestState(p)
	s.ps.decide(rootPkg, rootVersion)
	for _, pkg := range []string{prefPkgBeta, prefPkgZeta} {
		s.ps.derive(term{Package: pkg, Set: mustSet(t, "^1.0.0"), Positive: true}, mustDummyCause(t, s))
	}

	pkg, ok, err := s.pickPackage(t.Context())
	if !errors.Is(err, errPreferredLookup) || !strings.Contains(err.Error(), "preferred version of "+prefPkgBeta) || ok || pkg != "" {
		t.Fatalf("pickPackage = (%q, %v, %v), want an error naming the preferred version of %s", pkg, ok, err, prefPkgBeta)
	}
}

// TestPreferredNeverAskedAboutTheRoot calls preferredVersion directly, since Solve
// decides the root as an exact pin before any preference: a provider answering
// for the root is never asked about it, while a real package still is.
func TestPreferredNeverAskedAboutTheRoot(t *testing.T) {
	t.Parallel()
	p := newPreferringProvider(newFakeProvider()).preferAll(map[string]string{
		rootPkg: rootVersionString, prefPkgFoo: prefVersion150,
	})
	s := newTestState(p)

	v, ok, err := s.preferredVersion(t.Context(), rootPkg)
	if err != nil || ok || v != (Version{}) {
		t.Fatalf("preferredVersion(root) = (%q, %v, %v), want no preference and no error", v.Original(), ok, err)
	}
	v, ok, err = s.preferredVersion(t.Context(), prefPkgFoo)
	if err != nil || !ok || v.Original() != prefVersion150 {
		t.Fatalf("control: preferredVersion(%s) = (%q, %v, %v), want (1.5.0, true, nil)", prefPkgFoo, v.Original(), ok, err)
	}
	if want := map[string]int{prefPkgFoo: 1}; !maps.Equal(p.preferredCalls, want) {
		t.Fatalf("Preferred calls = %v, want %v: the root is never asked", p.preferredCalls, want)
	}
}

// TestConfirmAskedOnlyForAPassingPreference pins that a preference the
// accumulation excludes is never confirmed, so the provider's check costs
// nothing there, while a passing one is confirmed once and decided.
func TestConfirmAskedOnlyForAPassingPreference(t *testing.T) {
	t.Parallel()
	p := newPreferringProvider(newFakeProvider().
		withVersions(prefPkgFoo, testVersion100, prefVersion150, prefVersion200, prefVersion250).
		withVersions(prefPkgBeta, testVersion100, prefVersion200)).
		preferAll(map[string]string{prefPkgFoo: prefVersion150, prefPkgBeta: testVersion100})
	reqs := []Requirement{{Package: prefPkgFoo, Constraint: ">=2.0.0"}, {Package: prefPkgBeta, Constraint: "*"}}

	res, err := Solve(t.Context(), reqs, p)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	want := Resolution{prefPkgFoo: prefVersion250, prefPkgBeta: testVersion100}
	if !maps.Equal(res.Versions, want) {
		t.Fatalf("Versions = %v, want %v", res.Versions, want)
	}
	if !maps.Equal(p.confirmCalls, map[string]int{prefPkgBeta: 1}) {
		t.Fatalf("Confirm calls = %v, want %s once and never the excluded %s", p.confirmCalls, prefPkgBeta, prefPkgFoo)
	}
}

// TestUnconfirmedPreferenceGivesWay pins Confirm answering false: the
// preference is dropped before its dependencies are asked, and the package is
// decided at its highest allowed version.
func TestUnconfirmedPreferenceGivesWay(t *testing.T) {
	t.Parallel()
	p := newPreferringProvider(newFakeProvider().
		withVersions(prefPkgFoo, testVersion100, prefVersion150, prefVersion200)).
		prefer(prefPkgFoo, prefVersion150)
	p.unconfirmed[prefPkgFoo] = true

	res, err := Solve(t.Context(), []Requirement{{Package: prefPkgFoo, Constraint: "*"}}, p)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if got := res.Versions[prefPkgFoo]; got != prefVersion200 {
		t.Fatalf("Versions[%s] = %q, want 2.0.0 past the unconfirmed preference", prefPkgFoo, got)
	}
	if got := p.confirmCalls[prefPkgFoo]; got != 1 {
		t.Fatalf("Confirm asked %d times for %s, want 1", got, prefPkgFoo)
	}
	if got := p.depsCalls[prefPkgFoo+"@"+prefVersion150]; got != 0 {
		t.Fatalf("Dependencies of the unconfirmed %s@1.5.0 asked %d times, want 0", prefPkgFoo, got)
	}
}

// TestConfirmErrorEndsTheSolve pins that a Confirm error ends the solve
// wrapped, naming the package, before its dependencies are asked.
func TestConfirmErrorEndsTheSolve(t *testing.T) {
	t.Parallel()
	p := newPreferringProvider(newFakeProvider().withVersions(prefPkgFoo, testVersion100, prefVersion200)).
		prefer(prefPkgFoo, testVersion100)
	p.confirmFailing[prefPkgFoo] = errPreferredLookup

	res, err := Solve(t.Context(), []Requirement{{Package: prefPkgFoo, Constraint: "*"}}, p)
	if res != nil || !errors.Is(err, errPreferredLookup) {
		t.Fatalf("Solve = (%v, %v), want no result and an error wrapping errPreferredLookup", res, err)
	}
	if !strings.Contains(err.Error(), "confirming preferred version of "+prefPkgFoo) {
		t.Fatalf("error = %q, want it to name the confirmation for %s", err, prefPkgFoo)
	}
	if got := p.totalDepsCalls(); got != 0 {
		t.Fatalf("Dependencies asked %d times after the error, want 0", got)
	}
}

// TestConfirmAskedOncePerPackage pins Confirm's memo: acme.alpha's preference
// is confirmed once, although the backjump that undoes its decision leaves the
// pick and the decision step to weigh it again.
func TestConfirmAskedOncePerPackage(t *testing.T) {
	t.Parallel()
	g := backjumpedPreferenceGraph()
	p := newPreferringProvider(g.provider()).prefer(prefPkgAlpha, testVersion100)
	if _, err := Solve(t.Context(), g.roots, p); err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if !maps.Equal(p.confirmCalls, map[string]int{prefPkgAlpha: 1}) {
		t.Fatalf("Confirm calls = %v, want %s once", p.confirmCalls, prefPkgAlpha)
	}
}

// TestConflictErrorNamesThePackagesOfItsProof pins Packages: every package the
// derivation names, sorted, and not one the conflict does not involve.
func TestConflictErrorNamesThePackagesOfItsProof(t *testing.T) {
	t.Parallel()
	g := generatedGraph{
		versions: map[string][]string{prefPkgFoo: {testVersion100}, prefPkgBeta: {testVersion100}, prefPkgZeta: {testVersion100}},
		deps:     map[string]map[string]string{prefPkgFoo + "@" + testVersion100: {prefPkgBeta: ">=2.0.0"}},
		pkgs:     []string{prefPkgFoo, prefPkgBeta, prefPkgZeta},
		roots:    []Requirement{{Package: prefPkgFoo, Constraint: "*"}, {Package: prefPkgZeta, Constraint: "*"}},
	}
	_, err := Solve(t.Context(), g.roots, g.provider())
	conflict, ok := errors.AsType[*ConflictError](err)
	if !ok {
		t.Fatalf("Solve error = %v, want a *ConflictError", err)
	}
	if got, want := conflict.Packages(), []string{prefPkgBeta, prefPkgFoo}; !slices.Equal(got, want) {
		t.Fatalf("Packages() = %q, want %q", got, want)
	}
}
