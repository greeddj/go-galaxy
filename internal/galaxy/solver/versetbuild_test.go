package solver

import (
	"errors"
	"testing"

	"github.com/Masterminds/semver/v3"
)

// differentialConstraintPool is the differential suite's constraint corpus:
// every vendored operator in plain, minor-dirty, patch-dirty and wildcarded
// forms, with prereleases, ranges, OR groups and metadata, plus the sharp pool.
func differentialConstraintPool() []string {
	pool := sharpConstraintPool()
	pool = append(pool,
		"<=1.2.3", "<1.0.0", ">1.2.3", "<0.0.0", "<=0.0.1-0", ">=0.0.0-0", ">0.0.0-0", "<0.0.0-0",
		">=1.2.3-beta.1", ">1.2.3-beta.1", "<=1.2.3-beta.1", "<1.2.3-beta.1",
		"~1.2.3", "~1.2", "~1", "~0.0.0", "~*", "~1.x", "~1.2.x", "~>1.2.3", "~1.2.3-beta.1", "~0.0.0-alpha",
		"^1.2.3", "^0.2.3", "^0.0.3", "^0.0.3-beta", "^*", "^0", "^0.0", "^1.x", "^0.x", "^1.2.3-beta.1",
		"=1.2.3", "==1.2.3", "1.2.3", "0.0.0", "1.0.0-rc.1", "=1.2.3-beta.1", "=0.0.0-0", "2.x", "1.2.x",
		"!=1.2.3", "!=1.5.0-beta", "!=1.x", "!=1.2.x", "!=*", "!=0.0.0",
		"1.2.3 - 2.3.4", "1.2 - 2.3", "1.2.3-beta.1 - 2.0.0",
		">=1.0.0 <2.0.0", ">=1.0.0,<2.0.0 || >=3.0.0", "1.x || 3.x",
		"v1.2.3", ">=v1.2.3", "1.2.3+build", "=1.2.3+build.7", ">=1.2.3-beta.1+meta",
		">11", ">11.1", ">11.1.x", "<2.x", ">=1.x", "<=2.x", "<=1.2.x", "<=*", ">*", "=>1.2.3", "=<1.2.3",
		">=1.2.3-beta.1,<2.0.0",
	)
	return pool
}

// differentialProbePool is the differential suite's version corpus: releases
// and prereleases around every boundary the constraint pool names, subline
// minima, and build-metadata variants, which must be invisible.
func differentialProbePool() []string {
	return []string{
		"0.0.0-0", "0.0.0-alpha", "0.0.0", "0.0.1-0", "0.0.1", "0.0.3-0", "0.0.3", "0.0.4-0", "0.0.4",
		"0.1.0-0", "0.1.0", "0.2.0", "0.2.5", "0.3.0-0", "0.3.0",
		"1.0.0-0", "1.0.0-alpha", "1.0.0-alpha.0", "1.0.0-alpha.beta", "1.0.0-beta.1", "1.0.0-beta.1.0",
		"1.0.0-beta.2", "1.0.0-rc.1", "1.0.0-rc1", "1.0.0", "1.0.1-0", "1.0.1",
		"1.2.0-0", "1.2.0", "1.2.2", "1.2.3-0", "1.2.3-beta.0", "1.2.3-beta.1", "1.2.3-beta.1.0",
		"1.2.3-beta.2", "1.2.3", "1.2.4-0", "1.2.4-beta.1", "1.2.4", "1.2.5", "1.3.0-0", "1.3.0",
		"1.5.0-beta", "1.5.0", "1.9.9", "2.0.0-0", "2.0.0-rc.1", "2.0.0", "2.0.1", "2.1.0",
		"2.3.4-0", "2.3.4", "2.3.5-0", "2.3.5", "3.0.0-0", "3.0.0", "3.5.0", "4.0.0",
		"11.0.0-0", "11.0.0", "11.1.0", "11.1.1", "11.2.0-0", "11.2.0", "12.0.0-0", "12.0.0",
		"1.2.3+build", "1.2.3+build.7", "1.0.0+x", "2.0.0+meta", "1.2.3-beta.1+meta",
	}
}

// TestVerSetGroundTruthRows drives newVerSet through the same pinned semver
// semantics rows that gate the symbolic pipeline, error rows included.
func TestVerSetGroundTruthRows(t *testing.T) {
	t.Parallel()
	for _, tt := range symbolicSemanticsCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			set, err := newVerSet(tt.constraint)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("newVerSet(%q) expected an error", tt.constraint)
				}
				return
			}
			if err != nil {
				t.Fatalf("newVerSet(%q) unexpected error: %v", tt.constraint, err)
			}
			assertCanonicalVerSet(t, set)
			if got := set.contains(mustV(t, tt.version)); got != tt.wantMatch {
				t.Fatalf("contains(%q) against %q = %v, want %v", tt.version, tt.constraint, got, tt.wantMatch)
			}
		})
	}
}

