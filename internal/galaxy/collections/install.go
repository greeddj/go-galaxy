package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

const versionLimit = 100

// maxVersionPages bounds the offset-paginated requests loadVersionsListCached
// issues for one versions list (about 10,000 versions at versionLimit a page),
// failing hard rather than trusting a server that reports pages forever.
const maxVersionPages = 100

// installCollection downloads, extracts, and records a collection install.
func installCollection(
	ctx context.Context,
	col collection,
	deps installDeps,
	resolvedDeps []string,
	metaOverride *types.GalaxyCollectionVersionInfo,
	prefetched downloadResult,
) error {
	cfg := deps.cfg
	runtime := deps.runtime
	st := deps.st

	installStart := time.Now()
	defer func() {
		runtime.Output.DebugSincef(installStart, "%s", col.key())
	}()

	// One target and one os.Root, built once and threaded through every write
	// below, so no call site rederives the path and skips its validation.
	target, ok := newInstallTarget(deps.root, cfg, col)
	if !ok {
		return fmt.Errorf("%w: ns=%q name=%q version=%q",
			helpers.ErrUnsafeCollectionIdentifier, col.Namespace, col.Name, col.Version)
	}

	filename := helpers.ArtifactFilename(col.Namespace, col.Name, col.Version)

	// A skip verifies nothing, and no verdict is persisted, since the cache is
	// attacker-writable; recordSkippedUnverified counts the skip so the run
	// reports how many collections it left unverified.
	if state, ok := canSkipInstall(target, col, st, runtime.Output); ok {
		// The one write a skip makes: repair a sidecar that disagrees with the
		// record just proven valid, since nothing else on this path rewrites it.
		reconcileGalaxyInfo(runtime, target, cfg, col, state)
		deps.verify.recordSkippedUnverified()
		runtime.Output.Printf("Skipping install, already installed: %s/%s/%s", col.Namespace, col.Name, col.Version)
		// Release a prefetched temp handed to a skipped install rather than leave
		// it until Close; defensive, since a scheduled prefetch is never already
		// installed.
		cleanupIfNeeded(prefetched.Cleanup)
		return nil
	}

	payload, err := prepareAndExtract(ctx, deps, col, metaOverride, prefetched, filename, target)
	if err != nil {
		return err
	}
	// The winning payload is cleaned up here; prepareAndExtract already cleaned
	// every losing attempt, so no temp is freed twice or leaked.
	defer cleanupIfNeeded(payload.artifact.Cleanup)

	writeGalaxyInfoIfPresent(runtime, target, cfg, col, payload.meta)
	recordInstall(st, col, target.path, payload.artifactSHA, resolvedDeps)
	return nil
}

// verifyPinnedSHA enforces a lockfile SHA256 pin against the artifact hash,
// so a frozen run never installs drifted bytes. An empty pin (an older lockfile
// with no checksum) is a no-op.
func verifyPinnedSHA(col collection, actual string) error {
	expected := strings.TrimSpace(col.SHA256)
	if expected == "" {
		return nil
	}
	got := strings.TrimSpace(actual)
	if expected == got {
		return nil
	}
	return fmt.Errorf("%w: %s: locked %s != actual %s", helpers.ErrSHA256Mismatch, col.key(), expected, got)
}

type installPayload struct {
	meta        *types.GalaxyCollectionVersionInfo
	artifact    artifactData
	artifactSHA string
	// metaUnavailable marks a nil meta whose load failed on a cache hit, so
	// signature verification says it could not learn whether the collection is
	// signed rather than reporting it unsigned.
	metaUnavailable bool
	// artifactSHAComputed reports that this process hashed the bytes at
	// artifact.Path itself rather than reading artifactSHA from a record; the
	// extracted store's Ensure skips its re-hash only when this holds.
	artifactSHAComputed bool
}

type artifactData struct {
	Meta    map[string]string
	Cleanup func()
	Path    string
	SHA     string
}

// isCacheHit reports whether col's artifact can be served from the artifact
// cache. deps.presence spares a second Has probe, and is safe to reuse only
// because the prefetch scan never records a key it scheduled for writing.
func isCacheHit(ctx context.Context, deps installDeps, col collection, forceDownload bool) bool {
	if forceDownload || deps.cfg.NoCache || deps.artifacts == nil {
		return false
	}
	if deps.presence[artifactKey(col)] {
		return true
	}
	return artifactExists(ctx, deps.artifacts, col)
}

