package solver

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestRenderNodeRecordsNonDerivedIncompatibilityAsSolverBug pins that a
// derived node renders one line with no bug, while an external node handed to
// renderNode directly is recorded as an errSolverBug invariant violation.
func TestRenderNodeRecordsNonDerivedIncompatibilityAsSolverBug(t *testing.T) {
	t.Parallel()
	tm := term{Package: "foo", Set: mustSet(t, ">=1.0.0"), Positive: true}
	extA := &incompatibility{Terms: []term{tm}, Cause: causeNoVersions{term: tm}}
	extB := &incompatibility{Terms: []term{tm}, Cause: causeNoVersions{term: tm}}

	cases := []struct {
		inc       *incompatibility
		name      string
		wantLines int
		wantBug   bool
	}{
		{
			name:      "two distinct external causes render as an ordinary derived node",
			inc:       &incompatibility{Terms: []term{tm}, Cause: causeConflict{Left: extA, Right: extB}},
			wantLines: 1,
		},
		{
			name:    "an external cause reached directly is a solver invariant violation",
			inc:     extA,
			wantBug: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestState(newFakeProvider())
			b := newReportBuilder()
			b.renderNode(s, tc.inc, false)

			if tc.wantBug {
				if b.bug == nil {
					t.Fatalf("renderNode on a non-derived incompatibility left b.bug nil")
				}
				if !errors.Is(b.bug, errSolverBug) {
					t.Fatalf("b.bug = %v, want an error wrapping errSolverBug", b.bug)
				}
				// A recorded bug must not leave a partial proof line behind.
				if len(b.lines) != 0 {
					t.Fatalf("b.lines = %v, want empty", b.lines)
				}
				return
			}

			if b.bug != nil {
				t.Fatalf("renderNode on a derived incompatibility set b.bug = %v, want nil", b.bug)
			}
			if len(b.lines) != tc.wantLines {
				t.Fatalf("len(b.lines) = %d, want %d", len(b.lines), tc.wantLines)
			}
		})
	}
}

// TestReportBuilderOutcomePrefersRecordedBug pins outcome's dispatch: the
// built ConflictError when no bug is recorded, otherwise the recorded bug
// alone, with the partial proof discarded.
func TestReportBuilderOutcomePrefersRecordedBug(t *testing.T) {
	t.Parallel()
	tm := term{Package: "foo", Set: mustSet(t, ">=1.0.0"), Positive: true}
	inc := &incompatibility{Terms: []term{tm}, Cause: causeNoVersions{term: tm}}

	t.Run("no recorded bug returns the built ConflictError", func(t *testing.T) {
		t.Parallel()
		s := newTestState(newFakeProvider())
		b := newReportBuilder()
		b.lines = []string{"line"}

		err := b.outcome(s, inc)

		var ce *ConflictError
		if !errors.As(err, &ce) {
			t.Fatalf("outcome() = %v, want a *ConflictError", err)
		}
		if !slices.Equal(ce.ProofLines(), b.lines) {
			t.Fatalf("ProofLines() = %v, want %v", ce.ProofLines(), b.lines)
		}
	})

	t.Run("a recorded bug is returned instead of the built ConflictError", func(t *testing.T) {
		t.Parallel()
		s := newTestState(newFakeProvider())
		b := newReportBuilder()
		b.lines = []string{"line"}
		b.bug = fmt.Errorf("x: %w", errSolverBug)

		err := b.outcome(s, inc)

		if !errors.Is(err, errSolverBug) {
			t.Fatalf("outcome() = %v, want an error wrapping errSolverBug", err)
		}
		if _, ok := errors.AsType[*ConflictError](err); ok {
			t.Fatalf("outcome() = %v, want not a *ConflictError", err)
		}
	})
}

// phraseCase is one describe row: an incompatibility of terms under cause,
// described once foo's universe is fetched as fooUniverse, when that is set.
type phraseCase struct {
	cause       cause
	name        string
	want        string
	terms       []term
	fooUniverse []string
}

// posTerm builds a positive term for the phrase tables.
func posTerm(pkg string, set verSet) term { return term{Package: pkg, Set: set, Positive: true} }

// negTerm builds a negative term for the phrase tables.
func negTerm(pkg string, set verSet) term { return term{Package: pkg, Set: set} }

