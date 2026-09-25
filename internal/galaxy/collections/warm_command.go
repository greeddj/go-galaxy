package collections

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/psvmcc/hub/pkg/types"
)

// Warm resolves dependencies and fills the artifact cache and the extracted
// store without writing any install path, so a baked CI image's later
// installs only hardlink.
func Warm(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	return runWarm(ctx, cfg, runtime)
}

// runWarm runs warmWithState through withBackend, which owns the lock and the
// lock-loss verdict, after refusing --no-cache.
func runWarm(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	// Warm's only output is cache state, so --no-cache is refused before the
	// backend is opened or locked: no lock, no network.
	if cfg.NoCache {
		return helpers.ErrWarmCacheDisabled
	}
	return withBackend(ctx, cfg, runtime, "Warming caches", warmWithState)
}

// warmWithState resolves, warms every collection and role, saves the
// snapshot and writes the metrics report, against a backend runWarm already
// opened and locked; it never releases or closes that backend itself.
func warmWithState(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, start time.Time) error {
	roots, roleRoots, err := loadRoots(cfg, runtime)
	if err != nil {
		return err
	}
	// A verification setup error is returned as is, not joined behind
	// helpers.ErrInstallationFailed, so it exits as the usage error it is.
	verify, err := newVerifyContext(cfg, runtime, roots)
	if err != nil {
		return err
	}
	resolved, graph, err := resolveOrLoadLockfile(ctx, cfg, runtime, state, roots, verify)
	if err != nil {
		return err
	}
	// Warm orders no install and discards the levels, but a graph install
	// would refuse fails here, before a role is resolved or a collection warmed.
	collections, _, err := planCollections(runtime, roots, resolved, graph)
	if err != nil {
		return err
	}
	roles, err := resolveOrLoadRoles(ctx, cfg, runtime, state, roleRoots)
	if err != nil {
		return err
	}
	counts := runCounts{Collections: len(collections), Roles: len(roles.roles)}

	if cfg.DryRun {
		return warmDryRun(ctx, cfg, runtime, state, collections, roles, start)
	}

	summary := warmCollections(ctx, cfg, runtime, state, collections, verify)
	if summary.count == 0 {
		summary = warmRoles(ctx, cfg, runtime, state, roles)
	}
	// finalizeInstall's tail contract: metrics are written even if the save
	// failed, and a warm failure stays the primary error, with the save error
	// joined by annotateSaveFailure so errors.Is still matches it.
	saveErr := state.backend.SaveStore(ctx, state.store)
	counts.Failures = int(summary.count)
	writeRunMetrics(cfg, runtime, "warm", start, counts, cfg.Frozen)
	if summary.count > 0 {
		return annotateSaveFailure(summary.warmError(), saveErr)
	}
	if saveErr != nil {
		return saveErr
	}
	runtime.Output.Okf("Warm complete: %s cached", counts.describe())
	return nil
}

// warmRoles caches every resolved role's artifact and extracted tree on the
// Workers-bounded pool, touching no roles tree. It runs only when every
// collection warmed, so a collection failure's summary stays undiluted.
func warmRoles(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, roles roleResolution) failureSummary {
	var failures failureRecorder
	depsCtx := newInstallDeps(cfg, runtime, state.store, state.backend.Artifacts(), state.extractStore, nil, nil, nil)
	depsCtx.collectionDeps = depsCtx.withSources(state.backend.Artifacts(), state.gitMemo, state.roleMemo, state.urlMemo)
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(cfg.Workers, 1))
	for _, name := range roles.order {
		if ctx.Err() != nil {
			break
		}
		role := roles.roles[name]
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := warmRole(ctx, depsCtx, role); err != nil {
				runtime.Output.ErrorVersionf(role.Version, fmt.Sprintf("error: %s", err),
					"Failed: role %s", role.Name)
				failures.recordRole(err)
			} else {
				runtime.Output.OkVersionf(role.Version, "Cached: role %s", role.Name)
			}
		})
	}
	wg.Wait()
	return failures.summary()
}

// warmRole caches one role's artifact and extracted tree, recording the
// warmed entry under a "role:" key so a role and a collection sharing a
// name@version never overwrite each other's record.
func warmRole(ctx context.Context, deps installDeps, r resolvedRole) error {
	artifact, err := fetchRoleArtifact(ctx, deps, r)
	if err != nil {
		return err
	}
	defer cleanupIfNeeded(artifact.Cleanup)
	sha, computed, err := resolveArtifactSHA(artifact.Path, nil, artifact.Meta, artifact.SHA, "")
	if err != nil {
		return err
	}
	if deps.extractStore == nil {
		return nil
	}
	if _, err := deps.extractStore.Ensure(ctx, sha, artifact.Path, shaProvenance(computed)); err != nil {
		return err
	}
	if deps.st != nil {
		deps.st.SetWarmed("role:"+r.key(), sha)
	}
	return nil
}

