package collections

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/psvmcc/hub/pkg/types"
)

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
	policy := cachePolicyForConstraint(cfg, exact)

	rootMetadata, err := loadRootMetadataCached(ctx, deps, col, policy)
	if err != nil {
		return nil, fmt.Errorf("failed to load root metadata: %w", err)
	}
	versionsURL := normalizeVersionsURL(col.Source, rootMetadata.VersionsURL)
	if !strings.HasSuffix(versionsURL, "/") {
		versionsURL += "/"
	}
	runtime.Output.Debugf("versions_url resolved: base=%s ref=%s -> %s", col.Source, rootMetadata.VersionsURL, versionsURL)

	versionURL := rootMetadata.HighestVersion.Href

	if exact {
		versionURL = versionsURL + version + "/"
	}

	if !exact && col.Version != "*" {
		versions, err := loadVersionsListCached(ctx, deps, versionsURL, versionLimit, policy)
		if err != nil {
			return nil, fmt.Errorf("failed to load versions list: %w", err)
		}
		selected, err := selectVersion(versions, []string{col.Version})
		if err != nil {
			return nil, err
		}
		versionURL = versionsURL + selected + "/"
	}

	versionURL = normalizeVersionsURL(col.Source, versionURL)
	var versionMetadataInfo types.GalaxyCollectionVersionInfo
	if err := fetchJSONWithCachePolicy(ctx, runtime.HTTP, versionURL, st, &versionMetadataInfo, policy); err != nil {
		return nil, err
	}

	return &versionMetadataInfo, nil
}

// loadRootMetadataCached loads the root collection metadata from candidates.
func loadRootMetadataCached(
	ctx context.Context,
	deps collectionDeps,
	col collection,
	policy cacheManager.Policy,
) (*types.GalaxyCollection, error) {
	cfg := deps.cfg
	runtime := deps.runtime
	st := deps.st

	var lastErr error
	hasExplicitSource := strings.TrimSpace(col.Source) != ""
	candidates := rootMetadataURLCandidates(cfg, col)
	runtime.Output.Debugf("root metadata candidates for %s: %s", col.key(), strings.Join(candidates, ", "))

	for _, url := range candidates {
		runtime.Output.Debugf("root metadata GET %s", url)
		var root types.GalaxyCollection
		if err := fetchJSONWithCachePolicy(ctx, runtime.HTTP, url, st, &root, policy); err != nil {
			var statusErr *cacheManager.HTTPStatusError
			if hasExplicitSource {
				return nil, err
			}
			if errors.As(err, &statusErr) && statusErr.Code == http.StatusNotFound {
				runtime.Output.Debugf("root metadata 404 %s", url)
				lastErr = err
				continue
			}
			return nil, err
		}
		runtime.Output.Debugf("root metadata OK %s", url)
		return &root, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, helpers.ErrLoadMetadataFailed
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
	base := normalizeVersionsURL(source, versionsURL)
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	url := fmt.Sprintf("%s%s/", base, version)
	var info types.GalaxyCollectionVersionInfo
	if err := fetchJSONWithCachePolicy(ctx, runtime.HTTP, url, st, &info, policy); err != nil {
		return nil, err
	}
	return &info, nil
}

// normalizeVersionsURL resolves version URLs relative to a source.
func normalizeVersionsURL(source, versionsURL string) string {
	base := strings.TrimSpace(versionsURL)
	if after, ok := strings.CutPrefix(base, "https//"); ok {
		base = "https://" + after
	}
	if after, ok := strings.CutPrefix(base, "http//"); ok {
		base = "http://" + after
	}
	if strings.HasPrefix(base, "https://") || strings.HasPrefix(base, "http://") {
		return base
	}
	return resolveURL(source, base)
}
