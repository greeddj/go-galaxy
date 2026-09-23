package solver

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// noConflictsProvider builds the reference spec's "No Conflicts" example,
// with bar's highest pointed at the resolvable version so a solve that never
// backtracks is shown to need no Universe fetch either.
func noConflictsProvider() *fakeProvider {
	return newFakeProvider().
		withVersions("foo", testVersion100).
		withVersions("bar", testVersion100, "2.0.0").
		withDeps("foo", testVersion100, map[string]string{"bar": "^1.0.0"}).
		withHighest("bar", testVersion100)
}

func TestFixtureNoConflicts(t *testing.T) {
	t.Parallel()
	p := noConflictsProvider()
	reqs := []Requirement{{Package: "foo", Constraint: "^1.0.0"}}

	result, err := Solve(t.Context(), reqs, p)
	if err != nil {
		t.Fatalf("Solve: unexpected error: %v", err)
	}
	want := Resolution{"foo": testVersion100, "bar": testVersion100}
	if len(result.Versions) != len(want) || result.Versions["foo"] != want["foo"] || result.Versions["bar"] != want["bar"] {
		t.Fatalf("Versions = %v, want %v", result.Versions, want)
	}
	if got := p.totalUniverseCalls(); got != 0 {
		t.Fatalf("totalUniverseCalls = %d, want 0 (laziness violated)", got)
	}
}

// avoidingConflictProvider builds "Avoiding Conflict During Decision
// Making": root needs foo ^1.0.0 and bar ^1.0.0, and foo 1.1.0 needs bar
// ^2.0.0.
func avoidingConflictProvider() *fakeProvider {
	return newFakeProvider().
		withVersions("foo", testVersion100, "1.1.0").
		withVersions("bar", testVersion100, "1.1.0", "2.0.0").
		withDeps("foo", "1.1.0", map[string]string{"bar": "^2.0.0"})
}

func TestFixtureAvoidingConflict(t *testing.T) {
	t.Parallel()
	p := avoidingConflictProvider()
	reqs := []Requirement{
		{Package: "bar", Constraint: "^1.0.0"},
		{Package: "foo", Constraint: "^1.0.0"},
	}

	result, err := Solve(t.Context(), reqs, p)
	if err != nil {
		t.Fatalf("Solve: unexpected error: %v", err)
	}
	// Only the final resolution is asserted, not the order of decisions
	// that reached it.
	if result.Versions["foo"] != testVersion100 || result.Versions["bar"] != "1.1.0" {
		t.Fatalf("Versions = %v, want foo=1.0.0 bar=1.1.0", result.Versions)
	}
}

// conflictResolutionProvider builds "Performing Conflict Resolution": root
// depends on foo >=1.0.0; foo 2.0.0 depends on bar ^1.0.0; foo 1.0.0 has no
// dependencies; bar 1.0.0 depends on foo ^1.0.0.
func conflictResolutionProvider() *fakeProvider {
	return newFakeProvider().
		withVersions("foo", testVersion100, "2.0.0").
		withVersions("bar", testVersion100).
		withDeps("foo", "2.0.0", map[string]string{"bar": "^1.0.0"}).
		withDeps("bar", testVersion100, map[string]string{"foo": "^1.0.0"})
}

func TestFixturePerformingConflictResolution(t *testing.T) {
	t.Parallel()
	p := conflictResolutionProvider()
	reqs := []Requirement{{Package: "foo", Constraint: ">=1.0.0"}}

	result, err := Solve(t.Context(), reqs, p)
	if err != nil {
		t.Fatalf("Solve: unexpected error: %v", err)
	}
	if len(result.Versions) != 1 || result.Versions["foo"] != testVersion100 {
		t.Fatalf("Versions = %v, want exactly {foo: 1.0.0} (bar must never be decided)", result.Versions)
	}
}

// partialSatisfierProvider builds "Conflict Resolution With a Partial
// Satisfier".
func partialSatisfierProvider() *fakeProvider {
	return newFakeProvider().
		withVersions("foo", testVersion100, "1.1.0").
		withVersions("left", testVersion100).
		withVersions("right", testVersion100).
		withVersions("shared", testVersion100, "2.0.0").
		withVersions("target", testVersion100, "2.0.0").
		withDeps("foo", "1.1.0", map[string]string{"left": "^1.0.0", "right": "^1.0.0"}).
		withDeps("left", testVersion100, map[string]string{"shared": ">=1.0.0"}).
		withDeps("right", testVersion100, map[string]string{"shared": "<2.0.0"}).
		withDeps("shared", testVersion100, map[string]string{"target": "^1.0.0"})
}

