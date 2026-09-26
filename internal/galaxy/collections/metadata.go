package collections

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/psvmcc/hub/pkg/types"
)

// versionMetadata is loadCollectionMetadata, except that a collection carrying
// its locked download URL gets a document of that URL and its pin alone, with
// no request: a frozen install then asks the server for nothing but the bytes.
func versionMetadata(ctx context.Context, deps collectionDeps, col collection) (*types.GalaxyCollectionVersionInfo, error) {
	if col.DownloadURL == "" {
		return loadCollectionMetadata(ctx, deps, col)
	}
	meta := &types.GalaxyCollectionVersionInfo{Version: col.Version, DownloadURL: col.DownloadURL}
	meta.Artifact.Sha256 = col.SHA256
	return meta, nil
}

// loadCollectionMetadata resolves and fetches metadata for a collection version.
func loadCollectionMetadata(
	ctx context.Context,
	deps collectionDeps,
	col collection,
) (*types.GalaxyCollectionVersionInfo, error) {
	cfg := deps.cfg
	runtime := deps.runtime
	st := deps.st

	version, exact, err := exactVersionFromConstraints([]string{col.Version})
	if err != nil {
		return nil, err
	}
	policy := cacheManager.PolicyForConstraint(cfg, exact)

	rootMetadata, base, err := loadRootMetadataCached(ctx, deps, col, policy)
	if err != nil {
		return nil, fmt.Errorf("failed to load root metadata: %w", err)
	}
	versionsURL, err := normalizeVersionsURL(base, rootMetadata.VersionsURL)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(versionsURL, "/") {
		versionsURL += "/"
	}
	// The server-chosen ref is quoted so it cannot forge a log line, and both
	// server-influenced values are cut textually, since checkMetadataURLUserinfo
	// passes what url.Parse refuses. base is the winning server's own, not cut.
	runtime.Output.Debugf("Versions_url resolved: base=%s ref=%q -> %s",
		base, helpers.WithoutCredentials(rootMetadata.VersionsURL), helpers.WithoutCredentials(versionsURL))

	versionURL := rootMetadata.HighestVersion.Href

	if exact {
		versionURL = versionsURL + version + "/"
	}

	versionURL, err = normalizeVersionsURL(base, versionURL)
	if err != nil {
		return nil, err
	}
	var versionMetadataInfo types.GalaxyCollectionVersionInfo
	if err := fetchJSONWithCachePolicy(ctx, runtime, versionURL, st, &versionMetadataInfo, policy); err != nil {
		return nil, err
	}

	return &versionMetadataInfo, nil
}

// loadRootMetadataCached walks serverCandidates for col's root metadata and
// returns it with the answering server's normalized base, which owns col for
// the rest of the run. Only a 404, with web pages at most beside it, advances.
func loadRootMetadataCached(
	ctx context.Context,
	deps collectionDeps,
	col collection,
	policy cacheManager.Policy,
) (*types.GalaxyCollection, string, error) {
	// Every caller branches on the locator prefixes before asking a server;
	// this guard turns a caller that forgot into a plain resolution failure
	// instead of a walk over servers that cannot know the collection.
	if col.isGit() || col.isURL() {
		return nil, "", fmt.Errorf("%w: %s comes from a %s source, which no Galaxy server answers for",
			helpers.ErrLoadMetadataFailed, col.key(), col.Type)
	}
	var lastErr error
	fqdn := col.Namespace + "." + col.Name
	for _, srv := range serverCandidates(deps, col) {
		if notFound := deps.absent.lookup(srv.base, fqdn); notFound != nil {
			deps.runtime.Output.Debugf("Root metadata for %s: server %s answered 404 earlier this phase", fqdn, srv.label())
			lastErr = notFound
			continue
		}
		meta, ok, err := tryServerRootMetadata(ctx, deps, col, policy, srv)
		if ok {
			return meta, srv.base, nil
		}
		if err == nil {
			continue
		}
		// A 404 across this server's candidates means only that it lacks col,
		// so try the next server; anything else, auth or availability included,
		// must not be routed around and aborts the walk.
		if isNotFoundStatus(err) {
			deps.absent.record(srv.base, fqdn, err)
			lastErr = err
			continue
		}
		return nil, "", err
	}
	if lastErr != nil {
		return nil, "", lastErr
	}
	return nil, "", helpers.ErrLoadMetadataFailed
}

