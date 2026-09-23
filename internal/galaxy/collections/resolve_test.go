package collections

import (
	"errors"
	"slices"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
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

// unsortedLevelGraph is a single-level six-node graph whose keys are unlike
// any plausible insertion order, so a sort check cannot pass by accident.
func unsortedLevelGraph() map[string][]string {
	return map[string][]string{
		"z.z@1.0.0": nil,
		"a.a@1.0.0": nil,
		"m.m@1.0.0": nil,
		"b.b@1.0.0": nil,
		"y.y@1.0.0": nil,
		"c.c@1.0.0": nil,
	}
}

// TestInstallLevelsAreSortedWithinLevel pins that buildInstallLevels sorts each
// level by key rather than leaving map-iteration order; it repeats 20 times
// because that order is randomized per run.
func TestInstallLevelsAreSortedWithinLevel(t *testing.T) {
	t.Parallel()
	graph := unsortedLevelGraph()
	want := []string{"a.a@1.0.0", "b.b@1.0.0", "c.c@1.0.0", "m.m@1.0.0", "y.y@1.0.0", "z.z@1.0.0"}

	const iterations = 20
	for i := range iterations {
		levels, err := buildInstallLevels(graph)
		if err != nil {
			t.Fatalf("iteration %d: buildInstallLevels error: %v", i, err)
		}
		if len(levels) != 1 {
			t.Fatalf("iteration %d: expected 1 level, got %d", i, len(levels))
		}
		got := levels[0]
		if len(got) != len(want) {
			t.Fatalf("iteration %d: levels[0] = %v, want %v", i, got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("iteration %d: levels[0] = %v, want %v", i, got, want)
			}
		}
	}
}

// TestInstallLevelsMembershipUnchangedBySort is the positive control for
// TestInstallLevelsAreSortedWithinLevel: the fixture still yields one level of
// the same keys, so that test catches order rather than broken leveling.
func TestInstallLevelsMembershipUnchangedBySort(t *testing.T) {
	t.Parallel()
	graph := unsortedLevelGraph()
	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels error: %v", err)
	}
	if len(levels) != 1 {
		t.Fatalf("expected 1 level, got %d", len(levels))
	}
	assertLevel(t, levels[0], []string{"z.z@1.0.0", "a.a@1.0.0", "m.m@1.0.0", "b.b@1.0.0", "y.y@1.0.0", "c.c@1.0.0"})
}

// TestParseDependenciesMalformedKey pins that a malformed server-supplied
// dependency key is refused; the forged-line case passes the split but not the
// alphabet, and would otherwise reach the operator's stderr.
func TestParseDependenciesMalformedKey(t *testing.T) {
	t.Parallel()
	for _, tc := range malformedDependencyKeyCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseDependencies(map[string]string{tc.key: ">=1.0.0"})
			if !errors.Is(err, helpers.ErrInvalidDependencyKey) {
				t.Fatalf("parseDependencies(%q) error = %v, want errors.Is helpers.ErrInvalidDependencyKey", tc.key, err)
			}
		})
	}
}

// malformedDependencyKeyCase is one row of TestParseDependenciesMalformedKey.
type malformedDependencyKeyCase struct {
	name string
	key  string
}

// malformedDependencyKeyCases covers a key that fails the split and three that
// pass it and fail the alphabet.
func malformedDependencyKeyCases() []malformedDependencyKeyCase {
	return []malformedDependencyKeyCase{
		{name: "no dot", key: "notanfqdn"},
		{name: "forged line", key: "evil.pkg\n[CRITICAL] FORGED DEP LINE"},
		{name: "uppercase half", key: "Evil.pkg"},
		{name: "path separator", key: "../../etc.passwd"},
	}
}

