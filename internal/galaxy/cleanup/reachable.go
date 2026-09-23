package cleanup

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// buildReachable returns the installed keys every recorded project reaches and
// every on-disk copy by key. All workspaces are scanned before any root
// resolves, so a root reaches a copy any project installed, in any order.
func buildReachable(
	runtime *infra.Infra, registry *store.ProjectRegistry, st *store.Store,
) (map[string]bool, map[string][]installedCollection, roleReachability, error) {
	reachable := make(map[string]bool)
	roles := roleReachability{reachable: make(map[string]bool), byName: make(rolesByName)}
	installedIndex := make(map[string][]installedCollection)
	depsByKey := make(map[string]map[string]string)
	// installedByKey holds every on-disk copy of a key across projects, so
	// removeUnused removes all of them in one run.
	installedByKey := make(map[string][]installedCollection)
	// constraints caches each parsed constraint (nil when unparseable) so one
	// string is parsed once across the whole walk.
	constraints := make(map[string]*semver.Constraints)

	projectPaths := slices.Sorted(maps.Keys(registry.Projects))

	// Phase 1: every project's workspace into the shared index, before any
	// root resolves against it.
	for _, projectPath := range projectPaths {
		if err := scanProjectWorkspace(
			runtime.Output, projectPath, registry.Projects[projectPath], installedIndex, installedByKey, depsByKey,
		); err != nil {
			return nil, nil, roleReachability{}, err
		}
		if err := scanProjectRoles(runtime.Output, projectPath, registry.Projects[projectPath], st, roles.byName); err != nil {
			return nil, nil, roleReachability{}, err
		}
	}

	// Phase 2: every recorded project's roots, against the complete index.
	for _, projectPath := range projectPaths {
		file, rolesUnread, err := projectRequirementRoots(runtime.Output, projectPath, registry.Projects[projectPath])
		if err != nil {
			return nil, nil, roleReachability{}, err
		}
		if rolesUnread {
			keepProjectRoles(registry.Projects[projectPath], roles)
		}
		markReachableRoles(roleRootNames(file), roles.byName, roles.reachable)
		markCollectionRoots(st, file.Collections, reachable, installedByKey, installedIndex, depsByKey, constraints)
	}
	return reachable, installedByKey, roles, nil
}

// markCollectionRoots marks every installed collection one project's
// collection roots reach.
func markCollectionRoots(
	st *store.Store,
	roots []requirements.CollectionRequirement,
	reachable map[string]bool,
	installedByKey, installedIndex map[string][]installedCollection,
	depsByKey map[string]map[string]string,
	constraints map[string]*semver.Constraints,
) {
	for _, root := range roots {
		if root.IsGit() {
			for _, key := range gitRootKeys(st, installedByKey, root) {
				markReachable(key, reachable, depsByKey, installedIndex, constraints)
			}
			continue
		}
		if root.IsURL() {
			for _, key := range urlRootKeys(st, installedByKey, root) {
				markReachable(key, reachable, depsByKey, installedIndex, constraints)
			}
			continue
		}
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		for _, inst := range selectInstalled(installedIndex, constraints, fqdn, root.Version) {
			markReachable(inst.Key, reachable, depsByKey, installedIndex, constraints)
		}
	}
}

// roleReachability is the roles half of what buildReachable computed: every
// role name some project reaches, and every installed copy by name.
type roleReachability struct {
	reachable map[string]bool
	byName    rolesByName
}

// roleRootNames lists the install names a requirements file's roles: entries
// ask for.
func roleRootNames(file requirements.File) []string {
	names := make([]string, 0, len(file.Roles))
	for _, r := range file.Roles {
		names = append(names, r.Name)
	}
	return names
}

// gitRootKeys returns the installed keys a git requirement keeps alive: those
// its store pin records, or, with no pin, every record from its repository at
// its subdir or a child, whatever the commit, the safe direction for a sweep.
func gitRootKeys(st *store.Store, installedByKey map[string][]installedCollection, root requirements.CollectionRequirement) []string {
	if pin, ok := st.GetGitPin(gitsource.PinKey(root.Source, root.Ref, root.Subdir)); ok {
		return pinnedGitKeys(pin, installedByKey, root)
	}
	return installedGitKeys(st, installedByKey, root)
}

// pinnedGitKeys is gitRootKeys' pin branch: the installed keys among the
// collections the pin records, narrowed to the named one when the root names
// its collection.
func pinnedGitKeys(pin store.GitPinEntry, installedByKey map[string][]installedCollection,
	root requirements.CollectionRequirement,
) []string {
	keys := make([]string, 0)
	for _, c := range pin.Collections {
		if root.Name != "" && (c.Namespace != root.Namespace || c.Name != root.Name) {
			continue
		}
		key := fmt.Sprintf("%s.%s@%s", c.Namespace, c.Name, c.Version)
		if _, installed := installedByKey[key]; installed {
			keys = append(keys, key)
		}
	}
	return keys
}

