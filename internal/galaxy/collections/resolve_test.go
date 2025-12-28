package collections

import (
	"errors"
	"sort"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestBuildInstallLevels(t *testing.T) {
	t.Parallel()
	graph := map[string][]string{
		"A": {"B", "C"},
		"B": {"C"},
		"C": nil,
	}
	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels error: %v", err)
	}
	if len(levels) != 3 {
		t.Fatalf("expected 3 levels, got %d", len(levels))
	}
	assertLevel(t, levels[0], []string{"C"})
	assertLevel(t, levels[1], []string{"B"})
	assertLevel(t, levels[2], []string{"A"})
}

func TestBuildInstallLevelsCycle(t *testing.T) {
	t.Parallel()
	graph := map[string][]string{
		"A": {"B"},
		"B": {"A"},
	}
	_, err := buildInstallLevels(graph)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, helpers.ErrDependencyGraphHasACycle) {
		t.Fatalf("expected ErrDependencyGraphHasACycle, got %v", err)
	}
}

func assertLevel(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	gotCopy := append([]string(nil), got...)
	wantCopy := append([]string(nil), want...)
	sort.Strings(gotCopy)
	sort.Strings(wantCopy)
	for i := range gotCopy {
		if gotCopy[i] != wantCopy[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}