func TestFixturePartialSatisfier(t *testing.T) {
	t.Parallel()
	p := partialSatisfierProvider()
	reqs := []Requirement{
		{Package: "foo", Constraint: "^1.0.0"},
		{Package: "target", Constraint: "^2.0.0"},
	}

	result, err := Solve(t.Context(), reqs, p)
	if err != nil {
		t.Fatalf("Solve: unexpected error: %v", err)
	}
	if len(result.Versions) != 2 || result.Versions["foo"] != testVersion100 || result.Versions["target"] != "2.0.0" {
		t.Fatalf("Versions = %v, want exactly {foo: 1.0.0, target: 2.0.0}", result.Versions)
	}
}

// linearErrorProvider builds "Linear Error Reporting": root depends on foo
// ^1.0.0 and baz ^1.0.0; foo 1.0.0 depends on bar ^2.0.0; bar 2.0.0 depends
// on baz ^3.0.0; baz 1.0.0 and 3.0.0 have no dependencies.
func linearErrorProvider() *fakeProvider {
	return newFakeProvider().
		withVersions("foo", testVersion100).
		withVersions("bar", "2.0.0").
		withVersions("baz", testVersion100, "3.0.0").
		withDeps("foo", testVersion100, map[string]string{"bar": "^2.0.0"}).
		withDeps("bar", "2.0.0", map[string]string{"baz": "^3.0.0"})
}

func TestFixtureLinearErrorReporting(t *testing.T) {
	t.Parallel()
	p := linearErrorProvider()
	reqs := []Requirement{
		{Package: "foo", Constraint: "^1.0.0"},
		{Package: "baz", Constraint: "^1.0.0"},
	}

	_, err := Solve(t.Context(), reqs, p)
	if err == nil {
		t.Fatalf("Solve: expected a conflict, got a resolution")
	}
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("Solve error is not a *ConflictError: %v (%T)", err, err)
	}
	if !errors.Is(err, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("errors.Is(err, ErrNoVersionSatisfiesConstraints) = false")
	}

	// The proof may establish the conflict through bar and baz without
	// restating foo; what is pinned is that a real conflict is proven and the
	// final line has the standard shape.
	proof := strings.Join(conflictErr.ProofLines(), "\n")
	requireContains(t, proof, "bar")
	requireContains(t, proof, "baz")
	last := lastNonEmpty(conflictErr.ProofLines())
	if !strings.HasPrefix(last, "So,") {
		t.Fatalf("final proof line = %q, want a prefix of \"So,\"", last)
	}
	if !strings.HasSuffix(last, "version solving failed.") {
		t.Fatalf("final proof line = %q, want a suffix of \"version solving failed.\"", last)
	}
}

// branchingErrorProvider builds "Branching Error Reporting".
func branchingErrorProvider() *fakeProvider {
	return newFakeProvider().
		withVersions("foo", testVersion100, "1.1.0").
		withVersions("a", testVersion100).
		withVersions("b", testVersion100, "2.0.0").
		withVersions("x", testVersion100).
		withVersions("y", testVersion100, "2.0.0").
		withDeps("foo", testVersion100, map[string]string{"a": "^1.0.0", "b": "^1.0.0"}).
		withDeps("foo", "1.1.0", map[string]string{"x": "^1.0.0", "y": "^1.0.0"}).
		withDeps("a", testVersion100, map[string]string{"b": "^2.0.0"}).
		withDeps("x", testVersion100, map[string]string{"y": "^2.0.0"})
}

func TestFixtureBranchingErrorReporting(t *testing.T) {
	t.Parallel()
	p := branchingErrorProvider()
	reqs := []Requirement{{Package: "foo", Constraint: "^1.0.0"}}

	_, err := Solve(t.Context(), reqs, p)
	if err == nil {
		t.Fatalf("Solve: expected a conflict, got a resolution")
	}
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("Solve error is not a *ConflictError: %v (%T)", err, err)
	}
	if !errors.Is(err, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("errors.Is(err, ErrNoVersionSatisfiesConstraints) = false")
	}

	lines := conflictErr.ProofLines()
	proof := strings.Join(lines, "\n")
	for _, want := range []string{"foo", "a ", "b ", "x ", "y "} {
		requireContains(t, proof, strings.TrimSpace(want))
	}
	requireBranchingShape(t, lines, proof)
}

