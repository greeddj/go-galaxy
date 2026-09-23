package collections

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// gitPin is what discovery learned about one git collection, in the shape the
// solver and install phase consume: locator, requested ref, exact version,
// validated deps and, under --no-cache only, the build for the install phase.
type gitPin struct {
	prebuilt *downloadResult
	deps     map[string]string
	locator  string
	ref      string
	version  string
}

// gitDiscoveryMemo is the run-wide table of discovered git collections by
// fqdn, created once per run and shared by the resolve, prefetch and install
// phases, so the solver answers a git fqdn without contacting the remote.
type gitDiscoveryMemo struct {
	pins map[string]gitPin
	mu   sync.Mutex
}

func newGitDiscoveryMemo() *gitDiscoveryMemo {
	return &gitDiscoveryMemo{pins: make(map[string]gitPin)}
}

func (m *gitDiscoveryMemo) put(fqdn string, pin gitPin) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pins[fqdn] = pin
}

// takePrebuilt hands out a --no-cache build exactly once: the install worker
// that takes it owns its cleanup from then on.
func (m *gitDiscoveryMemo) takePrebuilt(fqdn string) (downloadResult, bool) {
	if m == nil {
		return downloadResult{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pin, ok := m.pins[fqdn]
	if !ok || pin.prebuilt == nil {
		return downloadResult{}, false
	}
	result := *pin.prebuilt
	pin.prebuilt = nil
	m.pins[fqdn] = pin
	return result, true
}

// snapshot returns a copy of the pin table for the solver provider.
func (m *gitDiscoveryMemo) snapshot() map[string]gitPin {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]gitPin, len(m.pins))
	for fqdn, pin := range m.pins {
		pin.prebuilt = nil
		out[fqdn] = pin
	}
	return out
}

// cleanup removes every --no-cache build nobody took.
func (m *gitDiscoveryMemo) cleanup() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for fqdn, pin := range m.pins {
		if pin.prebuilt != nil {
			cleanupIfNeeded(pin.prebuilt.Cleanup)
			pin.prebuilt = nil
			m.pins[fqdn] = pin
		}
	}
}

// expandGitRoots replaces every unpinned git root with the collections its
// repository holds at the resolved commit, on the download-worker pool and in
// input order. It is the only place resolution contacts a git remote.
func expandGitRoots(ctx context.Context, deps collectionDeps, roots []collection) ([]collection, error) {
	if !anyUnpinnedGit(roots) {
		return roots, nil
	}
	if deps.runtime == nil || deps.runtime.Git == nil {
		return nil, fmt.Errorf("%w: no git client is wired into this run", helpers.ErrConfigIsNil)
	}
	results := make([][]collection, len(roots))
	errs := make([]error, len(roots))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(deps.cfg.DownloadWorkers, 1))
	for i, root := range roots {
		if !root.isGit() || isPinnedLocator(root.Source) {
			results[i] = []collection{root}
			continue
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i], errs[i] = expandGitRoot(ctx, deps, root)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	expanded := make([]collection, 0, len(roots))
	for _, group := range results {
		expanded = append(expanded, group...)
	}
	return expanded, nil
}

func anyUnpinnedGit(roots []collection) bool {
	for _, root := range roots {
		if root.isGit() && !isPinnedLocator(root.Source) {
			return true
		}
	}
	return false
}

func isPinnedLocator(source string) bool {
	loc, err := gitsource.ParseLocator(source)
	return err == nil && loc.Pinned()
}

func checkExpandedDuplicates(roots []collection) ([]collection, error) {
	seen := make(map[string]string, len(roots))
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		if other, dup := seen[fqdn]; dup {
			return nil, fmt.Errorf("%w for %s (%s and %s)", helpers.ErrDuplicateCollectionRequirement, fqdn,
				helpers.URLForMessage(other), helpers.URLForMessage(root.Source))
		}
		seen[fqdn] = root.Source
	}
	return roots, nil
}

// gitRootRequest is one unpinned git root taken apart: the parsed URL and
// ref, the subdir, the credential bound to the URL, and the pin key.
type gitRootRequest struct {
	subdir  string
	pinKey  string
	display string
	cred    gitsource.Credential
	root    collection
	url     gitsource.URL
	ref     gitsource.Ref
}

