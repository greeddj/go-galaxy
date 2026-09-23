package collections

import (
	"context"
	"fmt"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
)

// solveCollections is the production cold-resolve path: it solves roots and
// returns resolved (keyed by fqdn) and graph (collection.key() to the keys of
// its dependencies).
func solveCollections(ctx context.Context, deps collectionDeps, roots []collection) (map[string]collection, map[string][]string, error) {
	sources := rootSourceMap(roots)
	// Built over deps, not a fresh collectionDeps, so it shares the API root and
	// unmatched-source memos and the Store with prewarmRootMetadata: nothing
	// either already fetched or probed is requested again.
	mp := newMetadataProviderWithDeps(deps, sources)
	var provider solver.Provider = mp
	if deps.cfg.NoDeps {
		provider = NewNoDepsProvider(provider)
	}

	reqs, err := buildSolverRequirements(roots)
	if err != nil {
		return nil, nil, err
	}

	result, err := solver.Solve(ctx, reqs, provider)
	if err != nil {
		return nil, nil, err
	}
	// mp.bindings is unguarded and read only after Solve returns: the solver
	// core drives every MetadataProvider method from this one goroutine.
	return solverResultToResolvedGraph(result, deps.cfg, mp.bindings, sources, mp.pins)
}

// rootSourceMap maps each root fqdn to its own Source ("" when unpinned). A
// transitive dependency is deliberately never a key: a dependency never
// inherits its parent's source and walks the configured server list.
func rootSourceMap(roots []collection) map[string]string {
	sources := make(map[string]string, len(roots))
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		sources[fqdn] = root.Source
	}
	return sources
}

// buildSolverRequirements builds one solver.Requirement per root fqdn from its
// Constraint, else its Version. A bare or "*" root never conflicts with a
// constrained one; two different normalized constraints are a conflict.
func buildSolverRequirements(roots []collection) ([]solver.Requirement, error) {
	order := make([]string, 0, len(roots))
	seen := make(map[string]bool, len(roots))
	constraints := make(map[string]string, len(roots))

	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		if !seen[fqdn] {
			seen[fqdn] = true
			order = append(order, fqdn)
		}

		raw := root.Constraint
		if raw == "" {
			raw = root.Version
		}
		normalized := helpers.NormalizeConstraint(raw)
		if normalized == "" {
			continue
		}
		if existing, ok := constraints[fqdn]; ok && existing != normalized {
			return nil, fmt.Errorf("%w for %s: %q vs %q", helpers.ErrConflictingRootConstraints, fqdn, existing, normalized)
		}
		constraints[fqdn] = normalized
	}

	reqs := make([]solver.Requirement, 0, len(order))
	for _, fqdn := range order {
		reqs = append(reqs, solver.Requirement{Package: fqdn, Constraint: constraints[fqdn]})
	}
	return reqs, nil
}

// solverResultToResolvedGraph maps a solver.Result onto the (resolved, graph)
// shape the install pipeline consumes, stamping every entry's Source from the
// provider's pins, bindings and root sources through stampResolvedSource.
func solverResultToResolvedGraph(
	result *solver.Result,
	cfg *config.Config,
	bindings, sources map[string]string,
	pins map[string]exactPin,
) (map[string]collection, map[string][]string, error) {
	resolved := make(map[string]collection, len(result.Versions))
	for fqdn, version := range result.Versions {
		ns, name, ok := helpers.SplitFQDN(fqdn)
		if !ok {
			return nil, nil, fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionName, fqdn)
		}
		col := collection{
			Namespace: ns,
			Name:      name,
			Version:   version,
		}
		stampResolvedSource(&col, fqdn, pins, bindings, sources, cfg)
		resolved[fqdn] = col
	}

	depsByParent := make(map[string]map[string]string, len(result.Graph))
	for parentFQDN, depFQDNs := range result.Graph {
		if len(depFQDNs) == 0 {
			continue
		}
		deps := make(map[string]string, len(depFQDNs))
		for _, depFQDN := range depFQDNs {
			// buildGraphFromDeps reads only the key set, never the value.
			deps[depFQDN] = ""
		}
		depsByParent[parentFQDN] = deps
	}

	graph, err := buildGraphFromDeps(resolved, depsByParent)
	if err != nil {
		return nil, nil, err
	}
	ensureGraphNodes(resolved, graph)
	return resolved, graph, nil
}

// stampResolvedSource stamps col from its git or url discovery pin, which the
// solve never binds to a server, else from sourceFor. A url pin's sha256 is
// what makes verifyPinnedSHA check the origin digest on every install.
func stampResolvedSource(
	col *collection,
	fqdn string,
	pins map[string]exactPin,
	bindings, sources map[string]string,
	cfg *config.Config,
) {
	switch pin, pinned := pins[fqdn]; {
	case pinned && pin.typ == typeGit:
		col.Source, col.Type, col.Ref = pin.locator, typeGit, pin.ref
	case pinned:
		col.Source, col.Type, col.SHA256 = pin.locator, typeURL, pin.sha256
	default:
		col.Source = sourceFor(fqdn, bindings, sources, cfg)
	}
}

// sourceFor returns fqdn's binding from the solve, else the server its root's
// source: resolves to (an id is stamped as that server's URL, the spelling a
// binding records), else firstServerURL. Artifact keys and lockfiles use it.
func sourceFor(fqdn string, bindings, sources map[string]string, cfg *config.Config) string {
	if base, ok := bindings[fqdn]; ok {
		return base
	}
	// A nil cfg (tests only) would panic in pinnedServerCandidate.
	if source := sources[fqdn]; source != "" && cfg != nil {
		candidate, _ := pinnedServerCandidate(cfg, source)
		return candidate.base
	}
	return firstServerURL(cfg)
}

// firstServerURL returns Servers[0].URL, else cfg.Server for a hand-built test
// config; resolveServers keeps the two equal in production.
func firstServerURL(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if len(cfg.Servers) > 0 {
		return cfg.Servers[0].URL
	}
	return cfg.Server
}
