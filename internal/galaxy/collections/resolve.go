package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
	"github.com/psvmcc/hub/pkg/types"
)

// resolveMode selects whether a resolveCollectionsInternal call may replay the
// persisted resolve snapshot and record its result. The two move together:
// only a whole-requirements resolve's result is the snapshot's next value.
type resolveMode int

const (
	// resolveTopLevel is a whole-requirements resolve: it may reuse the
	// persisted resolve snapshot (still subject to snapshotReuseVetoed's
	// run-wide veto) and records its result back into the store.
	resolveTopLevel resolveMode = iota
	// resolveNestedPartial is tryIncrementalResolveWithSnapshot's changed-roots
	// re-solve: a subset of the requirements, so it neither replays the snapshot
	// nor records its partial result; the caller records the merged graph.
	resolveNestedPartial
)

// resolveCollectionsInternal resolves versions and dependencies for roots.
// --refresh bypasses only version-free answers (this snapshot, and non-exact
// metadata via cache.PolicyForConstraint); --no-cache bypasses exact ones too.
func resolveCollectionsInternal(
	ctx context.Context,
	deps collectionDeps,
	roots []collection,
	mode resolveMode,
) (map[string]collection, map[string][]string, error) {
	cfg := deps.cfg
	st := deps.st
	allowSnapshot := mode == resolveTopLevel

	// Git and url roots are expanded first so the requirements signature covers
	// their pinned locators: over the unexpanded roots, a --clear-cache run
	// (pins dropped, resolved snapshot kept) would replay the old graph.
	roots, err := expandSourceRoots(ctx, deps, roots)
	if err != nil {
		return nil, nil, err
	}

	reqSpec := buildRequirementsSpec(roots)
	reqHash := requirementsSignatureFromSpec(reqSpec, cfg.NoDeps, serversSignature(cfg))

	// snapshotReuseVetoed is a run-wide veto enforced here rather than by
	// each caller, so no caller can forget it.
	snapshotAllowed := allowSnapshot && st != nil && !snapshotReuseVetoed(cfg)
	if snapshotAllowed {
		resolvedSnap, graphSnap, ok, err := resolveFromSnapshots(ctx, deps, roots, reqSpec, reqHash)
		if shouldReturnSnapshot(ok, err) {
			return resolvedSnap, graphSnap, err
		}
	}

	// The prewarm must stay below the snapshot-replay return: a run that replays
	// the snapshot has to issue zero metadata requests.
	prewarmRootMetadata(ctx, deps, roots)
	resolved, graph, err := solveCollections(ctx, deps, roots)
	if err != nil {
		return nil, nil, err
	}
	recordResolutionIfNeeded(st, mode == resolveTopLevel, resolved, graph, reqHash, cfg.Server, reqSpec)
	return resolved, graph, nil
}

func shouldReturnSnapshot(ok bool, err error) bool {
	return ok || err != nil
}

// snapshotReuseVetoed reports whether --refresh or --no-cache vetoes reuse of
// the resolve snapshot. --offline outranks both, as cache.PolicyForConstraint
// does for a version-free answer, so each flag's halves agree; nil never vetoes.
func snapshotReuseVetoed(cfg *config.Config) bool {
	return cfg != nil && (cfg.Refresh || cfg.NoCache) && !cfg.Offline
}

func recordResolutionIfNeeded(
	st *store.Store,
	record bool,
	resolved map[string]collection,
	graph map[string][]string,
	reqHash string,
	server string,
	reqSpec map[string]store.RequirementSpec,
) {
	if !record || st == nil {
		return
	}
	recordResolution(st, resolved, graph, reqHash, server, reqSpec)
}

func resolveFromSnapshots(
	ctx context.Context,
	deps collectionDeps,
	roots []collection,
	reqSpec map[string]store.RequirementSpec,
	reqHash string,
) (map[string]collection, map[string][]string, bool, error) {
	cfg := deps.cfg
	st := deps.st

	if resolved, graph, ok := loadResolvedFromSnapshot(cfg, st, roots, reqHash); ok {
		return resolved, graph, true, nil
	}
	resolved, graph, ok, err := tryIncrementalResolve(ctx, deps, roots, reqSpec, reqHash)
	if err != nil {
		return nil, nil, false, err
	}
	return resolved, graph, ok, nil
}

func recordResolution(
	st *store.Store,
	resolved map[string]collection,
	graph map[string][]string,
	reqHash string,
	server string,
	reqSpec map[string]store.RequirementSpec,
) {
	setResolvedAll(st, resolved)
	st.SetGraphSnapshot(graph)
	st.SetMetaRequirements(reqHash, server)
	st.SetRequirements(reqSpec)
}

