package collections

import (
	"context"
	"fmt"
	"os"
	"sync"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// installRolesDryRunVerbs is the wording `install --dry-run` reports roles
// with: the collection verbs with the kind named, for a mixed report.
//
//nolint:gochecknoglobals // a fixed, immutable wording table, not mutable shared state.
var installRolesDryRunVerbs = dryRunVerbs{
	settled:        "Up to date (role)",
	action:         "Would install (role)",
	summaryAction:  "roles would install",
	summarySettled: "roles already up to date",
}

// warmRolesDryRunVerbs is the wording `warm --dry-run` reports roles with.
//
//nolint:gochecknoglobals // a fixed, immutable wording table, not mutable shared state.
var warmRolesDryRunVerbs = dryRunVerbs{
	settled:        "Already warm (role)",
	action:         "Would warm (role)",
	summaryAction:  "roles would warm",
	summarySettled: "roles already warm",
}

// roleDryRunProbe answers one role's dry-run verdict without mutating
// anything; see dryRunProbe.
type roleDryRunProbe func(ctx context.Context, r resolvedRole) dryRunClassification

// classifyRolesDryRun is classifyDryRun for the resolved roles, reported in
// discovery order; it prints its summary line only when the run has roles,
// so a collections-only dry run is unchanged.
func classifyRolesDryRun(
	ctx context.Context,
	runtime *infra.Infra,
	cfg *config.Config,
	roles roleResolution,
	verbs dryRunVerbs,
	probe roleDryRunProbe,
) failureSummary {
	if len(roles.order) == 0 {
		return failureSummary{}
	}
	keys := make([]string, 0, len(roles.order))
	results := make([]dryRunClassification, len(roles.order))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(cfg.Workers, 1))
	for i, name := range roles.order {
		role := roles.roles[name]
		keys = append(keys, role.key())
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = probe(ctx, role)
		})
	}
	wg.Wait()

	var failures failureRecorder
	wouldAct, settled := reportDryRunResults(runtime, verbs, keys, results, cfg, &failures)
	summary := failures.summary()
	runtime.Output.PersistentPrintf(
		"Dry run: %d %s, %d %s, %d would fail",
		wouldAct, verbs.summaryAction, settled, verbs.summarySettled, summary.count,
	)
	return summary
}

// installRoleDryRunProbe returns install's dry-run verdict for a role: settled
// when record and marker agree, else cached or not, refusing a foreign
// directory or unsafe name exactly as a real install would before fetching.
func installRoleDryRunProbe(cfg *config.Config, st *store.Store, artifacts cacheManager.ArtifactStore, root *os.Root) roleDryRunProbe {
	return func(ctx context.Context, r resolvedRole) dryRunClassification {
		target, ok := newRoleTarget(root, cfg, r)
		if ok {
			if entry, matched := roleRecordMatches(target, r, st); matched && checkExtractMarker(target, entry.ArtifactSHA256).matches() {
				return dryRunClassification{settled: true}
			}
		}
		cached := roleArtifactPresent(ctx, cfg, artifacts, r)
		if !ok {
			if root == nil {
				return dryRunClassification{cached: cached}
			}
			return dryRunClassification{cached: cached, fail: fmt.Errorf("%w: role name %q", helpers.ErrUnsafeCollectionIdentifier, r.Name)}
		}
		if _, err := checkRoleDirectoryOwned(target); err != nil {
			return dryRunClassification{cached: cached, fail: err}
		}
		return dryRunClassification{cached: cached}
	}
}

// warmRoleDryRunProbe returns warm's dry-run verdict for a role: settled only
// when both halves of warm's product exist - the artifact in the store and
// its tree in the extracted store under the sha the warmed set recorded.
func warmRoleDryRunProbe(artifacts cacheManager.ArtifactStore, extractStore *extracted.Store, warmed map[string]string) roleDryRunProbe {
	return func(ctx context.Context, r resolvedRole) dryRunClassification {
		cached := artifacts != nil && roleArtifactPresent(ctx, nil, artifacts, r)
		sha, ok := warmed["role:"+r.key()]
		if cached && ok && extractStore != nil && extractStore.Ready(sha) {
			return dryRunClassification{settled: true}
		}
		return dryRunClassification{cached: cached}
	}
}

// roleArtifactPresent asks the artifact store whether the role's artifact is
// cached; a --no-cache run and a missing store both read as not cached.
func roleArtifactPresent(ctx context.Context, cfg *config.Config, artifacts cacheManager.ArtifactStore, r resolvedRole) bool {
	if artifacts == nil || (cfg != nil && cfg.NoCache) {
		return false
	}
	ok, err := artifacts.Has(ctx, roleArtifactKey(r))
	return err == nil && ok
}
