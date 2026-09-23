package solver

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestFuelGuard pins that exhausting a lowered fuelLimit returns the internal
// iteration-limit error, never a *ConflictError or an endless loop.
func TestFuelGuard(t *testing.T) {
	// Not parallel: fuelLimit is a shared package-level var.
	old := fuelLimit
	fuelLimit = 3
	defer func() { fuelLimit = old }()

	p := newFakeProvider().
		withVersions("a", "1.0.0").
		withVersions("b", "1.0.0").
		withVersions("c", "1.0.0").
		withVersions("d", "1.0.0").
		withDeps("a", "1.0.0", map[string]string{"b": "^1.0.0"}).
		withDeps("b", "1.0.0", map[string]string{"c": "^1.0.0"}).
		withDeps("c", "1.0.0", map[string]string{"d": "^1.0.0"})
	reqs := []Requirement{{Package: "a", Constraint: "^1.0.0"}}

	_, err := Solve(t.Context(), reqs, p)
	if err == nil {
		t.Fatalf("Solve succeeded despite a fuel limit of 3; want the iteration-limit error")
	}
	if errors.As(err, new(*ConflictError)) {
		t.Fatalf("fuel exhaustion must not surface as a *ConflictError: %v", err)
	}
	if !strings.Contains(err.Error(), "iteration limit exceeded") {
		t.Fatalf("error = %q, want it to mention the iteration limit", err.Error())
	}
}

// TestExactRelationDetectsDisjointRanges pins that disjoint positive ranges on
// one unfetched package relate as contradicted, and that the same conflict
// through two dependers ends a whole solve as a clean *ConflictError.
func TestExactRelationDetectsDisjointRanges(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	s.ps.decide(rootPkg, rootVersion)
	rangeA := term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}
	s.ps.derive(rangeA, mustDummyCause(t, s))

	rangeB := term{Package: "foo", Set: mustSet(t, "^2.0.0"), Positive: true}
	// P(^1.0.0) and P(^2.0.0) conjoin to P({}) with no universe fetched.
	if got := relation(rangeB, s.ps); got != termContradicted {
		t.Fatalf("relation(disjoint range against an unfetched package) = %v, want termContradicted (exact algebra)", got)
	}

	// Two dependers carry the ranges, since two different root constraints on
	// one package are rejected before they reach the core.
	p2 := newFakeProvider().
		withVersions("mid1", "1.0.0").
		withVersions("mid2", "1.0.0").
		withVersions("foo", "1.0.0", "2.0.0").
		withDeps("mid1", "1.0.0", map[string]string{"foo": "^1.0.0"}).
		withDeps("mid2", "1.0.0", map[string]string{"foo": "^2.0.0"})
	_, err := Solve(t.Context(), []Requirement{
		{Package: "mid1", Constraint: "^1.0.0"},
		{Package: "mid2", Constraint: "^1.0.0"},
	}, p2)
	if err == nil {
		t.Fatalf("Solve: expected a conflict (foo cannot be both ^1.0.0 and ^2.0.0 at once)")
	}
	if _, ok := errors.AsType[*ConflictError](err); !ok {
		t.Fatalf("Solve error is not a *ConflictError: %v (%T)", err, err)
	}
}

