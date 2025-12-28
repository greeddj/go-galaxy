package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

const versionLimit = 100

// installCollection downloads, extracts, and records a collection install.
func installCollection(
	ctx context.Context,
	col collection,
	deps installDeps,
	resolvedDeps []string,
	metaOverride *types.GalaxyCollectionVersionInfo,
) error {
	cfg := deps.cfg
	runtime := deps.runtime
	st := deps.st

	installStart := time.Now()
	defer func() {
		runtime.Output.DebugSincef(installStart, "%s", col.key())
	}()

	filename := fmt.Sprintf("%s-%s-%s.tar.gz", col.Namespace, col.Name, col.Version)
	installPath := filepath.Join(cfg.DownloadPath, "ansible_collections", col.Namespace, col.Name)

	if canSkipInstall(cfg, col, installPath, st) {
		runtime.Output.Printf("⏭️ Skipping install, already installed: %s/%s/%s", col.Namespace, col.Name, col.Version)
		return nil
	}

	payload, err := prepareInstall(ctx, deps, col, metaOverride, filename)
	if err != nil {
		return err
	}
	if payload.artifact.Cleanup != nil {
		defer payload.artifact.Cleanup()
	}

	extractStart := time.Now()
	err = extractCollection(col, payload.artifact.Path, installPath, runtime, payload.artifactSHA)
	if err != nil {
		return fmt.Errorf("failed to extract %s: %w", filename, err)
	}
	runtime.Output.DebugSincef(extractStart, "%s", "extract "+col.key())
	depsList, err := resolveDependencies(ctx, installPath, deps, resolvedDeps, col, filename)
	if err != nil {
		return err
	}
	writeGalaxyInfoIfPresent(runtime, cfg, payload.meta)
	recordInstall(st, col, installPath, payload.artifactSHA, depsList)
	return nil
}

type installPayload struct {
	meta        *types.GalaxyCollectionVersionInfo
	artifact    artifactData
	artifactSHA string
}

type artifactData struct {
	Path    string
	SHA     string
	Meta    map[string]string
	Cleanup func()
}

func prepareInstall(
	ctx context.Context,
	deps installDeps,
	col collection,
	metaOverride *types.GalaxyCollectionVersionInfo,
	filename string,
) (installPayload, error) {
	cfg := deps.cfg
	runtime := deps.runtime
	artifacts := deps.artifacts

	meta := metaOverride
	useCache := !cfg.NoCache
	cacheHit := useCache && artifacts != nil && artifactExists(ctx, artifacts, col)

	if cacheHit && meta == nil {
		runtime.Output.Printf("📦 Using cached %s", filename)
	}

	meta, err := resolveMetadata(ctx, deps.collectionDeps, col, meta, cacheHit)
	if err != nil && !errors.Is(err, helpers.ErrMetadataUnavailable) {
		return installPayload{}, err
	}

	artifact, err := fetchArtifact(ctx, deps, col, meta, cacheHit, useCache)
	if err != nil {
		return installPayload{}, err
	}
	artifactSHA, err := resolveArtifactSHA(artifact.Path, meta, artifact.Meta, artifact.SHA)
	if err != nil {
		if artifact.Cleanup != nil {
			artifact.Cleanup()
		}
		return installPayload{}, err
	}
	return installPayload{meta: meta, artifact: artifact, artifactSHA: artifactSHA}, nil
}

func resolveDependencies(
	ctx context.Context,
	installPath string,
	deps installDeps,
	resolvedDeps []string,
	col collection,
	filename string,
) ([]string, error) {
	cfg := deps.cfg
	runtime := deps.runtime

	if resolvedDeps != nil || cfg.NoDeps {
		return resolvedDeps, nil
	}
	depsStart := time.Now()
	depsList, err := installDependencies(ctx, installPath, deps)
	if err != nil {
		return nil, fmt.Errorf("failed to install dependencies for %s: %w", filename, err)
	}
	runtime.Output.DebugSincef(depsStart, "%s", "deps "+col.key())
	return depsList, nil
}

func writeGalaxyInfoIfPresent(runtime *infra.Infra, cfg *config.Config, meta *types.GalaxyCollectionVersionInfo) {
	if err := writeGalaxyInfo(cfg, meta); err != nil {
		runtime.Output.Printf("⚠️ Failed to write GALAXY.yml: %v", err)
	}
}

