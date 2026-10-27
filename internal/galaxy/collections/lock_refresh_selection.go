package collections

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
)

// refreshRelease is what --collection and --role set aside under --refresh:
// the named galaxy.lock entries and every entry they reach, collections by
// name and roles by install name.
type refreshRelease struct {
	collections map[string]struct{}
	roles       map[string]struct{}
}

// hasRefreshSelection reports whether --collection or --role narrows --refresh.
func hasRefreshSelection(cfg *config.Config) bool {
	return len(cfg.RefreshCollections) > 0 || len(cfg.RefreshRoles) > 0
}

// newRefreshRelease closes cfg's selection over lf: deps, the collections of a
// git root owning a released one, and url roles sharing a released one's URL.
func newRefreshRelease(cfg *config.Config, lf *lockfile.File, roots []collection) refreshRelease {
	collectionDeps := make(map[string][]string, len(lf.Collections))
	var gitEntries []lockfile.Entry
	for _, e := range lf.Collections {
		collectionDeps[e.Name] = e.Deps
		if e.IsGit() {
			gitEntries = append(gitEntries, e)
		}
	}
	gitSiblings := gitRootSiblings(roots, gitEntries)
	roleDeps := make(map[string][]string, len(lf.Roles))
	// A url role's pin is keyed by its URL alone, so roles sharing one move together.
	urlRoles, roleURL := make(map[string][]string), make(map[string]string)
	var namedRoles []string
	for _, e := range lf.Roles {
		roleDeps[e.Name] = e.Deps
		if e.IsURL() {
			urlRoles[e.Source], roleURL[e.Name] = append(urlRoles[e.Source], e.Name), e.Source
		}
		if slices.Contains(cfg.RefreshRoles, e.Name) || slices.Contains(cfg.RefreshRoles, e.Galaxy) {
			namedRoles = append(namedRoles, e.Name)
		}
	}
	return refreshRelease{
		collections: reachableEntries(cfg.RefreshCollections, func(name string) []string {
			return append(slices.Clone(collectionDeps[name]), gitSiblings[name]...)
		}),
		roles: reachableEntries(namedRoles, func(name string) []string {
			return append(slices.Clone(roleDeps[name]), urlRoles[roleURL[name]]...)
		}),
	}
}

// gitRootSiblings maps each git entry to every entry its root owns, by the
// rule gitRoots pins by: one commit holds them all, so releasing one moves the
// rest. A root asked at a commit never moves, so it groups nothing.
func gitRootSiblings(roots []collection, entries []lockfile.Entry) map[string][]string {
	siblings := make(map[string][]string)
	matches, err := matchGitRoots(roots, entries)
	if err != nil {
		return siblings
	}
	for i, match := range matches {
		if gitsource.IsCommitHash(roots[i].Ref) {
			continue
		}
		for _, owned := range match.Owned {
			for _, sibling := range match.Owned {
				siblings[owned.Name] = append(siblings[owned.Name], sibling.Name)
			}
		}
	}
	return siblings
}

// reachableEntries returns roots and every name next reaches from them.
func reachableEntries(roots []string, next func(string) []string) map[string]struct{} {
	seen := make(map[string]struct{}, len(roots))
	queue := slices.Clone(roots)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		queue = append(queue, next(name)...)
	}
	return seen
}

// releasesCollection reports whether the selection sets aside the pin of the
// collection named name.
func (r refreshRelease) releasesCollection(name string) bool {
	_, ok := r.collections[name]
	return ok
}

// releasesRole reports whether the selection sets aside the pin of the role
// installed as name.
func (r refreshRelease) releasesRole(name string) bool {
	_, ok := r.roles[name]
	return ok
}

// announce prints, as a report line before the resolve, every pin the
// selection sets aside, what it reaches included; an empty selection prints nothing.
func (r refreshRelease) announce(out output.Printer) {
	names := slices.Sorted(maps.Keys(r.collections))
	for _, name := range slices.Sorted(maps.Keys(r.roles)) {
		names = append(names, "role "+name)
	}
	if len(names) > 0 {
		out.PersistentPrintf("Setting aside the pins of %s", strings.Join(names, ", "))
	}
}

// checkRefreshSelection refuses a selection over a lockfile that does not load,
// with its cause as %v so the exit is 2, not 6, and every name it does not pin.
func checkRefreshSelection(cfg *config.Config, existing existingLockfile) error {
	if !hasRefreshSelection(cfg) || lockfile.IsNotExist(existing.err) {
		return nil
	}
	if existing.err != nil {
		//nolint:errorlint // deliberately %v, not %w: see the doc comment above.
		return fmt.Errorf("%w: %s cannot be read (%v); a plain `go-galaxy lock` replaces a file that does not load",
			helpers.ErrRefreshSelectionLockfileUnreadable, existing.path, existing.err)
	}
	var unknown []string
	for _, name := range cfg.RefreshCollections {
		if !slices.ContainsFunc(existing.file.Collections, func(e lockfile.Entry) bool { return e.Name == name }) {
			unknown = append(unknown, "collection "+name)
		}
	}
	for _, name := range cfg.RefreshRoles {
		if !slices.ContainsFunc(existing.file.Roles, func(e lockfile.RoleEntry) bool { return e.Name == name || e.Galaxy == name }) {
			unknown = append(unknown, "role "+name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("%w %s: %s", helpers.ErrRefreshSelectionNotLocked, existing.path, strings.Join(unknown, ", "))
}
