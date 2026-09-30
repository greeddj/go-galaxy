package cleanup

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
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
			runtime.Output, projectPath, registry.Projects[projectPath], installedIndex, installedByKey,
		); err != nil {
			return nil, nil, roleReachability{}, err
		}
		if err := scanProjectRoles(runtime.Output, projectPath, registry.Projects[projectPath], st, roles.byName); err != nil {
			return nil, nil, roleReachability{}, err
		}
	}

	// Phase 2: every recorded project's roots, against the complete index.
	// Every project left with nothing to read is gathered, so one error names all.
	var unanchored []string
	for _, projectPath := range projectPaths {
		project := registry.Projects[projectPath]
		if projectLeft(projectPath, project) {
			runtime.Output.Warnf("project %q no longer exists; it keeps nothing this run", projectPath)
			continue
		}
		roots, anchored, err := projectRequirementRoots(runtime.Output, registry.Location, projectPath, project)
		if err != nil {
			return nil, nil, roleReachability{}, err
		}
		if !anchored {
			unanchored = append(unanchored, projectPath)
			continue
		}
		markProjectRoots(st, project, roots, reachable, roles, installedByKey, installedIndex, constraints)
	}
	if len(unanchored) > 0 {
		return nil, nil, roleReachability{}, unanchoredProjectsError(registry.Location, unanchored)
	}
	markKeptCopyDependencies(installedByKey, reachable, installedIndex, constraints)
	return reachable, installedByKey, roles, nil
}

// markProjectRoots marks what one anchored project's roots reach: its files'
// entries, a stand-in galaxy.lock's exact keys and role names, and every role
// under its roles path when a roles: list could not be read.
func markProjectRoots(
	st *store.Store,
	project store.ProjectRecord,
	roots projectRoots,
	reachable map[string]bool,
	roles roleReachability,
	installedByKey, installedIndex map[string][]installedCollection,
	constraints map[string]*semver.Constraints,
) {
	if roots.rolesUnread {
		markReachableRoles(keepProjectRoles(project, roles.byName), roles.byName, roles.reachable)
	}
	markReachableRoles(roleRootNames(roots.file), roles.byName, roles.reachable)
	markReachableRoles(roots.lockedRoles, roles.byName, roles.reachable)
	markCollectionRoots(st, roots.file.Collections, reachable, installedByKey, installedIndex, constraints)
	for _, key := range roots.lockedKeys {
		markReachable(key, reachable, installedByKey, installedIndex, constraints)
	}
}

// markKeptCopyDependencies marks what each untrusted copy's own manifest needs:
// cleanup keeps that copy, so deleting its dependencies would break it. The
// copy's key stays unmarked, so a trusted copy of it elsewhere still goes.
func markKeptCopyDependencies(
	installedByKey map[string][]installedCollection,
	reachable map[string]bool,
	installedIndex map[string][]installedCollection,
	constraints map[string]*semver.Constraints,
) {
	for _, insts := range installedByKey {
		for _, inst := range insts {
			if inst.Trusted {
				continue
			}
			for depFQDN, constraint := range inst.Deps {
				for _, dep := range selectInstalled(installedIndex, constraints, depFQDN, constraint) {
					markReachable(dep.Key, reachable, installedByKey, installedIndex, constraints)
				}
			}
		}
	}
}

