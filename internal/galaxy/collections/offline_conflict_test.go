package collections

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
)

// errTestUnrelatedFailure is an unclassified sentinel used only to prove
// annotateOfflineConflict leaves a non-conflict error alone.
var errTestUnrelatedFailure = errors.New("some unrelated failure")

// TestAnnotateOfflineConflictAddsNoteButStaysClassifiable pins that the
// offline note is added while errors.Is, errors.As and exitcode.FromError
// still classify the conflict, which flattening it with %v would break.
func TestAnnotateOfflineConflictAddsNoteButStaysClassifiable(t *testing.T) {
	t.Parallel()
	base := &solver.ConflictError{}
	wrapped := fmt.Errorf("resolve: %w", base)

	cfg := &config.Config{Offline: true}
	got := annotateOfflineConflict(cfg, wrapped)

	if !strings.Contains(got.Error(), offlineConflictNote) {
		t.Fatalf("Error() = %q, want it to contain the offline note %q", got.Error(), offlineConflictNote)
	}
	if _, ok := errors.AsType[*solver.ConflictError](got); !ok {
		t.Fatalf("errors.As(got, &*solver.ConflictError) = false, want true")
	}
	if !errors.Is(got, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("errors.Is(got, ErrNoVersionSatisfiesConstraints) = false, want true")
	}
	if code := exitcode.FromError(got); code != exitcode.ExitResolution {
		t.Fatalf("exitcode.FromError(got) = %d, want ExitResolution (%d)", code, exitcode.ExitResolution)
	}
}

// TestAnnotateOfflineConflictPassesThroughUnaffectedErrors pins that an online
// conflict, a non-conflict error under --offline, and nil come back as the
// exact same value.
func TestAnnotateOfflineConflictPassesThroughUnaffectedErrors(t *testing.T) {
	t.Parallel()
	conflictErr := fmt.Errorf("resolve: %w", &solver.ConflictError{})

	// Identity comparison, not errors.Is: errors.Is would still pass if the
	// value were wrapped again, which is the regression this test catches.
	//nolint:err113,errorlint // intentional identity comparison, not error-equality checking; see comment above
	if got := annotateOfflineConflict(&config.Config{Offline: false}, conflictErr); got != conflictErr {
		t.Fatalf("annotateOfflineConflict changed an online conflict error: got %v, want it unchanged", got)
	}
	//nolint:err113,errorlint // intentional identity comparison, not error-equality checking; see comment above
	if got := annotateOfflineConflict(&config.Config{Offline: true}, errTestUnrelatedFailure); got != errTestUnrelatedFailure {
		t.Fatalf("annotateOfflineConflict changed a non-conflict error under --offline: got %v, want it unchanged", got)
	}
	if got := annotateOfflineConflict(&config.Config{Offline: false}, nil); got != nil {
		t.Fatalf("annotateOfflineConflict(cfg, nil) = %v, want nil", got)
	}
}
