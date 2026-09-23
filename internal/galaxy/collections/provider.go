package collections

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/Masterminds/semver/v3"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// MetadataProvider adapts go-galaxy's Galaxy metadata layer to solver.Provider,
// sharing the install pipeline's candidate URLs, cache buckets and offline
// behavior. It is not safe for concurrent use.
type MetadataProvider struct {
	deps collectionDeps
	// sources maps each root fqdn to its explicit install source, "" when it
	// has none (see sourceOf). A transitive dependency is never a key.
	sources map[string]string
	// bindings maps each fqdn to the server base that answered it, which
	// becomes its resolved source. No mutex: one Solve drives a provider from
	// a single goroutine, and prewarm builds a fresh provider per call.
	bindings map[string]string
	// pins holds every fqdn a git or url discovery produced this run. A pinned
	// fqdn is answered from its identity document, never from a Galaxy server,
	// and records no binding. Read-only after construction.
	pins map[string]exactPin
}

// exactPin is the provider's one view of a git or url discovery pin: the
// locator, exact version and validated dependencies, plus the kind-specific
// ref (git) or sha256 (url), each empty on the other kind.
type exactPin struct {
	deps    map[string]string
	locator string
	version string
	ref     string
	sha256  string
	typ     string
}

// mergeExactPins folds the git and url discovery tables into the provider's
// one pin map. The two tables cannot share an fqdn: expandSourceRoots'
// duplicate check refused that before any solve began.
func mergeExactPins(git map[string]gitPin, urls map[string]urlPin) map[string]exactPin {
	out := make(map[string]exactPin, len(git)+len(urls))
	for fqdn, pin := range git {
		out[fqdn] = exactPin{deps: pin.deps, locator: pin.locator, version: pin.version, ref: pin.ref, typ: typeGit}
	}
	for fqdn, pin := range urls {
		out[fqdn] = exactPin{deps: pin.deps, locator: pin.locator, version: pin.version, sha256: pin.sha256, typ: typeURL}
	}
	return out
}

// NewMetadataProvider builds a MetadataProvider over a fresh collectionDeps
// sharing cfg, runtime and st with the install pipeline, so a warm cache
// entry serves both. A nil sources leaves every fqdn unpinned.
func NewMetadataProvider(
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	sources map[string]string,
) *MetadataProvider {
	return newMetadataProviderWithDeps(newCollectionDeps(cfg, runtime, st), sources)
}

// newMetadataProviderWithDeps builds a MetadataProvider over an existing
// collectionDeps, sharing its memos and Store with every other user of
// deps, such as the resolve phase's prewarm.
func newMetadataProviderWithDeps(deps collectionDeps, sources map[string]string) *MetadataProvider {
	return &MetadataProvider{
		deps:     deps,
		sources:  sources,
		bindings: make(map[string]string),
		pins:     mergeExactPins(deps.gitMemo.snapshot(), deps.urlMemo.snapshot()),
	}
}

// Highest returns fqdn's registry-reported highest_version without checking
// constraints. An unknown package or an empty highest_version reports
// ok=false, sending the core to Universe.
func (p *MetadataProvider) Highest(ctx context.Context, fqdn string) (solver.Version, bool, error) {
	if v, pinned, err := p.pinnedVersion(fqdn); pinned || err != nil {
		return v, pinned, err
	}
	ns, name, err := splitFQDN(fqdn)
	if err != nil {
		return solver.Version{}, false, err
	}
	policy := cacheManager.PolicyForConstraint(p.deps.cfg, false)
	rootMeta, _, known, err := p.resolveRoot(ctx, fqdn, ns, name, policy)
	if err != nil {
		return solver.Version{}, false, err
	}
	if !known || rootMeta.HighestVersion.Version == "" {
		return solver.Version{}, false, nil
	}
	v, err := solver.NewVersion(rootMeta.HighestVersion.Version)
	if err != nil {
		return solver.Version{}, false, fmt.Errorf("parsing highest_version for %s: %w", fqdn, err)
	}
	return v, true, nil
}

// Universe returns every published version of fqdn, deduplicated and sorted
// in the solver's descending total order (see buildSolverUniverse). An
// unknown package returns (nil, nil), per solver.Provider's contract.
func (p *MetadataProvider) Universe(ctx context.Context, fqdn string) ([]solver.Version, error) {
	if v, pinned, err := p.pinnedVersion(fqdn); pinned || err != nil {
		if err != nil {
			return nil, err
		}
		return []solver.Version{v}, nil
	}
	ns, name, err := splitFQDN(fqdn)
	if err != nil {
		return nil, err
	}
	policy := cacheManager.PolicyForConstraint(p.deps.cfg, false)
	_, versionsURL, known, err := p.resolveRoot(ctx, fqdn, ns, name, policy)
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, nil
	}
	raw, err := loadVersionsListCached(ctx, p.deps, versionsURL, policy)
	if err != nil {
		return nil, err
	}
	return buildSolverUniverse(raw), nil
}

