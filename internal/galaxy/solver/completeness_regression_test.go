package solver

import "testing"

// These fixtures are oracle-corpus graphs (generateGraph seeds 1623 and 2373
// at n=4, maxVersions=3) once falsely rejected, pinned as named providers;
// each has exactly one valid resolution.

// falseReject1623Provider builds a graph where only the dependency-free
// gen.p0@0.2.0 resolves, so the solver must backtrack through both higher
// gen.p0 versions instead of declaring the graph unsolvable.
func falseReject1623Provider() *fakeProvider {
	return newFakeProvider().
		withVersions("gen.p0", "0.2.0", "0.2.5", testVersion100).
		withVersions("gen.p1", "0.0.3").
		withVersions("gen.p2", "1.0.0-rc.1").
		withVersions("gen.p3", testVersion100, "1.0.0-rc.1", "1.5.0").
		withDeps("gen.p0", "0.2.5", map[string]string{"gen.p1": "!=1.5.0", "gen.p2": "*", "gen.p3": "!=1.5.0"}).
		withDeps("gen.p0", testVersion100, map[string]string{"gen.p3": "^0.0.3"}).
		withDeps("gen.p2", "1.0.0-rc.1", map[string]string{"gen.p3": "^0.2.0"})
}

func TestCompletenessRegressionSeed1623(t *testing.T) {
	t.Parallel()
	res, err := Solve(t.Context(), []Requirement{{Package: "gen.p0", Constraint: ">=0.2.0"}}, falseReject1623Provider())
	if err != nil {
		t.Fatalf("Solve false-rejected the solvable graph: %v", err)
	}
	if res.Versions["gen.p0"] != "0.2.0" || len(res.Versions) != 1 {
		t.Fatalf("Versions = %v, want exactly gen.p0=0.2.0 (the graph's only valid resolution)", res.Versions)
	}
}

// falseReject2373Provider builds a graph where only the dependency-free
// gen.p0@1.0.0 resolves: gen.p0@1.2.0 needs, through gen.p2, a gen.p3 1.x
// release, and gen.p3's only 1.x candidate is a prerelease "1.x" excludes.
func falseReject2373Provider() *fakeProvider {
	return newFakeProvider().
		withVersions("gen.p0", testVersion100, "1.2.0").
		withVersions("gen.p1", "0.0.3", "0.2.0", "1.0.0-rc.1").
		withVersions("gen.p2", testVersion100, "1.2.0").
		withVersions("gen.p3", "0.2.0", "0.2.5", "1.0.0-rc.1").
		withDeps("gen.p0", "1.2.0", map[string]string{"gen.p1": ">=1.0.0-0", "gen.p2": ">=0.2.0"}).
		withDeps("gen.p1", "0.0.3", map[string]string{"gen.p3": ">=1.0.0-0"}).
		withDeps("gen.p1", "0.2.0", map[string]string{"gen.p3": "*"}).
		withDeps("gen.p1", "1.0.0-rc.1", map[string]string{"gen.p3": "<2.0.0"}).
		withDeps("gen.p2", testVersion100, map[string]string{"gen.p3": "1.x"}).
		withDeps("gen.p2", "1.2.0", map[string]string{"gen.p3": "1.x"})
}

func TestCompletenessRegressionSeed2373(t *testing.T) {
	t.Parallel()
	res, err := Solve(t.Context(), []Requirement{{Package: "gen.p0", Constraint: ">=1.0.0-0"}}, falseReject2373Provider())
	if err != nil {
		t.Fatalf("Solve false-rejected the solvable graph: %v", err)
	}
	if res.Versions["gen.p0"] != testVersion100 || len(res.Versions) != 1 {
		t.Fatalf("Versions = %v, want exactly gen.p0=1.0.0 (the graph's only valid resolution)", res.Versions)
	}
}