func TestParseDependenciesWellFormed(t *testing.T) {
	t.Parallel()
	got, err := parseDependencies(map[string]string{"ns.name": "  >=1.0.0  "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{"ns.name": ">=1.0.0"}
	if len(got) != len(want) || got["ns.name"] != want["ns.name"] {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func assertLevel(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	gotCopy := append([]string(nil), got...)
	wantCopy := append([]string(nil), want...)
	slices.Sort(gotCopy)
	slices.Sort(wantCopy)
	for i := range gotCopy {
		if gotCopy[i] != wantCopy[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

// TestRequirementsSignatureModePartition pins that --no-deps and deps-following
// signatures differ, so neither mode's snapshot satisfies the other, and that
// each is independent of root order.
func TestRequirementsSignatureModePartition(t *testing.T) {
	t.Parallel()
	rootsForward := []collection{
		{Namespace: "acme", Name: "app", Constraint: ">=1.0.0"},
		{Namespace: "acme", Name: "lib", Constraint: ">=2.0.0"},
	}
	rootsReversed := []collection{
		{Namespace: "acme", Name: "lib", Constraint: ">=2.0.0"},
		{Namespace: "acme", Name: "app", Constraint: ">=1.0.0"},
	}

	specForward := buildRequirementsSpec(rootsForward)
	specReversed := buildRequirementsSpec(rootsReversed)

	depsSigForward := requirementsSignatureFromSpec(specForward, false, "")
	depsSigReversed := requirementsSignatureFromSpec(specReversed, false, "")
	if depsSigForward != depsSigReversed {
		t.Fatalf("deps-mode signature is order-dependent: %q != %q", depsSigForward, depsSigReversed)
	}
	if depsSigForward != requirementsSignatureFromSpec(specForward, false, "") {
		t.Fatalf("deps-mode signature is not deterministic across repeated calls")
	}

	noDepsSigForward := requirementsSignatureFromSpec(specForward, true, "")
	noDepsSigReversed := requirementsSignatureFromSpec(specReversed, true, "")
	if noDepsSigForward != noDepsSigReversed {
		t.Fatalf("no-deps-mode signature is order-dependent: %q != %q", noDepsSigForward, noDepsSigReversed)
	}
	if noDepsSigForward != requirementsSignatureFromSpec(specForward, true, "") {
		t.Fatalf("no-deps-mode signature is not deterministic across repeated calls")
	}

	if depsSigForward == noDepsSigForward {
		t.Fatalf("expected --no-deps and deps-following signatures to differ, both got %q", depsSigForward)
	}
}

// TestSnapshotReuseVetoed pins snapshotReuseVetoed's veto table, including a
// nil cfg and --offline outranking both flags, and that every row agrees with
// whether cache.PolicyForConstraint reads a version-free answer.
func TestSnapshotReuseVetoed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cfg  *config.Config
		name string
		want bool
	}{
		{name: "nil cfg never vetoes", cfg: nil, want: false},
		{name: "refresh off", cfg: &config.Config{Refresh: false, Offline: false}, want: false},
		{name: "refresh on, online", cfg: &config.Config{Refresh: true, Offline: false}, want: true},
		{name: "refresh on, offline: offline outranks refresh", cfg: &config.Config{Refresh: true, Offline: true}, want: false},
		{name: "refresh off, offline: still no veto", cfg: &config.Config{Refresh: false, Offline: true}, want: false},
		{name: "no-cache on, online", cfg: &config.Config{NoCache: true, Offline: false}, want: true},
		{name: "no-cache on, offline: offline outranks no-cache", cfg: &config.Config{NoCache: true, Offline: true}, want: false},
		{name: "no-cache and refresh on, online", cfg: &config.Config{NoCache: true, Refresh: true}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := snapshotReuseVetoed(tc.cfg)
			if got != tc.want {
				t.Errorf("snapshotReuseVetoed(%+v) = %v, want %v", tc.cfg, got, tc.want)
			}
			if policyRead := cacheManager.PolicyForConstraint(tc.cfg, false).Read; got == policyRead {
				t.Errorf("snapshotReuseVetoed(%+v) = %v while a version-free policy read is %v, want them opposite",
					tc.cfg, got, policyRead)
			}
		})
	}
}