// TestUnsatisfiableDependencyBacktrackResolves pins that acme.foo@2.0.0's
// unsatisfiable dependency backtracks to acme.foo@1.0.0, whose "*" dependency
// must still land acme.bar in Versions.
func TestUnsatisfiableDependencyBacktrackResolves(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().
		withVersions("acme.foo", "1.0.0", "2.0.0").
		withVersions("acme.bar", "1.0.0").
		withDeps("acme.foo", "2.0.0", map[string]string{"acme.bar": ">=5.0.0"}).
		withDeps("acme.foo", "1.0.0", map[string]string{"acme.bar": "*"})

	res, err := Solve(t.Context(), []Requirement{{Package: "acme.foo", Constraint: ">=1.0.0"}}, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Versions["acme.foo"] != testVersion100 || res.Versions["acme.bar"] != testVersion100 {
		t.Fatalf("Versions = %v, want foo=1.0.0 bar=1.0.0 (the * dependency must resolve after the unsatisfiable-2.0.0 backtrack)", res.Versions)
	}
}

// TestExtractResultGuardFiresOnConstructedIncompleteResolution exercises the
// completeness guard directly: a decided package with a dependency edge to a
// package that was never decided must surface a loud internal-bug error.
func TestExtractResultGuardFiresOnConstructedIncompleteResolution(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	s.ps.decide(rootPkg, rootVersion)
	fooV := mustNewVersion(testVersion100)
	s.store.add(&incompatibility{
		Terms: []term{
			{Package: rootPkg, Set: singletonVerSet(rootVersion), Positive: true},
			{Package: "acme.foo", Set: fullVerSet(), Positive: false},
		},
		Cause: causeDependency{Parent: rootPkg, ParentVersion: rootVersion, Dep: "acme.foo", Constraint: "*"},
	})
	s.ps.decide("acme.foo", fooV)
	s.store.add(&incompatibility{
		Terms: []term{
			{Package: "acme.foo", Set: singletonVerSet(fooV), Positive: true},
			{Package: "acme.bar", Set: fullVerSet(), Positive: false},
		},
		Cause: causeDependency{Parent: "acme.foo", ParentVersion: fooV, Dep: "acme.bar", Constraint: "*"},
	})
	if _, err := s.extractResult(); err == nil || !errors.Is(err, errSolverBug) ||
		!strings.Contains(err.Error(), "acme.bar") {
		t.Fatalf("guard did not fire on a constructed incomplete resolution: err=%v", err)
	}
}

// conditionalityDropCase is one regression where foo@2.0.0's unsatisfiable
// constraintHi on bar leaves a residual after backtrack, and foo@1.0.0's
// constraintLo must still resolve bar to wantBar.
type conditionalityDropCase struct {
	name         string
	constraintHi string
	constraintLo string
	wantBar      string
	barVersions  []string
}

func conditionalityDropCases() []conditionalityDropCase {
	return []conditionalityDropCase{
		{"star-after-empty-collapse", ">=5.0.0", "*", testVersion100, []string{testVersion100}},
		{"star-after-boundary-collapse", "1.x", "*", "0.2.0", []string{"0.2.0"}},
		{"vacuous-neq-survivor", ">=5.0.0", "!=1.5.0", "1.2.5", []string{"0.2.3", "1.2.5"}},
		{"point-pinned-residual-survivor", "!=1.5.0", ">=1.0.0-0", "1.5.0", []string{"1.5.0"}},
	}
}

// TestConditionalityDropFamily covers the resolutions that a residual left by
// a backtracked, unsatisfiable parent version must still constrain, rather
// than being dropped once the parent version that introduced it is rejected.
func TestConditionalityDropFamily(t *testing.T) {
	t.Parallel()
	root := []Requirement{{Package: "acme.foo", Constraint: ">=1.0.0"}}
	for _, tc := range conditionalityDropCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newFakeProvider().
				withVersions("acme.foo", testVersion100, "2.0.0").
				withVersions("acme.bar", tc.barVersions...).
				withDeps("acme.foo", "2.0.0", map[string]string{"acme.bar": tc.constraintHi}).
				withDeps("acme.foo", testVersion100, map[string]string{"acme.bar": tc.constraintLo})
			res, err := Solve(t.Context(), root, p)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Versions["acme.foo"] != testVersion100 || res.Versions["acme.bar"] != tc.wantBar {
				t.Fatalf("Versions = %v, want foo=1.0.0 bar=%s", res.Versions, tc.wantBar)
			}
		})
	}
}

