package collections

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestProviderRefusesMalformedFQDNs pins that every provider entry point
// refuses a name that is not namespace.name as a contract violation, before
// any request reaches a server.
func TestProviderRefusesMalformedFQDNs(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	p := newTestMetadataProvider(t, srv)
	v := mustSolverVersion(t)
	ctx := t.Context()

	_, _, highestErr := p.Highest(ctx, "acme")
	_, universeErr := p.Universe(ctx, "acme")
	_, depsErr := p.Dependencies(ctx, "acme", v)
	_, confirmErr := p.Confirm(ctx, "acme", v)
	_, noDepsErr := NewNoDepsProvider(p).Dependencies(ctx, "acme", v)
	for name, err := range map[string]error{
		"Highest": highestErr, "Universe": universeErr, "Dependencies": depsErr, "Confirm": confirmErr, "no-deps Dependencies": noDepsErr,
	} {
		if !errors.Is(err, helpers.ErrInvalidDependencyKey) {
			t.Errorf("%s(\"acme\") error = %v, want ErrInvalidDependencyKey", name, err)
		}
	}
	if n := srv.Total(); n != 0 {
		t.Fatalf("server requests = %d, want 0 for malformed names", n)
	}
}

// newPinnedProvider is a provider over an empty server whose pins hold
// acme.git at refetchVersion, depending on acme.lib, and acme.bad at a
// version that does not parse.
func newPinnedProvider(t *testing.T) (*MetadataProvider, *fakegalaxy.Server) {
	t.Helper()
	srv := fakegalaxy.New(t)
	p := newTestMetadataProvider(t, srv)
	p.pins = map[string]exactPin{
		"acme.git": {version: refetchVersion, typ: typeGit, deps: map[string]string{"acme.lib": " >=1.0.0 "}},
		"acme.bad": {version: "not-semver", typ: typeURL},
	}
	return p, srv
}

// TestProviderAnswersPinsWithoutAServer pins a pin's version answers: its one
// version for Highest and Universe, no preference, and no request to any
// server.
func TestProviderAnswersPinsWithoutAServer(t *testing.T) {
	t.Parallel()
	p, srv := newPinnedProvider(t)
	ctx := t.Context()

	if v, ok, err := p.Highest(ctx, "acme.git"); err != nil || !ok || v.Original() != refetchVersion {
		t.Fatalf("Highest(pinned) = (%v, %v, %v), want %s", v, ok, err, refetchVersion)
	}
	if vs, err := p.Universe(ctx, "acme.git"); err != nil || len(vs) != 1 || vs[0].Original() != refetchVersion {
		t.Fatalf("Universe(pinned) = (%v, %v), want [%s]", vs, err, refetchVersion)
	}
	if _, ok, err := p.Preferred(ctx, "acme.git"); ok || err != nil {
		t.Fatalf("Preferred(pinned) = (%v, %v), want no preference", ok, err)
	}
	if n := srv.Total(); n != 0 {
		t.Fatalf("server requests = %d, want 0 for pinned names", n)
	}
}

// TestProviderRefusesAnUnparsablePin pins that a pin whose version does not
// parse fails Highest and Universe rather than falling back to a server.
func TestProviderRefusesAnUnparsablePin(t *testing.T) {
	t.Parallel()
	p, srv := newPinnedProvider(t)
	_, _, highestErr := p.Highest(t.Context(), "acme.bad")
	_, universeErr := p.Universe(t.Context(), "acme.bad")
	for name, err := range map[string]error{"Highest": highestErr, "Universe": universeErr} {
		if err == nil || !strings.Contains(err.Error(), "parsing url collection version for acme.bad") {
			t.Errorf("%s(unparsable pin) error = %v, want the pin's parse failure", name, err)
		}
	}
	if n := srv.Total(); n != 0 {
		t.Fatalf("server requests = %d, want 0 for pinned names", n)
	}
}

// TestProviderPinDependencies pins a pin's dependency answers: its own
// normalized dependencies at its version only, and none under --no-deps,
// which binds no server for a pin.
func TestProviderPinDependencies(t *testing.T) {
	t.Parallel()
	p, srv := newPinnedProvider(t)
	ctx := t.Context()
	pinned, err := solver.NewVersion(refetchVersion)
	if err != nil {
		t.Fatalf("solver.NewVersion: %v", err)
	}

	deps, err := p.Dependencies(ctx, "acme.git", pinned)
	if err != nil || len(deps) != 1 || deps["acme.lib"] != ">=1.0.0" {
		t.Fatalf("Dependencies(pinned) = (%v, %v), want acme.lib >=1.0.0", deps, err)
	}
	if _, err := p.Dependencies(ctx, "acme.git", mustSolverVersion(t)); !errors.Is(err, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("Dependencies(pinned, 1.0.0) error = %v, want ErrNoVersionSatisfiesConstraints", err)
	}
	if deps, err := NewNoDepsProvider(p).Dependencies(ctx, "acme.git", pinned); err != nil || len(deps) != 0 {
		t.Fatalf("no-deps Dependencies(pinned) = (%v, %v), want none and no error", deps, err)
	}
	if n := srv.Total(); n != 0 {
		t.Fatalf("server requests = %d, want 0 for pinned names", n)
	}
}

// TestProviderHighestRefusesAnUnparsableHighest pins that a highest_version
// the server spells as no version aborts Highest rather than being guessed.
func TestProviderHighestRefusesAnUnparsableHighest(t *testing.T) {
	t.Parallel()
	srv := newHighestVersionServer(t, "latest")
	p := NewMetadataProvider(&config.Config{Server: srv.URL}, infra.New(noopPrinter{}, srv.Client()), store.New(), nil)

	_, ok, err := p.Highest(t.Context(), "acme.widgets")
	if err == nil || ok || !strings.Contains(err.Error(), "parsing highest_version for acme.widgets") {
		t.Fatalf("Highest() = (%v, %v), want the highest_version parse failure", ok, err)
	}
}

// TestNoDepsProviderWithoutPreferrer pins that the no-deps wrapper over a
// provider with no preferences prefers nothing and confirms nothing.
func TestNoDepsProviderWithoutPreferrer(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	wrapped := NewNoDepsProvider(&countingProvider{Provider: newTestMetadataProvider(t, srv)})
	preferrer, ok := wrapped.(solver.Preferrer)
	if !ok {
		t.Fatal("the no-deps wrapper does not forward solver.Preferrer")
	}
	if _, ok, err := preferrer.Preferred(t.Context(), "acme.app"); ok || err != nil {
		t.Fatalf("Preferred() = (%v, %v), want no preference", ok, err)
	}
	if ok, err := preferrer.Confirm(t.Context(), "acme.app", mustSolverVersion(t)); ok || err != nil {
		t.Fatalf("Confirm() = (%v, %v), want false without a preferrer", ok, err)
	}
}

// TestBuildSolverUniverseDropsDuplicatesAndGarbage pins that the universe
// holds each parseable version once, highest first.
func TestBuildSolverUniverseDropsDuplicatesAndGarbage(t *testing.T) {
	t.Parallel()
	got := buildSolverUniverse([]string{testVersion100, "not-a-version", "2.0.0", testVersion100, ""})
	strs := make([]string, len(got))
	for i, v := range got {
		strs[i] = v.Original()
	}
	if !equalStrings(strs, []string{"2.0.0", testVersion100}) {
		t.Fatalf("buildSolverUniverse() = %v, want [2.0.0 1.0.0]", strs)
	}
}
