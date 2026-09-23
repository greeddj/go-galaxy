package solver

import "testing"

// TestFullConstraintInvisibilityBacktrack pins that a "*" dependency stays a
// visible contributor: gen.p0@1.5.0 reaches an unsatisfiable gen.p2 through
// one, so the solver must backtrack to gen.p0@1.2.0, not reject the graph.
func TestFullConstraintInvisibilityBacktrack(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().
		withVersions("gen.p0", "1.2.0", "1.5.0").
		withVersions("gen.p1", "1.5.0").
		withVersions("gen.p2", "0.2.5").
		withDeps("gen.p0", "1.5.0", map[string]string{"gen.p1": "*"}).
		withDeps("gen.p0", "1.2.0", map[string]string{"gen.p2": "*"}).
		withDeps("gen.p1", "1.5.0", map[string]string{"gen.p2": "^0.0.3"})
	res, err := Solve(t.Context(), []Requirement{{Package: "gen.p0", Constraint: "*"}}, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Versions["gen.p0"] != "1.2.0" || res.Versions["gen.p2"] != "0.2.5" {
		t.Fatalf("Versions = %v, want gen.p0=1.2.0 gen.p2=0.2.5", res.Versions)
	}
}

// TestResultExcludesBacktrackedOverInstall pins that the result is only what
// root reaches through decided edges: a gen.p3 decision left from an
// abandoned gen.p0 branch must not linger as an over-install.
func TestResultExcludesBacktrackedOverInstall(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().
		withVersions("gen.p0", "0.2.0", "1.0.0", "2.0.0").
		withVersions("gen.p1", "0.2.5").
		withVersions("gen.p2", "1.2.0").
		withVersions("gen.p3", "2.0.0").
		withDeps("gen.p0", "1.0.0", map[string]string{"gen.p1": "<2.0.0", "gen.p2": "^0.2.0"}).
		withDeps("gen.p0", "2.0.0", map[string]string{"gen.p1": ">=0.2.0", "gen.p3": "<2.0.0"}).
		withDeps("gen.p1", "0.2.5", map[string]string{"gen.p3": "<2.0.0"}).
		withDeps("gen.p2", "1.2.0", map[string]string{"gen.p3": "!=1.5.0"})
	res, err := Solve(t.Context(), []Requirement{{Package: "gen.p0", Constraint: ">=0.2.0"}}, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Versions) != 1 || res.Versions["gen.p0"] != "0.2.0" {
		t.Fatalf("Versions = %v, want exactly {gen.p0:0.2.0} (no over-installed gen.p3)", res.Versions)
	}
}