// setResolvedAll stores resolved collection versions in the snapshot.
func setResolvedAll(st *store.Store, resolved map[string]collection) {
	if st == nil {
		return
	}
	entries := make(map[string]store.ResolvedEntry, len(resolved))
	for fqdn, col := range resolved {
		entries[fqdn] = store.ResolvedEntry{Version: col.Version, Source: col.Source, Ref: col.Ref}
	}
	st.SetResolvedAll(entries)
}

func buildGraphFromDeps(resolved map[string]collection, depsByParent map[string]map[string]string) (map[string][]string, error) {
	graph := make(map[string][]string, len(depsByParent))
	for parentFQDN, deps := range depsByParent {
		parentCol, ok := resolved[parentFQDN]
		if !ok {
			return nil, fmt.Errorf("%w: %s", helpers.ErrMissingResolvedParent, parentFQDN)
		}
		parentKey := parentCol.key()
		depKeys := make([]string, 0, len(deps))
		for depFQDN := range deps {
			depCol, ok := resolved[depFQDN]
			if !ok {
				return nil, fmt.Errorf("%w: %s", helpers.ErrMissingResolvedDependency, depFQDN)
			}
			depKeys = append(depKeys, depCol.key())
		}
		graph[parentKey] = depKeys
	}
	return graph, nil
}

func ensureGraphNodes(resolved map[string]collection, graph map[string][]string) {
	for _, col := range resolved {
		key := col.key()
		if _, ok := graph[key]; !ok {
			graph[key] = nil
		}
	}
}

func cacheDeps(st *store.Store, policy cacheManager.Policy, cacheKey string, deps map[string]string) {
	if st == nil || !policy.Write {
		return
	}
	st.SetDepsCache(cacheKey, deps)
}

func cachedDeps(st *store.Store, policy cacheManager.Policy, cacheKey string) (map[string]string, bool) {
	if st == nil || !policy.Read {
		return nil, false
	}
	deps, ok := st.GetDepsCache(cacheKey)
	return deps, ok
}

// resolvedRoot is resolveRootMetadata's result: the root metadata, the
// versions URL to page through, and the server that answered.
type resolvedRoot struct {
	meta        *types.GalaxyCollection
	versionsURL string
	// base is the server that answered the root-metadata fetch, never col.Source,
	// which may be empty (unpinned) or stale (an old snapshot or lockfile).
	base string
}

// resolveRootMetadata loads col's root metadata and its versions URL: the
// server's versions_url resolved against the winning base (failing if it
// carries userinfo), or else one this program builds from that base.
func resolveRootMetadata(
	ctx context.Context,
	deps collectionDeps,
	col collection,
	policy cacheManager.Policy,
	label string,
) (resolvedRoot, error) {
	runtime := deps.runtime
	rootMeta, base, err := loadRootMetadataCached(ctx, deps, col, policy)
	if err != nil {
		return resolvedRoot{}, err
	}
	versionsURL := collectionVersionsURL(collection{Namespace: col.Namespace, Name: col.Name, Source: base})
	if rootMeta != nil && rootMeta.VersionsURL != "" {
		versionsURL, err = normalizeVersionsURL(base, rootMeta.VersionsURL)
		if err != nil {
			return resolvedRoot{}, err
		}
		runtime.Output.Debugf("Versions URL for %s: %s", label, helpers.WithoutCredentials(versionsURL))
	}
	return resolvedRoot{meta: rootMeta, versionsURL: versionsURL, base: base}, nil
}

func extractDependencies(info *types.GalaxyCollectionVersionInfo) map[string]string {
	if len(info.Metadata.Dependencies) > 0 {
		return info.Metadata.Dependencies
	}
	return info.Manifest.CollectionInfo.Dependencies
}

func parseDependencies(deps map[string]string) (map[string]string, error) {
	parsedDeps := make(map[string]string, len(deps))
	for dep, constraint := range deps {
		// A server-chosen key is judged by alphabet, not just shape: the solver
		// prints it, so a key like "evil.pkg\n[CRITICAL] ..." would forge a log line.
		if !helpers.IsCollectionName(dep) {
			return nil, fmt.Errorf("%w: %q", helpers.ErrInvalidDependencyKey, dep)
		}
		parsedDeps[dep] = strings.TrimSpace(constraint)
	}
	return parsedDeps, nil
}