func newGitRootRequest(deps collectionDeps, root collection) (gitRootRequest, error) {
	loc, err := root.gitLocator()
	if err != nil {
		return gitRootRequest{}, err
	}
	u, err := gitsource.ParseURL(loc.URL)
	if err != nil {
		return gitRootRequest{}, err
	}
	ref, err := gitsource.ParseRef(root.Ref)
	if err != nil {
		return gitRootRequest{}, err
	}
	cred, _ := gitsource.MatchCredential(u, gitCredentialsOf(deps))
	return gitRootRequest{
		root:    root,
		url:     u,
		ref:     ref,
		cred:    cred,
		subdir:  loc.Subdir,
		pinKey:  gitsource.PinKey(loc.URL, ref.Name, loc.Subdir),
		display: helpers.URLForMessage(loc.URL),
	}, nil
}

// gitCredentialsOf returns the run's revealed git credentials, which the
// command wiring stores on the Infra beside the client.
func gitCredentialsOf(deps collectionDeps) []gitsource.Credential {
	if deps.runtime == nil {
		return nil
	}
	return deps.runtime.GitCredentials
}

// expandGitRoot resolves one unpinned git root into its collections.
func expandGitRoot(ctx context.Context, deps collectionDeps, root collection) ([]collection, error) {
	req, err := newGitRootRequest(deps, root)
	if err != nil {
		return nil, err
	}
	policy := cacheManager.PolicyForConstraint(deps.cfg, req.ref.IsCommit())
	if policy.Read {
		if pin, ok := deps.st.GetGitPin(req.pinKey); ok {
			return replayGitPin(deps, req, pin)
		}
	}
	if deps.cfg != nil && deps.cfg.Offline {
		return nil, fmt.Errorf("%w: git source %s@%s is not recorded in the cache", helpers.ErrOfflineMode, req.display, req.ref.Name)
	}
	if refreshed, ok, err := refreshGitPin(ctx, deps, req, policy); ok || err != nil {
		return refreshed, err
	}
	return acquireGitRoot(ctx, deps, req, policy, "")
}

// refreshGitPin is the cheap half of --refresh for a branch or tag pin: one
// advertisement keeps the pin if the commit is unchanged and every artifact is
// cached, else acquires the new tip. ok=false means there was no pin to refresh.
func refreshGitPin(ctx context.Context, deps collectionDeps, req gitRootRequest, policy cacheManager.Policy) ([]collection, bool, error) {
	if deps.cfg == nil || !deps.cfg.Refresh || req.ref.IsCommit() {
		return nil, false, nil
	}
	pin, ok := deps.st.GetGitPin(req.pinKey)
	if !ok {
		return nil, false, nil
	}
	commit, _, err := deps.runtime.Git.Advertise(ctx, req.url, req.ref, req.cred)
	if err != nil {
		return nil, false, err
	}
	if commit != pin.Commit || !gitArtifactsCached(ctx, deps, req, pin) {
		expanded, err := acquireGitRoot(ctx, deps, req, policy, commit)
		return expanded, true, err
	}
	if policy.Write {
		deps.st.SetGitPin(req.pinKey, pin)
	}
	expanded, err := replayGitPin(deps, req, pin)
	return expanded, true, err
}

