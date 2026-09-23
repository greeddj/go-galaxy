package solver

import "testing"

// testPkgFoo and testVersion100 are this file's dominant test fixture
// literals, pulled out as consts purely to satisfy goconst - they carry no
// domain meaning beyond "a generic package" and "its generic first version".
const (
	testPkgFoo     = "foo"
	testVersion100 = "1.0.0"
)

// TestMakeDecisionProbeSatisfiesZeroUniverseCalls pins the common fast path:
// when provider.Highest returns a candidate that already satisfies every
// assignment, the package is decided without ever calling Universe.
func TestMakeDecisionProbeSatisfiesZeroUniverseCalls(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().withVersions(testPkgFoo, testVersion100, "2.0.0").withHighest(testPkgFoo, testVersion100)
	s := newTestState(p)
	s.ps.decide(rootPkg, rootVersion)
	s.ps.derive(term{Package: testPkgFoo, Set: mustSet(t, "^1.0.0"), Positive: true}, mustDummyCause(t, s))

	pkg, done, err := s.makeDecision(t.Context())
	if err != nil {
		t.Fatalf("makeDecision: %v", err)
	}
	if done || pkg != testPkgFoo {
		t.Fatalf("makeDecision = (%q, %v), want (\"foo\", false)", pkg, done)
	}
	if p.universeCalls[testPkgFoo] != 0 {
		t.Fatalf("Universe was called %d times, want 0 (the probe should have sufficed)", p.universeCalls[testPkgFoo])
	}
	if s.ps.pkgState(testPkgFoo).decisionVersion.Original() != testVersion100 {
		t.Fatalf("decided version = %q, want 1.0.0 (highest override, satisfies ^1.0.0)", s.ps.pkgState(testPkgFoo).decisionVersion.Original())
	}
}

// TestMakeDecisionProbeUnavailableFallsBackToMaterialize pins that a probe
// the provider reports unavailable (ok == false) falls back to fetching the
// universe, as a probe candidate that fails the accumulation does.
func TestMakeDecisionProbeUnavailableFallsBackToMaterialize(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().withVersions(testPkgFoo, testVersion100, "2.0.0").withNoHighest(testPkgFoo)
	s := newTestState(p)
	s.ps.decide(rootPkg, rootVersion)
	s.ps.derive(term{Package: testPkgFoo, Set: mustSet(t, "^1.0.0"), Positive: true}, mustDummyCause(t, s))

	pkg, done, err := s.makeDecision(t.Context())
	if err != nil {
		t.Fatalf("makeDecision: %v", err)
	}
	if done || pkg != testPkgFoo {
		t.Fatalf("makeDecision = (%q, %v), want (\"foo\", false)", pkg, done)
	}
	if p.highestCalls[testPkgFoo] == 0 {
		t.Fatalf("Highest was never called")
	}
	if p.universeCalls[testPkgFoo] != 1 {
		t.Fatalf("Universe was called %d times, want exactly 1 (probe unavailable must still fall back)", p.universeCalls[testPkgFoo])
	}
	if got := s.ps.pkgState(testPkgFoo).decisionVersion.Original(); got != testVersion100 {
		t.Fatalf("decided version = %q, want 1.0.0 (the only version satisfying ^1.0.0)", got)
	}
}

// TestMakeDecisionProbeFailsFallsBackToMaterialize pins that a probe
// candidate failing the accumulated constraints makes decision making fetch
// the universe and decide the highest allowed version instead.
func TestMakeDecisionProbeFailsFallsBackToMaterialize(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().withVersions(testPkgFoo, testVersion100, "1.5.0", "2.0.0")
	s := newTestState(p)
	s.ps.decide(rootPkg, rootVersion)
	// foo's highest (2.0.0, the true max) does not satisfy ^1.0.0,<2.0.0.
	s.ps.derive(term{Package: testPkgFoo, Set: mustSet(t, "^1.0.0"), Positive: true}, mustDummyCause(t, s))
	s.ps.derive(term{Package: testPkgFoo, Set: mustSet(t, "<2.0.0"), Positive: true}, mustDummyCause(t, s))

	pkg, done, err := s.makeDecision(t.Context())
	if err != nil {
		t.Fatalf("makeDecision: %v", err)
	}
	if done || pkg != testPkgFoo {
		t.Fatalf("makeDecision = (%q, %v), want (\"foo\", false)", pkg, done)
	}
	if p.universeCalls[testPkgFoo] != 1 {
		t.Fatalf("Universe was called %d times, want exactly 1 (universe-fetch fallback)", p.universeCalls[testPkgFoo])
	}
	if got := s.ps.pkgState(testPkgFoo).decisionVersion.Original(); got != "1.5.0" {
		t.Fatalf("decided version = %q, want 1.5.0 (highest allowed under both constraints)", got)
	}
}