// prepareInstall produces an installable artifact for col, from the cache or
// a download. The bool reports a cache hit, the only case worth one eviction;
// forceDownload bypasses the cache without trusting Delete to have removed it.
func prepareInstall(
	ctx context.Context,
	deps installDeps,
	col collection,
	metaOverride *types.GalaxyCollectionVersionInfo,
	prefetched downloadResult,
	filename string,
	forceDownload bool,
) (installPayload, bool, error) {
	// A prefetched temp holds fresh origin bytes and wins, except on the forced
	// retry: it was never evicted, so retrying it would repeat the failure.
	if prefetched.Path != "" && !forceDownload {
		return payloadFromPrefetched(col, metaOverride, prefetched)
	}

	cfg := deps.cfg
	runtime := deps.runtime

	meta := metaOverride
	useCache := !cfg.NoCache
	cacheHit := isCacheHit(ctx, deps, col, forceDownload)

	if servableFromCacheAlone(deps, cacheHit, meta) {
		runtime.Output.Printf("Using cached %s", filename)
		payload, err := prepareFromCache(ctx, deps, col)
		return payload, true, err
	}

	meta, err := resolveMetadata(ctx, deps.collectionDeps, col, meta, cacheHit)
	if err != nil && !errors.Is(err, helpers.ErrMetadataUnavailable) {
		return installPayload{}, cacheHit, err
	}
	// Carried rather than dropped: only a cache hit whose metadata load failed
	// reports this, and verification must then say so, not report unsigned.
	metaUnavailable := errors.Is(err, helpers.ErrMetadataUnavailable)

	artifact, err := fetchArtifact(ctx, deps, col, meta, cacheHit, useCache)
	if err != nil {
		return installPayload{}, cacheHit, err
	}
	artifactSHA, shaComputed, err := resolveArtifactSHA(artifact.Path, meta, artifact.Meta, artifact.SHA, col.SHA256)
	if err != nil {
		cleanupIfNeeded(artifact.Cleanup)
		return installPayload{}, cacheHit, err
	}
	return installPayload{
		meta:                meta,
		artifact:            artifact,
		artifactSHA:         artifactSHA,
		metaUnavailable:     metaUnavailable,
		artifactSHAComputed: shaComputed,
	}, cacheHit, nil
}

// servableFromCacheAlone reports whether col installs from the cached
// artifact with no metadata request. Never in a verifying run: a server's
// signatures ride on that metadata, so a server-signed hit would pass vacuously.
func servableFromCacheAlone(deps installDeps, cacheHit bool, meta *types.GalaxyCollectionVersionInfo) bool {
	return cacheHit && meta == nil && !deps.verify.enabled()
}

// verifyAndExtract enforces col's lockfile pin and signatures against
// payload and extracts it into target; prepareAndExtract reruns it on a
// refetched replacement after evicting a corrupt cache hit.
func verifyAndExtract(
	ctx context.Context,
	deps installDeps,
	col collection,
	payload installPayload,
	target installTarget,
	filename string,
) error {
	if err := verifyPinnedSHA(col, payload.artifactSHA); err != nil {
		return err
	}
	// Pin first (these are the lockfile's bytes), then signature (who published
	// them), both before any write a playbook could find.
	if err := verifyCollectionSignatures(ctx, deps, col, payload); err != nil {
		return err
	}
	extractStart := time.Now()
	err := extractCollection(ctx, col, payload.artifact.Path, target, deps.runtime, deps.extractStore,
		payload.artifactSHA, payload.artifactSHAComputed)
	if err != nil {
		return fmt.Errorf("failed to extract %s: %w", filename, err)
	}
	deps.runtime.Output.DebugSincef(extractStart, "%s", "Extract "+col.key())
	return nil
}