// requireBranchingShape asserts a branching derivation graph's markers, a
// paragraph break and a numbered back-reference, plus the standard final
// line.
func requireBranchingShape(t *testing.T, lines []string, proof string) {
	t.Helper()
	hasBlank, hasLineNumber := scanProofShape(lines)
	if !hasBlank {
		t.Fatalf("proof has no paragraph break; want the branching (non-linear) derivation graph to force one:\n%s", proof)
	}
	if !hasLineNumber {
		t.Fatalf("proof has no numbered back-reference; want a node referenced by two derivations to be numbered:\n%s", proof)
	}
	last := lastNonEmpty(lines)
	if !strings.HasPrefix(last, "So,") || !strings.HasSuffix(last, "version solving failed.") {
		t.Fatalf("final proof line = %q, want prefix \"So,\" and suffix \"version solving failed.\"", last)
	}
}

// scanProofShape reports whether lines contains a blank paragraph-break
// entry and a "(1)" numbered back-reference.
func scanProofShape(lines []string) (bool, bool) {
	hasBlank := false
	hasLineNumber := false
	for _, l := range lines {
		if l == "" {
			hasBlank = true
		}
		if strings.Contains(l, "(1)") {
			hasLineNumber = true
		}
	}
	return hasBlank, hasLineNumber
}

// TestFixtureUnknownPackage pins that solving for a package with no
// published versions ends in a *ConflictError through CauseUnknownPackage,
// never an internal-invariant defect.
func TestFixtureUnknownPackage(t *testing.T) {
	t.Parallel()
	p := newFakeProvider() // "ghost" has no registered versions
	reqs := []Requirement{{Package: "ghost", Constraint: "^1.0.0"}}

	_, err := Solve(t.Context(), reqs, p)
	if err == nil {
		t.Fatalf("Solve: expected a conflict for an unknown package, got a resolution")
	}
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("Solve error is not a *ConflictError: %v (%T)", err, err)
	}
	if !errors.Is(err, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("errors.Is(err, ErrNoVersionSatisfiesConstraints) = false")
	}
	proof := strings.Join(conflictErr.ProofLines(), "\n")
	requireContains(t, proof, "ghost has no published versions")
	last := lastNonEmpty(conflictErr.ProofLines())
	if !strings.HasPrefix(last, "So,") || !strings.HasSuffix(last, "version solving failed.") {
		t.Fatalf("final proof line = %q, want prefix \"So,\" and suffix \"version solving failed.\"", last)
	}
}

// unknownPackageTransitiveProvider builds a provider where root's own
// dependency resolves fine, but that dependency in turn requires a package
// the provider has never heard of.
func unknownPackageTransitiveProvider() *fakeProvider {
	return newFakeProvider().
		withVersions("foo", "1.0.0").
		withDeps("foo", "1.0.0", map[string]string{"ghost": "^2.0.0"})
}

// TestFixtureUnknownPackageTransitive pins that a transitively required
// unknown package yields a *ConflictError whose proof states the
// no-published-versions leaf once, not duplicated.
func TestFixtureUnknownPackageTransitive(t *testing.T) {
	t.Parallel()
	p := unknownPackageTransitiveProvider()
	_, err := Solve(t.Context(), []Requirement{{Package: "foo", Constraint: "^1.0.0"}}, p)
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("Solve error is not a *ConflictError: %v (%T)", err, err)
	}
	if !errors.Is(err, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("errors.Is(err, ErrNoVersionSatisfiesConstraints) = false")
	}
	proof := strings.Join(conflictErr.ProofLines(), "\n")
	requireContains(t, proof, "ghost has no published versions")
	if strings.Contains(proof, "versions and ghost has no published versions") {
		t.Fatalf("proof still duplicates the no-versions leaf:\n%s", proof)
	}
	last := lastNonEmpty(conflictErr.ProofLines())
	if !strings.HasPrefix(last, "So,") || !strings.HasSuffix(last, "version solving failed.") {
		t.Fatalf("final proof line = %q, want prefix \"So,\" and suffix \"version solving failed.\"", last)
	}
}

// TestHintPrereleaseOnly covers the prerelease-only hint: a package that
// publishes exclusively prereleases can never satisfy a plain constraint.
func TestHintPrereleaseOnly(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().withVersions("foo", "1.0.0-rc1", "1.0.0-rc2")
	reqs := []Requirement{{Package: "foo", Constraint: ">=1.0.0"}}

	_, err := Solve(t.Context(), reqs, p)
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("Solve error is not a *ConflictError: %v", err)
	}
	hints := conflictErr.Hints()
	if len(hints) != 1 {
		t.Fatalf("Hints() = %v, want exactly one hint", hints)
	}
	want := "foo publishes only pre-release versions, which plain constraints exclude; " +
		"if a pre-release is acceptable, pin one exactly or use a >=X.Y.Z-0 floor - " +
		"otherwise no published version can satisfy this requirement"
	if hints[0] != want {
		t.Fatalf("Hints()[0] = %q, want %q", hints[0], want)
	}
}

