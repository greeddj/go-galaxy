package collections

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// errNoHighestVersion reports a root metadata document that named no
// highest_version. It has no sentinel of its own: it classifies like every
// other lookup failure, through helpers.ErrLatestVersionLookupFailed.
var errNoHighestVersion = errors.New("no highest_version in metadata")

// outdatedEntry summarizes one entry's current-vs-latest delta. Err is nil on
// success, newer or not, and non-nil when the lookup itself failed: one error
// value rather than a message plus a flag, which could disagree.
type outdatedEntry struct {
	Err    error
	Name   string
	Locked string
	Latest string
	Newer  bool
	// Role marks an entry of the roles list, whose failure the headline
	// counts apart from the collections'.
	Role bool
}

// Outdated compares what the project runs (the lockfile, else the installed
// tree) with each server's latest version in parallel and prints a report. It
// opens no cache backend: no lock is taken and every answer is live, not cached.
func Outdated(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	if cfg == nil {
		// Defensive: runCollectionCommand never passes a nil config.
		return helpers.ErrConfigIsNil
	}
	if cfg.Offline {
		return fmt.Errorf("%w: outdated requires network access", helpers.ErrOfflineMode)
	}
	// Before loading the lockfile, not after: this disclosure is about the
	// run's own configuration and must not depend on whether the lockfile
	// happens to load.
	warnUnhonoredFlags(runtime, cfg)

	start := time.Now()
	src, err := outdatedInput(cfg, runtime)
	if err != nil {
		return err
	}

	results := queryLatestVersions(ctx, cfg, runtime, src.collections)
	results = append(results, queryLatestRoleVersions(ctx, newCollectionDeps(cfg, runtime, nil), src.roles)...)
	// Sorted once, here, before both the report and the error build below, so
	// the report's line order and the joined-cause order in the returned
	// error agree with each other. reportOutdated itself mutates nothing.
	slices.SortFunc(results, func(a, b outdatedEntry) int { return strings.Compare(a.Name, b.Name) })

	reportOutdated(runtime, results, src.label, cfg.Verbose)

	var failures failureRecorder
	for _, r := range results {
		switch {
		case r.Err == nil:
		case r.Role:
			failures.recordRole(r.Err)
		default:
			failures.record(r.Err)
		}
	}
	summary := failures.summary()
	// The report counts the lookups performed and failed, never how many
	// entries are behind. frozen is a literal false because outdated never
	// honors --frozen (see warnUnhonoredFlags).
	writeRunMetrics(cfg, runtime, "outdated", start,
		runCounts{Collections: len(src.collections), Roles: len(src.roles), Failures: int(summary.count)}, false)
	return summary.outdatedError()
}

// outdatedSource is one run's current side: the entries to compare and the
// label the summary line names as their origin, a lockfile or a tree path.
type outdatedSource struct {
	label       string
	collections []lockfile.Entry
	roles       []lockfile.RoleEntry
}

// outdatedInput reads the current side from the lockfile, falling back to the
// installed collections tree only when the lockfile is absent. A lockfile that
// exists and fails to load is returned as is: reading the tree would hide it.
func outdatedInput(cfg *config.Config, runtime *infra.Infra) (outdatedSource, error) {
	lockPath := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, err := lockfile.Load(lockPath)
	switch {
	case err == nil:
		return outdatedSource{label: lockPath, collections: lf.Collections, roles: lf.Roles}, nil
	case !lockfile.IsNotExist(err):
		return outdatedSource{}, err
	}

	scan, scanErr := scanInstalledTree(cfg, runtime)
	if scanErr != nil {
		if isTreeAbsent(scanErr) {
			return outdatedSource{}, errNoInstalledCollections(lockPath, cfg.DownloadPath, scanErr)
		}
		return outdatedSource{}, scanErr
	}
	runtime.Output.Printf("No lockfile at %s; reading installed collections from %s", lockPath, cfg.DownloadPath)
	reportInstalledGaps(runtime, scan, installedRoleCount(cfg))
	return outdatedSource{label: cfg.DownloadPath, collections: scan.entries}, nil
}

// unhonoredFlags names, in a fixed order, every configured flag outdated cannot
// act on. --cache-dir is absent because config holds a resolved value that
// cannot be told apart from its default; --download-path is honored.
func unhonoredFlags(cfg *config.Config) []string {
	var names []string
	if cfg.ClearCache {
		names = append(names, "--clear-cache")
	}
	if cfg.NoCache {
		names = append(names, "--no-cache")
	}
	if cfg.Refresh {
		names = append(names, "--refresh")
	}
	if cfg.NoDeps {
		names = append(names, "--no-deps")
	}
	if cfg.Frozen {
		names = append(names, "--frozen")
	}
	if cfg.S3Cache.Enabled {
		names = append(names, "--s3-bucket")
	}
	return names
}

// warnUnhonoredFlags prints at most one stderr line naming every inert flag.
// It warns rather than refuses because the same values often arrive from a CI
// environment shared with install, warm and lock, where they are harmless.
func warnUnhonoredFlags(runtime *infra.Infra, cfg *config.Config) {
	names := unhonoredFlags(cfg)
	if len(names) == 0 {
		return
	}
	runtime.Output.Warnf(
		"outdated does not honor %s; it only reads what the project is running and queries each server for the latest version",
		strings.Join(names, ", "),
	)
}