// tryServerRootMetadata tries one server's apiRoot candidates, deciding each by
// its answer: a 404 moves on to the next, a web page is skipped, and any other
// failure aborts the walk, as do web pages alone.
func tryServerRootMetadata(
	ctx context.Context,
	deps collectionDeps,
	col collection,
	policy cacheManager.Policy,
	srv serverCandidate,
) (*types.GalaxyCollection, bool, error) {
	runtime := deps.runtime
	st := deps.st

	var lastErr, webPage error
	candidates := rootMetadataURLCandidates(srv.base, col, deps.apiRoots)
	runtime.Output.Debugf("Root metadata candidates for %s on server %s: %s", col.key(), srv.label(), joinCandidateURLs(candidates))

	for _, cand := range candidates {
		runtime.Output.Debugf("Root metadata GET %s", cand.url)
		var root types.GalaxyCollection
		if err := fetchJSONWithCachePolicy(ctx, runtime, cand.url, st, &root, policy); err != nil {
			// A web page at an API root (galaxy.ansible.com's /v3) is skipped,
			// never kept as lastErr: only a real 404 says the server lacks col.
			if cacheManager.IsWebPage(err) {
				runtime.Output.Debugf("Root metadata is a web page %s", cand.url)
				if webPage == nil {
					webPage = err
				}
				continue
			}
			if isNotFoundStatus(err) {
				runtime.Output.Debugf("Root metadata 404 %s", cand.url)
				lastErr = err
				continue
			}
			return nil, false, rootAbortError(srv, err)
		}
		runtime.Output.Debugf("Root metadata OK %s", cand.url)
		// Recorded only on success: a 404 means this collection is absent under
		// the apiRoot, not that the apiRoot is wrong, so it never blacklists one.
		deps.apiRoots.recordWinner(cand.base, cand.apiRoot)
		return &root, true, nil
	}
	return nil, false, serverWalkError(srv, lastErr, webPage)
}

// isNotFoundStatus reports whether err is a 404 answer, the one status that
// says a server lacks something rather than that the server failed.
func isNotFoundStatus(err error) bool {
	statusErr, ok := errors.AsType[*cacheManager.HTTPStatusError](err)
	return ok && statusErr.Code == http.StatusNotFound
}

// rootAbortError names the server whose status answer aborts the walk; any
// other failure already names what failed and is returned as is.
func rootAbortError(srv serverCandidate, err error) error {
	if _, ok := errors.AsType[*cacheManager.HTTPStatusError](err); ok {
		return fmt.Errorf("server %s: %w", srv.label(), err)
	}
	return err
}

// serverWalkError ends a server whose candidates none answered: its last 404,
// else an abort when only web pages answered, which are no evidence of absence
// (an SSO front serves every path its login page), else nil for no candidates.
func serverWalkError(srv serverCandidate, last404, webPage error) error {
	if last404 != nil || webPage == nil {
		return last404
	}
	return fmt.Errorf("server %s answers a web page at every API root: %w", srv.label(), webPage)
}

// fetchVersionMetadataCached fetches metadata for a specific version.
func fetchVersionMetadataCached(
	ctx context.Context,
	deps collectionDeps,
	source string,
	versionsURL string,
	version string,
	policy cacheManager.Policy,
) (*types.GalaxyCollectionVersionInfo, error) {
	runtime := deps.runtime
	st := deps.st

	version = strings.TrimSpace(strings.TrimPrefix(version, "= "))
	base, err := normalizeVersionsURL(source, versionsURL)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	versionURL := fmt.Sprintf("%s%s/", base, version)
	var info types.GalaxyCollectionVersionInfo
	if err := fetchJSONWithCachePolicy(ctx, runtime, versionURL, st, &info, policy); err != nil {
		return nil, err
	}
	return &info, nil
}

// normalizeVersionsURL resolves a server-chosen metadata reference against
// source and refuses a result carrying userinfo. It must stay outside the
// status-routed server walk: the refusal has no HTTP status to be routed by.
func normalizeVersionsURL(source, versionsURL string) (string, error) {
	base := strings.TrimSpace(versionsURL)
	if after, ok := strings.CutPrefix(base, "https//"); ok {
		base = "https://" + after
	}
	if after, ok := strings.CutPrefix(base, "http//"); ok {
		base = "http://" + after
	}
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		base = resolveURL(source, base)
	}
	return base, checkMetadataURLUserinfo(base)
}

// checkMetadataURLUserinfo refuses a metadata URL with provable userinfo. A
// value url.Parse refuses passes: net/http cannot build a request from it, and
// every message about it is cut by helpers.WithoutCredentials.
func checkMetadataURLUserinfo(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return nil
	}
	return fmt.Errorf("%w: %q", helpers.ErrMetadataURLUserinfo, helpers.URLForMessage(raw))
}