// prepareWithRecovery runs action on col's prepared artifact, evicting and
// refetching once when a cache hit proves corrupt. Eviction forces the next
// download and a forced attempt never evicts, so the loop runs at most twice.
func prepareWithRecovery(
	ctx context.Context,
	deps installDeps,
	col collection,
	metaOverride *types.GalaxyCollectionVersionInfo,
	prefetched downloadResult,
	filename string,
	action func(installPayload) error,
) (installPayload, error) {
	forceDownload := false
	for {
		payload, fromCache, err := prepareInstall(ctx, deps, col, metaOverride, prefetched, filename, forceDownload)
		if err != nil {
			// Only the S3 backend's read-time sha256 mismatch is retried here;
			// any other prepare failure would recur against the same entry or
			// origin.
			if canRetryCacheHit(deps, fromCache, forceDownload) && errors.Is(err, helpers.ErrSHA256Mismatch) {
				evictCorruptCachedArtifact(ctx, deps, col, filename, err)
				forceDownload = true
				continue
			}
			return installPayload{}, err
		}
		actionErr := action(payload)
		if actionErr == nil {
			return payload, nil
		}
		cleanupIfNeeded(payload.artifact.Cleanup)
		if !canRetryCacheHit(deps, fromCache, forceDownload) || unrepairableByRefetch(actionErr) {
			return installPayload{}, actionErr
		}
		evictCorruptCachedArtifact(ctx, deps, col, filename, actionErr)
		forceDownload = true
	}
}

// unrepairableByRefetch reports whether no different artifact could fix an
// action failure: the destination was at fault, the signature material was
// never in hand, or the verdict was over a blob set a refetch does not change.
func unrepairableByRefetch(err error) bool {
	return isDestinationSideFailure(err) || isSignatureSourceFailure(err) || isBlobSetVerdict(err)
}

// isDestinationSideFailure reports whether err's cause is the write
// destination rather than the artifact. Evicting for it would hand a hostile
// requirements.yml a deterministic delete against the shared cache.
func isDestinationSideFailure(err error) bool {
	return errors.Is(err, helpers.ErrCollectionsPathEscape) || errors.Is(err, helpers.ErrUnsafeCollectionIdentifier)
}

// isSignatureSourceFailure reports whether err says a signature was never in
// hand (unfetchable, over deadline, unsupported or credentialed source); no
// refetched artifact changes that, so evicting would only spend a shared delete.
func isSignatureSourceFailure(err error) bool {
	return errors.Is(err, helpers.ErrSignatureSourceUnavailable) ||
		errors.Is(err, helpers.ErrSignatureFetchDeadline) ||
		errors.Is(err, helpers.ErrUnsupportedSignatureSource) ||
		errors.Is(err, helpers.ErrSignatureSourceUserinfo)
}

// isBlobSetVerdict reports whether err is a verdict over the gathered
// signature set, which a retry re-reads from the same cached metadata; a
// swapped artifact then fails closed every run until --clear-cache.
func isBlobSetVerdict(err error) bool {
	return errors.Is(err, helpers.ErrSignatureVerificationFailed)
}

// canRetryCacheHit reports whether prepareWithRecovery may spend its one
// eviction: a cache hit, not already the retry, and not offline, where deleting
// the only local copy with nothing to refetch from would be pure data loss.
func canRetryCacheHit(deps installDeps, fromCache, forceDownload bool) bool {
	return fromCache && !forceDownload && !deps.cfg.Offline
}

// evictCorruptCachedArtifact logs and deletes col's cached artifact ahead of
// a forced refetch, shared by both recovery arms so the line reads the same.
func evictCorruptCachedArtifact(ctx context.Context, deps installDeps, col collection, filename string, cause error) {
	deps.runtime.Output.Printf("Evicting corrupt cached %s and refetching: %v", filename, cause)
	if deps.artifacts != nil {
		_ = deps.artifacts.Delete(ctx, artifactKey(col))
	}
}

// prepareAndExtract prepares an installable artifact for col and verifies +
// extracts it into target's install directory, delegating the bounded
// eviction-and-refetch retry to prepareWithRecovery.
func prepareAndExtract(
	ctx context.Context,
	deps installDeps,
	col collection,
	metaOverride *types.GalaxyCollectionVersionInfo,
	prefetched downloadResult,
	filename string,
	target installTarget,
) (installPayload, error) {
	return prepareWithRecovery(ctx, deps, col, metaOverride, prefetched, filename, func(payload installPayload) error {
		return verifyAndExtract(ctx, deps, col, payload, target, filename)
	})
}

