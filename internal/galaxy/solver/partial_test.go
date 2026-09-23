package solver

import (
	"errors"
	"testing"
)

// backtrackToFixture appends one decision-shaped assignment for "foo" wrapping
// set directly, so decisionVersion stays unset, and returns foo's
// *packageAssignments as captured before any backtrackTo.
func backtrackToFixture(t *testing.T, set verSet) (*solveState, *packageAssignments) {
	t.Helper()
	s := newTestState(newFakeProvider())
	s.ps.append(term{Package: "foo", Set: set, Positive: true}, 1, -1)
	return s, s.ps.packages["foo"]
}

// TestPartialSolutionBacktrackTo pins that backtrackTo's rebuild restores a
// singleton decision's version, and refuses a non-singleton decision term
// with an errSolverBug error that leaves the package map untouched.
func TestPartialSolutionBacktrackTo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		set     verSet
		wantErr bool
	}{
		{
			name: "singleton decision term rebuilds decisionVersion",
			set:  singletonVerSet(mustV(t, "1.0.0")),
		},
		{
			name:    "non-singleton decision term reports an invariant error",
			set:     mustSet(t, ">=1.0.0"),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, before := backtrackToFixture(t, tc.set)

			if tc.wantErr {
				err := s.ps.backtrackTo(1)
				if err == nil {
					t.Fatalf("backtrackTo(1) = nil, want an error wrapping errSolverBug")
				}
				if !errors.Is(err, errSolverBug) {
					t.Fatalf("backtrackTo(1) error = %v, want one wrapping errSolverBug", err)
				}
				// A failed rebuild must not replace the live package map.
				if s.ps.packages["foo"] != before {
					t.Fatalf("backtrackTo(1) replaced packages[%q] despite returning an error", "foo")
				}
				return
			}

			// append leaves decisionVersion unset, so the rebuild is what
			// establishes it.
			if got := s.ps.pkgState("foo").decisionVersion.Original(); got != "" {
				t.Fatalf("decisionVersion before backtrackTo = %q, want empty", got)
			}
			if err := s.ps.backtrackTo(1); err != nil {
				t.Fatalf("backtrackTo(1): %v", err)
			}
			if got := s.ps.pkgState("foo").decisionVersion.Original(); got != "1.0.0" {
				t.Fatalf("decisionVersion after backtrackTo = %q, want %q", got, "1.0.0")
			}
		})
	}
}