func gitArtifactsCached(ctx context.Context, deps collectionDeps, req gitRootRequest, pin store.GitPinEntry) bool {
	if deps.gitStore == nil {
		return false
	}
	for _, c := range pin.Collections {
		loc := gitsource.Locator{URL: req.url.String(), Subdir: c.Subdir, Commit: pin.Commit}
		key := helpers.ArtifactKey(loc.String(), helpers.ArtifactFilename(c.Namespace, c.Name, c.Version))
		ok, err := deps.gitStore.Has(ctx, key)
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// replayGitPin turns a recorded pin into expanded roots, re-validating every
// identity it carries: the pin is cache state, and cache state is judged on
// the way in exactly as a remote's answer is.
func replayGitPin(deps collectionDeps, req gitRootRequest, pin store.GitPinEntry) ([]collection, error) {
	if !gitsource.IsCommitHash(pin.Commit) {
		return nil, fmt.Errorf("%w: recorded pin for %s names commit %q", helpers.ErrInvalidGitLocator, req.display, pin.Commit)
	}
	expanded := make([]collection, 0, len(pin.Collections))
	pins := make(map[string]gitPin, len(pin.Collections))
	for _, c := range pin.Collections {
		col, p, err := gitCollectionRoot(req, pin.Commit, c.Namespace, c.Name, c.Version, c.Subdir, c.Dependencies)
		if err != nil {
			return nil, err
		}
		expanded = append(expanded, col)
		pins[col.fqdn()] = p
	}
	return recordSelected(deps, req, expanded, pins)
}

// acquireGitRoot fetches the repository, builds and stores its collections,
// records the pin and returns the expanded roots. A non-empty commit is the
// tip a --refresh advertisement just resolved, and exactly that is fetched.
func acquireGitRoot(
	ctx context.Context, deps collectionDeps, req gitRootRequest, policy cacheManager.Policy, commit string,
) ([]collection, error) {
	runtime := deps.runtime
	runtime.Output.Printf("Fetching %s@%s", req.display, req.ref.Name)
	start := time.Now()
	gitCtx, cancel := context.WithTimeout(ctx, runtime.GitDeadline())
	defer cancel()
	result, err := runtime.Git.Acquire(gitCtx, gitsource.Request{
		URL:      req.url,
		Ref:      req.ref,
		Subdir:   req.subdir,
		Commit:   commit,
		Auth:     req.cred,
		TempFile: gitTempFile(deps),
	})
	if err != nil {
		return nil, artifactDeadlineError(ctx, gitCtx, runtime.GitDeadline(), err)
	}
	runtime.Output.DebugSincef(start, "Fetch %s@%s (%d bytes, %d collections)",
		req.display, req.ref.Name, result.BytesFetched, len(result.Collections))
	runtime.Metrics.AddBytesDownloaded(result.BytesFetched)
	for _, warning := range result.Warnings {
		runtime.Output.Warnf("%s: %s", req.display, warning)
	}

	expanded := make([]collection, 0, len(result.Collections))
	pins := make(map[string]gitPin, len(result.Collections))
	entry := store.GitPinEntry{Commit: result.Commit}
	for _, built := range result.Collections {
		col, p, err := gitCollectionRoot(req, result.Commit,
			built.Namespace, built.Name, built.Version, built.Subdir, built.Dependencies)
		if err != nil {
			releaseBuilt(result.Collections)
			return nil, err
		}
		if keepsPrebuilt(deps) {
			p.prebuilt = &downloadResult{Path: built.ArtifactPath, SHA: built.ArtifactSHA, Cleanup: built.Cleanup}
		}
		expanded = append(expanded, col)
		pins[col.fqdn()] = p
		entry.Collections = append(entry.Collections, store.GitPinCollection{
			Namespace:    built.Namespace,
			Name:         built.Name,
			Version:      built.Version,
			Subdir:       built.Subdir,
			Dependencies: built.Dependencies,
		})
	}
	if err := storeGitArtifacts(ctx, deps, expanded, result.Collections); err != nil {
		return nil, err
	}
	if policy.Write {
		deps.st.SetGitPin(req.pinKey, entry)
	}
	return recordSelected(deps, req, expanded, pins)
}

// recordSelected narrows the expansion to what the root named before putting it
// in the memo, since a memo entry owns its fqdn in the solver against any Galaxy
// root or dependency. Builds of unselected siblings are removed here.
func recordSelected(deps collectionDeps, req gitRootRequest, expanded []collection, pins map[string]gitPin) ([]collection, error) {
	selected, err := selectExplicit(req, expanded)
	if err != nil {
		for _, p := range pins {
			if p.prebuilt != nil {
				cleanupIfNeeded(p.prebuilt.Cleanup)
			}
		}
		return nil, err
	}
	chosen := make(map[string]bool, len(selected))
	for _, col := range selected {
		chosen[col.fqdn()] = true
		deps.gitMemo.put(col.fqdn(), pins[col.fqdn()])
	}
	for fqdn, p := range pins {
		if !chosen[fqdn] && p.prebuilt != nil {
			cleanupIfNeeded(p.prebuilt.Cleanup)
		}
	}
	return selected, nil
}

// gitTempFile adapts the artifact store's TempFile to the client's callback
// under the download temp prefix, so a killed run's leftover is swept like a
// download temp. With no store, the run's temp directory stands in.
func gitTempFile(deps collectionDeps) gitsource.TempFileFunc {
	if deps.gitStore != nil {
		return func(ctx context.Context) (*os.File, func(), error) {
			return deps.gitStore.TempFile(ctx, helpers.ArtifactDownloadTempPrefix)
		}
	}
	dir := ""
	if deps.runtime != nil && deps.runtime.TempDir != nil {
		dir = deps.runtime.TempDir()
	}
	return func(context.Context) (*os.File, func(), error) {
		f, err := os.CreateTemp(dir, helpers.ArtifactDownloadTempPrefix+"*")
		if err != nil {
			return nil, nil, err
		}
		name := f.Name()
		return f, func() { _ = os.Remove(name) }, nil
	}
}

func releaseBuilt(built []gitsource.Collection) {
	for _, b := range built {
		cleanupIfNeeded(b.Cleanup)
	}
}

// gitCollectionRoot judges an identity and deps read from galaxy.yml or a
// cached pin by the same predicates as a Galaxy answer, and returns the root
// and its pin. It records nothing; recordSelected fills the memo.
func gitCollectionRoot(req gitRootRequest, commit, namespace, name, version, subdir string,
	rawDeps map[string]string,
) (collection, gitPin, error) {
	if !helpers.IsCollectionNamePart(namespace) || !helpers.IsCollectionNamePart(name) {
		return collection{}, gitPin{}, fmt.Errorf("%w: %s declares %q.%q",
			helpers.ErrInvalidCollectionName, req.display, namespace, name)
	}
	if !helpers.IsExactVersion(version) {
		return collection{}, gitPin{}, fmt.Errorf("%w: %s declares %s.%s version %q",
			helpers.ErrGitCollectionVersionNotExact, req.display, namespace, name, version)
	}
	parsedDeps, err := parseDependencies(rawDeps)
	if err != nil {
		return collection{}, gitPin{}, fmt.Errorf("%s.%s from %s: %w", namespace, name, req.display, err)
	}
	locator := gitsource.Locator{URL: req.url.String(), Subdir: subdir, Commit: commit}.String()
	pin := gitPin{locator: locator, ref: req.ref.Name, version: version, deps: parsedDeps}
	return collection{
		Namespace:  namespace,
		Name:       name,
		Version:    version,
		Source:     locator,
		Constraint: version,
		Type:       typeGit,
		Ref:        req.ref.Name,
	}, pin, nil
}

// keepsPrebuilt reports whether a build is handed to the install phase rather
// than committed to the artifact store: under --no-cache, or when no store is
// at hand, and never under --dry-run, which installs nothing.
func keepsPrebuilt(deps collectionDeps) bool {
	if deps.cfg != nil && deps.cfg.DryRun {
		return false
	}
	return deps.gitStore == nil || (deps.cfg != nil && deps.cfg.NoCache)
}

// storeGitArtifacts commits each build under its locator-scoped key, leaves it
// for the install phase under --no-cache or with no store, or discards it under
// --dry-run. ProbeTarGz is skipped: the builder already read the archive back.
func storeGitArtifacts(ctx context.Context, deps collectionDeps, expanded []collection, built []gitsource.Collection) error {
	for i, b := range built {
		col := expanded[i]
		switch {
		case deps.cfg != nil && deps.cfg.DryRun:
			cleanupIfNeeded(b.Cleanup)
		case deps.cfg != nil && deps.cfg.NoCache, deps.gitStore == nil:
			// acquireGitRoot kept it as the pin's prebuilt; the install phase takes it.
		default:
			key := helpers.ArtifactKey(col.Source, helpers.ArtifactFilename(col.Namespace, col.Name, col.Version))
			if _, err := commitDownload(ctx, deps.gitStore, key, b.ArtifactPath, b.ArtifactSHA, b.Cleanup); err != nil {
				releaseBuilt(built[i+1:])
				return fmt.Errorf("committing git artifact %s: %w", col.key(), err)
			}
			deps.runtime.Metrics.AddCacheMiss()
		}
	}
	return nil
}

// selectExplicit narrows a repository's collections to the one a root named
// (name: beside a git source:), as ansible installs only that one, else keeps
// all. A name the repository does not carry is helpers.ErrGitNameMismatch.
func selectExplicit(req gitRootRequest, expanded []collection) ([]collection, error) {
	if req.root.Namespace == "" && req.root.Name == "" {
		sort.SliceStable(expanded, func(i, j int) bool { return expanded[i].key() < expanded[j].key() })
		return expanded, nil
	}
	for _, col := range expanded {
		if col.Namespace == req.root.Namespace && col.Name == req.root.Name {
			return []collection{col}, nil
		}
	}
	found := make([]string, 0, len(expanded))
	for _, col := range expanded {
		found = append(found, col.Namespace+"."+col.Name)
	}
	sort.Strings(found)
	return nil, fmt.Errorf("%w: %s.%s is not among %v in %s", helpers.ErrGitNameMismatch,
		req.root.Namespace, req.root.Name, found, req.display)
}