func recordInstall(st *store.Store, col collection, installPath, artifactSHA string, deps []string) {
	if st == nil {
		return
	}
	st.SetInstalled(col.key(), installedEntry{
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
		downloadStart := time.Now()
		result, err := downloadCollectionToCache(ctx, deps, artifactKey(col), meta, useCache)
		if err != nil {
			return artifactData{}, err
		}
		runtime.Output.DebugSincef(downloadStart, "%s", "download "+col.key())
		return artifactData{Path: result.Path, Cleanup: result.Cleanup, SHA: result.SHA}, nil
	}
	if artifacts == nil {
		return artifactData{}, helpers.ErrArtifactCacheNotConfigured
	}
	cached, err := artifacts.Fetch(ctx, artifactKey(col))
	if err != nil {
		return artifactData{}, err
	}
	return artifactData{Path: cached.Path, Cleanup: cached.Cleanup, Meta: cached.Meta}, nil
}

func resolveArtifactSHA(
	path string,
	meta *types.GalaxyCollectionVersionInfo,
	artifactMeta map[string]string,
	artifactSHA string,
) (string, error) {
	sha := strings.TrimSpace(artifactSHA)
	if sha == "" && meta != nil && meta.Artifact.Sha256 != "" {
		sha = strings.TrimSpace(meta.Artifact.Sha256)
	}
	if sha == "" && artifactMeta != nil {
		sha = strings.TrimSpace(artifactMeta["sha256"])
	}
	if sha != "" {
		return sha, nil
	}
	return archive.FileHashSHA256(path)
}

// artifactKey builds the cache key for a collection tarball.
func artifactKey(col collection) string {
	filename := fmt.Sprintf("%s-%s-%s.tar.gz", col.Namespace, col.Name, col.Version)
	return url.QueryEscape(filename)
}

// canSkipInstall reports whether a collection is already installed.
func canSkipInstall(cfg *config.Config, col collection, installPath string, st *store.Store) bool {
	if cfg == nil || st == nil {
		return false
	}
	entry, ok := st.GetInstalled(col.key())
	if !ok {
		return false
	}
	if entry.InstallPath == "" || entry.InstallPath != installPath {
		return false
	}
	if entry.ArtifactSHA256 == "" {
		return false
	}

	marker := filepath.Join(installPath, ".extract-done."+entry.ArtifactSHA256)
	if _, err := os.Stat(marker); err != nil {
		return false
	}

	infoDir := filepath.Join(cfg.DownloadPath, "ansible_collections", fmt.Sprintf("%s.%s-%s.info", col.Namespace, col.Name, col.Version))
	if _, err := os.Stat(filepath.Join(infoDir, "GALAXY.yml")); err != nil {
		return false
	}

	return true
}

// downloadCollection fetches an artifact and returns the HTTP response.
func downloadCollection(ctx context.Context, runtime *infra.Infra, collectionURL string) (*http.Response, error) {
	runtime.Output.Printf("🌐 Downloading %s", collectionURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, collectionURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := runtime.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: %s (%s)", helpers.ErrDownloadFailed, collectionURL, resp.Status)
	}
	return resp, nil
}

// downloadResult describes a downloaded artifact file and metadata.
type downloadResult struct {
	Path    string
	SHA     string
	Cleanup func()
}

// downloadCollectionToCache downloads an artifact and optionally stores it.
func downloadCollectionToCache(
	ctx context.Context,
	deps installDeps,
	key string,
	meta *types.GalaxyCollectionVersionInfo,
	useCache bool,
) (downloadResult, error) {
	if err := validateDownloadInputs(deps.cfg, deps.artifacts, meta); err != nil {
		return downloadResult{}, err
	}
	resp, err := downloadCollection(ctx, deps.runtime, meta.DownloadURL)
	if err != nil {
		return downloadResult{}, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	tmpPath, cleanup, sha, err := writeDownloadToTemp(ctx, deps.artifacts, resp.Body)
	if err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	if err := verifyDownloadSHA(meta, sha); err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	if useCache {
		return commitDownload(ctx, deps.artifacts, key, tmpPath, sha, cleanup)
	}
	return downloadResult{Path: tmpPath, SHA: sha, Cleanup: cleanup}, nil
}

func validateDownloadInputs(cfg *config.Config, artifacts cacheManager.ArtifactStore, meta *types.GalaxyCollectionVersionInfo) error {
	if meta == nil {
		return helpers.ErrMetadataIsNil
	}
	if meta.DownloadURL == "" {
		return helpers.ErrMissingDownloadURL
	}
	if cfg == nil {
		return helpers.ErrConfigIsNil
	}
	if artifacts == nil {
		return helpers.ErrArtifactCacheNotConfigured
	}
	return nil
}

func writeDownloadToTemp(ctx context.Context, artifacts cacheManager.ArtifactStore, body io.Reader) (string, func(), string, error) {
	tmpFile, cleanup, err := artifacts.TempFile(ctx, ".download-")
	if err != nil {
		return "", cleanup, "", err
	}
	hasher := sha256.New()
	writer := io.MultiWriter(tmpFile, hasher)
	if _, err := io.Copy(writer, body); err != nil {
		_ = tmpFile.Close()
		return "", cleanup, "", err
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
	metaStart := time.Now()
	meta, err := loadCollectionMetadata(ctx, deps, col)
	runtime.Output.DebugSincef(metaStart, "%s", "metadata "+col.key())
	if err != nil {
		if cacheHit {
			runtime.Output.Printf("⚠️ Failed to load metadata for %s: %v", col.key(), err)
			return nil, helpers.ErrMetadataUnavailable
		}
		return nil, fmt.Errorf("failed to load metadata: %w", err)
	}
	return meta, nil
}

// installDependencies installs dependent collections from MANIFEST.json.
func installDependencies(ctx context.Context, installPath string, depsCtx installDeps) ([]string, error) {
	cfg := depsCtx.cfg
	runtime := depsCtx.runtime

	manifestPath := filepath.Join(installPath, "MANIFEST.json")
	//nolint:gosec // manifestPath is derived from install path and is trusted.
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var parsed types.GalaxyCollectionVersionInfoManifest
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("invalid MANIFEST.json: %w", err)
	}

	deps := make([]string, 0, len(parsed.CollectionInfo.Dependencies))
	for fqdn, version := range parsed.CollectionInfo.Dependencies {
		parts := strings.Split(fqdn, ".")
		if len(parts) != helpers.CollectionNameParts {
			runtime.Output.Printf("⚠️ Skipping invalid dependency: %s", fqdn)
			continue
		}
		depCol := collection{Namespace: parts[0], Name: parts[1], Version: version, Source: cfg.Server}
		deps = append(deps, depCol.key())
		runtime.Output.Printf("🔁 Installing dependency: %s %s", fqdn, version)
		if err := installCollection(ctx, depCol, depsCtx, nil, nil); err != nil {
			runtime.Output.Printf("⚠️ Failed to install dependency: %s: %v", fqdn, err)
		}
	}
	return deps, nil
}