// payloadFromPrefetched builds a payload from a prefetched artifact. Its sha
// was computed while downloading, so a pin is still checked against the real
// bytes; it reports no cache hit, since fresh origin bytes never merit eviction.
func payloadFromPrefetched(
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
	prefetched downloadResult,
) (installPayload, bool, error) {
	artifact := artifactData{Path: prefetched.Path, Cleanup: prefetched.Cleanup, SHA: prefetched.SHA}
	artifactSHA, shaComputed, err := resolveArtifactSHA(artifact.Path, meta, artifact.Meta, artifact.SHA, col.SHA256)
	// Unreached in practice, since every handoff carries a SHA; kept to fail
	// closed rather than install unhashed bytes.
	if err != nil {
		cleanupIfNeeded(artifact.Cleanup)
		return installPayload{}, false, err
	}
	return installPayload{meta: meta, artifact: artifact, artifactSHA: artifactSHA, artifactSHAComputed: shaComputed}, false, nil
}

func prepareFromCache(ctx context.Context, deps installDeps, col collection) (installPayload, error) {
	artifact, err := fetchArtifact(ctx, deps, col, nil, true, true)
	if err != nil {
		return installPayload{}, err
	}
	artifactSHA, shaComputed, err := resolveArtifactSHA(artifact.Path, nil, artifact.Meta, artifact.SHA, col.SHA256)
	if err != nil {
		cleanupIfNeeded(artifact.Cleanup)
		return installPayload{}, err
	}
	return installPayload{meta: nil, artifact: artifact, artifactSHA: artifactSHA, artifactSHAComputed: shaComputed}, nil
}

// writeGalaxyInfoIfPresent writes col's GALAXY.yml sidecar and never fails
// the install. An unsafe identifier or a collections-path escape (a symlink
// planted after validation) is an integrity signal and warns past --quiet.
func writeGalaxyInfoIfPresent(
	runtime *infra.Infra,
	target installTarget,
	cfg *config.Config,
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
) {
	err := writeGalaxyInfo(target, cfg, col, meta)
	if err == nil {
		return
	}
	if errors.Is(err, helpers.ErrUnsafeCollectionIdentifier) || errors.Is(err, helpers.ErrCollectionsPathEscape) {
		runtime.Output.Warnf("Refusing to write GALAXY.yml: %v", err)
		return
	}
	runtime.Output.Printf("Failed to write GALAXY.yml: %v", err)
}

func recordInstall(st *store.Store, col collection, installPath, artifactSHA string, deps []string) {
	if st == nil {
		return
	}
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    installPath,
		Source:         col.Source,
		ArtifactSHA256: artifactSHA,
		InstalledAt:    time.Now().UTC(),
		Deps:           deps,
	})
	if deps != nil {
		st.SetGraph(col.key(), deps)
	}
}

// fetchArtifactMiss is fetchArtifact's cache-miss arm: a prebuilt git or url
// artifact from discovery is taken as is, offline mode refuses the network,
// and otherwise the artifact is fetched from its origin.
func fetchArtifactMiss(
	ctx context.Context,
	deps installDeps,
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
	useCache bool,
) (artifactData, error) {
	if prebuilt, ok := deps.gitMemo.takePrebuilt(col.fqdn()); ok {
		// A --no-cache discovery built this artifact minutes ago and no
		// store holds it; the fetch it stands for happened once.
		return artifactData{Path: prebuilt.Path, Cleanup: prebuilt.Cleanup, SHA: prebuilt.SHA}, nil
	}
	if prebuilt, ok := deps.urlMemo.takePrebuilt(col.fqdn()); ok {
		// The url counterpart of the handoff above: discovery downloaded it.
		return artifactData{Path: prebuilt.Path, Cleanup: prebuilt.Cleanup, SHA: prebuilt.SHA}, nil
	}
	if deps.cfg != nil && deps.cfg.Offline {
		return artifactData{}, fmt.Errorf("%w: artifact %s not in cache", helpers.ErrOfflineMode, col.key())
	}
	downloadStart := time.Now()
	var (
		result downloadResult
		err    error
	)
	switch {
	case col.isGit():
		result, err = gitFetchToCache(ctx, deps, col, useCache)
	case col.isURL():
		result, err = urlFetchToCache(ctx, deps, col, useCache)
	default:
		result, err = downloadCollectionToCache(ctx, deps, artifactKey(col), col.Source, meta, useCache)
	}
	if err != nil {
		return artifactData{}, err
	}
	deps.runtime.Output.DebugSincef(downloadStart, "%s", "Download "+col.key())
	return artifactData{Path: result.Path, Cleanup: result.Cleanup, SHA: result.SHA}, nil
}

