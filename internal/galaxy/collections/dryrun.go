package collections

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"sync"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// dryRunBanner announces --dry-run once per run through Warnf (stderr, survives
// --quiet), since an ambient GO_GALAXY_DRY_RUN must never silently no-op a job.
// It names what still happens: discovery's fetches and saveDryRunSnapshotIfPersisted.
func dryRunBanner(runtime *infra.Infra) {
	runtime.Output.Warnf(
		"--dry-run is active: no Galaxy artifact will be downloaded and no artifact installed or cached; " +
			"a role, git source or url source with no usable recorded pin is still fetched to learn its identity, then discarded; " +
			"the resolved metadata caches are still saved",
	)
}

// dryRunClassification is one collection's dry-run verdict, computed by the
// parallel probe pass of classifyDryRun and rendered by reportDryRunResults in
// real-run order: settled, then uncached under --offline, then fail.
type dryRunClassification struct {
	// fail, when non-nil, refuses the collection: a lockfile pin its cached
	// recorded digest contradicts under --offline (dryRunPinVerdict), or a
	// collections-tree write a real install would refuse.
	fail error
	// settled means the command's product already exists: for install a
	// matching install record and extract marker, for warm a cached artifact
	// whose extracted tree is ready under a known sha.
	settled bool
	cached  bool
}

// dryRunProbe answers one collection's dry-run verdict without mutating
// anything; each command closes over exactly the state its verdict needs.
type dryRunProbe func(ctx context.Context, col collection) dryRunClassification

// dryRunVerbs holds one command's dry-run report wording, so the report pass
// stays command-agnostic. Fields are plain phrases, not format strings.
type dryRunVerbs struct {
	// settled is the tier-report verb for a collection whose product already
	// exists (install: "Up to date"; warm: "Already warm").
	settled string
	// action is the tier-report verb for a collection that would still need
	// work (install: "Would install"; warm: "Would warm").
	action string
	// summaryAction is the lowercase noun phrase used in the trailing summary
	// line's would-act count (install: "would install"; warm: "would warm").
	summaryAction string
	// summarySettled is the lowercase noun phrase used in the trailing
	// summary line's settled count (install: "already up to date"; warm:
	// "already warm").
	summarySettled string
}

// installDryRunVerbs is the wording `install --dry-run` reports with.
//
//nolint:gochecknoglobals // a fixed, immutable wording table, not mutable shared state.
var installDryRunVerbs = dryRunVerbs{
	settled:        "Up to date",
	action:         "Would install",
	summaryAction:  "would install",
	summarySettled: "already up to date",
}

// warmDryRunVerbs is the wording `warm --dry-run` reports with.
//
//nolint:gochecknoglobals // a fixed, immutable wording table, not mutable shared state.
var warmDryRunVerbs = dryRunVerbs{
	settled:        "Already warm",
	action:         "Would warm",
	summaryAction:  "would warm",
	summarySettled: "already warm",
}

// classifyDryRun reports, without mutating anything, what a command would do
// for each collection as judged by probe, and returns every would-fail cause
// so the preview exits with the code a real run hitting that cause would.
func classifyDryRun(
	ctx context.Context,
	runtime *infra.Infra,
	cfg *config.Config,
	collections map[string]collection,
	verbs dryRunVerbs,
	probe dryRunProbe,
) failureSummary {
	keys := slices.Sorted(maps.Keys(collections))

	// A probe can cost an S3 round trip and an extract-marker tree walk, so
	// probes run in parallel bounded by cfg.Workers; each result lands in its
	// key's index slot, so the report below stays in sorted key order.
	results := make([]dryRunClassification, len(keys))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(cfg.Workers, 1))
	for i, key := range keys {
		col := collections[key]
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = probe(ctx, col)
		})
	}
	wg.Wait()
	warnIfFrozenOffline(runtime, cfg)

	var failures failureRecorder
	wouldAct, settled := reportDryRunResults(runtime, verbs, keys, results, cfg, failures.record)
	summary := failures.summary()
	runtime.Output.PersistentPrintf(
		"Dry run: %d %s, %d %s, %d would fail",
		wouldAct, verbs.summaryAction, settled, verbs.summarySettled, summary.count,
	)
	return summary
}

// reportDryRunResults prints each verdict in sorted key order, passing each
// would-fail cause to record. Case order mirrors a real run: settled skips
// first, then the offline guard, and only then a pin or tree failure.
func reportDryRunResults(
	runtime *infra.Infra,
	verbs dryRunVerbs,
	keys []string,
	results []dryRunClassification,
	cfg *config.Config,
	record func(error),
) (int, int) {
	var wouldAct, settled int
	for i, key := range keys {
		res := results[i]
		switch {
		case res.settled:
			settled++
			runtime.Output.PersistentPrintf("%s: %s", verbs.settled, key)
		case !res.cached && cfg.Offline:
			record(fmt.Errorf("%s: %w: artifact not in cache", key, helpers.ErrOfflineMode))
			runtime.Output.Errorf("Would fail: %s (not cached and --offline forbids downloading)", key)
		case res.fail != nil:
			record(fmt.Errorf("%s: %w", key, res.fail))
			runtime.Output.Errorf("Would fail: %s (%v)", key, res.fail)
		case res.cached:
			wouldAct++
			runtime.Output.Okf("%s: %s (artifact cached)", verbs.action, key)
		default:
			wouldAct++
			runtime.Output.Okf("%s: %s (would download)", verbs.action, key)
		}
	}
	return wouldAct, settled
}

