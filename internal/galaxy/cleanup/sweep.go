package cleanup

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// legacyArtifactKey builds the retired flat artifact key that predates server
// scoping (helpers.ArtifactKey); no lookup reaches it any more. The filename
// is spelled literally so it keeps matching what old binaries wrote.
func legacyArtifactKey(namespace, name, version string) string {
	filename := fmt.Sprintf("%s-%s-%s.tar.gz", namespace, name, version)
	return url.QueryEscape(filename)
}

// sweepLegacyArtifacts deletes every scanned collection's artifact cached
// under legacyArtifactKey, reachable or not, since removeUnused purges only
// what it removes; a dry run reports only candidates that exist.
func sweepLegacyArtifacts(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	installedByKey map[string][]installedCollection,
) {
	artifacts := backend.Artifacts()
	if artifacts == nil {
		return
	}
	// Sorted so dry-run lines are stable and a canceled pass stops on a
	// prefix of one fixed order.
	for _, mapKey := range slices.Sorted(maps.Keys(installedByKey)) {
		// A canceled pass stops silently: runCleanup's LockLostError check
		// judges the run, and an unswept legacy key costs only disk.
		if ctx.Err() != nil {
			return
		}
		insts := installedByKey[mapKey]
		if len(insts) == 0 {
			continue
		}
		// Every on-disk copy of the same key shares the same namespace/name/
		// version, and therefore the same legacy key, regardless of which
		// project installed it - so only the first copy needs inspecting.
		inst := insts[0]
		legacyKey := legacyArtifactKey(inst.Namespace, inst.Name, inst.Version)
		// A namespace may contain ".", so "<12-hex>.acme" forges a legacy key
		// equal to a live server-scoped one; never purge a key of that shape.
		if helpers.IsScopedArtifactKey(legacyKey) {
			continue
		}
		if cfg.DryRun {
			reportLegacyArtifactSweepCandidate(ctx, runtime, artifacts, legacyKey)
			continue
		}
		_ = artifacts.Delete(ctx, legacyKey)
	}
}

// reportLegacyArtifactSweepCandidate prints a dry-run line for key only when
// it exists; a Has error counts as absent, so the report never overclaims.
func reportLegacyArtifactSweepCandidate(ctx context.Context, runtime *infra.Infra, artifacts cacheManager.ArtifactStore, key string) {
	has, err := artifacts.Has(ctx, key)
	if err != nil || !has {
		return
	}
	// No %q needed: url.QueryEscape percent-encodes control bytes, unlike the
	// raw directory names reportExtractedSweepPlan prints.
	runtime.Output.Printf("Would sweep legacy artifact %s", key)
}

// sweepExtractedStore drops extracted entries that no kept installed, role or
// fresh warmed snapshot record references. It trusts the snapshot, not a scan,
// and skips a snapshot with no recorded content, whose keep set would be empty.
func sweepExtractedStore(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	reachable map[string]bool,
	installedByKey map[string][]installedCollection,
	roles roleReachability,
) {
	if cfg == nil || cfg.CacheDir == "" || st == nil || !st.HasRecordedContent() {
		return
	}
	extractedStore := extracted.NewStore(cfg.CacheDir)
	if extractedStore == nil {
		return
	}
	keep := extractedKeepSet(st, reachable, installedByKey)
	maps.Copy(keep, roleKeepSHAs(st, roles.reachable, roles.byName))

	if cfg.DryRun {
		reportExtractedSweepPlan(runtime, extractedStore, keep)
		return
	}
	// Reported but not fatal: the layer is rebuildable, yet silence would hide
	// a containment refusal or a lost cache lock behind a successful run.
	if err := extractedStore.Sweep(ctx, keep); err != nil {
		runtime.Output.Errorf("Failed to sweep the extracted cache: %v", err)
	}
}

// extractedKeepSet returns the SHAs of installed records removeUnused keeps
// plus every fresh warmed SHA. The warmed half never consults wouldRemove:
// warm's intent is independent of install reachability.
func extractedKeepSet(
	st *store.Store,
	reachable map[string]bool,
	installedByKey map[string][]installedCollection,
) map[string]bool {
	wouldRemove := make(map[string]bool, len(installedByKey))
	for key := range installedByKey {
		if !reachable[key] {
			wouldRemove[key] = true
		}
	}

	shaByKey := st.InstalledArtifactSHAByKey()
	warmedByKey := st.WarmedArtifactSHAByKey()
	keep := make(map[string]bool, len(shaByKey)+len(warmedByKey))
	for key, sha := range shaByKey {
		if wouldRemove[key] {
			continue
		}
		keep[sha] = true
	}
	for _, sha := range warmedByKey {
		keep[sha] = true
	}
	return keep
}

// reportExtractedSweepPlan prints, without deleting anything, the extracted
// entries a real run would sweep given keep.
func reportExtractedSweepPlan(runtime *infra.Infra, extractedStore *extracted.Store, keep map[string]bool) {
	plan, err := extractedStore.SweepPlan(keep)
	if err != nil {
		runtime.Output.Errorf("Failed to plan extracted cache sweep: %v", err)
		return
	}
	for _, name := range plan {
		// name comes from a raw directory listing nothing validated, so a local
		// writer controls it; hence %q.
		runtime.Output.Printf("Would sweep extracted %q", name)
	}
}