// TestTransitiveUnsatisfiableBacktrack pins that a root version whose
// transitive dependency is unsatisfiable is rejected and the solve backtracks
// to foo@1.2.0 instead of reporting a false conflict.
func TestTransitiveUnsatisfiableBacktrack(t *testing.T) {
	t.Parallel()
	const survivor = "1.2.0"
	p := newFakeProvider().
		withVersions("acme.foo", survivor, "2.0.0").
		withVersions("acme.mid", "1.5.0").
		withVersions("acme.leaf", survivor).
		withDeps("acme.foo", "2.0.0", map[string]string{"acme.mid": "1.x"}).
		withDeps("acme.foo", survivor, map[string]string{"acme.leaf": "<2.0.0"}).
		withDeps("acme.mid", "1.5.0", map[string]string{"acme.leaf": "^0.0.3"})
	res, err := Solve(t.Context(), []Requirement{{Package: "acme.foo", Constraint: "*"}}, p)
	if err != nil {
		t.Fatalf("unexpected error (was a false ConflictError before stage 1): %v", err)
	}
	if res.Versions["acme.foo"] != survivor || res.Versions["acme.leaf"] != survivor {
		t.Fatalf("Versions = %v, want foo=1.2.0 leaf=1.2.0", res.Versions)
	}
}

// TestTransitiveUnknownPackageBacktrack pins that acme.foo@2.0.0 depending on
// an unknown package is rejected and the solve settles on acme.foo@1.2.0
// rather than reporting a false conflict.
func TestTransitiveUnknownPackageBacktrack(t *testing.T) {
	t.Parallel()
	const survivor = "1.2.0"
	p := newFakeProvider().
		withVersions("acme.foo", survivor, "2.0.0").
		withDeps("acme.foo", "2.0.0", map[string]string{"acme.ghost": "^1.0.0"})
	res, err := Solve(t.Context(), []Requirement{{Package: "acme.foo", Constraint: "*"}}, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Versions["acme.foo"] != survivor {
		t.Fatalf("Versions = %v, want acme.foo=%s", res.Versions, survivor)
	}
}

// TestSolveStopsOnCanceledContext pins that an already-canceled ctx returns
// context.Canceled, not errSolverBug, before any provider call, while the same
// fixture under a live ctx resolves.
func TestSolveStopsOnCanceledContext(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().withVersions("acme.foo", "1.0.0", "2.0.0")
	reqs := []Requirement{{Package: "acme.foo", Constraint: ">=1.0.0"}}

	t.Run("canceled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res, err := Solve(ctx, reqs, p)

		if res != nil {
			t.Fatalf("Solve returned a non-nil result on an already-canceled context: %v", res.Versions)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Solve error = %v, want errors.Is(err, context.Canceled)", err)
		}
		if errors.Is(err, errSolverBug) {
			t.Fatalf("Solve error = %v, wraps errSolverBug; caller cancellation must never look like an internal-invariant defect", err)
		}
		if got := p.totalHighestCalls(); got != 0 {
			t.Fatalf("totalHighestCalls() = %d, want 0 (Solve must never reach the provider on an already-canceled context)", got)
		}
		// Shape check: no provider method was reached at all.
		if got := p.totalUniverseCalls(); got != 0 {
			t.Fatalf("totalUniverseCalls() = %d, want 0", got)
		}
		if got := p.totalDepsCalls(); got != 0 {
			t.Fatalf("totalDepsCalls() = %d, want 0", got)
		}
	})

	// "live" is the positive control: the same fixture resolves, so the zero
	// calls above come from the cancellation check.
	t.Run("live", func(t *testing.T) {
		t.Parallel()
		res, err := Solve(t.Context(), reqs, newFakeProvider().withVersions("acme.foo", "1.0.0", "2.0.0"))
		if err != nil {
			t.Fatalf("Solve: unexpected error: %v", err)
		}
		if res.Versions["acme.foo"] != "2.0.0" {
			t.Fatalf("Versions[acme.foo] = %q, want 2.0.0", res.Versions["acme.foo"])
		}
	})
}