// loadVersionsListCached returns a versions list, paging versionsURL by
// versionLimit under one MetadataDeadline budget shared by every page, and
// failing past maxVersionPages requests rather than truncating the list.
func loadVersionsListCached(
	ctx context.Context,
	deps collectionDeps,
	versionsURL string,
	policy cacheManager.Policy,
) ([]string, error) {
	if versions, ok := cachedVersionsList(deps.st, policy, versionsURL); ok {
		return versions, nil
	}

	budget := deps.runtime.MetadataDeadline()
	dlCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	pager := &versionsPager{
		parent:      ctx,
		dlCtx:       dlCtx,
		deps:        deps,
		versionsURL: versionsURL,
		policy:      policy,
		budget:      budget,
	}
	all, err := pager.collectAll()
	if err != nil {
		return nil, err
	}
	cacheVersionsList(deps.st, policy, versionsURL, all)
	return all, nil
}

// versionsPager is one loadVersionsListCached call's shared state: the
// caller's context (only to classify errors), the budget-bounded dlCtx every
// page request runs under, and the request parameters.
type versionsPager struct {
	//nolint:containedctx // the caller's original context, consulted only
	// to classify a failing page's error as caller-canceled versus
	// budget-expired; it never starts work of its own.
	parent context.Context
	//nolint:containedctx // the budget-bounded context every page request
	// runs under; fetchPage funnels both walkPages and the prefetch
	// workers through it, so it lives on the shared state rather than in
	// each call's signature.
	dlCtx       context.Context
	deps        collectionDeps
	versionsURL string
	policy      cacheManager.Policy
	budget      time.Duration
}

// pageResult is one fetched page: its version strings, the total the server
// declared alongside them, and the fetch's error, already normalized by
// fetchPage.
type pageResult struct {
	err      error
	versions []string
	total    int
}

// fetchPage fetches the page at offset under the shared budget. It is the one
// funnel that normalizes a failure, via cacheManager.MetadataDeadlineError,
// into at most one helpers.ErrMetadataFetchDeadline sentinel.
func (p *versionsPager) fetchPage(offset int) pageResult {
	versions, total, err := fetchVersionsPage(p.dlCtx, p.deps, p.policy, p.versionsURL, versionLimit, offset)
	return pageResult{versions: versions, total: total, err: cacheManager.MetadataDeadlineError(p.parent, p.dlCtx, p.budget, err)}
}

// pagingExceeded is the verdict for a list needing more than maxVersionPages
// requests. versionsURL may be the server's own, so it is printed cut.
func (p *versionsPager) pagingExceeded() error {
	return fmt.Errorf("%w: %s", helpers.ErrVersionsPagingExceeded, helpers.WithoutCredentials(p.versionsURL))
}

// collectAll fetches page 0 and decides the rest: a total past maxVersionPages
// fails before any further request or allocation sized by it; otherwise the
// scheduled offsets are prefetched and consumed by walkPages.
func (p *versionsPager) collectAll() ([]string, error) {
	first := p.fetchPage(0)
	if first.err != nil {
		return nil, first.err
	}
	if len(first.versions) < versionLimit || (first.total > 0 && versionLimit >= first.total) {
		return first.versions, nil
	}
	if first.total > maxVersionPages*versionLimit {
		return nil, p.pagingExceeded()
	}
	var scheduled []pageResult
	capacity := versionLimit
	if first.total > 0 {
		scheduled = p.prefetchScheduled(first.total)
		capacity = first.total
	}
	return p.walkPages(first.versions, scheduled, capacity)
}

// prefetchScheduled concurrently fetches every offset the declared total
// schedules past page 0, bounded by cfg.DownloadWorkers. Workers write
// disjoint elements and distinct APICache keys, so no result mutex is needed.
func (p *versionsPager) prefetchScheduled(total int) []pageResult {
	results := make([]pageResult, (total-1)/versionLimit)
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(p.deps.cfg.DownloadWorkers, 1))
	for i := range results {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = p.fetchPage((i + 1) * versionLimit)
		})
	}
	wg.Wait()
	return results
}

// walkPages assembles the list in offset order, ending on each page's own
// length and total, never the schedule: later prefetched results are discarded
// unread, and pages past the schedule are fetched on demand.
func (p *versionsPager) walkPages(first []string, scheduled []pageResult, capacity int) ([]string, error) {
	all := make([]string, 0, capacity)
	all = append(all, first...)
	offset := versionLimit
	for page := 1; ; page++ {
		if page >= maxVersionPages {
			return nil, p.pagingExceeded()
		}
		result := p.pageAt(page, offset, scheduled)
		if result.err != nil {
			return nil, result.err
		}
		all = append(all, result.versions...)
		if len(result.versions) < versionLimit {
			return all, nil
		}
		offset += versionLimit
		if result.total > 0 && offset >= result.total {
			return all, nil
		}
	}
}

