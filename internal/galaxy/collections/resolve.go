package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"

	"github.com/Masterminds/semver"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// resolveTask describes a dependency resolution task.
type resolveTask struct {
	FQDN        string
	Namespace   string
	Name        string
	Constraints []string
	Source      string
}

// resolveResult captures the outcome of resolving one collection.
type resolveResult struct {
	FQDN      string
	Namespace string
	Name      string
	Source    string
	Version   string
	Deps      map[string]string
	Err       error
}

// resolveCollectionsInternal resolves versions and dependencies for roots.
func resolveCollectionsInternal(
	ctx context.Context,
	deps collectionDeps,
	roots []collection,
	allowSnapshot bool,
	record bool,
) (map[string]collection, map[string][]string, error) {
	cfg := deps.cfg
	st := deps.st
	if cfg.NoDeps {
		resolved, graph := resolveWithoutDeps(cfg, st, roots, record)
		return resolved, graph, nil
	}

	reqSpec := buildRequirementsSpec(cfg, roots)
	reqHash := requirementsSignatureFromSpec(reqSpec)

	snapshotAllowed := allowSnapshot && st != nil
	if snapshotAllowed {
		resolvedSnap, graphSnap, ok, err := resolveFromSnapshots(ctx, deps, roots, reqSpec, reqHash)
		if shouldReturnSnapshot(ok, err) {
			return resolvedSnap, graphSnap, err
		}
	}

	state, err := newResolverState(cfg, roots)
	if err != nil {
		return nil, nil, err
	}
	if err := state.resolveQueue(ctx, deps); err != nil {
		return nil, nil, err
	}
	resolved, graph, err := state.buildGraph(roots)
	if err != nil {
		return nil, nil, err
	}
	recordResolutionIfNeeded(st, record, resolved, graph, reqHash, cfg.Server, reqSpec)
	return resolved, graph, nil
}

func shouldReturnSnapshot(ok bool, err error) bool {
	return ok || err != nil
}

func recordResolutionIfNeeded(
	st *store.Store,
	record bool,
	resolved map[string]collection,
	graph map[string][]string,
	reqHash string,
	server string,
	reqSpec map[string]requirementSpec,
) {
	if !record || st == nil {
		return
	}
	recordResolution(st, resolved, graph, reqHash, server, reqSpec)
}

type resolverState struct {
	cfg            *config.Config
	resolved       map[string]collection
	depsByParent   map[string]map[string]string
	depConstraints map[string]map[string]string
	sourceByFQDN   map[string]string
	queue          []string
	queued         map[string]bool
}

func resolveWithoutDeps(cfg *config.Config, st *store.Store, roots []collection, record bool) (map[string]collection, map[string][]string) {
	resolved := make(map[string]collection, len(roots))
	graph := make(map[string][]string, len(roots))
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		resolved[fqdn] = root
		graph[root.key()] = nil
	}
	if record && st != nil {
		spec := buildRequirementsSpec(cfg, roots)
		recordResolution(st, resolved, graph, requirementsSignatureFromSpec(spec), cfg.Server, spec)
	}
	return resolved, graph
}

func resolveFromSnapshots(
	ctx context.Context,
	deps collectionDeps,
	roots []collection,
	reqSpec map[string]requirementSpec,
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
	reqSpec map[string]requirementSpec,
) {
	setResolvedAll(st, resolved)
	st.SetGraphSnapshot(graph)
	st.SetMetaRequirements(reqHash, server)
	st.SetRequirements(reqSpec)
}

func newResolverState(cfg *config.Config, roots []collection) (*resolverState, error) {
	state := &resolverState{
		cfg:            cfg,
		resolved:       make(map[string]collection),
		depsByParent:   make(map[string]map[string]string),
		depConstraints: make(map[string]map[string]string),
		sourceByFQDN:   make(map[string]string),
		queue:          make([]string, 0, len(roots)),
		queued:         make(map[string]bool),
	}
	if err := state.enqueueRoots(roots); err != nil {
		return nil, err
	}
	return state, nil
}

