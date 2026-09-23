package collections

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// Start installs collections according to the provided configuration.
func Start(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	return runInstall(ctx, cfg, runtime)
}

// runInstall drives install through withBackend, which owns the backend
// lifecycle and the lock-loss verdict, with installWithState as the work.
func runInstall(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	return withBackend(ctx, cfg, runtime, "Starting installation process", installWithState)
}

// installWithState builds the plan, installs every level, saves the snapshot
// and then writes the metrics report whether or not the save succeeded. It
// runs under runInstall's open, locked backend and never releases either.
func installWithState(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, start time.Time) error {
	// One root for the prefetcher and every install worker, so a symlinked
	// ansible_collections fails the run once rather than once per collection;
	// a dry run must not create the directory it only describes.
	root, err := openCollectionsRoot(cfg.DownloadPath, !cfg.DryRun)
	if err != nil {
		return err
	}
	defer func() {
		if root != nil {
			_ = root.Close()
		}
	}()

	plan, err := prepareInstallPlan(ctx, cfg, runtime, state, root)
	if err != nil {
		return err
	}
	// Deferred in this callee so every prefetch worker is joined before
	// runInstall's own defers release the lock: a late worker can never commit
	// to the cache unlocked. On a dry run the prefetcher never started.
	defer plan.prefetch.Close()

	// Opened only when the plan holds a role, so a collections-only project
	// never grows an empty roles directory; no prefetcher reads it, so it can
	// wait for the plan. A dry run never creates it.
	rolesRoot, err := openRolesRootIfNeeded(cfg, plan)
	if err != nil {
		return err
	}
	defer func() {
		if rolesRoot != nil {
			_ = rolesRoot.Close()
		}
	}()

	if cfg.DryRun {
		return installDryRun(ctx, cfg, runtime, state, plan, start, root, rolesRoot)
	}

	depsCtx := newInstallDeps(
		cfg, runtime, state.store, state.backend.Artifacts(), state.extractStore, root, plan.prefetch.cachedArtifacts(), plan.verify,
	)
	depsCtx.collectionDeps = depsCtx.withSources(state.backend.Artifacts(), state.gitMemo, state.roleMemo, state.urlMemo)
	depsCtx.rolesRoot = rolesRoot
	summary, err := installLevels(ctx, depsCtx, plan)
	if err != nil {
		return err
	}
	// Roles install only when every collection level succeeded: a failed run
	// must not report unattempted roles as failed or install them against a
	// half-done tree.
	if summary.count == 0 {
		var roleFailures failureRecorder
		installRoles(ctx, depsCtx, plan.roles, &roleFailures)
		summary = summary.join(roleFailures.summary())
	}

	// Reported after every worker has joined and before the run's own tail, so
	// the count is complete and the line sits with the other result-tier lines
	// rather than among the per-collection ones.
	plan.verify.reportSkippedUnverified(runtime)

	finalErr := finalizeInstall(ctx, runtime, state.backend, state.store, summary, start)
	writeRunMetrics(cfg, runtime, "install", start, plan.counts(int(summary.count)), cfg.Frozen)
	return finalErr
}

// openRolesRootIfNeeded opens the roles root when the plan holds a role and
// returns nil otherwise.
func openRolesRootIfNeeded(cfg *config.Config, plan *installPlan) (*os.Root, error) {
	if len(plan.roles.order) == 0 {
		return nil, nil //nolint:nilnil // no roles in the plan is "no roles tree to open", not a failure.
	}
	return openRolesRoot(cfg.RolesPath, !cfg.DryRun)
}

// counts is the plan's size with the run's failure count, for the report.
func (p *installPlan) counts(failures int) runCounts {
	return runCounts{Collections: len(p.collections), Roles: len(p.roles.roles), Failures: failures}
}

