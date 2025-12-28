package cleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver"
	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// installedCollection tracks an installed collection discovered on disk.
type installedCollection struct {
	Key            string
	FQDN           string
	Version        string
	InstallPath    string
	CollectionsDir string
}

type cleanupState struct {
	backend  cacheManager.Backend
	store    *store.Store
	registry *store.ProjectRegistry
	release  func() error
}

// Start runs the cleanup process for unused collections.
func Start(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	var err error
	defer func() {
		if err != nil {
			runtime.Output.Errorf("Error: %s", err.Error())
		}
	}()

	state, err := initCleanup(ctx, cfg, runtime)
	if err != nil {
		return err
	}
	if state == nil || state.registry == nil || len(state.registry.Projects) == 0 {
		runtime.Output.Printf("ℹ️ No projects recorded for GC.")
		return nil
	}
	defer func() {
		if state.release != nil {
			_ = state.release()
		}
	}()
	defer func() {
		if state.backend != nil {
			_ = state.backend.Close(ctx)
		}
	}()

	reachable, installedByKey, err := buildReachable(runtime, state.registry)
	if err != nil {
		return err
	}
	removed, err := removeUnused(ctx, cfg, runtime, state.backend, state.store, reachable, installedByKey)
	if err != nil {
		return err
	}
	return finalizeCleanup(ctx, cfg, runtime, state.backend, state.store, removed)
}

func initCleanup(ctx context.Context, cfg *config.Config, runtime *infra.Infra) (*cleanupState, error) {
	runtime.Output.Printf("🚀 init cache backend")
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		return nil, err
	}
	if err := backend.Open(ctx); err != nil {
		return nil, err
	}
	releaseLock, err := backend.Lock(ctx)
	if err != nil {
		_ = backend.Close(ctx)
		return nil, err
	}
	runtime.Output.Printf("🚀 load storage")
	st, err := backend.LoadStore(ctx)
	if err != nil {
		_ = releaseLock()
		_ = backend.Close(ctx)
		return nil, err
	}
	runtime.Output.Printf("🚀 load projects registry")
	registry, err := backend.LoadProjectRegistry(ctx)
	if err != nil {
		_ = releaseLock()
		_ = backend.Close(ctx)
		return nil, err
	}
	return &cleanupState{
		backend:  backend,
		store:    st,
		registry: registry,
		release:  releaseLock,
	}, nil
}

func buildReachable(runtime *infra.Infra, registry *store.ProjectRegistry) (map[string]bool, map[string]installedCollection, error) {
	reachable := make(map[string]bool)
	installedIndex := make(map[string][]installedCollection)
	depsByKey := make(map[string]map[string]string)
	installedByKey := make(map[string]installedCollection)

	for projectPath, project := range registry.Projects {
		collectionsPath := pickCollectionsPath(projectPath, project)
		if collectionsPath == "" {
			continue
		}
		if err := scanInstalledCollections(collectionsPath, installedIndex, installedByKey, depsByKey); err != nil {
			return nil, nil, err
		}
		roots, err := loadRequirements(project.RequirementsFile, "")
		if err != nil {
			runtime.Output.Printf("⚠️ Failed to load requirements %s: %v", project.RequirementsFile, err)
			continue
		}
		for _, root := range roots {
			fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
			for _, inst := range selectInstalled(installedIndex, fqdn, root.Version) {
				markReachable(inst.Key, reachable, depsByKey, installedIndex)
			}
		}
	}
	return reachable, installedByKey, nil
}

func removeUnused(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
	reachable map[string]bool,
	installedByKey map[string]installedCollection,
) (int, error) {
	var removed int
	for key, inst := range installedByKey {
		if reachable[key] {
			continue
		}
		removed++
		if cfg.DryRun {
			runtime.Output.Printf("🧹 remove %s", key)
			continue
		}
		if err := removeInstalled(ctx, inst, backend.Artifacts()); err != nil {
			return removed, err
		}
		runtime.Output.Printf("🧹 remove %s", key)
		if st != nil {
			st.DeleteInstalled(key)
			st.DeleteGraph(key)
			st.DeleteDepsCache(key)
		}
	}
	return removed, nil
}

func finalizeCleanup(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
	removed int,
) error {
	if !cfg.DryRun {
		if err := backend.SaveStore(ctx, st); err != nil {
			return err
		}
	}
	if cfg.DryRun {
		runtime.Output.PersistentPrintf("🫡 Dry-run cleanup complete. Candidates: %d", removed)
		return nil
	}
	runtime.Output.PersistentPrintf("✨ Cleanup complete. Removed: %d", removed)
	return nil
}