func (r *resolverState) enqueueRoots(roots []collection) error {
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		source := root.Source
		if source == "" {
			source = r.cfg.Server
		}
		r.sourceByFQDN[fqdn] = source
		constraint := root.Constraint
		if constraint == "" {
			constraint = root.Version
		}
		if err := addRootConstraint(r.depConstraints, fqdn, constraint); err != nil {
			return err
		}
		if !r.queued[fqdn] {
			r.queue = append(r.queue, fqdn)
			r.queued[fqdn] = true
		}
	}
	return nil
}

func (r *resolverState) resolveQueue(ctx context.Context, deps collectionDeps) error {
	for len(r.queue) > 0 {
		tasks, err := r.buildTasks()
		if err != nil {
			return err
		}
		r.resetQueue()
		results := resolveBatch(ctx, deps, tasks)
		if err := r.applyResults(results); err != nil {
			return err
		}
	}
	return nil
}

func (r *resolverState) buildTasks() ([]resolveTask, error) {
	tasks := make([]resolveTask, 0, len(r.queue))
	for _, fqdn := range r.queue {
		namespace, name, ok := helpers.SplitFQDN(fqdn)
		if !ok {
			return nil, fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionName, fqdn)
		}
		constraints := constraintsFor(r.depConstraints, fqdn)
		source := r.sourceByFQDN[fqdn]
		if source == "" {
			source = r.cfg.Server
		}
		tasks = append(tasks, resolveTask{
			FQDN:        fqdn,
			Namespace:   namespace,
			Name:        name,
			Constraints: constraints,
			Source:      source,
		})
	}
	return tasks, nil
}

func (r *resolverState) resetQueue() {
	r.queue = r.queue[:0]
	r.queued = make(map[string]bool)
}

func (r *resolverState) applyResults(results []resolveResult) error {
	for _, res := range results {
		if res.Err != nil {
			return res.Err
		}
		r.applyResult(res)
	}
	return nil
}

func (r *resolverState) applyResult(res resolveResult) {
	parentFQDN := res.FQDN
	previous, ok := r.resolved[parentFQDN]
	if !ok || previous.Version != res.Version {
		r.resolved[parentFQDN] = collection{
			Namespace: res.Namespace,
			Name:      res.Name,
			Version:   res.Version,
			Source:    res.Source,
		}
	}

	changedDeps := applyDependencyConstraints(parentFQDN, res.Deps, r.depConstraints, r.depsByParent)
	for depFQDN := range res.Deps {
		if _, ok := r.sourceByFQDN[depFQDN]; !ok {
			r.sourceByFQDN[depFQDN] = r.cfg.Server
		}
	}
	r.enqueueChanges(changedDeps)
}

func (r *resolverState) enqueueChanges(changedDeps map[string]bool) {
	for depFQDN := range changedDeps {
		if !r.queued[depFQDN] {
			r.queue = append(r.queue, depFQDN)
			r.queued[depFQDN] = true
		}
	}
}

func (r *resolverState) buildGraph(roots []collection) (map[string]collection, map[string][]string, error) {
	r.pruneUnreachable(roots)
	graph, err := buildGraphFromDeps(r.resolved, r.depsByParent)
	if err != nil {
		return nil, nil, err
	}
	ensureGraphNodes(r.resolved, graph)
	return r.resolved, graph, nil
}

func (r *resolverState) pruneUnreachable(roots []collection) {
	reachable := collectReachable(roots, r.depsByParent)
	for fqdn := range r.resolved {
		if !reachable[fqdn] {
			delete(r.resolved, fqdn)
		}
	}
	for parent, deps := range r.depsByParent {
		if !reachable[parent] {
			delete(r.depsByParent, parent)
			continue
		}
		for dep := range deps {
			if !reachable[dep] {
				delete(deps, dep)
			}
		}
	}
}