func queryLatestVersions(
	ctx context.Context, cfg *config.Config, runtime *infra.Infra, entries []lockfile.Entry,
) []outdatedEntry {
	deps := newCollectionDeps(cfg, runtime, nil)
	out := make([]outdatedEntry, len(entries))
	var wg sync.WaitGroup
	workers := max(cfg.Workers, 1)
	jobs := make(chan int, len(entries))
	for i := range entries {
		jobs <- i
	}
	close(jobs)
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				out[i] = lookupOutdated(ctx, deps, entries[i])
			}
		})
	}
	wg.Wait()
	return out
}

func lookupOutdated(ctx context.Context, deps collectionDeps, e lockfile.Entry) outdatedEntry {
	if e.IsGit() {
		return lookupGitOutdated(ctx, deps, e)
	}
	if e.IsURL() {
		// A url pin is content-addressed and has no version feed; drift is
		// --refresh and --frozen territory, so it is current with no network.
		return outdatedEntry{Name: e.Name, Locked: e.Version, Latest: e.Version, Newer: false}
	}
	ns, name, ok := helpers.SplitFQDN(e.Name)
	if !ok {
		return outdatedEntry{
			Name:   e.Name,
			Locked: e.Version,
			Err:    fmt.Errorf("%w: invalid name %q", helpers.ErrLockfileInvalid, e.Name),
		}
	}
	source := e.Source
	if source == "" {
		source = deps.cfg.Server
	}
	col := collection{Namespace: ns, Name: name, Source: source}
	policy := cacheManager.PolicyForConstraint(deps.cfg, false)
	root, err := resolveRootMetadata(ctx, deps, col, policy, e.Name)
	if err != nil {
		return outdatedEntry{Name: e.Name, Locked: e.Version, Err: err}
	}
	latest := strings.TrimSpace(root.meta.HighestVersion.Version)
	if latest == "" {
		return outdatedEntry{Name: e.Name, Locked: e.Version, Err: errNoHighestVersion}
	}
	return classifyOutdated(e.Name, e.Version, latest)
}

// lookupGitOutdated reports the drift of the locked ref as full commit hashes.
// A commit ref is current by definition and costs no round trip; a run with no
// git client reports a configuration defect rather than a remote failure.
func lookupGitOutdated(ctx context.Context, deps collectionDeps, e lockfile.Entry) outdatedEntry {
	entry := outdatedEntry{Name: e.Name, Locked: e.Commit}
	ref, err := gitsource.ParseRef(e.Ref)
	if err != nil {
		entry.Err = fmt.Errorf("%w: %s: %w", helpers.ErrLockfileInvalid, e.Name, err)
		return entry
	}
	if ref.IsCommit() {
		entry.Latest = e.Commit
		return entry
	}
	if deps.runtime == nil || deps.runtime.Git == nil {
		entry.Err = fmt.Errorf("%w: no git client is wired into this run", helpers.ErrConfigIsNil)
		return entry
	}
	u, err := gitsource.ParseURL(e.Source)
	if err != nil {
		entry.Err = fmt.Errorf("%w: %s: %w", helpers.ErrLockfileInvalid, e.Name, err)
		return entry
	}
	cred, _ := gitsource.MatchCredential(u, deps.runtime.GitCredentials)
	gitCtx, cancel := context.WithTimeout(ctx, deps.runtime.GitDeadline())
	defer cancel()
	latest, _, err := deps.runtime.Git.Advertise(gitCtx, u, ref, cred)
	if err != nil {
		entry.Err = artifactDeadlineError(ctx, gitCtx, deps.runtime.GitDeadline(), err)
		return entry
	}
	entry.Latest = latest
	entry.Newer = latest != e.Commit
	return entry
}

// classifyOutdated compares locked against latest. A version that does not
// parse as semver is reported through Err, never as up to date, so the run
// exits non-zero and names the entry.
func classifyOutdated(name, locked, latest string) outdatedEntry {
	newer, err := isNewerVersion(latest, locked)
	if err != nil {
		return outdatedEntry{Name: name, Locked: locked, Latest: latest, Err: fmt.Errorf("version parse: %w", err)}
	}
	return outdatedEntry{Name: name, Locked: locked, Latest: latest, Newer: newer}
}

func isNewerVersion(latest, locked string) (bool, error) {
	l, err := semver.NewVersion(latest)
	if err != nil {
		return false, err
	}
	c, err := semver.NewVersion(locked)
	if err != nil {
		return false, err
	}
	return l.GreaterThan(c), nil
}

// reportOutdated prints a result-tier line per entry needing attention (an
// up-to-date one only under verbose) and a summary counting every entry. Only
// the failure line %q-quotes the lockfile name; safeout.Clean bounds each line.
func reportOutdated(runtime *infra.Infra, results []outdatedEntry, lockPath string, verbose bool) {
	upToDate, outdated, failed := 0, 0, 0
	for _, r := range results {
		switch {
		case r.Err != nil:
			runtime.Output.Errorf("Lookup failed: %q@%s: %s", r.Name, r.Locked, r.Err)
			failed++
		case r.Newer:
			runtime.Output.Updatef("Outdated: %s %s -> %s", r.Name, r.Locked, r.Latest)
			outdated++
		default:
			if verbose {
				runtime.Output.OkVersionf(r.Locked, "Up to date: %s", r.Name)
			}
			upToDate++
		}
	}
	runtime.Output.PersistentPrintf("%s: %d up to date, %d outdated, %d failed", lockPath, upToDate, outdated, failed)
}
