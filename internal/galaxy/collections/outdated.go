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

// errNoHighestVersion is lookupOutdated's plain local error for a root
// metadata document that answered but named no highest_version. It carries
// no sentinel of its own, deliberately: this failure classifies the same way
// every other lookup failure does, through outdated's own aggregation
// headline (helpers.ErrLatestVersionLookupFailed), so a dedicated sentinel
// would add a name nothing needs to match on.
var errNoHighestVersion = errors.New("no highest_version in metadata")

// outdatedEntry summarizes a single collection's locked-vs-latest delta. Err
// is nil on success (whether or not a newer version exists) and non-nil when
// the lookup itself failed - one field rather than a Message string plus a
// Failed bool, which could disagree with each other (a non-empty Message
// with Failed false, or the reverse) in a way a single error value cannot.
type outdatedEntry struct {
	Err    error
	Name   string
	Locked string
	Latest string
	Newer  bool
}

// Outdated reads what the project is running - the lockfile, or the
// installed collections tree when there is no lockfile (see outdatedInput) -
// and queries Galaxy for the latest available version of every entry in
// parallel, printing a summary report. Exit status is non-zero only when at
// least one query failed.
//
// outdated deliberately opens no cache backend: it never calls
// internal/cache.New, so it never takes the exclusive distributed lock (it
// can run alongside an install or warm against the same cache directory or
// bucket) and every version it reports is a live answer from the server
// rather than one served from cached metadata, which is the whole point of
// asking what is "latest".
func Outdated(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	if cfg == nil {
		// Defensive only: every production call site (cmd/go-galaxy/commands's
		// runCollectionCommand) always builds a non-nil *config.Config before
		// reaching here. Kept so this function is total rather than resting on
		// that invariant holding forever.
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
		if r.Err != nil {
			failures.record(r.Err)
		}
	}
	summary := failures.summary()
	// outdated's report describes the run's own work - the lookups it
	// performed and how many failed - never the verdict of how many
	// collections are behind: a dashboard branches on the exit code for that,
	// never on a report that can look clean either way. frozen is a literal
	// false, never cfg.Frozen: outdated never honors --frozen (see
	// warnUnhonoredFlags), so passing cfg.Frozen through would re-commit the
	// exact false claim already fixed for `lock`'s own report.
	writeRunMetrics(cfg, runtime, "outdated", start,
		runCounts{Collections: len(src.collections), Roles: len(src.roles), Failures: int(summary.count)}, false)
	return summary.outdatedError()
}

// outdatedSource is the current side of one outdated run: the entries to
// compare against the servers, and the label the report's summary line names
// as where they were read from - the lockfile's path, or the collections
// tree's.
type outdatedSource struct {
	label       string
	collections []lockfile.Entry
	roles       []lockfile.RoleEntry
}

// outdatedInput reads the current side, preferring the lockfile and falling
// back to the installed collections tree when there is none.
//
// The fallback exists because a lockfile is where the versions in use are
// written down, not the only place they can be read: a project that installs
// without locking still has every version it is running on disk, along with
// the server each came from, so refusing to answer would be a limitation of
// this command rather than of the question. It is the same shape `hash`
// already takes when it falls back to requirements.yml, and it stays a
// fallback rather than a choice: with a lockfile present that file is the
// answer, since it covers roles and git refs the tree cannot.
//
// A lockfile that exists and does not load is propagated untouched. Only its
// absence opens the fallback - a malformed lockfile is a defect to fix, and
// quietly reporting from somewhere else would hide it.
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

// unhonoredFlags returns the human-readable name of every flag this run
// configured that outdated cannot act on, in a fixed order so
// warnUnhonoredFlags' single warning line is deterministic across runs. It
// returns nil - allocating nothing - when the run configured none of them,
// which is the common case; this only allocates on a misconfigured run.
//
// --download-path is not listed because it is honored: with no lockfile it
// names the tree outdated reads the current side from (see outdatedInput).
// --cache-dir is excluded for a reason distinct from every flag checked
// below: config.Config records a resolved value, not whether the operator
// actually set the flag, so a defaulted string path is indistinguishable at
// this layer from one explicitly passed. Telling them apart would require
// threading cli.Command.IsSet down from the CLI layer into internal/galaxy,
// which is a layer violation this package does not accept.
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