func buildGraphFromDeps(resolved map[string]collection, depsByParent map[string]map[string]string) (map[string][]string, error) {
	graph := make(map[string][]string)
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

// resolveBatch resolves a batch of tasks concurrently.
func resolveBatch(ctx context.Context, deps collectionDeps, tasks []resolveTask) []resolveResult {
	results := make([]resolveResult, 0, len(tasks))
	if len(tasks) == 0 {
		return results
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, deps.cfg.Workers)

	for _, task := range tasks {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			res := resolveOne(ctx, deps, task)
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		})
	}

	wg.Wait()
	return results
}

// resolveOne resolves a single collection version and dependencies.
func resolveOne(ctx context.Context, deps collectionDeps, task resolveTask) resolveResult {
	cfg := deps.cfg
	st := deps.st

	col := collection{
		Namespace: task.Namespace,
		Name:      task.Name,
		Source:    task.Source,
	}

	version, exact, err := exactVersionFromConstraints(task.Constraints)
	if err != nil {
		return resolveResult{FQDN: task.FQDN, Namespace: task.Namespace, Name: task.Name, Err: err}
	}
	policy := cachePolicyForConstraint(cfg, exact)
	if exact {
		if res, ok := cachedResult(task, version, st, policy); ok {
			return res
		}
	}

	rootMeta, versionsURL, err := resolveRootMetadata(ctx, deps, col, policy, task.FQDN)
	if err != nil {
		return resolveResult{FQDN: task.FQDN, Namespace: task.Namespace, Name: task.Name, Err: err}
	}

	version, err = resolveFinalVersion(ctx, deps, task, policy, version, exact, rootMeta, versionsURL)
	if err != nil {
		return resolveResult{FQDN: task.FQDN, Namespace: task.Namespace, Name: task.Name, Err: err}
	}

	if res, ok := cachedResult(task, version, st, policy); ok {
		return res
	}

	versionInfo, err := fetchVersionMetadataCached(ctx, deps, col.Source, versionsURL, version, policy)
	if err != nil {
		return resolveResult{FQDN: task.FQDN, Namespace: task.Namespace, Name: task.Name, Err: err}
	}

	depMap, err := parseDependencies(extractDependencies(versionInfo), err)
	if err != nil {
		return resolveResult{
			FQDN:      task.FQDN,
			Namespace: task.Namespace,
			Name:      task.Name,
			Err:       err,
		}
	}

	cacheKey := fmt.Sprintf("%s.%s@%s", task.Namespace, task.Name, version)
	cacheDeps(st, policy, cacheKey, depMap)
	return buildResolveResult(task, version, depMap)
}

func resolveFinalVersion(
	ctx context.Context,
	deps collectionDeps,
	task resolveTask,
	policy cacheManager.Policy,
	version string,
	exact bool,
	rootMeta *types.GalaxyCollection,
	versionsURL string,
) (string, error) {
	if exact {
		return version, nil
	}
	return resolveNonExactVersion(ctx, deps, task, policy, version, rootMeta, versionsURL)
}

func cachedResult(task resolveTask, version string, st *store.Store, policy cacheManager.Policy) (resolveResult, bool) {
	cacheKey := fmt.Sprintf("%s.%s@%s", task.Namespace, task.Name, version)
	deps, ok := cachedDeps(st, policy, cacheKey)
	if !ok {
		return resolveResult{}, false
	}
	return buildResolveResult(task, version, deps), true
}

func cacheDeps(st *store.Store, policy cacheManager.Policy, cacheKey string, deps map[string]string) {
	if st == nil || !policy.Write {
		return
	}
	st.SetDepsCache(cacheKey, deps)
}

func buildResolveResult(task resolveTask, version string, deps map[string]string) resolveResult {
	return resolveResult{
		FQDN:      task.FQDN,
		Namespace: task.Namespace,
		Name:      task.Name,
		Source:    task.Source,
		Version:   version,
		Deps:      deps,
	}
}