// installedGitKeys is gitRootKeys' no-pin fallback: installed records from the
// root's repository under its subdir or an immediate child, narrowed to the
// named collection, sorted.
func installedGitKeys(st *store.Store, installedByKey map[string][]installedCollection,
	root requirements.CollectionRequirement,
) []string {
	keys := make([]string, 0)
	for key := range installedByKey {
		entry, ok := st.GetInstalled(key)
		if !ok || !gitsource.IsLocator(entry.Source) {
			continue
		}
		loc, err := gitsource.ParseLocator(entry.Source)
		if err != nil || loc.URL != root.Source || !gitSubdirWithin(loc.Subdir, root.Subdir) {
			continue
		}
		if root.Name != "" && !strings.HasPrefix(key, root.Namespace+"."+root.Name+"@") {
			continue
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// urlRootKeys returns the installed keys a url requirement keeps alive: the
// one its url pin records, or, with no pin, every installed record from the
// same URL whatever its sha, as gitRootKeys does.
func urlRootKeys(st *store.Store, installedByKey map[string][]installedCollection, root requirements.CollectionRequirement) []string {
	if pin, ok := st.GetURLPin(urlsource.PinKey(root.Source)); ok {
		key := fmt.Sprintf("%s.%s@%s", pin.Namespace, pin.Name, pin.Version)
		if _, installed := installedByKey[key]; installed {
			return []string{key}
		}
		return nil
	}
	return installedURLKeys(st, installedByKey, root)
}

// installedURLKeys is urlRootKeys' fallback when no pin is recorded: every
// installed record sourced from a locator of the root's URL, in sorted
// order.
func installedURLKeys(st *store.Store, installedByKey map[string][]installedCollection,
	root requirements.CollectionRequirement,
) []string {
	keys := make([]string, 0)
	for key := range installedByKey {
		entry, ok := st.GetInstalled(key)
		if !ok || !urlsource.IsLocator(entry.Source) {
			continue
		}
		loc, err := urlsource.ParseLocator(entry.Source)
		if err != nil || loc.URL != root.Source {
			continue
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// gitSubdirWithin reports whether an installed collection's subdir is the
// root's own subdir or an immediate child of it - the two shapes a git
// requirement expands into.
func gitSubdirWithin(entrySubdir, rootSubdir string) bool {
	if entrySubdir == rootSubdir {
		return true
	}
	parent := path.Dir(entrySubdir)
	if parent == "." {
		parent = ""
	}
	return parent == rootSubdir
}

// projectRequirementRoots loads a project's roots, even for a workspace the
// scan skipped. A missing file contributes nothing; any other failure aborts,
// since unknown roots could protect any project's copies.
func projectRequirementRoots(
	out output.Printer, projectPath string, project store.ProjectRecord,
) (requirements.File, bool, error) {
	file, err := loadRequirements(project.RequirementsFile, "")
	if err == nil {
		return file, false, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		out.Warnf("project %q: requirements file %q no longer exists; it contributes no reachability roots this run",
			projectPath, project.RequirementsFile)
		return requirements.File{}, false, nil
	}
	// An unreadable roles: list keeps every role under the project's roles
	// path and still judges its collections, so one such file cannot abort
	// every cleanup against a shared cache.
	if rolesErr, ok := errors.AsType[*requirements.RolesError](err); ok {
		out.Warnf("project %q: the roles list of %q cannot be read (%v); its roles are kept this run",
			projectPath, project.RequirementsFile, rolesErr.Err)
		return file, true, nil
	}
	return requirements.File{}, false, fmt.Errorf("%w: %s: %w", helpers.ErrProjectRequirementsUnreadable, project.RequirementsFile, err)
}

// keepProjectRoles marks every role installed under the project's roles
// path reachable, for a project whose role roots could not be read.
func keepProjectRoles(project store.ProjectRecord, roles roleReachability) {
	if project.RolesPath == "" {
		return
	}
	for name, copies := range roles.byName {
		for _, inst := range copies {
			if inst.RolesDir == project.RolesPath {
				roles.reachable[name] = true
			}
		}
	}
}

// selectInstalled filters installed collections by constraint, memoized in
// constraints; an empty or unparseable constraint keeps every installed
// version, the safe direction for a sweep.
func selectInstalled(
	index map[string][]installedCollection,
	constraints map[string]*semver.Constraints,
	fqdn, constraint string,
) []installedCollection {
	items := index[fqdn]
	if len(items) == 0 {
		return nil
	}
	normalized := helpers.NormalizeConstraint(constraint)
	if normalized == "" {
		return items
	}
	c, ok := constraints[normalized]
	if !ok {
		c, _ = semver.NewConstraint(normalized)
		constraints[normalized] = c
	}
	if c == nil {
		return items
	}
	out := make([]installedCollection, 0, len(items))
	for _, item := range items {
		if item.Parsed == nil {
			continue
		}
		if c.Check(item.Parsed) {
			out = append(out, item)
		}
	}
	return out
}

// markReachable marks all reachable dependencies starting at key.
func markReachable(
	key string,
	reachable map[string]bool,
	deps map[string]map[string]string,
	index map[string][]installedCollection,
	constraints map[string]*semver.Constraints,
) {
	queue := []string{key}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if reachable[current] {
			continue
		}
		reachable[current] = true
		for depFQDN, constraint := range deps[current] {
			for _, inst := range selectInstalled(index, constraints, depFQDN, constraint) {
				if !reachable[inst.Key] {
					queue = append(queue, inst.Key)
				}
			}
		}
	}
}