// singleTermPhraseCases are the zero- and one-term rows: a lone positive term
// is forbidden, a lone negative term required, and a positive root term (or
// no term at all) is the failure itself.
func singleTermPhraseCases(t *testing.T) []phraseCase {
	t.Helper()
	root, one, full := singletonVerSet(rootVersion), singletonVerSet(mustV(t, testVersion100)), fullVerSet()
	atLeast1, atLeast2 := mustSet(t, ">=1.0.0"), mustSet(t, ">=2.0.0")
	covered := []string{testVersion100, "2.0.0"}
	return []phraseCase{
		{name: "no terms", want: "version solving failed"},
		{name: "positive root", terms: []term{posTerm(rootPkg, root)}, want: "version solving failed"},
		{name: "negative root", cause: causeRoot{}, terms: []term{negTerm(rootPkg, root)}, want: "root is required"},
		{name: "positive range", terms: []term{posTerm("foo", atLeast2)}, want: "foo >=2.0.0 is forbidden"},
		{name: "negative range", terms: []term{negTerm("foo", atLeast2)}, want: "foo >=2.0.0 is required"},
		{name: "positive version", terms: []term{posTerm("foo", one)}, want: "foo 1.0.0 is forbidden"},
		{name: "negative version", terms: []term{negTerm("foo", one)}, want: "foo 1.0.0 is required"},
		{name: "positive full set", terms: []term{posTerm("foo", full)}, want: "every version of foo is forbidden"},
		{name: "negative full set", terms: []term{negTerm("foo", full)}, want: "foo is required"},
		{
			name: "positive range covering the universe", fooUniverse: covered,
			terms: []term{posTerm("foo", atLeast1)}, want: "every version of foo is forbidden",
		},
		{
			name: "negative range covering the universe keeps its range", fooUniverse: covered,
			terms: []term{negTerm("foo", atLeast1)}, want: "foo >=1.0.0 is required",
		},
	}
}

// multiTermPhraseCases are the two-term rows, one per pair of polarities in
// either order, and the three-or-more-term rows, which list each polarity.
func multiTermPhraseCases(t *testing.T) []phraseCase {
	t.Helper()
	one, two := singletonVerSet(mustV(t, testVersion100)), singletonVerSet(mustV(t, "2.0.0"))
	atLeast1, atLeast2, atLeast3 := mustSet(t, ">=1.0.0"), mustSet(t, ">=2.0.0"), mustSet(t, ">=3.0.0")
	return []phraseCase{
		{
			name: "positive then negative", terms: []term{posTerm("bar", one), negTerm("foo", atLeast2)},
			want: "bar 1.0.0 requires foo >=2.0.0",
		},
		{
			name: "negative then positive", terms: []term{negTerm("bar", atLeast3), posTerm("foo", one)},
			want: "foo 1.0.0 requires bar >=3.0.0",
		},
		{
			name: "two positive", terms: []term{posTerm("bar", one), posTerm("foo", two)},
			want: "bar 1.0.0 is incompatible with foo 2.0.0",
		},
		{
			name: "two negative", terms: []term{negTerm("bar", atLeast1), negTerm("foo", mustSet(t, "<2.0.0"))},
			want: "either bar >=1.0.0 or foo <2.0.0 is required",
		},
		{
			name: "three positive", terms: []term{posTerm("a", one), posTerm("b", one), posTerm("c", two)},
			want: "a 1.0.0, b 1.0.0 and c 2.0.0 are incompatible",
		},
		{
			name: "three negative", terms: []term{negTerm("a", atLeast1), negTerm("b", atLeast2), negTerm("c", atLeast3)},
			want: "one of a >=1.0.0, b >=2.0.0 or c >=3.0.0 is required",
		},
		{
			name: "one positive, two negative", terms: []term{posTerm("a", one), negTerm("b", atLeast2), negTerm("c", atLeast3)},
			want: "a 1.0.0 requires b >=2.0.0 or c >=3.0.0",
		},
		{
			name:  "one positive, three negative",
			terms: []term{posTerm("a", one), negTerm("b", atLeast1), negTerm("c", atLeast2), negTerm("d", atLeast3)},
			want:  "a 1.0.0 requires b >=1.0.0, c >=2.0.0 or d >=3.0.0",
		},
		{
			name:  "two positive, two negative",
			terms: []term{posTerm("a", one), posTerm("b", two), negTerm("c", atLeast2), negTerm("d", atLeast3)},
			want:  "a 1.0.0 and b 2.0.0 require c >=2.0.0 or d >=3.0.0",
		},
		{
			name:  "three positive, one negative",
			terms: []term{posTerm("a", one), posTerm("b", one), posTerm("c", two), negTerm("d", atLeast3)},
			want:  "a 1.0.0, b 1.0.0 and c 2.0.0 require d >=3.0.0",
		},
	}
}