func cachedDeps(st *store.Store, policy cacheManager.Policy, cacheKey string) (map[string]string, bool) {
	if st == nil || !policy.Read {
		return nil, false
	}
	deps, ok := st.GetDepsCache(cacheKey)
	return deps, ok
}

func resolveRootMetadata(
	ctx context.Context,
	deps collectionDeps,
	col collection,
	policy cacheManager.Policy,
	label string,
) (*types.GalaxyCollection, string, error) {
	runtime := deps.runtime
	versionsURL := collectionVersionsURL(col)
	rootMeta, err := loadRootMetadataCached(ctx, deps, col, policy)
	if err != nil {
		return nil, "", err
	}
	if rootMeta != nil && rootMeta.VersionsURL != "" {
		versionsURL = normalizeVersionsURL(col.Source, rootMeta.VersionsURL)
		runtime.Output.Debugf("versions URL for %s: %s", label, versionsURL)
	}
	return rootMeta, versionsURL, nil
}

func extractDependencies(info *types.GalaxyCollectionVersionInfo) map[string]string {
	if len(info.Metadata.Dependencies) > 0 {
		return info.Metadata.Dependencies
	}
	return info.Manifest.CollectionInfo.Dependencies
}

func parseDependencies(deps map[string]string, baseErr error) (map[string]string, error) {
	parsedDeps := make(map[string]string)
	for dep, constraint := range deps {
		if _, _, ok := helpers.SplitFQDN(dep); !ok {
			return nil, fmt.Errorf("invalid dependency key %q: %w", dep, baseErr)
		}
		parsedDeps[dep] = strings.TrimSpace(constraint)
	}
	return parsedDeps, nil
}

// selectVersion picks the highest version that satisfies constraints.
func selectVersion(versions, constraints []string) (string, error) {
	type candidate struct {
		version string
		semver  *semver.Version
	}
	candidates := make([]candidate, 0, len(versions))
	for _, v := range versions {
		parsed, err := semver.NewVersion(v)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{
			version: v,
			semver:  parsed,
		})
	}
	if len(candidates) == 0 {
		return "", helpers.ErrNoSemverCandidates
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].semver.GreaterThan(candidates[j].semver)
	})

	parsedConstraints, err := parseConstraints(constraints)
	if err != nil {
		return "", err
	}

	for _, c := range candidates {
		ok := true
		for _, constraint := range parsedConstraints {
			if !constraint.Check(c.semver) {
				ok = false
				break
			}
		}
		if ok {
			return c.version, nil
		}
	}

	return "", fmt.Errorf("%w: %v", helpers.ErrNoVersionSatisfiesConstraints, constraints)
}

// constraintsSatisfiedByVersion reports whether version matches constraints.
func constraintsSatisfiedByVersion(version string, constraints []string) (bool, error) {
	if len(constraints) == 0 {
		return true, nil
	}
	parsed, err := parseConstraints(constraints)
	if err != nil {
		return false, err
	}
	if len(parsed) == 0 {
		return true, nil
	}
	parsedVersion, err := semver.NewVersion(version)
	if err != nil {
		return false, fmt.Errorf("invalid version %q: %w", version, err)
	}
	for _, constraint := range parsed {
		if !constraint.Check(parsedVersion) {
			return false, nil
		}
	}
	return true, nil
}