func artifactExists(ctx context.Context, artifacts cacheManager.ArtifactStore, col collection) bool {
	ok, err := artifacts.Has(ctx, artifactKey(col))
	return err == nil && ok
}

func fetchArtifact(
	ctx context.Context,
	deps installDeps,
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
	cacheHit bool,
	useCache bool,
) (artifactData, error) {
	runtime := deps.runtime
	artifacts := deps.artifacts

	if !cacheHit {
		return fetchArtifactMiss(ctx, deps, col, meta, useCache)
	}
	if artifacts == nil {
		return artifactData{}, helpers.ErrArtifactCacheNotConfigured
	}
	// A cache-hit fetch gets the same deadline as an origin download, since
	// the S3 store streams a full body over HTTP; the backend only honors the
	// context, so no policy crosses the Backend seam.
	budget := runtime.ArtifactDeadline()
	fetchCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cached, err := artifacts.Fetch(fetchCtx, artifactKey(col))
	if err != nil {
		return artifactData{}, artifactDeadlineError(ctx, fetchCtx, budget, err)
	}
	runtime.Metrics.AddCacheHit()
	// cached.SHA is set only when the backend hashed the bytes while producing
	// the file, making it this process's own digest; the local backend leaves it
	// empty and resolves from the sidecar.
	return artifactData{Path: cached.Path, Cleanup: cached.Cleanup, Meta: cached.Meta, SHA: cached.SHA}, nil
}

// resolveArtifactSHA picks the sha256 to record and whether this process
// computed it. Under a pin no recorded value is trusted; a recorded value of
// bad shape fails hard, while this process's own hash is not re-validated.
func resolveArtifactSHA(
	path string,
	meta *types.GalaxyCollectionVersionInfo,
	artifactMeta map[string]string,
	artifactSHA string,
	pin string,
) (string, bool, error) {
	if sha := strings.TrimSpace(artifactSHA); sha != "" {
		return sha, true, nil
	}
	if strings.TrimSpace(pin) != "" {
		sha, err := archive.FileHashSHA256(path)
		return sha, err == nil, err
	}
	if meta != nil {
		if sha := strings.TrimSpace(meta.Artifact.Sha256); sha != "" {
			if !helpers.IsSHA256Hex(sha) {
				return "", false, fmt.Errorf("%w: %q", helpers.ErrMalformedArtifactSHA256, sha)
			}
			return sha, false, nil
		}
	}
	if artifactMeta != nil {
		if sha := strings.TrimSpace(artifactMeta["sha256"]); sha != "" {
			if !helpers.IsSHA256Hex(sha) {
				return "", false, fmt.Errorf("%w: %q", helpers.ErrMalformedArtifactSHA256, sha)
			}
			return sha, false, nil
		}
	}
	sha, err := archive.FileHashSHA256(path)
	return sha, err == nil, err
}

// artifactKey builds col's cache key scoped to col.Source, the server that
// actually resolved it, so two servers publishing the same tarball name never
// share one cache slot.
func artifactKey(col collection) string {
	return helpers.ArtifactKey(col.Source, helpers.ArtifactFilename(col.Namespace, col.Name, col.Version))
}

// installEntryMatches reports whether a recorded install still matches
// installPath, any lockfile pin, and col.Source; the source check reinstalls a
// collection that now resolves from another server at the same version.
func installEntryMatches(col collection, entry store.InstalledEntry, installPath string) bool {
	if entry.InstallPath == "" || entry.InstallPath != installPath {
		return false
	}
	if entry.ArtifactSHA256 == "" {
		return false
	}
	if entry.Source != col.Source {
		return false
	}
	if col.SHA256 != "" && strings.TrimSpace(entry.ArtifactSHA256) != strings.TrimSpace(col.SHA256) {
		return false
	}
	return true
}

// installedState is a matching install's store record, its GALAXY.yml
// sidecar and the sidecar's bytes, carried past the check so the skip path can
// repair a drifted sidecar without reading it again.
type installedState struct {
	info     GalaxyYAML
	infoData []byte
	entry    store.InstalledEntry
}