// TestMakeDecisionEmptyCandidateProducesCauseNoVersions pins that decision
// making, upon finding an empty allowed set for a known package, stores a
// CauseNoVersions incompatibility and reports "not decided, propagate on P".
func TestMakeDecisionEmptyCandidateProducesCauseNoVersions(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().withVersions(testPkgFoo, testVersion100, "2.0.0")
	s := newTestState(p)
	s.ps.decide(rootPkg, rootVersion)
	s.ps.derive(term{Package: testPkgFoo, Set: mustSet(t, "^1.0.0"), Positive: true}, mustDummyCause(t, s))
	s.ps.derive(term{Package: testPkgFoo, Set: mustSet(t, "^2.0.0"), Positive: true}, mustDummyCause(t, s))

	pkg, done, err := s.makeDecision(t.Context())
	if err != nil {
		t.Fatalf("makeDecision: %v", err)
	}
	if done || pkg != testPkgFoo {
		t.Fatalf("makeDecision = (%q, %v), want (\"foo\", false): an empty candidate set must not finish the solve", pkg, done)
	}
	if s.ps.pkgState(testPkgFoo).decisionIdx != -1 {
		t.Fatalf("foo was decided despite an empty candidate set")
	}
	found := false
	for _, inc := range s.store.all {
		if _, ok := inc.Cause.(causeNoVersions); ok {
			found = true
		}
	}
	if !found {
		t.Fatalf("no causeNoVersions incompatibility was recorded")
	}
}

// TestMakeDecisionUnknownPackageProducesCauseUnknownPackage pins that a
// package the provider reports zero versions for gets a CauseUnknownPackage
// incompatibility.
func TestMakeDecisionUnknownPackageProducesCauseUnknownPackage(t *testing.T) {
	t.Parallel()
	p := newFakeProvider() // "ghost" has no registered versions at all.
	s := newTestState(p)
	s.ps.decide(rootPkg, rootVersion)
	s.ps.derive(term{Package: "ghost", Set: mustSet(t, "^1.0.0"), Positive: true}, mustDummyCause(t, s))

	pkg, done, err := s.makeDecision(t.Context())
	if err != nil {
		t.Fatalf("makeDecision: %v", err)
	}
	if done || pkg != "ghost" {
		t.Fatalf("makeDecision = (%q, %v), want (\"ghost\", false)", pkg, done)
	}
	found := false
	for _, inc := range s.store.all {
		if _, ok := inc.Cause.(causeUnknownPackage); ok {
			found = true
		}
	}
	if !found {
		t.Fatalf("no causeUnknownPackage incompatibility was recorded")
	}
}

// TestMakeDecisionConservativeCheckDefersConflict pins PubGrub's "Avoiding
// Conflict During Decision Making": foo is not decided when its own new
// dependency incompatibility would relate satisfied against decided bar.
func TestMakeDecisionConservativeCheckDefersConflict(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().
		withVersions(testPkgFoo, "1.1.0").
		withVersions("bar", testVersion100, "2.0.0").
		withDeps(testPkgFoo, "1.1.0", map[string]string{"bar": "^2.0.0"})
	s := newTestState(p)
	s.ps.decide(rootPkg, rootVersion)
	s.ps.decide("bar", mustV(t, testVersion100)) // already decided, incompatible with foo's own dependency
	s.ps.derive(term{Package: testPkgFoo, Set: mustSet(t, "^1.0.0"), Positive: true}, mustDummyCause(t, s))

	pkg, done, err := s.makeDecision(t.Context())
	if err != nil {
		t.Fatalf("makeDecision: %v", err)
	}
	if done || pkg != testPkgFoo {
		t.Fatalf("makeDecision = (%q, %v), want (\"foo\", false)", pkg, done)
	}
	if s.ps.pkgState(testPkgFoo).decisionIdx != -1 {
		t.Fatalf("foo was decided despite its own new dependency incompatibility being immediately satisfied")
	}
}

// TestPickPackagePinFirst pins that an exact-pinned package is chosen ahead
// of an unpinned one, even when the unpinned package's name sorts first
// alphabetically (a name-only tie-break would pick the wrong one).
func TestPickPackagePinFirst(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	s.ps.decide(rootPkg, rootVersion)
	// "alpha" (unpinned, ranged) sorts before "zed" (exactly pinned) by name.
	s.ps.derive(term{Package: "alpha", Set: mustSet(t, "^1.0.0"), Positive: true}, mustDummyCause(t, s))
	s.ps.derive(term{Package: "zed", Set: mustSet(t, "=1.0.0"), Positive: true}, mustDummyCause(t, s))

	pkg, ok := s.pickPackage()
	if !ok || pkg != "zed" {
		t.Fatalf("pickPackage = (%q, %v), want (\"zed\", true): the exact pin must win over name order", pkg, ok)
	}
}

// mustDummyCause returns a fresh, valid store index usable as a derivation's
// CauseIndex in tests that only need a syntactically valid cause, not a
// specific one.
func mustDummyCause(t *testing.T, s *solveState) int {
	t.Helper()
	idx, _ := s.store.add(&incompatibility{Terms: []term{{Package: "\x00dummy", Set: fullVerSet(), Positive: true}}})
	return idx
}
