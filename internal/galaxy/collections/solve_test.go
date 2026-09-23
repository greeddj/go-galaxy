package collections

import (
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
)

// TestSolverResultSlotsIntoInstallLevels pins that solverResultToResolvedGraph
// output yields ns.name@version keys from buildCollectionsMap and leaf-first
// levels from buildInstallLevels over a three-deep transitive chain.
func TestSolverResultSlotsIntoInstallLevels(t *testing.T) {
	t.Parallel()
	result := &solver.Result{
		Versions: solver.Resolution{
			"a.b": "1.0.0",
			"c.d": "2.0.0",
			"e.f": "3.0.0",
		},
		Graph: map[string][]string{
			"a.b": {"c.d"},
			"c.d": {"e.f"},
			"e.f": {},
		},
	}
	cfg := &config.Config{Server: "https://galaxy.example"}

	resolved, graph, err := solverResultToResolvedGraph(result, cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("solverResultToResolvedGraph: unexpected error: %v", err)
	}

	collections, err := buildCollectionsMap(resolved)
	if err != nil {
		t.Fatalf("buildCollectionsMap: unexpected error: %v", err)
	}
	for _, wantKey := range []string{"a.b@1.0.0", "c.d@2.0.0", "e.f@3.0.0"} {
		if _, ok := collections[wantKey]; !ok {
			t.Fatalf("collections map missing key %q: %v", wantKey, collections)
		}
	}

	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels: unexpected error: %v", err)
	}
	want := [][]string{{"e.f@3.0.0"}, {"c.d@2.0.0"}, {"a.b@1.0.0"}}
	if len(levels) != len(want) {
		t.Fatalf("levels = %v, want %v", levels, want)
	}
	for i, level := range want {
		if len(levels[i]) != 1 || levels[i][0] != level[0] {
			t.Fatalf("levels[%d] = %v, want %v", i, levels[i], level)
		}
	}
}