// installDryRun reports what install would do; its error reuses installError
// so the preview exits per cause as a real run would. It saves the snapshot
// only if one existed, so cleanup never reads a fresh empty one as evidence.
func installDryRun(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	plan *installPlan,
	start time.Time,
	root *os.Root,
	rolesRoot *os.Root,
) error {
	summary := classifyDryRun(
		ctx, runtime, cfg, plan.collections, installDryRunVerbs, installDryRunProbe(cfg, state.store, state.backend.Artifacts(), root),
	)
	summary = summary.join(classifyRolesDryRun(ctx, runtime, cfg, plan.roles, installRolesDryRunVerbs,
		installRoleDryRunProbe(cfg, state.store, state.backend.Artifacts(), rolesRoot)))
	saveErr := saveDryRunSnapshotIfPersisted(ctx, runtime, state)
	writeRunMetrics(cfg, runtime, "install", start, plan.counts(int(summary.count)), cfg.Frozen)
	if summary.count > 0 {
		return annotateSaveFailure(summary.installError(), saveErr)
	}
	return saveErr
}

func installLevels(ctx context.Context, depsCtx installDeps, plan *installPlan) (failureSummary, error) {
	var failures failureRecorder
	for _, level := range plan.levels {
		if err := runInstallLevel(ctx, depsCtx, plan.collections, plan.graph, level, plan.prefetch, &failures); err != nil {
			// ErrMissingCollection is a usage-class plan bug: returned without
			// the causes earlier workers recorded, so it never misclassifies as
			// an install or integrity failure.
			return failures.summary(), err
		}
		if failures.count() > 0 {
			break
		}
	}
	return failures.summary(), nil
}

// runInstallLevel installs one level's keys on a Workers-bounded pool. wg.Wait
// is deferred ahead of the loop so every exit, the ErrMissingCollection guard
// included, joins dispatched workers before runInstall releases the lock.
func runInstallLevel(
	ctx context.Context,
	depsCtx installDeps,
	collections map[string]collection,
	graph map[string][]string,
	level []string,
	prefetch *prefetcher,
	failures *failureRecorder,
) error {
	var wg sync.WaitGroup
	// max(depsCtx.cfg.Workers, 1): see warmCollections's identical guard - a
	// zero Workers would make sem unbuffered and deadlock the first send.
	sem := make(chan struct{}, max(depsCtx.cfg.Workers, 1))
	defer wg.Wait()

	for _, key := range level {
		// On cancellation stop dispatching but return nil, not ctx.Err():
		// started workers finish, finalizeInstall still saves the snapshot, and
		// the exit code comes from the caught signal in main's handleResult.
		if ctx.Err() != nil {
			break
		}
		col, ok := collections[key]
		if !ok {
			return fmt.Errorf("%w for: %s", helpers.ErrMissingCollection, key)
		}
		depKeys := graph[key]
		if depKeys == nil {
			depKeys = []string{}
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			meta, prefetched, ok, prefetchErr := prefetch.Wait(col.key())
			if ok && prefetchErr != nil {
				depsCtx.runtime.Output.Warnf("Prefetch failed for %s: %v", col.key(), prefetchErr)
			}
			if err := installCollection(ctx, col, depsCtx, depKeys, meta, prefetched); err != nil {
				depsCtx.runtime.Output.ErrorVersionf(col.Version, fmt.Sprintf("error: %s", err),
					"Failed: %s.%s", col.Namespace, col.Name)
				failures.record(err)
			} else {
				depsCtx.runtime.Output.OkVersionf(col.Version, "Installed: %s.%s", col.Namespace, col.Name)
			}
		})
	}
	return nil
}

// finalizeInstall saves the snapshot and reports the outcome. A failed save is
// annotated onto the install error rather than replacing it, so a full disk in
// the tail never changes the exit class the collection failures decide.
func finalizeInstall(
	ctx context.Context,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
	summary failureSummary,
	start time.Time,
) error {
	saveStart := time.Now()
	saveErr := backend.SaveStore(ctx, st)
	if saveErr == nil {
		runtime.Output.DebugSincef(saveStart, "%s", "Save snapshot")
	}
	if summary.count > 0 {
		runtime.Output.PersistentPrintf("Completed with errors: %d failed. Took %s", summary.count, time.Since(start).Round(time.Second))
		return annotateSaveFailure(summary.installError(), saveErr)
	}
	if saveErr != nil {
		return saveErr
	}
	runtime.Output.Okf("All done. Took %s", time.Since(start).Round(time.Second))
	return nil
}
