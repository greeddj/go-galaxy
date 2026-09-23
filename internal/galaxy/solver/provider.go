// Package solver is a pure, deterministic PubGrub-style version solver: one
// version per package satisfying every constraint, or a proof none exists. It
// performs no I/O; all package metadata arrives through the Provider seam.
package solver

import (
	"context"

	"github.com/Masterminds/semver/v3"
)

// rootPkg names the synthetic root package. It can never collide with a real
// "ns.name", and its leading NUL sorts it before every real fqdn, which the
// decision heuristic and root-requirement processing rely on.
const rootPkg = "\x00root"

// rootVersionString is the single version of the synthetic root package.
const rootVersionString = "0.0.0"

// rootVersion is the root package's sole version, built once at package
// init.
//
//nolint:gochecknoglobals // a canonical, immutable singleton of the algorithm itself, not mutable shared state
var rootVersion = mustNewVersion(rootVersionString)

// mustNewVersion parses raw as a Version or panics. It exists only for the
// constant root version, evaluated at init before any solve can fail.
func mustNewVersion(raw string) Version {
	v, err := NewVersion(raw)
	if err != nil {
		panic("solver: invalid constant version " + raw + ": " + err.Error())
	}
	return v
}

// Requirement is one root requirement: a package name and its constraint.
// The caller validates and deduplicates requirements before Solve; the core
// neither dedupes nor normalizes them.
type Requirement struct {
	Package    string
	Constraint string
}

// Resolution maps a resolved package name to the original registry version
// string of the version Solve chose for it.
type Resolution map[string]string

// Constraint is a canonical constraint from helpers.NormalizeConstraint, where
// "" means unconstrained. A constraint Masterminds/semver cannot parse is a
// provider contract violation and aborts the solve.
type Constraint = string

// Result is the successful outcome of Solve.
type Result struct {
	// Versions holds every decided package except the synthetic root,
	// mapped to the original registry string of its chosen version.
	Versions Resolution
	// Graph holds, for every package in Versions, the sorted list of its
	// dependency package names at the decided version. Every edge target is
	// itself a key in Versions.
	Graph map[string][]string
}

// Version pairs a parsed semver version with the original registry string it
// came from. Providers build Versions via NewVersion; the core never
// constructs one from raw user input except for the synthetic root version.
type Version struct {
	parsed   *semver.Version
	original string
}

// NewVersion parses raw as a semver version. Providers use this to build the
// Versions they hand to the core.
func NewVersion(raw string) (Version, error) {
	parsed, err := semver.NewVersion(raw)
	if err != nil {
		return Version{}, err
	}
	return Version{parsed: parsed, original: raw}, nil
}

// Original returns the original registry version string v was parsed from.
// This is what ends up in Resolution and in user-facing proof text.
func (v Version) Original() string {
	return v.original
}

// sv returns the parsed *semver.Version backing v, for in-package use
// (membership checks and precedence comparisons). It is the sole accessor
// the rest of the package uses to reach into a Version.
func (v Version) sv() *semver.Version {
	return v.parsed
}

// Provider is the seam between the solver core and package metadata; one Solve
// drives it from one goroutine. Every I/O must use the supplied ctx and keep
// errors.Is(err, ctx.Err()) true for a cancellation it observes.
type Provider interface {
	// Highest returns the registry-reported highest version of pkg, unchecked
	// against any constraint (the core checks membership). When ok is false or
	// err is non-nil, the core falls back to Universe.
	Highest(ctx context.Context, pkg string) (Version, bool, error)

	// Universe returns every published version of pkg, deduplicated by
	// original string, in any order (the core re-sorts it). An unknown package
	// returns an empty slice and a nil error.
	Universe(ctx context.Context, pkg string) ([]Version, error)

	// Dependencies returns pkg@v's dependency fqdn mapped to its canonical
	// Constraint. A malformed key or constraint must be returned as an error
	// here, never left for the core to guess at.
	Dependencies(ctx context.Context, pkg string, v Version) (map[string]Constraint, error)
}