// TestHintPrereleasesExcluded covers the typical prereleases-excluded hint:
// a prerelease exists that would satisfy the constraint if a -0 floor were
// used, but a plain constraint excludes it.
func TestHintPrereleasesExcluded(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().withVersions("foo", testVersion100, "2.0.0-rc1")
	reqs := []Requirement{{Package: "foo", Constraint: ">=1.5.0"}}

	_, err := Solve(t.Context(), reqs, p)
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("Solve error is not a *ConflictError: %v", err)
	}
	hints := conflictErr.Hints()
	if len(hints) != 1 {
		t.Fatalf("Hints() = %v, want exactly one hint", hints)
	}
	want := "pre-release versions of foo exist and are excluded by plain constraints; " +
		"if you intended to allow them, use a >=X.Y.Z-0 floor or an exact pin"
	if hints[0] != want {
		t.Fatalf("Hints()[0] = %q, want %q", hints[0], want)
	}
}

func requireContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("proof %q does not contain %q", haystack, needle)
	}
}

func lastNonEmpty(lines []string) string {
	for _, line := range slices.Backward(lines) {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

// determinismCase is one TestDeterminism fixture: a provider builder plus its
// requirements, and whether it is expected to fail with a conflict.
type determinismCase struct {
	name    string
	build   func() *fakeProvider
	reqs    []Requirement
	wantErr bool
}

// TestDeterminism runs every fixture 100 times on a rebuilt, shuffling
// provider and asserts byte-identical resolutions and, for failing
// fixtures, byte-identical proofs.
func TestDeterminism(t *testing.T) {
	t.Parallel()

	cases := []determinismCase{
		{name: "no-conflicts", build: noConflictsProvider, reqs: []Requirement{{Package: "foo", Constraint: "^1.0.0"}}},
		{
			name: "avoiding-conflict", build: avoidingConflictProvider,
			reqs: []Requirement{{Package: "bar", Constraint: "^1.0.0"}, {Package: "foo", Constraint: "^1.0.0"}},
		},
		{
			name: "conflict-resolution", build: conflictResolutionProvider,
			reqs: []Requirement{{Package: "foo", Constraint: ">=1.0.0"}},
		},
		{
			name: "partial-satisfier", build: partialSatisfierProvider,
			reqs: []Requirement{{Package: "foo", Constraint: "^1.0.0"}, {Package: "target", Constraint: "^2.0.0"}},
		},
		{
			name: "linear-error", build: linearErrorProvider,
			reqs:    []Requirement{{Package: "foo", Constraint: "^1.0.0"}, {Package: "baz", Constraint: "^1.0.0"}},
			wantErr: true,
		},
		{
			name: "branching-error", build: branchingErrorProvider,
			reqs:    []Requirement{{Package: "foo", Constraint: "^1.0.0"}},
			wantErr: true,
		},
		{
			name: "unknown-package", build: newFakeProvider,
			reqs:    []Requirement{{Package: "ghost", Constraint: "^1.0.0"}},
			wantErr: true,
		},
		{
			name: "unknown-transitive", build: unknownPackageTransitiveProvider,
			reqs:    []Requirement{{Package: "foo", Constraint: "^1.0.0"}},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runDeterminismCase(t, tc)
		})
	}
}

// runDeterminismCase runs tc.build/Solve 100 times and asserts every run
// agrees with the first: the same resolution for a success case, or the same
// rendered proof text for a failure case.
func runDeterminismCase(t *testing.T, tc determinismCase) {
	t.Helper()
	var wantVersions Resolution
	var wantProof string

	for i := range 100 {
		p := tc.build()
		result, err := Solve(t.Context(), tc.reqs, p)
		if tc.wantErr {
			var conflictErr *ConflictError
			if !errors.As(err, &conflictErr) {
				t.Fatalf("run %d: expected *ConflictError, got %v", i, err)
			}
			proof := strings.Join(conflictErr.ProofLines(), "\n")
			if i == 0 {
				wantProof = proof
			} else if proof != wantProof {
				t.Fatalf("run %d: proof differs:\n--- want ---\n%s\n--- got ---\n%s", i, wantProof, proof)
			}
			continue
		}
		if err != nil {
			t.Fatalf("run %d: unexpected error: %v", i, err)
		}
		if i == 0 {
			wantVersions = result.Versions
		} else if !sameResolution(wantVersions, result.Versions) {
			t.Fatalf("run %d: resolution differs: got %v, want %v", i, result.Versions, wantVersions)
		}
	}
}

func sameResolution(a, b Resolution) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