// Dependencies returns fqdn@v's validated dependency map, served from the
// deps cache scoped to fqdn's bound server (see boundBaseFor) when warm. A
// malformed key or constraint aborts as a provider contract violation.
func (p *MetadataProvider) Dependencies(ctx context.Context, fqdn string, v solver.Version) (map[string]solver.Constraint, error) {
	if pin, ok := p.pins[fqdn]; ok {
		if v.Original() != pin.version {
			return nil, fmt.Errorf("%w: %s collection %s is built as %s, not %s",
				helpers.ErrNoVersionSatisfiesConstraints, pin.typ, fqdn, pin.version, v.Original())
		}
		return canonicalizeDependencies(fqdn, pin.deps)
	}
	ns, name, err := splitFQDN(fqdn)
	if err != nil {
		return nil, err
	}
	policy := cacheManager.PolicyForConstraint(p.deps.cfg, true)
	col := collection{Namespace: ns, Name: name, Source: p.sourceOf(fqdn)}

	base, err := p.boundBaseFor(ctx, col, fqdn, policy)
	if err != nil {
		return nil, err
	}
	cacheKey := helpers.ScopedDepsCacheKey(base, fmt.Sprintf("%s.%s@%s", ns, name, v.Original()))

	if raw, ok := cachedDeps(p.deps.st, policy, cacheKey); ok {
		return canonicalizeDependencies(fqdn, raw)
	}

	root, err := resolveRootMetadata(ctx, p.deps, col, policy, fqdn)
	if err != nil {
		return nil, err
	}
	p.recordBinding(fqdn, root.base)
	info, err := fetchVersionMetadataCached(ctx, p.deps, root.base, root.versionsURL, v.Original(), policy)
	if err != nil {
		return nil, err
	}
	raw, err := parseDependencies(extractDependencies(info))
	if err != nil {
		return nil, err
	}
	cacheDeps(p.deps.st, policy, cacheKey, raw)
	return canonicalizeDependencies(fqdn, raw)
}

// boundBaseFor returns the server base fqdn's deps-cache key is scoped to and
// records it as fqdn's binding, with no network access for a single candidate
// server; otherwise a prior binding or a root-metadata fetch settles it.
func (p *MetadataProvider) boundBaseFor(ctx context.Context, col collection, fqdn string, policy cacheManager.Policy) (string, error) {
	if candidates := serverCandidates(p.deps, col); len(candidates) == 1 {
		p.recordBinding(fqdn, candidates[0].base)
		return candidates[0].base, nil
	}
	if base, ok := p.bindings[fqdn]; ok {
		return base, nil
	}
	root, err := resolveRootMetadata(ctx, p.deps, col, policy, fqdn)
	if err != nil {
		return "", err
	}
	p.recordBinding(fqdn, root.base)
	return root.base, nil
}

// sourceOf returns fqdn's explicit install source, or "" (unpinned) for a
// root without one and for every transitive dependency: a dependency never
// inherits its parent's source.
func (p *MetadataProvider) sourceOf(fqdn string) string {
	return p.sources[fqdn]
}

// recordBinding remembers base as the server serving fqdn, for sourceFor to
// stamp onto the resolved collection. Callers record every root-metadata fetch
// success, whatever they then do with the answer; an empty base is ignored.
func (p *MetadataProvider) recordBinding(fqdn, base string) {
	if base == "" {
		return
	}
	p.bindings[fqdn] = base
}