// TestVerSetDifferentialAgainstCheck pins the builder's contract: membership
// in every built set agrees with Masterminds' Check (via propCheck) for every
// pooled constraint and probe. A divergence is always a builder bug.
func TestVerSetDifferentialAgainstCheck(t *testing.T) {
	t.Parallel()
	probes := make([]Version, 0, len(differentialProbePool()))
	for _, raw := range differentialProbePool() {
		probes = append(probes, mustV(t, raw))
	}
	for _, c := range differentialConstraintPool() {
		set, err := newVerSet(c)
		if err != nil {
			t.Fatalf("newVerSet(%q): %v", c, err)
		}
		assertCanonicalVerSet(t, set)
		for _, v := range probes {
			want := propCheck(v.Original(), c)
			if got := set.contains(v); got != want {
				t.Fatalf("contains(%q) against %q = %v, Check says %v", v.Original(), c, got, want)
			}
		}
	}
}

// TestVerSetVacuousFormsAdmitPrereleases pins the deliberate divergence from
// Masterminds: the unconstrained forms ("", "*") build the full set,
// prereleases included, as the solver's vacuous truth requires.
func TestVerSetVacuousFormsAdmitPrereleases(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "*", "  ", " * "} {
		set, err := newVerSet(raw)
		if err != nil {
			t.Fatalf("newVerSet(%q): %v", raw, err)
		}
		if !set.isFull() {
			t.Fatalf("newVerSet(%q) is not the full set", raw)
		}
		if !set.contains(mustV(t, "999.999.999-rc1")) {
			t.Fatalf("newVerSet(%q) does not admit prereleases", raw)
		}
	}
}

// TestVerSetUnsupportedCombErrors pins the one grammar exclusion: a patch
// x-ranged "!=" with a prerelease operand, which Masterminds accepts, is
// refused with errNonIntervalConstraint since it excludes an infinite comb.
func TestVerSetUnsupportedCombErrors(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"!=1.2.x-beta", "!=1.2-beta", ">=0.0.1,!=4.1.x-beta"} {
		if _, err := semver.NewConstraint(raw); err != nil {
			t.Fatalf("Masterminds rejected %q; the exclusion would be dead code", raw)
		}
		_, err := newVerSet(raw)
		if err == nil || !errors.Is(err, errNonIntervalConstraint) {
			t.Fatalf("newVerSet(%q) error = %v, want errNonIntervalConstraint", raw, err)
		}
	}
}

// TestVerSetSingletonDetection pins which expressions carry a recoverable
// pinned version: a single bare or "=" comparator does (with the original
// spelling, metadata and v-prefix included), anything wider does not.
func TestVerSetSingletonDetection(t *testing.T) {
	t.Parallel()
	pinned := map[string]string{
		"1.2.3":         "1.2.3",
		"=1.2.3":        "1.2.3",
		"==1.2.3":       "1.2.3",
		"1.2.3+build.7": "1.2.3+build.7",
		"v1.2.3":        "v1.2.3",
		"=1.0.0-rc.1":   "1.0.0-rc.1",
	}
	for raw, wantOriginal := range pinned {
		set, err := newVerSet(raw)
		if err != nil {
			t.Fatalf("newVerSet(%q): %v", raw, err)
		}
		v, ok := set.decidedVersion()
		if !ok || v.Original() != wantOriginal {
			t.Fatalf("newVerSet(%q).decidedVersion() = (%q, %v), want (%q, true)", raw, v.Original(), ok, wantOriginal)
		}
	}
	for _, raw := range []string{">=1.2.3", "1.x", "*", "", "=1.2.3 || =2.0.0", "!=1.2.3", "~1.2.3"} {
		set, err := newVerSet(raw)
		if err != nil {
			t.Fatalf("newVerSet(%q): %v", raw, err)
		}
		if _, ok := set.decidedVersion(); ok {
			t.Fatalf("newVerSet(%q) unexpectedly carries a pinned version", raw)
		}
	}
}

// TestSingletonVerSetRoundtrip pins the decision-term constructor: the set
// denotes exactly the version's precedence point and hands the original
// spelling (metadata included) back through decidedVersion.
func TestSingletonVerSetRoundtrip(t *testing.T) {
	t.Parallel()
	v := mustV(t, "1.2.3+build")
	s := singletonVerSet(v)
	assertCanonicalVerSet(t, s)
	got, ok := s.decidedVersion()
	if !ok || got.Original() != v.Original() {
		t.Fatalf("singletonVerSet roundtrip = (%q, %v), want (%q, true)", got.Original(), ok, v.Original())
	}
	if !s.contains(mustV(t, "1.2.3")) || s.contains(mustV(t, "1.2.4")) || s.contains(mustV(t, "1.2.3-rc.1")) {
		t.Fatalf("singletonVerSet(1.2.3+build) membership is not the single precedence point")
	}
}