// pageAt serves the walk's page from the prefetched schedule when the
// schedule covers it, and fetches it fresh otherwise.
func (p *versionsPager) pageAt(page, offset int, scheduled []pageResult) pageResult {
	if page <= len(scheduled) {
		return scheduled[page-1]
	}
	return p.fetchPage(offset)
}

func cachedVersionsList(st *store.Store, policy cacheManager.Policy, versionsURL string) ([]string, bool) {
	if st == nil || !policy.Read || policy.TTL != 0 {
		return nil, false
	}
	versions, ok := st.GetVersionsCache(versionsURL)
	if !ok || len(versions) == 0 {
		return nil, false
	}
	return versions, true
}

// fetchVersionsPage fetches one limit/offset page of the versions list and
// returns its versions with the server's declared total (meta.count or count).
func fetchVersionsPage(
	ctx context.Context,
	deps collectionDeps,
	policy cacheManager.Policy,
	versionsURL string,
	limit, offset int,
) ([]string, int, error) {
	url := fmt.Sprintf("%s?limit=%d&offset=%d", versionsURL, limit, offset)
	var payload map[string]any
	if err := fetchJSONWithCachePolicy(ctx, deps.runtime, url, deps.st, &payload, policy); err != nil {
		return nil, 0, err
	}
	return parseVersionsPayload(payload)
}

func cacheVersionsList(st *store.Store, policy cacheManager.Policy, versionsURL string, versions []string) {
	if st == nil || !policy.Write || policy.TTL != 0 {
		return
	}
	st.SetVersionsCache(versionsURL, versions)
}

// collectionVersionsURL builds the versions API URL for a collection.
func collectionVersionsURL(col collection) string {
	base := strings.TrimRight(col.Source, "/")
	return fmt.Sprintf("%s/api/v3/collections/%s/%s/versions/", base, col.Namespace, col.Name)
}

// normalizeSignatures trims, sorts, and filters signatures, cutting each query
// so no presigned capability is persisted. Keep the cut here, not only in the
// store's backstop, or tryIncrementalResolve's hash self-check never matches.
func normalizeSignatures(signatures []string) []string {
	if len(signatures) == 0 {
		return nil
	}
	out := make([]string, 0, len(signatures))
	for _, value := range signatures {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		out = append(out, helpers.WithoutQuery(trimmed))
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return out
}

// normalizeRequirementConstraint normalizes a constraint for hashing.
func normalizeRequirementConstraint(value string) string {
	normalized := helpers.NormalizeConstraint(value)
	if normalized == "" {
		return "*"
	}
	return normalized
}

// requirementSpecEqual reports whether two requirement specs are equal.
func requirementSpecEqual(a, b store.RequirementSpec) bool {
	if a.Constraint != b.Constraint || a.Source != b.Source || a.Type != b.Type {
		return false
	}
	return slices.Equal(normalizeSignatures(a.Signatures), normalizeSignatures(b.Signatures))
}

// exactVersionFromConstraints returns a single exact version if specified: a
// constraint is exact only if it parses as a semver.Version after one leading
// "="; ranges and x-ranges ("1.x") pin nothing, anything else is malformed.
func exactVersionFromConstraints(constraints []string) (string, bool, error) {
	exact := ""
	for _, raw := range constraints {
		normalized := helpers.NormalizeConstraint(raw)
		if normalized == "" {
			continue
		}
		candidate := normalized
		if after, hasPrefix := strings.CutPrefix(normalized, "="); hasPrefix {
			candidate = strings.TrimSpace(after)
		}
		if _, err := semver.NewVersion(candidate); err != nil {
			if _, cErr := semver.NewConstraint(normalized); cErr != nil {
				return "", false, fmt.Errorf("invalid version constraint %q: %w", raw, cErr)
			}
			// A valid range/wildcard constraint (e.g. ">=1.0.0" or "1.x") is
			// non-exact by definition; it contributes no pin.
			continue
		}
		if exact == "" {
			exact = candidate
			continue
		}
		if exact != candidate {
			return "", false, fmt.Errorf("%w: %s vs %s", helpers.ErrConflictingExactVersions, exact, candidate)
		}
	}
	if exact == "" {
		return "", false, nil
	}
	return exact, true, nil
}

// splitCollectionKey splits a key of the form "ns.name@version".
func splitCollectionKey(key string) (string, string, error) {
	parts := strings.SplitN(key, "@", helpers.CollectionNameParts)
	if len(parts) != helpers.CollectionNameParts || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionKey, key)
	}
	return parts[0], parts[1], nil
}

