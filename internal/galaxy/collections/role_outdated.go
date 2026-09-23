package collections

import (
	"context"
	"fmt"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/galaxyv1"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// queryLatestRoleVersions is queryLatestVersions for the roles list, on the
// same worker count and with the same nil store: no backend is opened, so
// every answer is live.
func queryLatestRoleVersions(ctx context.Context, deps collectionDeps, roles []lockfile.RoleEntry) []outdatedEntry {
	out := make([]outdatedEntry, len(roles))
	forEachIndex(len(roles), max(deps.cfg.Workers, 1), func(i int) {
		out[i] = lookupRoleOutdated(ctx, deps, roles[i])
	})
	return out
}

// lookupRoleOutdated reports one locked role's drift: a git role by commit,
// a Galaxy role by the v1 API's highest tag compared by name (role tags are
// not semver), or by its default branch's commit when it lists no tags.
func lookupRoleOutdated(ctx context.Context, deps collectionDeps, e lockfile.RoleEntry) outdatedEntry {
	display := "role " + e.Name
	if e.IsURL() {
		// A url pin is content-addressed and has no version feed; changed
		// bytes behind the URL are drift --refresh and --frozen own.
		return outdatedEntry{Name: display, Locked: e.Version, Latest: e.Version, Newer: false}
	}
	if e.IsGit() {
		entry := lookupGitOutdated(ctx, deps, lockfile.Entry{Name: e.Name, Source: e.Source, Ref: e.Ref, Commit: e.Commit})
		entry.Name = display
		return entry
	}
	entry := outdatedEntry{Name: display, Locked: e.Version}
	owner, name, ok := helpers.SplitRoleName(e.Galaxy)
	if !ok {
		entry.Err = fmt.Errorf("%w: %s: galaxy %q", helpers.ErrLockfileInvalid, e.Name, e.Galaxy)
		return entry
	}
	fetch := func(ctx context.Context, u string, out any, policy cacheManager.Policy) error {
		return fetchJSONWithCachePolicy(ctx, deps.runtime, u, deps.st, out, policy)
	}
	policy := cacheManager.PolicyForConstraint(deps.cfg, false)
	res, found, _, err := galaxyv1.Resolve(ctx, fetch, serverForRole(deps, e), owner, name, "", policy)
	switch {
	case err != nil:
		entry.Err = err
		return entry
	case !found:
		entry.Err = fmt.Errorf("%w: %s", helpers.ErrRoleNotFound, e.Galaxy)
		return entry
	}
	if len(res.Versions) == 0 {
		return roleBranchOutdated(ctx, deps, entry, e, res)
	}
	entry.Latest = res.Version
	entry.Newer = entry.Latest != e.Version
	return entry
}

// serverForRole is the Galaxy server a locked role is asked about: the one
// it was looked up on, else the run's default.
func serverForRole(deps collectionDeps, e lockfile.RoleEntry) string {
	if e.Source != "" {
		return e.Source
	}
	return deps.cfg.Server
}

// roleBranchOutdated judges a Galaxy role the server lists no tags for by
// the commit its default branch points at now, against the locked commit.
func roleBranchOutdated(
	ctx context.Context, deps collectionDeps, entry outdatedEntry, e lockfile.RoleEntry, res galaxyv1.Resolution,
) outdatedEntry {
	branch := lookupGitOutdated(ctx, deps, lockfile.Entry{Name: e.Name, Source: res.RepoURL.String(), Ref: res.Ref.Name, Commit: e.Commit})
	entry.Locked, entry.Latest, entry.Newer, entry.Err = branch.Locked, branch.Latest, branch.Newer, branch.Err
	return entry
}

// forEachIndex runs fn over 0..n-1 on a pool of workers, the shape
// queryLatestVersions uses for the collections.
func forEachIndex(n, workers int, fn func(i int)) {
	jobs := make(chan int, n)
	for i := range n {
		jobs <- i
	}
	close(jobs)
	done := make(chan struct{})
	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range jobs {
				fn(i)
			}
		}()
	}
	for range workers {
		<-done
	}
}
