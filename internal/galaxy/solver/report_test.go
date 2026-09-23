package solver

import (
	"errors"
	"fmt"
	"slices"
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