// warnUnhonoredFlags discloses, in at most one stderr line, every flag this
// run configured that outdated cannot honor. This is a design property, not
// an oversight: outdated opens no cache backend at all, so every cache-shaped
// flag has nothing to act on, --no-deps has no dependency graph to skip
// resolving, and --frozen has no meaning left to honor since the lockfile is
// already the only source of the locked side and the server is always asked
// for the latest. The flag is announced rather than rejected as a usage
// error, because the same values commonly arrive from an ambient CI
// environment variable block shared with install/warm/lock, where refusing
// to run over a flag that is merely inert would be the harshest possible
// outcome for the least harmful mistake.
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
		// A url pin is content-addressed: the source has no version feed,
		// and "the same URL now serves different bytes" is drift --refresh
		// and --frozen own, not "newer". No network, current by
		// construction.
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

// lookupGitOutdated asks the remote what the locked ref points at now and
// reports the commit drift: Locked and Latest are full commit hashes, so a
// reader can paste either into a lockfile or a git command without
// disambiguating an abbreviation. A ref that is itself a commit is up to date
// by definition and costs no round trip. A run wired without a git client
// reports that as a configuration defect rather than as a remote failure.
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

// classifyOutdated compares locked against latest and builds the resulting
// entry. A version that fails to parse as semver is reported through Err
// (not silently treated as up to date), so the command still exits non-zero
// and the operator sees which entry needs attention.
//
// This function draws no conclusion about which side is more likely to fail:
// it is callable with any two strings and treats both the same way. In
// production, after lockfile.Load has validated every entry's version is
// helpers.IsExactVersion, the locked side is provably parseable by the time
// this runs, so a parse failure here can in practice only be latest - the
// server's own highest_version - but that is a fact about this function's
// one production caller, not an invariant classifyOutdated itself enforces.
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

// reportOutdated prints one line per entry that needs attention plus a
// trailing summary, entirely through the Printer so every line is sanitized
// and --quiet-aware like the rest of the program's output.
//
// An up-to-date entry gets a line only under --verbose. The report exists to
// name what to act on, and a line per current entry buries the few that are
// not; the summary still counts every entry, so a default run loses no
// total. Under --verbose that line is the one install prints for a subject it
// has nothing left to do for - the success marker, then the version as
// OkVersionf's dimmed "== <version>" tag - so the same fact reads the same
// way in both commands.
//
// Every line here is result tier (OkVersionf/Updatef/Errorf/
// PersistentPrintf), matching the fact that this report is itself the run's
// product: an up-to-date entry uses OkVersionf since there is nothing to do,
// an outdated entry uses Updatef, whose marker says neither success nor
// failure, and a failed lookup uses Errorf on stderr so a diagnostic never
// contaminates stdout; the trailing summary is a total rather than a verdict
// about any one collection, so it carries no marker at all. Result tier means
// --quiet suppresses none of the lines this report prints - it only ever
// suppresses the transient tier, and --quiet and --verbose never hold
// together (config ignores the first when the second is set) - which is a
// deliberate divergence from classifyDryRun's own dry-run report mapping:
// reporting "there is a newer version available" through a green checkmark
// would be a wrong statement, so the two reports are not aligned on purpose.
//
// The failure line alone renders r.Name with %q; the other two render it
// with %s, matching every other report in this package. r.Name is an
// untrusted identifier - it comes straight from the lockfile, with no
// character-class validation beyond helpers.SplitFQDN's "exactly one dot,
// both halves non-empty" - and %q does two things to it: it delimits it, so
// a reader can see exactly where it ends, and it escapes any control
// character inside it before safeout.Clean ever sees that occurrence. The
// two defenses therefore overlap on this one operand rather than one
// standing in for the other. Clean still runs over the whole formatted line
// and is what bounds r.Err, whose text comes from a Galaxy server. On the
// up-to-date and outdated lines there is no overlap at all - Clean alone
// bounds r.Name there, and on the up-to-date line it bounds the version tag
// too, cleaned apart from the message - which is why the property is pinned
// on Clean itself (internal/safeout) and on every Printer tier
// (internal/progress) rather than on this call site.
//
// Quoting only this one line is deliberate: the up-to-date and outdated
// lines are the normal report an operator reads on every run, and
// %q-quoting a name that is almost always benign would make that ordinary
// report harder to read.
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