// matchingInstalledRecord reports whether col's store entry, extract marker
// and a parsed sidecar naming col agree, without walking the tree. Every call
// goes through target.root, so a symlink-only fake never counts as installed.
func matchingInstalledRecord(target installTarget, col collection, st *store.Store) (installedState, bool) {
	if st == nil {
		return installedState{}, false
	}
	entry, ok := st.GetInstalled(col.key())
	if !ok || !installEntryMatches(col, entry, target.path) {
		return installedState{}, false
	}

	markerRelPath, ok := markerRel(target, entry.ArtifactSHA256)
	if !ok {
		return installedState{}, false
	}
	if _, err := target.root.Stat(markerRelPath); err != nil {
		return installedState{}, false
	}

	info, infoData, ok := readGalaxyInfo(target)
	if !ok || !info.describes(col) {
		return installedState{}, false
	}

	return installedState{entry: entry, info: info, infoData: infoData}, true
}

// installRecordMatches is matchingInstalledRecord's boolean form.
func installRecordMatches(target installTarget, col collection, st *store.Store) bool {
	_, ok := matchingInstalledRecord(target, col, st)
	return ok
}

// canSkipInstall is matchingInstalledRecord plus verifyExtractMarker's tally
// walk: the gate that actually skips work, where a false yes would keep serving
// a corrupt extracted tree. A false verdict's zero installedState is unusable.
func canSkipInstall(target installTarget, col collection, st *store.Store, out output.Printer) (installedState, bool) {
	state, ok := matchingInstalledRecord(target, col, st)
	if !ok {
		return installedState{}, false
	}
	if !verifyExtractMarker(out, target, state.entry.ArtifactSHA256) {
		return installedState{}, false
	}
	return state, true
}

// downloadCollection makes one GET attempt, wrapping a failure in
// *downloadAttemptError for the caller's retry. Messages cut the URL while the
// request uses it whole, since a presigned query is what authenticates the GET.
func downloadCollection(ctx context.Context, runtime *infra.Infra, collectionURL string) (*http.Response, error) {
	runtime.Output.Printf("Downloading %s", helpers.WithoutCredentials(collectionURL))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, collectionURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := runtime.HTTP.Do(req)
	if err != nil {
		// net/http masks only a password, so CutTransportURL cuts the query
		// too. The cause it wraps is not cut, so no RoundTripper installed here
		// may render its request URL; Unwrap keeps classification unchanged.
		return nil, &downloadAttemptError{err: helpers.CutTransportURL(collectionURL, err)}
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		// Cut for the same reason: a presigned query in a log line is a live
		// capability, and the host, path and status an operator needs survive.
		return nil, &downloadAttemptError{
			err:    fmt.Errorf("%w: %s (%s)", helpers.ErrDownloadFailed, helpers.WithoutCredentials(collectionURL), resp.Status),
			status: resp.StatusCode,
		}
	}
	return resp, nil
}

// downloadResult describes a downloaded artifact file and metadata.
type downloadResult struct {
	Cleanup func()
	Path    string
	SHA     string
}

// downloadCollectionToCache is the one funnel for origin downloads: fresh
// attempts retried under a single ArtifactDeadline budget that also covers the
// backoffs, the concurrent extraction and the cache commit.
func downloadCollectionToCache(
	ctx context.Context,
	deps installDeps,
	key string,
	base string,
	meta *types.GalaxyCollectionVersionInfo,
	useCache bool,
) (downloadResult, error) {
	if err := validateDownloadInputs(deps.cfg, deps.artifacts, meta); err != nil {
		return downloadResult{}, err
	}

	budget := deps.runtime.ArtifactDeadline()
	dlCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var result downloadResult
	err := helpers.Retry(dlCtx, helpers.FetchRetryPolicy(), func() error {
		attempted, attemptErr := attemptDownloadToCache(dlCtx, deps, key, base, meta, useCache)
		if attemptErr != nil {
			return artifactDeadlineError(ctx, dlCtx, budget, attemptErr)
		}
		result = attempted
		return nil
	}, downloadRetryable)
	if err != nil {
		return downloadResult{}, artifactDeadlineError(ctx, dlCtx, budget, err)
	}
	// Counted once per acquisition, not per attempt, so a retried 5xx is one
	// miss.
	deps.runtime.Metrics.AddCacheMiss()
	return result, nil
}