// warmDryRun replaces warmCollections and warmRoles on a dry run, so no mode
// flag reaches warmOne; it saves only a snapshot that already existed and
// wraps failures with warmError, so the preview exits per cause like a warm.
func warmDryRun(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	collections map[string]collection,
	roles roleResolution,
	start time.Time,
) error {
	warmed := state.store.WarmedArtifactSHAByKey()
	probe := warmDryRunProbe(cfg, state.backend.Artifacts(), state.extractStore, warmed)
	summary := classifyDryRun(ctx, runtime, cfg, collections, warmDryRunVerbs, probe)
	summary = summary.join(classifyRolesDryRun(ctx, runtime, cfg, roles, warmRolesDryRunVerbs,
		warmRoleDryRunProbe(state.backend.Artifacts(), state.extractStore, warmed)))
	saveErr := saveDryRunSnapshotIfPersisted(ctx, runtime, state)
	counts := runCounts{Collections: len(collections), Roles: len(roles.roles), Failures: int(summary.count)}
	writeRunMetrics(cfg, runtime, "warm", start, counts, cfg.Frozen)
	if summary.count > 0 {
		return annotateSaveFailure(summary.warmError(), saveErr)
	}
	return saveErr
}

func warmCollections(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	collections map[string]collection,
	verify *verifyContext,
) failureSummary {
	var failures failureRecorder
	// Install's prefetcher with a nil root and nil levels: it schedules on the
	// cache probe alone, in key order. The deferred Close joins its workers
	// before warmWithState returns, so no download outlives the backend lock.
	prefetchDeps := newPrefetchDeps(cfg, runtime, state.store, state.backend.Artifacts(), nil)
	prefetchDeps.collectionDeps = prefetchDeps.withSources(state.backend.Artifacts(), state.gitMemo, state.roleMemo, state.urlMemo)
	prefetch := startPrefetcher(ctx, prefetchDeps, collections, nil)
	defer prefetch.Close()
	// A nil root: warm never touches the collections tree, and
	// newInstallTarget's nil-root guard fails any accidental call closed.
	depsCtx := newInstallDeps(
		cfg, runtime, state.store, state.backend.Artifacts(), state.extractStore, nil, prefetch.cachedArtifacts(), verify,
	)
	depsCtx.collectionDeps = depsCtx.withSources(state.backend.Artifacts(), state.gitMemo, state.roleMemo, state.urlMemo)
	var wg sync.WaitGroup
	// A zero Workers would make sem unbuffered and deadlock the first send.
	sem := make(chan struct{}, max(cfg.Workers, 1))
	for _, col := range collections {
		// On cancel, stop dispatching but let started workers finish, and
		// still return the summary so the save and metrics tail runs; the exit
		// code comes from the caught signal, not from this return value.
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			meta, prefetched, ok, prefetchErr := prefetch.Wait(col.key())
			if ok && prefetchErr != nil {
				runtime.Output.Warnf("Prefetch failed for %s: %v", col.key(), prefetchErr)
			}
			if err := warmOne(ctx, depsCtx, col, meta, prefetched); err != nil {
				runtime.Output.ErrorVersionf(col.Version, fmt.Sprintf("error: %s", err),
					"Failed: %s.%s", col.Namespace, col.Name)
				failures.record(err)
			} else {
				runtime.Output.OkVersionf(col.Version, "Cached: %s.%s", col.Namespace, col.Name)
			}
		})
	}
	// Inline, not deferred: a deferred wait would let the return evaluate
	// failures.summary() before the workers finish, under-reporting failures.
	wg.Wait()
	return failures.summary()
}

// warmOne materializes col into the artifact cache and the extracted store.
// meta and prefetched are the prefetch handoff (nil and zero when there is
// none, in which case prepareWithRecovery fetches on its own).
func warmOne(
	ctx context.Context,
	deps installDeps,
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
	prefetched downloadResult,
) error {
	filename := helpers.ArtifactFilename(col.Namespace, col.Name, col.Version)
	payload, err := prepareWithRecovery(ctx, deps, col, meta, prefetched, filename, func(payload installPayload) error {
		return warmVerifyAndEnsure(ctx, deps, col, payload)
	})
	if err != nil {
		return err
	}
	defer cleanupIfNeeded(payload.artifact.Cleanup)
	recordWarmed(deps, col, payload.artifactSHA)
	return nil
}

// recordWarmed stamps col's warmed entry on every warm, cache hits included,
// so cleanup keeps its extracted tree for WarmedEntryMaxAge after the last
// warm. Install must never call it, or a tree would outlive its install.
func recordWarmed(deps installDeps, col collection, artifactSHA string) {
	if deps.st == nil || deps.extractStore == nil {
		return
	}
	deps.st.SetWarmed(col.key(), artifactSHA)
}

// warmVerifyAndEnsure checks col's pin, then its signatures, before the
// extracted store gets a tree a later install would hardlink from; a failure
// drives prepareWithRecovery's bounded evict-and-refetch-once.
func warmVerifyAndEnsure(ctx context.Context, deps installDeps, col collection, payload installPayload) error {
	if err := verifyPinnedSHA(col, payload.artifactSHA); err != nil {
		return err
	}
	if err := verifyCollectionSignatures(ctx, deps, col, payload); err != nil {
		return err
	}
	if deps.extractStore == nil {
		return nil
	}
	_, err := deps.extractStore.Ensure(ctx, payload.artifactSHA, payload.artifact.Path, shaProvenance(payload.artifactSHAComputed))
	return err
}
