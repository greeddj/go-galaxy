package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/collectionbuild"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/manifest"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// expandSourceRoots expands git roots, then url roots, and only after both
// checks the result for a fqdn two roots produce: a root has no identity
// before its own expansion, so an earlier check would see false duplicates.
func expandSourceRoots(ctx context.Context, deps collectionDeps, roots []collection) ([]collection, error) {
	expanding := anyUnpinnedGit(roots) || anyUnpinnedURL(roots)
	roots, err := expandGitRoots(ctx, deps, roots)
	if err != nil {
		return nil, err
	}
	roots, err = expandURLRoots(ctx, deps, roots)
	if err != nil {
		return nil, err
	}
	if expanding {
		return checkExpandedDuplicates(roots)
	}
	return roots, nil
}

// urlPin is what discovery learned about a url requirement's collection:
// locator, origin sha256, exact version, validated dependencies and, under
// --no-cache only, the download itself, so install does not fetch it again.
type urlPin struct {
	prebuilt *downloadResult
	deps     map[string]string
	locator  string
	sha256   string
	version  string
}

// urlDiscoveryMemo is the run-wide table of discovered url collections by
// fqdn, gitDiscoveryMemo's counterpart, shared by resolve, prefetch and install.
type urlDiscoveryMemo struct {
	pins map[string]urlPin
	mu   sync.Mutex
}

func newURLDiscoveryMemo() *urlDiscoveryMemo {
	return &urlDiscoveryMemo{pins: make(map[string]urlPin)}
}

func (m *urlDiscoveryMemo) put(fqdn string, pin urlPin) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pins[fqdn] = pin
}

// takePrebuilt hands out a --no-cache download exactly once: the install
// worker that takes it owns its cleanup from then on.
func (m *urlDiscoveryMemo) takePrebuilt(fqdn string) (downloadResult, bool) {
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
func (m *urlDiscoveryMemo) snapshot() map[string]urlPin {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]urlPin, len(m.pins))
	for fqdn, pin := range m.pins {
		pin.prebuilt = nil
		out[fqdn] = pin
	}
	return out
}