// collectGraphKeysFromKeys walks the graph starting from root keys.
func collectGraphKeysFromKeys(graph map[string][]string, roots []string) map[string]bool {
	visited := make(map[string]bool)
	queue := make([]string, 0, len(roots))
	for _, key := range roots {
		if !visited[key] {
			visited[key] = true
			queue = append(queue, key)
		}
	}

	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		deps := graph[key]
		for _, dep := range deps {
			if !visited[dep] {
				visited[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	return visited
}

// sameDeps reports whether two dependency slices contain the same items.
func sameDeps(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, v := range a {
		counts[v]++
	}
	for _, v := range b {
		if counts[v] == 0 {
			return false
		}
		counts[v]--
	}
	return true
}

// tryIncrementalResolve reuses snapshot data when only some roots changed.
func tryIncrementalResolve(
	ctx context.Context,
	deps collectionDeps,
	roots []collection,
	currentSpec map[string]store.RequirementSpec,
	reqHash string,
) (map[string]collection, map[string][]string, bool, error) {
	prevSpec := deps.st.RequirementsSnapshot()
	if len(prevSpec) == 0 {
		return nil, nil, false, nil
	}

	// The stored spec does not record --no-deps, so its signature is recomputed in
	// this run's mode and must equal the persisted hash; a snapshot resolved in
	// the other mode falls through to a fresh resolve.
	if requirementsSignatureFromSpec(prevSpec, deps.cfg.NoDeps, serversSignature(deps.cfg)) != deps.st.MetaSnapshot().RequirementsHash {
		return nil, nil, false, nil
	}

	unchangedRoots, changedRoots := splitRootsByChange(roots, currentSpec, prevSpec)
	if len(unchangedRoots) == 0 || len(changedRoots) == 0 {
		return nil, nil, false, nil
	}

	return tryIncrementalResolveWithSnapshot(ctx, deps, unchangedRoots, changedRoots, currentSpec, reqHash)
}

func tryIncrementalResolveWithSnapshot(
	ctx context.Context,
	deps collectionDeps,
	unchangedRoots []collection,
	changedRoots []collection,
	currentSpec map[string]store.RequirementSpec,
	reqHash string,
) (map[string]collection, map[string][]string, bool, error) {
	resolvedSnap, graphSnap, ok := loadSnapshotData(deps.st)
	if !ok {
		return nil, nil, false, nil
	}

	preservedResolved, preservedGraph, ok := buildPreservedSnapshot(deps.cfg, unchangedRoots, resolvedSnap, graphSnap)
	if !ok {
		return nil, nil, false, nil
	}

	resolvedNew, graphNew, err := resolveCollectionsInternal(ctx, deps, changedRoots, resolveNestedPartial)
	if err != nil {
		return nil, nil, false, err
	}

	mergedResolved, mergedGraph, ok := mergeResolvedGraphs(preservedResolved, preservedGraph, resolvedNew, graphNew)
	if !ok {
		return nil, nil, false, nil
	}

	if !expandGraphFromSnapshot(deps.cfg, mergedResolved, mergedGraph, resolvedSnap, graphSnap) {
		return nil, nil, false, nil
	}
	if !validateMergedGraph(mergedResolved, mergedGraph) {
		return nil, nil, false, nil
	}

	if deps.st != nil {
		recordResolution(deps.st, mergedResolved, mergedGraph, reqHash, deps.cfg.Server, currentSpec)
	}

	return mergedResolved, mergedGraph, true, nil
}

func splitRootsByChange(roots []collection, currentSpec, prevSpec map[string]store.RequirementSpec) ([]collection, []collection) {
	unchangedRoots := make([]collection, 0, len(roots))
	changedRoots := make([]collection, 0, len(roots))
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		current, ok := currentSpec[fqdn]
		if ok {
			if prev, ok := prevSpec[fqdn]; ok && requirementSpecEqual(current, prev) {
				unchangedRoots = append(unchangedRoots, root)
				continue
			}
		}
		changedRoots = append(changedRoots, root)
	}
	return unchangedRoots, changedRoots
}

func loadSnapshotData(st *store.Store) (map[string]store.ResolvedEntry, map[string][]string, bool) {
	resolvedSnap := st.ResolvedSnapshot()
	graphSnap := st.GraphSnapshot()
	if len(resolvedSnap) == 0 || len(graphSnap) == 0 {
		return nil, nil, false
	}
	return resolvedSnap, graphSnap, true
}

func buildPreservedSnapshot(
	cfg *config.Config,
	unchangedRoots []collection,
	resolvedSnap map[string]store.ResolvedEntry,
	graphSnap map[string][]string,
) (map[string]collection, map[string][]string, bool) {
	rootKeys, ok := preservedRootKeys(unchangedRoots, resolvedSnap)
	if !ok {
		return nil, nil, false
	}
	preservedKeys := collectGraphKeysFromKeys(graphSnap, rootKeys)
	preservedGraph := make(map[string][]string, len(preservedKeys))
	preservedResolved := make(map[string]collection)
	for key := range preservedKeys {
		deps, ok := graphSnap[key]
		if !ok {
			return nil, nil, false
		}
		preservedGraph[key] = deps
		if !addPreservedEntry(cfg, preservedResolved, resolvedSnap, key) {
			return nil, nil, false
		}
	}
	return preservedResolved, preservedGraph, true
}

func preservedRootKeys(unchangedRoots []collection, resolvedSnap map[string]store.ResolvedEntry) ([]string, bool) {
	rootKeys := make([]string, 0, len(unchangedRoots))
	for _, root := range unchangedRoots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		entry, ok := resolvedSnap[fqdn]
		if !ok || entry.Version == "" {
			return nil, false
		}
		rootKeys = append(rootKeys, fmt.Sprintf("%s@%s", fqdn, entry.Version))
	}
	return rootKeys, true
}

func addPreservedEntry(
	cfg *config.Config,
	preservedResolved map[string]collection,
	resolvedSnap map[string]store.ResolvedEntry,
	key string,
) bool {
	fqdn, version, err := splitCollectionKey(key)
	if err != nil {
		return false
	}
	entry, ok := resolvedSnap[fqdn]
	if !ok || entry.Version != version {
		return false
	}
	col, ok := collectionFromResolvedEntry(cfg, fqdn, entry)
	if !ok {
		return false
	}
	preservedResolved[fqdn] = col
	return true
}

func mergeResolvedGraphs(
	preservedResolved map[string]collection,
	preservedGraph map[string][]string,
	resolvedNew map[string]collection,
	graphNew map[string][]string,
) (map[string]collection, map[string][]string, bool) {
	mergedResolved := make(map[string]collection, len(preservedResolved)+len(resolvedNew))
	maps.Copy(mergedResolved, preservedResolved)
	for fqdn, col := range resolvedNew {
		if existing, ok := mergedResolved[fqdn]; ok && existing.Version != col.Version {
			return nil, nil, false
		}
		mergedResolved[fqdn] = col
	}

	mergedGraph := make(map[string][]string, len(preservedGraph)+len(graphNew))
	maps.Copy(mergedGraph, preservedGraph)
	for key, deps := range graphNew {
		if existing, ok := mergedGraph[key]; ok {
			if !sameDeps(existing, deps) {
				return nil, nil, false
			}
			continue
		}
		mergedGraph[key] = deps
	}
	return mergedResolved, mergedGraph, true
}

func expandGraphFromSnapshot(
	cfg *config.Config,
	mergedResolved map[string]collection,
	mergedGraph map[string][]string,
	resolvedSnap map[string]store.ResolvedEntry,
	graphSnap map[string][]string,
) bool {
	queue := make([]string, 0, len(mergedGraph))
	for key := range mergedGraph {
		queue = append(queue, key)
	}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		deps := mergedGraph[key]
		for _, dep := range deps {
			if _, ok := mergedGraph[dep]; ok {
				continue
			}
			depDeps, ok := graphSnap[dep]
			if !ok {
				return false
			}
			mergedGraph[dep] = depDeps
			queue = append(queue, dep)
			if !ensureResolvedFromSnapshot(cfg, mergedResolved, resolvedSnap, dep) {
				return false
			}
		}
	}
	return true
}

