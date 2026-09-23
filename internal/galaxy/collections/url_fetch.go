package collections

import (
	"context"
	"fmt"

	"github.com/greeddj/go-galaxy/internal/galaxy/collectionbuild"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/manifest"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// urlFetchToCache re-downloads a pinned url collection whose cached artifact is
// missing or corrupt. The bytes must match the locator's sha256 and the manifest
// its identity, since an unsigned artifact never reaches checkManifestAttribution.
func urlFetchToCache(ctx context.Context, deps installDeps, col collection, useCache bool) (downloadResult, error) {
	loc, err := col.urlLocator()
	if err != nil {
		return downloadResult{}, err
	}
	if !loc.Pinned() {
		return downloadResult{}, fmt.Errorf("%w: %s is not pinned to a sha256", helpers.ErrInvalidURLLocator, col.key())
	}
	display := helpers.URLForMessage(loc.URL)
	result, err := downloadURLToTemp(ctx, deps.collectionDeps, loc.URL)
	if err != nil {
		return downloadResult{}, err
	}
	if result.SHA != loc.SHA256 {
		cleanupIfNeeded(result.Cleanup)
		return downloadResult{}, fmt.Errorf("%w: %s now serves sha256 %s, the pin records %s",
			helpers.ErrSHA256Mismatch, display, result.SHA, loc.SHA256)
	}
	if err := checkURLArtifactIdentity(ctx, col, result.Path, display); err != nil {
		cleanupIfNeeded(result.Cleanup)
		return downloadResult{}, err
	}
	if !useCache || deps.artifacts == nil {
		return result, nil
	}
	stored, err := commitDownload(ctx, deps.artifacts, artifactKey(col), result.Path, result.SHA, result.Cleanup)
	if err != nil {
		return downloadResult{}, err
	}
	deps.runtime.Metrics.AddCacheMiss()
	return stored, nil
}

// checkURLArtifactIdentity reads the downloaded artifact's MANIFEST.json and
// refuses an identity other than the collection being installed.
func checkURLArtifactIdentity(ctx context.Context, col collection, path, display string) error {
	manifestBytes, err := manifest.ReadFromTarGz(ctx, path)
	if err != nil {
		return fmt.Errorf("%s: %w", display, err)
	}
	meta, err := collectionbuild.ParseManifestInfo(manifestBytes)
	if err != nil {
		return fmt.Errorf("%s: %w", display, err)
	}
	if meta.Namespace != col.Namespace || meta.Name != col.Name || meta.Version != col.Version {
		return fmt.Errorf("%w: %s serves %s.%s@%s for %s",
			helpers.ErrURLArtifactIdentityMismatch, display, meta.Namespace, meta.Name, meta.Version, col.key())
	}
	return nil
}

// urlSourceOf parses col's locator URL for consumers that render or record
// the URL rather than the locator (the lockfile, GALAXY.yml).
func urlSourceOf(col collection) (urlsource.Locator, error) {
	loc, err := col.urlLocator()
	if err != nil {
		return urlsource.Locator{}, err
	}
	if !loc.Pinned() {
		return urlsource.Locator{}, fmt.Errorf("%w: %s is not pinned to a sha256", helpers.ErrInvalidURLLocator, col.key())
	}
	return loc, nil
}