// warnIfOffServerDownloadHost warns, past --quiet, when the download URL's
// origin (scheme, host, port) differs from base's: poisoned cached metadata can
// redirect a download, but a separate content host is legitimate, so no block.
func warnIfOffServerDownloadHost(runtime *infra.Infra, base, downloadURL string) {
	if strings.TrimSpace(base) == "" {
		return
	}
	// Hostname decides silence, before any origin is computed: the Origin of a
	// hostname-less URL is still well-formed and would raise a false alarm.
	server, err := url.Parse(base)
	if err != nil || server.Hostname() == "" {
		return
	}
	dl, err := url.Parse(downloadURL)
	if err != nil || dl.Hostname() == "" {
		return
	}
	serverOrigin := helpers.Origin(server)
	dlOrigin := helpers.Origin(dl)
	if serverOrigin == dlOrigin {
		return
	}
	runtime.Output.Warnf(
		"Downloading %s from origin %q, which differs from the configured server origin %q",
		helpers.WithoutCredentials(downloadURL), dlOrigin, serverOrigin,
	)
}

// attemptDownloadToCache performs one download attempt, streaming through
// the extracted store when one is configured or else to a verified temp file,
// and cleans up everything it created on failure so a retry never leaks.
func attemptDownloadToCache(
	ctx context.Context,
	deps installDeps,
	key string,
	base string,
	meta *types.GalaxyCollectionVersionInfo,
	useCache bool,
) (downloadResult, error) {
	warnIfOffServerDownloadHost(deps.runtime, base, meta.DownloadURL)
	resp, err := downloadCollection(ctx, deps.runtime, meta.DownloadURL)
	if err != nil {
		return downloadResult{}, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if deps.extractStore != nil {
		return streamDownloadAndExtract(ctx, deps, key, meta, resp.Body, useCache)
	}

	tmpPath, cleanup, sha, err := writeDownloadToTemp(ctx, deps, resp.Body)
	if err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	if err := verifyDownloadSHA(meta, sha); err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	// Only this arm probes the tar.gz shape (the streaming arm's ingest
	// already parses it): with no server-declared sha, an error page would
	// otherwise reach a shared cache slot.
	if err := archive.ProbeTarGz(ctx, tmpPath); err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	if useCache {
		return commitDownload(ctx, deps.artifacts, key, tmpPath, sha, cleanup)
	}
	return downloadResult{Path: tmpPath, SHA: sha, Cleanup: cleanup}, nil
}

// streamDownloadAndExtract tees the body once into a hasher, the temp file
// and the extracted store's ingest pipe, then promotes the tree to <root>/<sha>.
func streamDownloadAndExtract(
	ctx context.Context,
	deps installDeps,
	key string,
	meta *types.GalaxyCollectionVersionInfo,
	body io.Reader,
	useCache bool,
) (downloadResult, error) {
	tmpFile, tmpCleanup, err := deps.artifacts.TempFile(ctx, helpers.ArtifactDownloadTempPrefix)
	if err != nil {
		return downloadResult{}, err
	}

	pr, pw := io.Pipe()
	type ingestOutcome struct {
		err error
		tmp string
	}
	ingestCh := make(chan ingestOutcome, 1)
	go func() {
		tmp, ingestErr := deps.extractStore.IngestReader(ctx, pr)
		ingestCh <- ingestOutcome{tmp: tmp, err: ingestErr}
	}()

	hasher := sha256.New()
	limited := helpers.NewSizeLimitedReader(body, helpers.ArtifactMaxDownloadSize)
	n, copyErr := io.Copy(io.MultiWriter(tmpFile, hasher, pw), limited)
	deps.runtime.Metrics.AddBytesDownloaded(n)
	if copyErr != nil {
		// Label the bare, shared ErrResponseTooLarge so the message names this
		// surface; untested, since the 4 GiB ceiling is impractical to trip.
		copyErr = fmt.Errorf("collection artifact download: %w", copyErr)
		_ = pw.CloseWithError(copyErr)
	} else {
		_ = pw.Close()
	}
	closeErr := tmpFile.Close()
	out := <-ingestCh

	// Discard, not os.RemoveAll: the extracted store removes its own paths
	// through its containment root.
	if err := firstNonNil(copyErr, closeErr, out.err); err != nil {
		_ = deps.extractStore.Discard(out.tmp)
		cleanupIfNeeded(tmpCleanup)
		return downloadResult{}, err
	}

	sha := hex.EncodeToString(hasher.Sum(nil))
	if err := verifyDownloadSHA(meta, sha); err != nil {
		_ = deps.extractStore.Discard(out.tmp)
		cleanupIfNeeded(tmpCleanup)
		return downloadResult{}, err
	}
	if _, err := deps.extractStore.Promote(out.tmp, sha); err != nil {
		cleanupIfNeeded(tmpCleanup)
		return downloadResult{}, err
	}
	if useCache {
		return commitDownload(ctx, deps.artifacts, key, tmpFile.Name(), sha, tmpCleanup)
	}
	return downloadResult{Path: tmpFile.Name(), SHA: sha, Cleanup: tmpCleanup}, nil
}

func firstNonNil(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// checkDownloadURL refuses all but an absolute http(s) URL with a host and
// no userinfo, whose Basic credential would displace the operator's token.
// Scheme and host come first: a userinfo refusal names an otherwise good URL.
func checkDownloadURL(raw string) error {
	display := helpers.URLForMessage(raw)

	parsed, err := url.Parse(raw)
	if err != nil {
		// The parse error is deliberately not wrapped in: url.Error renders the
		// whole value it failed on, query string and userinfo included.
		return fmt.Errorf("%w: %q could not be parsed as a URL", helpers.ErrUnsupportedDownloadURLScheme, display)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if (scheme != "http" && scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%w: %q", helpers.ErrUnsupportedDownloadURLScheme, display)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: %q", helpers.ErrDownloadURLUserinfo, display)
	}
	return nil
}

func validateDownloadInputs(cfg *config.Config, artifacts cacheManager.ArtifactStore, meta *types.GalaxyCollectionVersionInfo) error {
	if meta == nil {
		return helpers.ErrMetadataIsNil
	}
	if meta.DownloadURL == "" {
		return helpers.ErrMissingDownloadURL
	}
	if err := checkDownloadURL(meta.DownloadURL); err != nil {
		return err
	}
	if cfg == nil {
		return helpers.ErrConfigIsNil
	}
	if artifacts == nil {
		return helpers.ErrArtifactCacheNotConfigured
	}
	return nil
}

func writeDownloadToTemp(ctx context.Context, deps installDeps, body io.Reader) (string, func(), string, error) {
	tmpFile, cleanup, err := deps.artifacts.TempFile(ctx, helpers.ArtifactDownloadTempPrefix)
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
		// Wrapped for the same reason as the streaming-ingest copy, so this
		// capped body also names its surface.
		return "", cleanup, "", fmt.Errorf("collection artifact download: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", cleanup, "", err
	}
	return tmpFile.Name(), cleanup, hex.EncodeToString(hasher.Sum(nil)), nil
}

func verifyDownloadSHA(meta *types.GalaxyCollectionVersionInfo, sha string) error {
	expected := strings.TrimSpace(meta.Artifact.Sha256)
	if expected == "" || expected == sha {
		return nil
	}
	return fmt.Errorf("%w: %s != %s", helpers.ErrSHA256Mismatch, expected, sha)
}

func commitDownload(
	ctx context.Context,
	artifacts cacheManager.ArtifactStore,
	key string,
	tmpPath string,
	sha string,
	cleanup func(),
) (downloadResult, error) {
	stored, err := artifacts.Commit(ctx, key, tmpPath, map[string]string{"sha256": sha})
	if err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	return downloadResult{Path: stored.Path, SHA: sha, Cleanup: stored.Cleanup}, nil
}

func cleanupIfNeeded(cleanup func()) {
	if cleanup != nil {
		cleanup()
	}
}

// resolveMetadata loads metadata when needed and handles cache-hit warnings.
func resolveMetadata(
	ctx context.Context,
	deps collectionDeps,
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
	cacheHit bool,
) (*types.GalaxyCollectionVersionInfo, error) {
	runtime := deps.runtime
	if meta != nil {
		return meta, nil
	}
	// A git or url collection has no Galaxy version document; a nil meta is the
	// value every downstream step handles, as for a metadata-free cache hit.
	if col.isGit() || col.isURL() {
		return nil, nil //nolint:nilnil // nil meta is the established "no server metadata" value downstream
	}
	metaStart := time.Now()
	meta, err := loadCollectionMetadata(ctx, deps, col)
	runtime.Output.DebugSincef(metaStart, "%s", "Metadata "+col.key())
	if err != nil {
		if cacheHit {
			runtime.Output.Warnf("Failed to load metadata for %s: %v", col.key(), err)
			return nil, helpers.ErrMetadataUnavailable
		}
		return nil, fmt.Errorf("failed to load metadata: %w", err)
	}
	return meta, nil
}