// pickCollectionsPath chooses the collections path for a project.
func pickCollectionsPath(projectPath string, project store.ProjectRecord) string {
	candidates := []string{}
	if project.CollectionsPath != "" {
		candidates = append(candidates, project.CollectionsPath)
	}
	if projectPath != "" {
		candidates = append(candidates, filepath.Join(projectPath, ".collections"), filepath.Join(projectPath, "collections"))
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if dirExists(filepath.Join(candidate, "ansible_collections")) {
			return candidate
		}
	}
	return ""
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// scanInstalledCollections indexes installed collections under collectionsPath.
func scanInstalledCollections(
	collectionsPath string,
	index map[string][]installedCollection,
	byKey map[string]installedCollection,
	deps map[string]map[string]string,
) error {
	root := filepath.Join(collectionsPath, "ansible_collections")
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || d.Name() != "MANIFEST.json" {
			return nil
		}
		manifest, ok, err := readManifest(path)
		if err != nil || !ok {
			return err
		}
		record, key, ok := buildInstalledRecord(collectionsPath, path, manifest)
		if !ok {
			return nil
		}
		index[record.FQDN] = append(index[record.FQDN], record)
		byKey[key] = record
		deps[key] = extractDeps(manifest)
		return nil
	})
}

func readManifest(path string) (types.GalaxyCollectionVersionInfoManifest, bool, error) {
	//nolint:gosec // path comes from WalkDir rooted at collectionsPath.
	data, err := os.ReadFile(path)
	if err != nil {
		return types.GalaxyCollectionVersionInfoManifest{}, false, err
	}
	var manifest types.GalaxyCollectionVersionInfoManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return types.GalaxyCollectionVersionInfoManifest{}, false, nil
	}
	return manifest, true, nil
}

func buildInstalledRecord(
	collectionsPath string,
	manifestPath string,
	manifest types.GalaxyCollectionVersionInfoManifest,
) (installedCollection, string, bool) {
	ns := manifest.CollectionInfo.Namespace
	name := manifest.CollectionInfo.Name
	version := manifest.CollectionInfo.Version
	if ns == "" || name == "" || version == "" {
		return installedCollection{}, "", false
	}
	installPath := filepath.Dir(manifestPath)
	key := fmt.Sprintf("%s.%s@%s", ns, name, version)
	fqdn := fmt.Sprintf("%s.%s", ns, name)
	return installedCollection{
		Key:            key,
		FQDN:           fqdn,
		Version:        version,
		InstallPath:    installPath,
		CollectionsDir: collectionsPath,
	}, key, true
}

func extractDeps(manifest types.GalaxyCollectionVersionInfoManifest) map[string]string {
	if manifest.CollectionInfo.Dependencies != nil {
		return manifest.CollectionInfo.Dependencies
	}
	return map[string]string{}
}

// normalizeConstraint normalizes semver constraints for matching.
func normalizeConstraint(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "*" {
		return ""
	}
	return trimmed
}

// selectInstalled filters installed collections by constraint.
func selectInstalled(index map[string][]installedCollection, fqdn, constraint string) []installedCollection {
	items := index[fqdn]
	if len(items) == 0 {
		return nil
	}
	normalized := normalizeConstraint(constraint)
	if normalized == "" {
		return items
	}
	c, err := semver.NewConstraint(normalized)
	if err != nil {
		return items
	}
	out := make([]installedCollection, 0, len(items))
	for _, item := range items {
		v, err := semver.NewVersion(item.Version)
		if err != nil {
			continue
		}
		if c.Check(v) {
			out = append(out, item)
		}
	}
	return out
}

// markReachable marks all reachable dependencies starting at key.
func markReachable(key string, reachable map[string]bool, deps map[string]map[string]string, index map[string][]installedCollection) {
	queue := []string{key}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if reachable[current] {
			continue
		}
		reachable[current] = true
		for depFQDN, constraint := range deps[current] {
			for _, inst := range selectInstalled(index, depFQDN, constraint) {
				if !reachable[inst.Key] {
					queue = append(queue, inst.Key)
				}
			}
		}
	}
}

// removeInstalled deletes collection files and cached artifacts.
func removeInstalled(ctx context.Context, inst installedCollection, artifacts cacheManager.ArtifactStore) error {
	parts := strings.Split(inst.FQDN, ".")
	if len(parts) != helpers.CollectionNameParts {
		return nil
	}
	namespace := parts[0]
	name := parts[1]
	if inst.InstallPath != "" {
		if err := os.RemoveAll(inst.InstallPath); err != nil {
			return err
		}
	}
	infoDir := filepath.Join(inst.CollectionsDir, "ansible_collections", fmt.Sprintf("%s.%s-%s.info", namespace, name, inst.Version))
	_ = os.RemoveAll(infoDir)

	if artifacts != nil {
		key := artifactKey(namespace, name, inst.Version)
		_ = artifacts.Delete(ctx, key)
	}
	return nil
}

// artifactKey builds the cache key for an artifact.
func artifactKey(namespace, name, version string) string {
	filename := fmt.Sprintf("%s-%s-%s.tar.gz", namespace, name, version)
	return url.QueryEscape(filename)
}