// warnIfFrozenOffline discloses under --frozen --offline that drifted bytes
// behind an unchanged recorded digest go undetected, since re-hashing costs a
// full S3 download. Tests count the banner, so it avoids "--dry-run is active".
func warnIfFrozenOffline(runtime *infra.Infra, cfg *config.Config) {
	if !cfg.Frozen || !cfg.Offline {
		return
	}
	runtime.Output.Warnf(
		"--frozen --offline: this preview checks the cached artifact's recorded digest against the pin, not its actual bytes; " +
			"bytes that drifted without updating that recorded digest will still fail the real run, which cannot refetch while offline",
	)
}

// installDryRunProbe returns install's read-only probe, checking settled
// (checkExtractMarker, never the marker-deleting canSkipInstall), the pin, then
// the namespace write; a nil root (no download path yet) is no tree failure.
func installDryRunProbe(cfg *config.Config, st *store.Store, artifacts cacheManager.ArtifactStore, root *os.Root) dryRunProbe {
	return func(ctx context.Context, col collection) dryRunClassification {
		target, ok := newInstallTarget(root, cfg, col)
		if ok && installRecordMatches(target, col, st) {
			// installRecordMatches established the entry exists; only its
			// extract marker still matching is in question.
			if entry, entryOK := st.GetInstalled(col.key()); entryOK && checkExtractMarker(target, entry.ArtifactSHA256).matches() {
				return dryRunClassification{settled: true}
			}
		}

		meta, cached := dryRunArtifactMeta(ctx, cfg, artifacts, col)
		if err := dryRunPinVerdict(cfg, cached, meta, col); err != nil {
			return dryRunClassification{cached: cached, fail: err}
		}
		if !ok {
			if root == nil {
				return dryRunClassification{cached: cached}
			}
			return dryRunClassification{cached: cached, fail: fmt.Errorf(
				"%w: ns=%q name=%q version=%q", helpers.ErrUnsafeCollectionIdentifier, col.Namespace, col.Name, col.Version,
			)}
		}
		if err := dryRunNamespaceProbe(target); err != nil {
			return dryRunClassification{cached: cached, fail: err}
		}
		return dryRunClassification{cached: cached}
	}
}

// warmDryRunProbe returns warm's probe: settled needs both the cached artifact
// and its extracted tree, since an S3 artifact store and the local extracted
// store are independent. The pin check precedes Ready, as in warmVerifyAndEnsure.
func warmDryRunProbe(
	cfg *config.Config, artifacts cacheManager.ArtifactStore, extractStore *extracted.Store, warmed map[string]string,
) dryRunProbe {
	return func(ctx context.Context, col collection) dryRunClassification {
		meta, cached := dryRunArtifactMeta(ctx, cfg, artifacts, col)
		if !cached {
			return dryRunClassification{}
		}
		if err := dryRunPinVerdict(cfg, cached, meta, col); err != nil {
			return dryRunClassification{cached: true, fail: err}
		}
		return dryRunClassification{cached: true, settled: extractStore.Ready(warmDryRunSHA(col, warmed))}
	}
}

// warmDryRunSHA names the sha to check the extracted store under: the lockfile
// pin, which a real run must end at, else the snapshot's warmed record. It
// never fetches the artifact to learn one, which on S3 is a full download.
func warmDryRunSHA(col collection, warmed map[string]string) string {
	if sha := strings.TrimSpace(col.SHA256); sha != "" {
		return sha
	}
	return warmed[col.key()]
}

// dryRunArtifactMeta reports col's cached metadata and whether a real run would
// hit the cache; it must mirror isCacheHit. One Meta call, whose found must
// equal Has for the key, costs the same single S3 HEAD as a presence check.
func dryRunArtifactMeta(
	ctx context.Context, cfg *config.Config, artifacts cacheManager.ArtifactStore, col collection,
) (map[string]string, bool) {
	if cfg.NoCache || artifacts == nil {
		return nil, false
	}
	meta, found, err := artifacts.Meta(ctx, artifactKey(col))
	if err != nil || !found {
		return nil, false
	}
	return meta, true
}

// dryRunPinVerdict refuses a cached artifact whose well-formed recorded digest
// contradicts the pin under --offline. A refusal, not a prediction: under a pin
// a real local-backend run re-hashes the bytes and can still succeed.
func dryRunPinVerdict(cfg *config.Config, cached bool, meta map[string]string, col collection) error {
	pin := strings.TrimSpace(col.SHA256)
	if pin == "" {
		return nil
	}
	if !cached {
		return nil
	}
	recorded := strings.TrimSpace(meta["sha256"])
	if recorded == "" {
		return nil
	}
	if !helpers.IsSHA256Hex(recorded) {
		return nil
	}
	if recorded == pin {
		return nil
	}
	if !cfg.Offline {
		return nil
	}
	return fmt.Errorf(
		"%w: the cached artifact's recorded digest does not match the lockfile pin and --offline forbids refetching",
		helpers.ErrSHA256Mismatch,
	)
}

// dryRunNamespaceProbe checks, read-only, that extraction could write under
// target's namespace directory. Stat, not Lstat (unlike ansible_collections):
// a real install's RemoveAll-then-MkdirAll succeeds through a dangling symlink.
func dryRunNamespaceProbe(target installTarget) error {
	nsRel := path.Dir(target.rel)
	info, err := target.root.Stat(nsRel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return classifyCollectionsRootError(target.root, nsRel, errCollectionsTreeNotUsable)
	}
	if !info.IsDir() {
		return classifyCollectionsRootError(target.root, nsRel, errCollectionsTreeNotUsable)
	}
	return nil
}