func ensureResolvedFromSnapshot(
	cfg *config.Config,
	mergedResolved map[string]collection,
	resolvedSnap map[string]store.ResolvedEntry,
	key string,
) bool {
	fqdn, version, err := splitCollectionKey(key)
	if err != nil {
		return false
	}
	if existing, ok := mergedResolved[fqdn]; ok {
		return existing.Version == version
	}
	entry, ok := resolvedSnap[fqdn]
	if !ok || entry.Version != version {
		return false
	}
	col, ok := collectionFromResolvedEntry(cfg, fqdn, entry)
	if !ok {
		return false
	}
	mergedResolved[fqdn] = col
	return true
}

func validateMergedGraph(mergedResolved map[string]collection, mergedGraph map[string][]string) bool {
	for key := range mergedGraph {
		fqdn, version, err := splitCollectionKey(key)
		if err != nil {
			return false
		}
		entry, ok := mergedResolved[fqdn]
		if !ok || entry.Version != version {
			return false
		}
	}
	return true
}

// buildRequirementsSpec builds a normalized requirement spec map. An unpinned
// root's Source stays "" rather than cfg.Server, so "no preference" and
// "pinned to the default server" hash differently.
func buildRequirementsSpec(roots []collection) map[string]store.RequirementSpec {
	spec := make(map[string]store.RequirementSpec, len(roots))
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		constraint := root.Constraint
		if constraint == "" {
			constraint = root.Version
		}
		constraint = normalizeRequirementConstraint(constraint)
		spec[fqdn] = store.RequirementSpec{
			Constraint: constraint,
			Source:     root.Source,
			Type:       root.Type,
			Signatures: normalizeSignatures(root.Signatures),
		}
	}
	return spec
}