// cleanup removes every --no-cache download nobody took.
func (m *urlDiscoveryMemo) cleanup() {
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

// expandURLRoots replaces each unpinned url root with the collection its
// tarball holds; it is resolution's only contact with a url origin. Roots run
// on the download pool and merge in input order, so the result is stable.
func expandURLRoots(ctx context.Context, deps collectionDeps, roots []collection) ([]collection, error) {
	if !anyUnpinnedURL(roots) {
		return roots, nil
	}
	results := make([][]collection, len(roots))
	errs := make([]error, len(roots))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(deps.cfg.DownloadWorkers, 1))
	for i, root := range roots {
		if !root.isURL() || isPinnedURLLocator(root.Source) {
			results[i] = []collection{root}
			continue
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i], errs[i] = expandURLRoot(ctx, deps, root)
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

func anyUnpinnedURL(roots []collection) bool {
	for _, root := range roots {
		if root.isURL() && !isPinnedURLLocator(root.Source) {
			return true
		}
	}
	return false
}

func isPinnedURLLocator(source string) bool {
	loc, err := urlsource.ParseLocator(source)
	return err == nil && loc.Pinned()
}

// urlRootRequest is one unpinned url root taken apart: the canonical URL,
// the version the entry asserted ("" for none), the pin key and the display
// form for messages.
type urlRootRequest struct {
	rawURL    string
	requested string
	pinKey    string
	display   string
}

func newURLRootRequest(root collection) (urlRootRequest, error) {
	loc, err := root.urlLocator()
	if err != nil {
		return urlRootRequest{}, err
	}
	return urlRootRequest{
		rawURL:    loc.URL,
		requested: root.Constraint,
		pinKey:    urlsource.PinKey(loc.URL),
		display:   helpers.URLForMessage(loc.URL),
	}, nil
}

// expandURLRoot resolves one unpinned url root into its collection. A pin is
// keyed by its URL and replayed whenever the policy allows a read, TTL ignored:
// it never ages out, and only --refresh or --clear-cache replaces it.
func expandURLRoot(ctx context.Context, deps collectionDeps, root collection) ([]collection, error) {
	req, err := newURLRootRequest(root)
	if err != nil {
		return nil, err
	}
	policy := cacheManager.PolicyForConstraint(deps.cfg, false)
	if policy.Read {
		if pin, ok := deps.st.GetURLPin(req.pinKey); ok {
			return replayURLPin(deps, req, pin)
		}
	}
	if deps.cfg != nil && deps.cfg.Offline {
		return nil, fmt.Errorf("%w: url source %s is not recorded in the cache", helpers.ErrOfflineMode, req.display)
	}
	return acquireURLRoot(ctx, deps, req, policy)
}

// replayURLPin turns a recorded pin into an expanded root, re-validating
// everything it carries: the pin is cache state, and cache state is judged
// on the way in exactly as an origin's answer is.
func replayURLPin(deps collectionDeps, req urlRootRequest, pin store.URLPinEntry) ([]collection, error) {
	if !helpers.IsSHA256Hex(pin.SHA256) {
		return nil, fmt.Errorf("%w: recorded pin for %s names sha256 %q", helpers.ErrInvalidURLLocator, req.display, pin.SHA256)
	}
	col, p, err := urlCollectionRoot(req, pin.SHA256, pin.Namespace, pin.Name, pin.Version, pin.Dependencies)
	if err != nil {
		return nil, err
	}
	deps.urlMemo.put(col.fqdn(), p)
	return []collection{col}, nil
}

// acquireURLRoot downloads the tarball, reads its identity, commits or keeps
// the artifact, records the pin, and returns the expanded root.
func acquireURLRoot(ctx context.Context, deps collectionDeps, req urlRootRequest, policy cacheManager.Policy) ([]collection, error) {
	result, err := downloadURLToTemp(ctx, deps, req.rawURL)
	if err != nil {
		return nil, err
	}
	manifestBytes, err := manifest.ReadFromTarGz(ctx, result.Path)
	if err != nil {
		cleanupIfNeeded(result.Cleanup)
		return nil, fmt.Errorf("%s: %w", req.display, err)
	}
	meta, err := collectionbuild.ParseManifestInfo(manifestBytes)
	if err != nil {
		cleanupIfNeeded(result.Cleanup)
		return nil, fmt.Errorf("%s: %w", req.display, err)
	}
	col, pin, err := urlCollectionRoot(req, result.SHA, meta.Namespace, meta.Name, meta.Version, meta.Dependencies)
	if err != nil {
		cleanupIfNeeded(result.Cleanup)
		return nil, err
	}
	entry := store.URLPinEntry{
		SHA256:       result.SHA,
		Namespace:    meta.Namespace,
		Name:         meta.Name,
		Version:      meta.Version,
		Dependencies: meta.Dependencies,
	}
	if err := storeURLArtifact(ctx, deps, col, result, &pin); err != nil {
		return nil, err
	}
	if policy.Write {
		deps.st.SetURLPin(req.pinKey, entry)
	}
	deps.urlMemo.put(col.fqdn(), pin)
	return []collection{col}, nil
}

// storeURLArtifact commits the download under its locator-scoped key, hands
// it to the pin under --no-cache, or discards it under --dry-run.
func storeURLArtifact(ctx context.Context, deps collectionDeps, col collection, result downloadResult, pin *urlPin) error {
	switch {
	case deps.cfg != nil && deps.cfg.DryRun:
		cleanupIfNeeded(result.Cleanup)
	case keepsPrebuilt(deps):
		pin.prebuilt = &downloadResult{Path: result.Path, SHA: result.SHA, Cleanup: result.Cleanup}
	default:
		key := helpers.ArtifactKey(col.Source, helpers.ArtifactFilename(col.Namespace, col.Name, col.Version))
		if _, err := commitDownload(ctx, deps.gitStore, key, result.Path, result.SHA, result.Cleanup); err != nil {
			return fmt.Errorf("committing url artifact %s: %w", col.key(), err)
		}
		deps.runtime.Metrics.AddCacheMiss()
	}
	return nil
}

// urlCollectionRoot judges an identity and dependencies, read from a manifest
// or replayed from a pin alike, and returns the exact-pin root and its solver
// pin. A version the entry asserts must equal the built one on either path.
func urlCollectionRoot(req urlRootRequest, sha, namespace, name, version string,
	rawDeps map[string]string,
) (collection, urlPin, error) {
	// The relaxed alphabet, not the Galaxy one: the manifest is authored
	// outside any Galaxy server, and real artifacts carry mixed-case
	// namespaces ansible installs. See helpers.IsURLCollectionNamePart.
	if !helpers.IsURLCollectionNamePart(namespace) || !helpers.IsURLCollectionNamePart(name) {
		return collection{}, urlPin{}, fmt.Errorf("%w: %s declares %q.%q",
			helpers.ErrInvalidCollectionName, req.display, namespace, name)
	}
	if !helpers.IsExactVersion(version) {
		return collection{}, urlPin{}, fmt.Errorf("%w: %s declares %s.%s version %q",
			helpers.ErrGitCollectionVersionNotExact, req.display, namespace, name, version)
	}
	if req.requested != "" && req.requested != version {
		return collection{}, urlPin{}, fmt.Errorf("%w: %s is built as %s.%s@%s, not %s",
			helpers.ErrURLCollectionVersionMismatch, req.display, namespace, name, version, req.requested)
	}
	parsedDeps, err := parseDependencies(rawDeps)
	if err != nil {
		return collection{}, urlPin{}, fmt.Errorf("%s.%s from %s: %w", namespace, name, req.display, err)
	}
	locator := urlsource.Locator{URL: req.rawURL, SHA256: sha}.String()
	pin := urlPin{locator: locator, sha256: sha, version: version, deps: parsedDeps}
	return collection{
		Namespace:  namespace,
		Name:       name,
		Version:    version,
		Source:     locator,
		Constraint: version,
		Type:       typeURL,
		SHA256:     sha,
	}, pin, nil
}

// downloadURLToTemp downloads rawURL over the url client into a hashed,
// shape-probed temp file under a Galaxy download's retry policy and single
// ArtifactDownloadDeadline budget. It commits nothing.
func downloadURLToTemp(ctx context.Context, deps collectionDeps, rawURL string) (downloadResult, error) {
	if err := checkDownloadURL(rawURL); err != nil {
		return downloadResult{}, err
	}
	runtime := deps.runtime
	if runtime == nil || runtime.URLHTTP == nil {
		return downloadResult{}, fmt.Errorf("%w: no url download client is wired into this run", helpers.ErrConfigIsNil)
	}
	if strings.HasPrefix(strings.ToLower(rawURL), "http://") {
		// Warnf, not Printf: the plaintext transport of the very bytes every
		// later sha256 check descends from must survive --quiet in CI.
		runtime.Output.Warnf("Downloading %s over plaintext http", helpers.WithoutCredentials(rawURL))
	}
	budget := runtime.ArtifactDeadline()
	dlCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var result downloadResult
	err := helpers.Retry(dlCtx, helpers.FetchRetryPolicy(), func() error {
		attempted, attemptErr := attemptURLDownload(dlCtx, deps, rawURL)
		if attemptErr != nil {
			return artifactDeadlineError(ctx, dlCtx, budget, attemptErr)
		}
		result = attempted
		return nil
	}, downloadRetryable)
	if err != nil {
		return downloadResult{}, artifactDeadlineError(ctx, dlCtx, budget, err)
	}
	return result, nil
}

// attemptURLDownload is one GET, capped and hashed into a temp file, then
// probed as tar.gz; every failure cleans up. Its errors mirror
// downloadCollection's so downloadRetryable and exit codes read both alike.
func attemptURLDownload(ctx context.Context, deps collectionDeps, rawURL string) (downloadResult, error) {
	runtime := deps.runtime
	runtime.Output.Printf("Downloading %s", helpers.WithoutCredentials(rawURL))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return downloadResult{}, err
	}
	resp, err := runtime.URLHTTP.Do(req)
	if err != nil {
		// helpers.CutTransportURL for the reason downloadCollection gives: a
		// url source is routinely a presigned or token-parameterized URL,
		// and net/http's own masking leaves the query intact.
		return downloadResult{}, &downloadAttemptError{err: helpers.CutTransportURL(rawURL, err)}
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return downloadResult{}, &downloadAttemptError{
			err:    fmt.Errorf("%w: %s (%s)", helpers.ErrDownloadFailed, helpers.WithoutCredentials(rawURL), resp.Status),
			status: resp.StatusCode,
		}
	}
	tmpPath, cleanup, sha, err := writeURLBodyToTemp(ctx, deps, resp.Body)
	if err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	if err := archive.ProbeTarGz(ctx, tmpPath); err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	return downloadResult{Path: tmpPath, SHA: sha, Cleanup: cleanup}, nil
}

// writeURLBodyToTemp is writeDownloadToTemp over discovery-phase deps: it
// streams body into a gitTempFile under the download size cap and returns the
// sha256 computed on the way.
func writeURLBodyToTemp(ctx context.Context, deps collectionDeps, body io.Reader) (string, func(), string, error) {
	tmpFile, cleanup, err := gitTempFile(deps)(ctx)
	if err != nil {
		return "", cleanup, "", err
	}
	hasher := sha256.New()
	writer := io.MultiWriter(tmpFile, hasher)
	limited := helpers.NewSizeLimitedReader(body, helpers.ArtifactMaxDownloadSize)
	n, err := io.Copy(writer, limited)
	deps.runtime.Metrics.AddBytesDownloaded(n)
	if err != nil {
		_ = tmpFile.Close()
		return "", cleanup, "", fmt.Errorf("collection artifact download: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", cleanup, "", err
	}
	return tmpFile.Name(), cleanup, hex.EncodeToString(hasher.Sum(nil)), nil
}
