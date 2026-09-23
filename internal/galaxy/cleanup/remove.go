package cleanup

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

func removeUnused(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
	reachable map[string]bool,
	installedByKey map[string][]installedCollection,
) (int, error) {
	var removed int
	// Sorted, so a run that stops at a removal failure leaves the same
	// partial result on disk every time.
	for _, key := range slices.Sorted(maps.Keys(installedByKey)) {
		// Checked before the reachable skip, so a cancellation stops the loop
		// at the next collection even in a long run of reachable keys.
		if err := ctx.Err(); err != nil {
			return removed, fmt.Errorf("cleanup stopped at %s: %w", key, err)
		}
		insts := installedByKey[key]
		if reachable[key] {
			continue
		}
		removed++
		// Every part of key passed helpers.IsPathElement, which rejects unsafe
		// runes, so key prints with a bare %s and keeps its greppable shape.
		if cfg.DryRun {
			runtime.Output.Printf("Would remove %s", key)
			continue
		}
		// Only the persisted InstalledEntry records the server the scoped
		// artifact key must be built from; the disk scan knows none.
		source := installedSource(st, key)
		// Every project's copy of key goes in this run; the snapshot is
		// pruned once afterward.
		for _, inst := range insts {
			if err := removeInstalled(ctx, inst, backend.Artifacts(), source); err != nil {
				return removed, err
			}
		}
		runtime.Output.Printf("Removed %s", key)
		if st != nil {
			st.DeleteInstalled(key)
			st.DeleteGraph(key)
			// The deps-cache entry lives under the server-scoped key; with no
			// known source it is left for CacheEntryMaxAge to evict.
			if source != "" {
				st.DeleteDepsCache(helpers.ScopedDepsCacheKey(source, key))
			}
		}
	}
	return removed, nil
}

// installedSource returns the Source of key's persisted InstalledEntry, or ""
// when there is none, in which case the caller skips the scoped artifact and
// deps-cache purge rather than guess a key.
func installedSource(st *store.Store, key string) string {
	if st == nil {
		return ""
	}
	entry, ok := st.GetInstalled(key)
	if !ok {
		return ""
	}
	return entry.Source
}

// removeInstalled deletes a collection's files through an os.Root at its
// collections path by the walked namespace and name, then its server-scoped
// artifact when source is known, even if the workspace is already gone.
func removeInstalled(ctx context.Context, inst installedCollection, artifacts cacheManager.ArtifactStore, source string) error {
	namespace := inst.Namespace
	name := inst.Name
	if !helpers.IsPathElement(namespace) || !helpers.IsPathElement(name) || !helpers.IsPathElement(inst.Version) {
		return fmt.Errorf("%w: ns=%q name=%q version=%q", helpers.ErrUnsafeRemovalPath, namespace, name, inst.Version)
	}

	if err := removeWorkspaceFiles(inst, namespace, name); err != nil {
		return err
	}

	if artifacts != nil && strings.TrimSpace(source) != "" {
		filename := helpers.ArtifactFilename(namespace, name, inst.Version)
		_ = artifacts.Delete(ctx, helpers.ArtifactKey(source, filename))
	}
	return nil
}

// removeWorkspaceFiles removes the install directory and .info sidecar
// through one os.Root at inst.CollectionsDir, so a symlink swapped in above
// the collection cannot carry a removal out of the tree.
func removeWorkspaceFiles(inst installedCollection, namespace, name string) error {
	root, err := os.OpenRoot(inst.CollectionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			// The collections path is gone; the caller still purges the artifact.
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()

	if err := removeInstallPath(root, inst, namespace, name); err != nil {
		return err
	}
	return removeInfoDir(root, inst, namespace, name)
}

// removeInstallPath removes ansible_collections/<namespace>/<name> through
// root. The WithinDir guard only catches a record built outside the scan,
// whose InstallPath could disagree with the walked namespace and name.
func removeInstallPath(root *os.Root, inst installedCollection, namespace, name string) error {
	if inst.InstallPath == "" {
		return nil
	}
	if !helpers.WithinDir(inst.CollectionsDir, inst.InstallPath) {
		return fmt.Errorf("%w: install path %q escapes %q", helpers.ErrUnsafeRemovalPath, inst.InstallPath, inst.CollectionsDir)
	}
	installRel := filepath.Join("ansible_collections", namespace, name)
	return root.RemoveAll(installRel)
}

// removeInfoDir best-effort removes the <namespace>.<name>-<version>.info
// sidecar through root: only a WithinDir containment failure is an error, a
// RemoveAll failure is ignored.
func removeInfoDir(root *os.Root, inst installedCollection, namespace, name string) error {
	acRoot := filepath.Join(inst.CollectionsDir, "ansible_collections")
	infoName := fmt.Sprintf("%s.%s-%s.info", namespace, name, inst.Version)
	infoDir := filepath.Join(acRoot, infoName)
	// Unreachable while removeInstalled checks IsPathElement, kept as the
	// last guard before a RemoveAll.
	if !helpers.WithinDir(acRoot, infoDir) {
		return fmt.Errorf("%w: info dir %q escapes %q", helpers.ErrUnsafeRemovalPath, infoDir, acRoot)
	}
	_ = root.RemoveAll(filepath.Join("ansible_collections", infoName))
	return nil
}