// markCollectionRoots marks every installed collection one project's
// collection roots reach.
func markCollectionRoots(
	st *store.Store,
	roots []requirements.CollectionRequirement,
	reachable map[string]bool,
	installedByKey, installedIndex map[string][]installedCollection,
	constraints map[string]*semver.Constraints,
) {
	for _, root := range roots {
		if root.IsGit() {
			for _, key := range gitRootKeys(st, installedByKey, root) {
				markReachable(key, reachable, installedByKey, installedIndex, constraints)
			}
			continue
		}
		if root.IsURL() {
			for _, key := range urlRootKeys(st, installedByKey, root) {
				markReachable(key, reachable, installedByKey, installedIndex, constraints)
			}
			continue
		}
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		for _, inst := range selectInstalled(installedIndex, constraints, fqdn, root.Version) {
			markReachable(inst.Key, reachable, installedByKey, installedIndex, constraints)
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

// projectRoots is what one recorded project keeps: the entries of the files
// read for it, the exact keys and role names a stand-in galaxy.lock pins, and
// whether a refused roles: list keeps its whole roles path.
type projectRoots struct {
	file        requirements.File
	lockedKeys  []string
	lockedRoles []string
	rolesUnread bool
}

// add joins one loaded requirements file into the project's roots.
func (r *projectRoots) add(file requirements.File, rolesUnread bool) {
	r.file.Collections = append(r.file.Collections, file.Collections...)
	r.file.Roles = append(r.file.Roles, file.Roles...)
	r.rolesUnread = r.rolesUnread || rolesUnread
}

// projectRequirementRoots joins the roots of every file a project that has not
// left remembers and, once one is gone, of its directory's stand-ins; anchored
// is false when nothing is left to read. Any other failure aborts.
func projectRequirementRoots(
	out output.Printer, registryLocation, projectPath string, project store.ProjectRecord,
) (projectRoots, bool, error) {
	files := project.Files()
	var roots projectRoots
	loaded, gone := 0, 0
	for _, path := range files {
		file, unread, err := loadRootsFile(out, registryLocation, projectPath, path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			out.Warnf("project %q: requirements file %q no longer exists; it contributes no reachability roots this run",
				projectPath, path)
			gone++
			continue
		case err != nil:
			return projectRoots{}, false, err
		}
		roots.add(file, unread)
		loaded++
	}
	if gone == 0 && len(files) > 0 {
		return roots, true, nil
	}
	for _, dir := range projectDirs(projectPath, files) {
		standIns, err := standInRoots(out, registryLocation, projectPath, dir, files, &roots)
		if err != nil {
			return projectRoots{}, false, err
		}
		loaded += standIns
	}
	return roots, loaded > 0, nil
}

// projectLeft reports whether a project has left this machine: none of the
// directories its recorded files sit in exists, which is how a checkout that
// is deleted leaves the registry, since no command prunes a record.
func projectLeft(projectPath string, project store.ProjectRecord) bool {
	return !slices.ContainsFunc(projectDirs(projectPath, project.Files()), dirExists)
}

// projectDirs lists the directories a project's recorded files sit in, which
// for every record a run writes is the one directory keying it; a record
// naming no file falls back to its key.
func projectDirs(projectPath string, files []string) []string {
	if len(files) == 0 {
		return []string{projectPath}
	}
	dirs := make([]string, 0, len(files))
	for _, f := range files {
		dirs = append(dirs, filepath.Dir(f))
	}
	slices.Sort(dirs)
	return slices.Compact(dirs)
}

// dirExists reports whether dir is anything but gone: a project leaves when
// its directory does, and any other Stat failure is left to the file loads.
func dirExists(dir string) bool {
	_, err := os.Stat(dir)
	return !errors.Is(err, fs.ErrNotExist)
}

// standInRoots reads what stands in for a project's gone files: the galaxy.toml
// and requirements.yml discovery would read in its directory, unless recorded,
// and the galaxy.lock beside them; it returns how many it found.
func standInRoots(
	out output.Printer, registryLocation, projectPath, dir string, recorded []string, roots *projectRoots,
) (int, error) {
	if !dirExists(dir) {
		return 0, nil
	}
	found := 0
	for _, name := range []string{helpers.RequirementsTOMLName, helpers.RequirementsYAMLName} {
		path := filepath.Join(dir, name)
		if slices.Contains(recorded, path) {
			continue
		}
		file, unread, err := loadRootsFile(out, registryLocation, projectPath, path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		out.Warnf("project %q: keeping what %q reaches in place of its missing requirements file", projectPath, path)
		roots.add(file, unread)
		found++
	}
	keys, roleNames, ok, err := lockedRoots(registryLocation, projectPath, dir)
	if err != nil || !ok {
		return found, err
	}
	out.Warnf("project %q: keeping what %q pins in place of its missing requirements file", projectPath,
		filepath.Join(dir, lockfile.DefaultName))
	roots.lockedKeys = append(roots.lockedKeys, keys...)
	roots.lockedRoles = append(roots.lockedRoles, roleNames...)
	return found + 1, nil
}

// lockedRoots reads the project directory's galaxy.lock: every collection key
// and role name it pins. Absent is not found; one lockfile.Load refuses, a
// fifo included before it is opened, aborts the run, naming it.
func lockedRoots(registryLocation, projectPath, dir string) ([]string, []string, bool, error) {
	path := filepath.Join(dir, lockfile.DefaultName)
	lf, err := lockfile.Load(path)
	if lockfile.IsNotExist(err) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("project %q: %w (%s)", projectPath, err,
			unreadableProjectHint(registryLocation, projectPath, path))
	}
	keys := make([]string, 0, len(lf.Collections))
	for _, e := range lf.Collections {
		keys = append(keys, e.Name+"@"+e.Version)
	}
	roleNames := make([]string, 0, len(lf.Roles))
	for _, r := range lf.Roles {
		roleNames = append(roleNames, r.Name)
	}
	return keys, roleNames, true, nil
}

// loadRootsFile loads one requirements file under the per-file policy: its
// roots, a bare fs.ErrNotExist when it is gone, its collections alone when its
// roles: list is refused, and ErrProjectRequirementsUnreadable otherwise.
func loadRootsFile(out output.Printer, registryLocation, projectPath, path string) (requirements.File, bool, error) {
	file, err := requirements.Load(path, "")
	if err == nil {
		return file, false, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return requirements.File{}, false, err
	}
	// An unreadable roles: list keeps every role under the project's roles
	// path and still judges its collections, so one such file cannot abort
	// every cleanup against a shared cache.
	if rolesErr, ok := errors.AsType[*requirements.RolesError](err); ok {
		out.Warnf("project %q: the roles list of %q cannot be read (%v); its roles are kept this run",
			projectPath, path, rolesErr.Err)
		return file, true, nil
	}
	return requirements.File{}, false, fmt.Errorf("%w: %s: %w (%s)",
		helpers.ErrProjectRequirementsUnreadable, path, err,
		unreadableProjectHint(registryLocation, projectPath, path))
}

// unanchoredProjectsError is ErrProjectRequirementsMissing naming every project
// whose directory remains with nothing to read, and the registry to delete a
// retired project's entry from.
func unanchoredProjectsError(registryLocation string, projects []string) error {
	registry := "the project registry"
	if registryLocation != "" {
		registry = registryLocation
	}
	quoted := make([]string, 0, len(projects))
	for _, p := range projects {
		quoted = append(quoted, strconv.Quote(p))
	}
	return fmt.Errorf("%w: %s (for each: restore a galaxy.toml, requirements.yml or galaxy.lock in that directory, "+
		"or, if no run there reads one any more, delete its entry from %s)",
		helpers.ErrProjectRequirementsMissing, strings.Join(quoted, ", "), registry)
}

// unreadableProjectHint names the three ways out of a recorded file cleanup
// cannot load: repair it, move it away so it reads as gone and the next record
// forgets it, or delete the project's entry from the registry the backend read.
func unreadableProjectHint(registryLocation, projectPath, requirementsFile string) string {
	registry := "the project registry"
	if registryLocation != "" {
		registry = registryLocation
	}
	return fmt.Sprintf("fix or restore %s, move it away if no run in %s reads it any more, "+
		"or delete the %q entry from %s", requirementsFile, projectPath, projectPath, registry)
}

// keepProjectRoles lists every role installed under the project's roles path,
// sorted, for a project whose role roots could not be read; the caller walks
// them as roots, so their dependencies stay reachable too.
func keepProjectRoles(project store.ProjectRecord, byName rolesByName) []string {
	if project.RolesPath == "" {
		return nil
	}
	var kept []string
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		if slices.ContainsFunc(byName[name], func(inst installedRole) bool { return inst.RolesDir == project.RolesPath }) {
			kept = append(kept, name)
		}
	}
	return kept
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

// markReachable marks key and what it reaches through the dependencies of
// every copy of each key, since two copies of one version can differ.
func markReachable(
	key string,
	reachable map[string]bool,
	byKey map[string][]installedCollection,
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
		for _, copyOf := range byKey[current] {
			for depFQDN, constraint := range copyOf.Deps {
				for _, inst := range selectInstalled(index, constraints, depFQDN, constraint) {
					if !reachable[inst.Key] {
						queue = append(queue, inst.Key)
					}
				}
			}
		}
	}
}