// externalPhraseCases are the rows whose cause phrases itself, including a
// no-versions leaf over each positiveFormSet arm, whose set carries no
// display of its own and must still render its range.
func externalPhraseCases(t *testing.T) []phraseCase {
	t.Helper()
	arithmetic := mustSet(t, ">=1.0.0").intersect(mustSet(t, "<2.0.0"))
	fromPositive := posTerm("foo", positiveFormSet(posTerm("foo", arithmetic)))
	fromNegative := posTerm("foo", positiveFormSet(negTerm("foo", mustSet(t, "<1.0.0 || >=2.0.0"))))
	for _, tm := range []term{fromPositive, fromNegative} {
		if tm.Set.display != "" {
			t.Fatalf("positiveFormSet display = %q, want empty so the row covers the rendered label", tm.Set.display)
		}
	}
	constraintBuilt := posTerm("foo", mustSet(t, ">=2.0.0"))
	dep := causeDependency{Parent: "bar", ParentVersion: mustV(t, testVersion100), Dep: "foo", Constraint: ">=2.0.0"}
	rootDep := causeDependency{Parent: rootPkg, ParentVersion: rootVersion, Dep: "foo"}
	return []phraseCase{
		{name: "no versions, constraint-built set", cause: causeNoVersions{term: constraintBuilt}, want: "no version of foo matches >=2.0.0"},
		{
			name: "no versions, positive accumulation", cause: causeNoVersions{term: fromPositive},
			want: "no version of foo matches >=1.0.0,<2.0.0",
		},
		{
			name: "no versions, negative accumulation", cause: causeNoVersions{term: fromNegative},
			want: "no version of foo matches >=1.0.0,<2.0.0 (all prereleases)",
		},
		{name: "dependency", cause: dep, want: "bar 1.0.0 depends on foo >=2.0.0"},
		{name: "root dependency, unconstrained", cause: rootDep, want: "root depends on foo *"},
		{name: "unknown package", cause: causeUnknownPackage{Package: "foo"}, want: "foo has no published versions"},
	}
}

// TestDescribePhrasesEveryShape pins describe's wording for every term shape
// and external cause: an incompatibility's terms cannot all hold, so each
// polarity reads the opposite way round from the term itself.
func TestDescribePhrasesEveryShape(t *testing.T) {
	t.Parallel()
	cases := slices.Concat(singleTermPhraseCases(t), multiTermPhraseCases(t), externalPhraseCases(t))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestState(newFakeProvider())
			if tc.fooUniverse != nil {
				s.uniFor("foo").setVersions(buildTestUniverse(t, tc.fooUniverse...))
			}
			c := tc.cause
			if c == nil {
				c = causeConflict{}
			}
			got := s.describe(&incompatibility{Terms: tc.terms, Cause: c})
			if got != tc.want {
				t.Fatalf("describe() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "  ") || strings.TrimSpace(got) != got {
				t.Fatalf("describe() = %q, want single spacing and no edge spaces", got)
			}
		})
	}
}

// requiredDependencyProvider models a collection whose every release in
// range needs a newer dependency than root allows, the shape a real
// ansible.netcommon >=8.7.0 with ansible.utils <3.0.0 hits.
func requiredDependencyProvider() *fakeProvider {
	return newFakeProvider().
		withVersions("ansible.netcommon", "8.6.0", "8.7.0", "8.7.1").
		withVersions("ansible.utils", "2.12.0", "3.0.0", "5.1.0").
		withDeps("ansible.netcommon", "8.7.0", map[string]string{"ansible.utils": ">=3.0.0"}).
		withDeps("ansible.netcommon", "8.7.1", map[string]string{"ansible.utils": ">=3.0.0"})
}

// TestConflictProofStatesRequiredDependencyRange solves that shape end to end
// and pins every proof line whole, so each term reads with its own polarity:
// the dependency range required, never forbidden, and the leaf by its range.
func TestConflictProofStatesRequiredDependencyRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		netcommon string
		want      []string
	}{
		{
			name:      "a range over several releases",
			netcommon: ">=8.7.0",
			want: []string{
				"Because no version of ansible.netcommon matches >=8.7.2 and ansible.netcommon 8.7.0 depends on ansible.utils >=3.0.0," +
					" ansible.netcommon =8.7.0 || >=8.7.2 requires ansible.utils >=3.0.0.",
				"And because ansible.netcommon 8.7.1 depends on ansible.utils >=3.0.0, ansible.netcommon >=8.7.0 requires ansible.utils >=3.0.0.",
				"So, because root depends on ansible.netcommon >=8.7.0 and root depends on ansible.utils <3.0.0, version solving failed.",
			},
		},
		{
			name:      "an exact pin",
			netcommon: "8.7.1",
			want: []string{
				"Because ansible.netcommon 8.7.1 depends on ansible.utils >=3.0.0 and root depends on ansible.netcommon 8.7.1," +
					" ansible.utils >=3.0.0 is required.",
				"So, because root depends on ansible.utils <3.0.0, version solving failed.",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reqs := []Requirement{
				{Package: "ansible.netcommon", Constraint: tc.netcommon},
				{Package: "ansible.utils", Constraint: "<3.0.0"},
			}
			_, err := Solve(t.Context(), reqs, requiredDependencyProvider())
			var conflictErr *ConflictError
			if !errors.As(err, &conflictErr) {
				t.Fatalf("Solve error = %v, want a *ConflictError", err)
			}
			proof := strings.Join(conflictErr.ProofLines(), "\n")
			for _, want := range tc.want {
				if !slices.Contains(conflictErr.ProofLines(), want) {
					t.Fatalf("proof has no line %q:\n%s", want, proof)
				}
			}
			for _, never := range []string{"ansible.utils >=3.0.0 is forbidden", "  "} {
				if strings.Contains(proof, never) {
					t.Fatalf("proof contains %q:\n%s", never, proof)
				}
			}
		})
	}
}
