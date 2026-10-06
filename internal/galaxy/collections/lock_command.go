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

// lockWithState resolves requirements, keeping the pins of the lockfile it
// reads once up front, then writes, previews or gates the lockfile. cfg.Check
// is checked first so the stricter lockCheck verdict also covers --dry-run.
func lockWithState(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, start time.Time) error {
	roots, roleRoots, err := loadRootsAndRecordProject(ctx, cfg, runtime, state.backend, false)
	if err != nil {
		return err
	}
	existing := readExistingLockfile(cfg, runtime)
	deps := state.resolveDeps(cfg, runtime).withLockPreferences(newLockPreferences(cfg, existing.file))
	resolved, graph, err := resolveLockCollections(ctx, deps, roots, resolveTopLevel)
	if err != nil {
		return err
	}
	roles, err := resolveRoles(ctx, deps, roleRoots)
	if err != nil {
		return err
	}
	lf, err := buildLockfile(ctx, deps, resolved, graph, roles)
	if deps.lockPrefs.keptVersionGone(resolved, err) {
		// Only a replay keeps a version without asking its server; a solve
		// asks, so it resolves the pin anew and warns about it.
		if resolved, graph, err = resolveLockCollections(ctx, deps, roots, resolveTopLevelAnew); err != nil {
			return err
		}
		lf, err = buildLockfile(ctx, deps, resolved, graph, roles)
	}
	if err != nil {
		return err
	}
	if cfg.Check {
		return lockCheck(ctx, cfg, runtime, state, lf, existing, start)
	}
	if cfg.DryRun {
		return lockDryRun(ctx, cfg, runtime, state, lf, existing, start)
	}
	if err := lockfile.Save(existing.path, lf); err != nil {
		return err
	}
	// Announced before the snapshot save: the file stays valid if that save
	// fails, which only costs the next run a cold cache and exits nonzero.
	runtime.Output.Okf("Lockfile written to %s (%s)", existing.path, lockCounts(lf).describe())
	saveErr := state.backend.SaveStore(ctx, state.store)
	// Metrics follow the save so the duration includes it. Failures stays a
	// truthful zero, and frozen is absent: lock registers no --frozen.
	writeRunMetrics(cfg, runtime, "lock", start, lockCounts(lf), false)
	return saveErr
}

// resolveLockCollections is lock's collection resolve under mode; once the
// result is known it warns about each kept pin its server no longer publishes.
func resolveLockCollections(
	ctx context.Context, deps collectionDeps, roots []collection, mode resolveMode,
) (map[string]collection, map[string][]string, error) {
	resolved, graph, err := resolveCollectionsInternal(ctx, deps, roots, mode)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve dependencies: %w", err)
	}
	deps.lockPrefs.warnUnpublished(deps.runtime.Output, resolved)
	return resolved, graph, nil
}

// existingLockfile is the lockfile a lock run reads once, before it resolves:
// the pins it keeps, the --dry-run baseline and the --check verdict. file is
// nil when the read failed, and err tells absence from a file not loadable.
type existingLockfile struct {
	file *lockfile.File
	err  error
	path string
}

// readExistingLockfile reads the lockfile at lock's path. One that exists but
// does not load warns, since the run resolves without its pins and a plain
// lock then replaces it; --check stays silent and fails on it after resolving.
func readExistingLockfile(cfg *config.Config, runtime *infra.Infra) existingLockfile {
	path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, err := lockfile.Load(path)
	if err != nil && !lockfile.IsNotExist(err) && !cfg.Check {
		if cfg.DryRun {
			runtime.Output.Warnf("Existing lockfile %s cannot be read (%v); resolving without its pins "+
				"and reporting every entry as added", path, err)
		} else {
			runtime.Output.Warnf("Existing lockfile %s cannot be read (%v); resolving without its pins", path, err)
		}
	}
	return existingLockfile{file: lf, err: err, path: path}
}

// lockDryRun reports how lf differs from the existing lockfile instead of
// writing it, so no "Lockfile written" line is printed. The snapshot still
// saves through saveLockSnapshot, and writeRunMetrics self-suppresses.
func lockDryRun(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	lf *lockfile.File,
	existing existingLockfile,
	start time.Time,
) error {
	// A file absent or not loadable is a nil baseline, so every entry reads as
	// added; readExistingLockfile already warned about one not loadable.
	reportLockfileDiff(runtime, lf, existing.path, dryRunDiffPrefix, lockfile.Compare(existing.file, lf))
	saveErr := saveLockSnapshot(ctx, cfg, runtime, state)
	writeRunMetrics(cfg, runtime, "lock", start, lockCounts(lf), false)
	return saveErr
}

// lockCheck fails with helpers.ErrLockfileDrift when lf differs from the
// existing lockfile, instead of overwriting it. A missing or unloadable file
// fails as LoadRequired fails, never as the drift Compare(nil, lf) would show.
func lockCheck(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	lf *lockfile.File,
	existing existingLockfile,
	start time.Time,
) error {
	if existing.err != nil {
		return lockfile.RequiredError(existing.path, existing.err)
	}

	diff := lockfile.Compare(existing.file, lf)
	reportLockfileDiff(runtime, lf, existing.path, checkDiffPrefix, diff)
	saveErr := saveLockSnapshot(ctx, cfg, runtime, state)
	writeRunMetrics(cfg, runtime, "lock", start, lockCounts(lf), false)
	if !diff.Empty() {
		return annotateSaveFailure(
			fmt.Errorf("%w: %s: run `go-galaxy lock` to update it", helpers.ErrLockfileDrift, existing.path),
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