// resolveRoot loads fqdn's root metadata and records its binding, turning an
// unknown package into known=false so Highest and Universe report it to the
// core. Any other failure, auth and availability included, aborts.
func (p *MetadataProvider) resolveRoot(
	ctx context.Context,
	fqdn, ns, name string,
	policy cacheManager.Policy,
) (*types.GalaxyCollection, string, bool, error) {
	col := collection{Namespace: ns, Name: name, Source: p.sourceOf(fqdn)}
	root, err := resolveRootMetadata(ctx, p.deps, col, policy, fqdn)
	if err != nil {
		if isUnknownPackageError(err) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	p.recordBinding(fqdn, root.base)
	return root.meta, root.versionsURL, true, nil
}

// isUnknownPackageError reports whether err means the package does not
// exist: the last 404 of an all-404 candidate walk, or ErrLoadMetadataFailed
// from an empty candidate list. Anything else must abort the solve.
func isUnknownPackageError(err error) bool {
	if errors.Is(err, helpers.ErrLoadMetadataFailed) {
		return true
	}
	statusErr, ok := errors.AsType[*cacheManager.HTTPStatusError](err)
	return ok && statusErr.Code == http.StatusNotFound
}

// splitFQDN validates fqdn as "namespace.name", wrapping
// helpers.ErrInvalidDependencyKey like parseDependencies, since a malformed
// key here is the same provider contract violation.
func splitFQDN(fqdn string) (string, string, error) {
	ns, name, ok := helpers.SplitFQDN(fqdn)
	if !ok {
		return "", "", fmt.Errorf("%w: %q", helpers.ErrInvalidDependencyKey, fqdn)
	}
	return ns, name, nil
}

// canonicalizeDependencies normalizes every constraint in raw into a fresh
// map. An empty normalized form is unconstrained; any other must parse via
// Masterminds/semver, or the call fails as a contract violation.
func canonicalizeDependencies(fqdn string, raw map[string]string) (map[string]solver.Constraint, error) {
	out := make(map[string]solver.Constraint, len(raw))
	for dep, rawConstraint := range raw {
		normalized := helpers.NormalizeConstraint(rawConstraint)
		if normalized != "" {
			if _, err := semver.NewConstraint(normalized); err != nil {
				return nil, fmt.Errorf("invalid dependency constraint %q for %s -> %s: %w", rawConstraint, fqdn, dep, err)
			}
		}
		out[dep] = normalized
	}
	return out, nil
}

// rankedVersion pairs a solver.Version with the *semver.Version parsed from
// the same original string, so buildSolverUniverse can sort by precedence
// without solver.Version exposing its own parsed form outside its package.
type rankedVersion struct {
	sv       *semver.Version
	original solver.Version
}

// buildSolverUniverse parses raw into solver.Versions, dropping unparseable
// and duplicate strings, sorted by semver precedence then original string,
// both descending: a total order even for "1.0.0" and "1.0.0+build".
func buildSolverUniverse(raw []string) []solver.Version {
	seen := make(map[string]bool, len(raw))
	ranked := make([]rankedVersion, 0, len(raw))
	for _, r := range raw {
		if seen[r] {
			continue
		}
		v, err := solver.NewVersion(r)
		if err != nil {
			continue
		}
		seen[r] = true
		sv, err := semver.NewVersion(r)
		if err != nil {
			// Unreachable: solver.NewVersion(r) above already parsed r
			// successfully via the exact same semver.NewVersion call.
			continue
		}
		ranked = append(ranked, rankedVersion{sv: sv, original: v})
	}
	slices.SortFunc(ranked, compareRankedVersionsDescending)

	out := make([]solver.Version, len(ranked))
	for i, r := range ranked {
		out[i] = r.original
	}
	return out
}

// compareRankedVersionsDescending implements buildSolverUniverse's total
// order: semver precedence descending, then original string descending.
func compareRankedVersionsDescending(a, b rankedVersion) int {
	if c := b.sv.Compare(a.sv); c != 0 {
		return c
	}
	ao, bo := a.original.Original(), b.original.Original()
	switch {
	case ao > bo:
		return -1
	case ao < bo:
		return 1
	default:
		return 0
	}
}

// noDepsProvider wraps a solver.Provider for a --no-deps run: Highest and
// Universe delegate unchanged, while Dependencies always reports none.
type noDepsProvider struct {
	solver.Provider
}

// NewNoDepsProvider wraps p so its Dependencies never contributes an edge,
// leaving Highest/Universe delegated to p unchanged.
func NewNoDepsProvider(p solver.Provider) solver.Provider {
	return noDepsProvider{Provider: p}
}

// Dependencies always reports no dependencies, without ever calling the
// wrapped provider.
func (noDepsProvider) Dependencies(context.Context, string, solver.Version) (map[string]solver.Constraint, error) {
	return map[string]solver.Constraint{}, nil
}

// pinnedVersion returns the single version a git or url pin declares for
// fqdn.
func (p *MetadataProvider) pinnedVersion(fqdn string) (solver.Version, bool, error) {
	pin, ok := p.pins[fqdn]
	if !ok {
		return solver.Version{}, false, nil
	}
	v, err := solver.NewVersion(pin.version)
	if err != nil {
		return solver.Version{}, false, fmt.Errorf("parsing %s collection version for %s: %w", pin.typ, fqdn, err)
	}
	return v, true, nil
}
