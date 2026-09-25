package collections

import (
	"context"
	"fmt"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// Lock resolves dependencies and writes a lockfile to disk. It is intended
// for the `lock` command and never installs anything; it does mutate the
// snapshot cache so that subsequent installs benefit from the work done.
func Lock(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	return runLock(ctx, cfg, runtime)
}

// runLock runs lockWithState under withBackend's lifecycle and lock-loss
// verdict. The banner names only resolving, the step every mode shares: a
// plain run then writes the file, while --check only compares against it.
func runLock(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	return withBackend(ctx, cfg, runtime, "Resolving for lockfile", lockWithState)
}

// lockWithState resolves requirements, then writes, previews or gates the
// lockfile, on a backend runLock already opened and locked. cfg.Check is
// checked first so the stricter lockCheck verdict also covers --dry-run.
func lockWithState(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, start time.Time) error {
	roots, roleRoots, err := loadRoots(cfg, runtime)
	if err != nil {
		return err
	}
	deps := state.resolveDeps(cfg, runtime)
	resolved, graph, err := resolveCollectionsInternal(ctx, deps, roots, resolveTopLevel)
	if err != nil {
		return fmt.Errorf("failed to resolve dependencies: %w", err)
	}
	roles, err := resolveRoles(ctx, deps, roleRoots)
	if err != nil {
		return err
	}
	lf, err := buildLockfile(ctx, deps, resolved, graph, roles)
	if err != nil {
		return err
	}
	path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	if cfg.Check {
		return lockCheck(ctx, cfg, runtime, state, lf, path, start)
	}
	if cfg.DryRun {
		return lockDryRun(ctx, cfg, runtime, state, lf, path, start)
	}
	if err := lockfile.Save(path, lf); err != nil {
		return err
	}
	// Announced before the snapshot save: the file stays valid if that save
	// fails, which only costs the next run a cold cache and exits nonzero.
	runtime.Output.Okf("Lockfile written to %s (%s)", path, lockCounts(lf).describe())
	saveErr := state.backend.SaveStore(ctx, state.store)
	// Metrics follow the save so the duration includes it. Failures stays a
	// truthful zero, and frozen is absent: lock registers no --frozen.
	writeRunMetrics(cfg, runtime, "lock", start, lockCounts(lf), false)
	return saveErr
}

// lockDryRun reports how lf differs from the lockfile at path instead of
// writing it, so no "Lockfile written" line is printed. The snapshot still
// saves through saveLockSnapshot, and writeRunMetrics self-suppresses.
func lockDryRun(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	lf *lockfile.File,
	path string,
	start time.Time,
) error {
	reportLockfileDiff(runtime, lf, path, dryRunDiffPrefix, lockfile.Compare(lockDryRunBaseline(runtime, path), lf))
	saveErr := saveLockSnapshot(ctx, cfg, runtime, state)
	writeRunMetrics(cfg, runtime, "lock", start, lockCounts(lf), false)
	return saveErr
}

// lockCheck fails with helpers.ErrLockfileDrift when lf differs from the
// lockfile at path, instead of overwriting it. A missing or unloadable file
// fails with its own sentinel, never as the drift Compare(nil, lf) would show.
func lockCheck(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	lf *lockfile.File,
	path string,
	start time.Time,
) error {
	existing, err := lockfile.LoadRequired(path)
	if err != nil {
		return err
	}

	diff := lockfile.Compare(existing, lf)
	reportLockfileDiff(runtime, lf, path, checkDiffPrefix, diff)
	saveErr := saveLockSnapshot(ctx, cfg, runtime, state)
	writeRunMetrics(cfg, runtime, "lock", start, lockCounts(lf), false)
	if !diff.Empty() {
		return annotateSaveFailure(
			fmt.Errorf("%w: %s: run `go-galaxy lock` to update it", helpers.ErrLockfileDrift, path),
			saveErr,
		)
	}
	return saveErr
}

// saveLockSnapshot is the one save rule lockDryRun and lockCheck share, so
// lockCheck under --dry-run saves exactly as lockDryRun does: through
// saveDryRunSnapshotIfPersisted under cfg.DryRun, a plain SaveStore otherwise.
func saveLockSnapshot(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState) error {
	if cfg.DryRun {
		return saveDryRunSnapshotIfPersisted(ctx, runtime, state)
	}
	return state.backend.SaveStore(ctx, state.store)
}

// lockCounts is the lockfile's size for the report and the announcement.
func lockCounts(lf *lockfile.File) runCounts {
	return runCounts{Collections: len(lf.Collections), Roles: len(lf.Roles)}
}