// requirementsSignatureFromSpec returns a stable signature of requirements:
// --no-deps and serversSignature header lines, then the sorted per-root lines.
// The headers contain no "|", so no per-root line can collide with them.
func requirementsSignatureFromSpec(spec map[string]store.RequirementSpec, noDeps bool, serversSig string) string {
	parts := make([]string, 0, len(spec))
	for fqdn, entry := range spec {
		constraint := entry.Constraint
		if constraint == "" {
			constraint = "*"
		}
		signatureKey := strings.Join(normalizeSignatures(entry.Signatures), ",")
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%s", fqdn, constraint, entry.Source, entry.Type, signatureKey))
	}
	slices.Sort(parts)
	header := fmt.Sprintf("no-deps=%t\nservers=%s", noDeps, serversSig)
	sum := sha256.Sum256([]byte(header + "\n" + strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// serversSignature hashes cfg's effective server list in order, since order
// decides first-match ownership. Only whether a server has a token is hashed,
// never the token: the signature is persisted in the snapshot.
func serversSignature(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}

	servers := cfg.Servers
	if len(servers) == 0 {
		// The shape a hand-built config (and every caller predating
		// multi-server support) still has: one effective server, no
		// credential, matching what unpinnedServerCandidates falls back to.
		servers = []config.Server{{URL: cfg.Server}}
	}

	entries := make([]string, 0, len(servers))
	for _, srv := range servers {
		auth := "0"
		if srv.Token.IsSet() {
			auth = "1"
		}
		entries = append(entries, srv.ID+"\x00"+srv.URL+"\x00"+auth)
	}
	sum := sha256.Sum256([]byte(strings.Join(entries, ",")))
	return hex.EncodeToString(sum[:])
}

// loadResolvedFromSnapshot loads resolved data when requirements match.
func loadResolvedFromSnapshot(
	cfg *config.Config,
	st *store.Store,
	roots []collection,
	reqHash string,
) (map[string]collection, map[string][]string, bool) {
	if !snapshotMatchesRequirements(st, reqHash) {
		return nil, nil, false
	}
	resolvedSnapshot, graphSnapshot, ok := loadSnapshotData(st)
	if !ok {
		return nil, nil, false
	}
	resolved, ok := buildResolvedSnapshot(cfg, resolvedSnapshot)
	if !ok {
		return nil, nil, false
	}
	if !rootsMatchSnapshot(roots, resolved, graphSnapshot) {
		return nil, nil, false
	}
	filtered := filterGraphSnapshot(graphSnapshot, resolved)
	return resolved, filtered, true
}

func snapshotMatchesRequirements(st *store.Store, reqHash string) bool {
	meta := st.MetaSnapshot()
	return meta.RequirementsHash != "" && meta.RequirementsHash == reqHash
}

func buildResolvedSnapshot(cfg *config.Config, resolvedSnapshot map[string]store.ResolvedEntry) (map[string]collection, bool) {
	resolved := make(map[string]collection, len(resolvedSnapshot))
	for fqdn, entry := range resolvedSnapshot {
		col, ok := collectionFromResolvedEntry(cfg, fqdn, entry)
		if !ok {
			return nil, false
		}
		resolved[fqdn] = col
	}
	return resolved, true
}

// collectionFromResolvedEntry rebuilds a snapshot entry for every snapshot
// reader: a Galaxy entry without a source takes the run's server, and a git or
// url entry keeps its pinned locator, which everything downstream keys on.
func collectionFromResolvedEntry(cfg *config.Config, fqdn string, entry store.ResolvedEntry) (collection, bool) {
	if entry.Version == "" {
		return collection{}, false
	}
	namespace, name, ok := helpers.SplitFQDN(fqdn)
	if !ok {
		return collection{}, false
	}
	col := collection{
		Namespace: namespace,
		Name:      name,
		Version:   entry.Version,
		Source:    entry.Source,
	}
	switch {
	case gitsource.IsLocator(entry.Source):
		loc, err := gitsource.ParseLocator(entry.Source)
		if err != nil || !loc.Pinned() {
			return collection{}, false
		}
		col.Type = typeGit
		col.Ref = entry.Ref
	case urlsource.IsLocator(entry.Source):
		loc, err := urlsource.ParseLocator(entry.Source)
		if err != nil || !loc.Pinned() {
			return collection{}, false
		}
		col.Type = typeURL
		col.SHA256 = loc.SHA256
	case entry.Source == "":
		col.Source = cfg.Server
	}
	return col, true
}

func rootsMatchSnapshot(roots []collection, resolved map[string]collection, graphSnapshot map[string][]string) bool {
	for _, root := range roots {
		if !isSupportedType(root.Type) {
			return false
		}
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		col, ok := resolved[fqdn]
		if !ok {
			return false
		}
		// A git or url root's locator carries its commit or digest: a snapshot entry
		// naming another one is another resolution, whatever its version.
		if !rootLocatorMatches(root, col) {
			return false
		}
		constraint := root.Constraint
		if constraint == "" {
			constraint = root.Version
		}
		ok, err := constraintSatisfied(col.Version, constraint)
		if err != nil || !ok {
			return false
		}
		if _, ok := graphSnapshot[col.key()]; !ok {
			return false
		}
	}
	return true
}

// rootLocatorMatches reports whether col's snapshot source still matches a
// locator-pinned root's own; a Galaxy root matches unconditionally, since
// its winning server is the resolution's to pick.
func rootLocatorMatches(root, col collection) bool {
	if !root.isGit() && !root.isURL() {
		return true
	}
	return col.Source == root.Source
}

func filterGraphSnapshot(graphSnapshot map[string][]string, resolved map[string]collection) map[string][]string {
	validKeys := make(map[string]bool, len(resolved))
	for _, col := range resolved {
		validKeys[col.key()] = true
	}
	filtered := make(map[string][]string, len(graphSnapshot))
	for key, deps := range graphSnapshot {
		if !validKeys[key] {
			continue
		}
		out := make([]string, 0, len(deps))
		for _, dep := range deps {
			if validKeys[dep] {
				out = append(out, dep)
			}
		}
		filtered[key] = out
	}
	return filtered
}

// constraintSatisfied reports whether version satisfies constraint.
func constraintSatisfied(version, constraint string) (bool, error) {
	normalized := helpers.NormalizeConstraint(constraint)
	if normalized == "" {
		return true, nil
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return false, fmt.Errorf("invalid version %q: %w", version, err)
	}
	c, err := semver.NewConstraint(normalized)
	if err != nil {
		return false, fmt.Errorf("invalid constraint %q: %w", normalized, err)
	}
	return c.Check(v), nil
}

// buildInstallLevels topologically groups nodes for installation order.
func buildInstallLevels(graph map[string][]string) ([][]string, error) {
	indegree, reverse := buildDependencyIndex(graph)
	return topologicalLevels(indegree, reverse)
}

func buildDependencyIndex(graph map[string][]string) (map[string]int, map[string][]string) {
	indegree := make(map[string]int, len(graph))
	reverse := make(map[string][]string, len(graph))
	for node, deps := range graph {
		if _, ok := indegree[node]; !ok {
			indegree[node] = 0
		}
		for _, dep := range deps {
			indegree[node]++
			reverse[dep] = append(reverse[dep], node)
			if _, ok := indegree[dep]; !ok {
				indegree[dep] = 0
			}
		}
	}
	return indegree, reverse
}

// topologicalLevels groups nodes into install levels by indegree and fails
// with ErrDependencyGraphHasACycle if any stay unplaced. Levels are sorted so
// dispatch order matches sortTasksByLevel's prefetch queue (latency only).
func topologicalLevels(indegree map[string]int, reverse map[string][]string) ([][]string, error) {
	current := make([]string, 0, len(indegree))
	for node, deg := range indegree {
		if deg == 0 {
			current = append(current, node)
		}
	}
	levels := make([][]string, 0, len(indegree))
	remaining := len(indegree)
	for len(current) > 0 {
		slices.Sort(current)
		levels = append(levels, current)
		remaining -= len(current)
		next := make([]string, 0, len(current))
		for _, node := range current {
			for _, child := range reverse[node] {
				indegree[child]--
				if indegree[child] == 0 {
					next = append(next, child)
				}
			}
		}
		current = next
	}
	if remaining > 0 {
		return nil, helpers.ErrDependencyGraphHasACycle
	}
	return levels, nil
}