// loadVersionsListCached loads the available versions list with caching.
func loadVersionsListCached(
	ctx context.Context,
	deps collectionDeps,
	versionsURL string,
	limit int,
	policy cacheManager.Policy,
) ([]string, error) {
	if versions, ok := cachedVersionsList(deps.st, policy, versionsURL); ok {
		return versions, nil
	}
	versions, total, err := fetchVersionsPage(ctx, deps, policy, versionsURL, limit)
	if err != nil {
		return nil, err
	}
	if total > limit {
		return loadVersionsListCached(ctx, deps, versionsURL, total, policy)
	}
	cacheVersionsList(deps.st, policy, versionsURL, versions)
	return versions, nil
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

func fetchVersionsPage(
	ctx context.Context,
	deps collectionDeps,
	policy cacheManager.Policy,
	versionsURL string,
	limit int,
) ([]string, int, error) {
	url := fmt.Sprintf("%s?limit=%d&offset=0", versionsURL, limit)
	var payload map[string]any
	if err := fetchJSONWithCachePolicy(ctx, deps.runtime.HTTP, url, deps.st, &payload, policy); err != nil {
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

// resolveNonExactVersion selects a version when constraints are not exact.
func resolveNonExactVersion(
	ctx context.Context,
	deps collectionDeps,
	task resolveTask,
	policy cacheManager.Policy,
	version string,
	rootMeta *types.GalaxyCollection,
	versionsURL string,
) (string, error) {
	runtime := deps.runtime

	if rootMeta != nil && rootMeta.HighestVersion.Version != "" {
		ok, err := constraintsSatisfiedByVersion(rootMeta.HighestVersion.Version, task.Constraints)
		if err != nil {
			return "", err
		}
		if ok {
			runtime.Output.Debugf("highest_version selected for %s: %s", task.FQDN, rootMeta.HighestVersion.Version)
			return rootMeta.HighestVersion.Version, nil
		}
	}
	if version != "" {
		return version, nil
	}
	runtime.Output.Debugf("resolving versions list for %s", task.FQDN)
	versionsMeta, err := loadVersionsListCached(ctx, deps, versionsURL, versionLimit, policy)
	if err != nil {
		return "", err
	}
	return selectVersion(versionsMeta, task.Constraints)
}

// parseConstraints parses version constraints into semver constraints.
func parseConstraints(list []string) ([]*semver.Constraints, error) {
	result := make([]*semver.Constraints, 0, len(list))
	for _, raw := range list {
		normalized := normalizeConstraint(raw)
		if normalized == "" {
			continue
		}
		c, err := semver.NewConstraint(normalized)
		if err != nil {
			return nil, fmt.Errorf("invalid constraint %q: %w", normalized, err)
		}
		result = append(result, c)
	}
	return result, nil
}

// addRootConstraint records a constraint for a root collection.
func addRootConstraint(depConstraints map[string]map[string]string, fqdn, version string) error {
	constraint := normalizeConstraint(version)
	if constraint == "" {
		return nil
	}
	if _, ok := depConstraints[fqdn]; !ok {
		depConstraints[fqdn] = make(map[string]string)
	}
	existing, ok := depConstraints[fqdn]["root"]
	if ok && existing != constraint {
		return fmt.Errorf("%w for %s: %q vs %q", helpers.ErrConflictingRootConstraints, fqdn, existing, constraint)
	}
	depConstraints[fqdn]["root"] = constraint
	return nil
}

// applyDependencyConstraints merges dependency constraints and reports changes.
func applyDependencyConstraints(
	parentFQDN string,
	newDeps map[string]string,
	depConstraints map[string]map[string]string,
	depsByParent map[string]map[string]string,
) map[string]bool {
	changed := make(map[string]bool)
	oldDeps := depsByParent[parentFQDN]
	if oldDeps == nil {
		oldDeps = make(map[string]string)
	}

	for dep := range oldDeps {
		if _, ok := newDeps[dep]; !ok {
			if removeConstraint(depConstraints, dep, parentFQDN) {
				changed[dep] = true
			}
		}
	}

	for dep, constraint := range newDeps {
		if setConstraint(depConstraints, dep, parentFQDN, constraint) {
			changed[dep] = true
		}
	}

	depsByParent[parentFQDN] = newDeps
	return changed
}

// setConstraint records a constraint for a dependency and reports changes.
func setConstraint(depConstraints map[string]map[string]string, dep, source, constraint string) bool {
	if _, ok := depConstraints[dep]; !ok {
		depConstraints[dep] = make(map[string]string)
	}
	current, ok := depConstraints[dep][source]
	if ok && current == constraint {
		return false
	}
	depConstraints[dep][source] = constraint
	return true
}

// removeConstraint removes a constraint and reports whether it changed.
func removeConstraint(depConstraints map[string]map[string]string, dep, source string) bool {
	if _, ok := depConstraints[dep]; !ok {
		return false
	}
	if _, ok := depConstraints[dep][source]; !ok {
		return false
	}
	delete(depConstraints[dep], source)
	if len(depConstraints[dep]) == 0 {
		delete(depConstraints, dep)
	}
	return true
}

// constraintsFor returns the normalized constraints for a collection.
func constraintsFor(depConstraints map[string]map[string]string, fqdn string) []string {
	sources := depConstraints[fqdn]
	if len(sources) == 0 {
		return nil
	}
	out := make([]string, 0, len(sources))
	for _, c := range sources {
		normalized := normalizeConstraint(c)
		if normalized == "" {
			continue
		}
		out = append(out, normalized)
	}
	return out
}

// collectionVersionsURL builds the versions API URL for a collection.
func collectionVersionsURL(col collection) string {
	base := strings.TrimRight(col.Source, "/")
	return fmt.Sprintf("%s/api/v3/collections/%s/%s/versions/", base, col.Namespace, col.Name)
}

// normalizeConstraint trims and normalizes a version constraint.
func normalizeConstraint(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "*" {
		return ""
	}
	return trimmed
}

// normalizeSignatures trims, sorts, and filters signatures.
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
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// normalizeRequirementConstraint normalizes a constraint for hashing.
func normalizeRequirementConstraint(value string) string {
	normalized := normalizeConstraint(value)
	if normalized == "" {
		return "*"
	}
	return normalized
}

// requirementSpecEqual reports whether two requirement specs are equal.
func requirementSpecEqual(a, b requirementSpec) bool {
	if a.Constraint != b.Constraint || a.Source != b.Source || a.Type != b.Type {
		return false
	}
	left := normalizeSignatures(a.Signatures)
	right := normalizeSignatures(b.Signatures)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// exactVersionFromConstraints returns a single exact version if specified.
func exactVersionFromConstraints(constraints []string) (string, bool, error) {
	exact := ""
	for _, raw := range constraints {
		normalized := normalizeConstraint(raw)
		if normalized == "" {
			continue
		}
		if after, contains := strings.CutPrefix(normalized, "="); contains {
			normalized = strings.TrimSpace(after)
		}
		if strings.ContainsAny(normalized, "<>~^!*|") || strings.Contains(normalized, " ") {
			return "", false, nil
		}
		_, err := semver.NewVersion(normalized)
		if err != nil {
			return "", false, fmt.Errorf("invalid version %q: %w", normalized, err)
		}
		if exact == "" {
			exact = normalized
			continue
		}
		if exact != normalized {
			return "", false, fmt.Errorf("%w: %s vs %s", helpers.ErrConflictingExactVersions, exact, normalized)
		}
	}
	if exact == "" {
		return "", false, nil
	}
	return exact, true, nil
}

// collectReachable returns the set of reachable collections from roots.
func collectReachable(roots []collection, depsByParent map[string]map[string]string) map[string]bool {
	reachable := make(map[string]bool)
	queue := make([]string, 0, len(roots))

	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		if !reachable[fqdn] {
			reachable[fqdn] = true
			queue = append(queue, fqdn)
		}
	}

	for len(queue) > 0 {
		fqdn := queue[0]
		queue = queue[1:]
		deps := depsByParent[fqdn]
		for dep := range deps {
			if !reachable[dep] {
				reachable[dep] = true
				queue = append(queue, dep)
			}
		}
	}

	return reachable
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
	currentSpec map[string]requirementSpec,
	reqHash string,
) (map[string]collection, map[string][]string, bool, error) {
	prevSpec := deps.st.RequirementsSnapshot()
	if len(prevSpec) == 0 {
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
	currentSpec map[string]requirementSpec,
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

	resolvedNew, graphNew, err := resolveCollectionsInternal(ctx, deps, changedRoots, false, false)
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

func splitRootsByChange(roots []collection, currentSpec, prevSpec map[string]requirementSpec) ([]collection, []collection) {
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
	namespace, name, ok := helpers.SplitFQDN(fqdn)
	if !ok {
		return false
	}
	source := entry.Source
	if source == "" {
		source = cfg.Server
	}
	preservedResolved[fqdn] = collection{
		Namespace: namespace,
		Name:      name,
		Version:   version,
		Source:    source,
	}
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
	namespace, name, ok := helpers.SplitFQDN(fqdn)
	if !ok {
		return false
	}
	source := entry.Source
	if source == "" {
		source = cfg.Server
	}
	mergedResolved[fqdn] = collection{
		Namespace: namespace,
		Name:      name,
		Version:   version,
		Source:    source,
	}
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

// buildRequirementsSpec builds a normalized requirement spec map.
func buildRequirementsSpec(cfg *config.Config, roots []collection) map[string]requirementSpec {
	spec := make(map[string]requirementSpec, len(roots))
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		source := root.Source
		if source == "" {
			source = cfg.Server
		}
		constraint := root.Constraint
		if constraint == "" {
			constraint = root.Version
		}
		constraint = normalizeRequirementConstraint(constraint)
		spec[fqdn] = requirementSpec{
			Constraint: constraint,
			Source:     source,
			Type:       root.Type,
			Signatures: normalizeSignatures(root.Signatures),
		}
	}
	return spec
}

// requirementsSignatureFromSpec returns a stable signature of requirements.
func requirementsSignatureFromSpec(spec map[string]requirementSpec) string {
	parts := make([]string, 0, len(spec))
	for fqdn, entry := range spec {
		constraint := entry.Constraint
		if constraint == "" {
			constraint = "*"
		}
		signatureKey := strings.Join(normalizeSignatures(entry.Signatures), ",")
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%s", fqdn, constraint, entry.Source, entry.Type, signatureKey))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
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
		if entry.Version == "" {
			return nil, false
		}
		namespace, name, ok := helpers.SplitFQDN(fqdn)
		if !ok {
			return nil, false
		}
		source := entry.Source
		if source == "" {
			source = cfg.Server
		}
		resolved[fqdn] = collection{
			Namespace: namespace,
			Name:      name,
			Version:   entry.Version,
			Source:    source,
		}
	}
	return resolved, true
}

func rootsMatchSnapshot(roots []collection, resolved map[string]collection, graphSnapshot map[string][]string) bool {
	for _, root := range roots {
		if !isGalaxyType(root.Type) {
			return false
		}
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		col, ok := resolved[fqdn]
		if !ok {
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
	normalized := normalizeConstraint(constraint)
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
	indegree := make(map[string]int)
	reverse := make(map[string][]string)
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

func topologicalLevels(indegree map[string]int, reverse map[string][]string) ([][]string, error) {
	levels := make([][]string, 0)
	for len(indegree) > 0 {
		level := nextLevel(indegree)
		if len(level) == 0 {
			return nil, helpers.ErrDependencyGraphHasACycle
		}
		levels = append(levels, level)
		applyLevel(indegree, reverse, level)
	}
	return levels, nil
}

func nextLevel(indegree map[string]int) []string {
	level := make([]string, 0)
	for node, deg := range indegree {
		if deg == 0 {
			level = append(level, node)
		}
	}
	return level
}

func applyLevel(indegree map[string]int, reverse map[string][]string, level []string) {
	for _, node := range level {
		delete(indegree, node)
		for _, child := range reverse[node] {
			indegree[child]--
		}
	}
}
