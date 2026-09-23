package collections

import (
	"fmt"
	"slices"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// roleLockfileEntries renders every resolved role as a lockfile entry in
// install-name order, taking the locator apart so the reviewed file names
// the repository and commit rather than an internal key.
func roleLockfileEntries(cfg *config.Config, roles roleResolution) ([]lockfile.RoleEntry, error) {
	if len(roles.roles) == 0 {
		return nil, nil
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range roles.roles {
			if !yield(name) {
				return
			}
		}
	})
	entries := make([]lockfile.RoleEntry, 0, len(names))
	for _, name := range names {
		entry, err := roleLockfileEntry(cfg, roles.roles[name])
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func roleLockfileEntry(cfg *config.Config, r resolvedRole) (lockfile.RoleEntry, error) {
	if urlsource.IsLocator(r.Source) {
		return urlRoleLockfileEntry(r)
	}
	loc, err := gitsource.ParseLocator(r.Source)
	if err != nil {
		return lockfile.RoleEntry{}, fmt.Errorf("lockfile: role %s: %w", r.Name, err)
	}
	if !loc.Pinned() {
		return lockfile.RoleEntry{}, fmt.Errorf("lockfile: role %s: %w: role is not pinned to a commit", r.Name, helpers.ErrInvalidGitLocator)
	}
	entry := lockfile.RoleEntry{
		Name:    r.Name,
		Version: r.Version,
		Ref:     r.Ref,
		Commit:  loc.Commit,
		Deps:    slices.Clone(r.Deps),
	}
	if r.Kind == requirements.TypeGalaxy {
		entry.Type = lockfile.RoleTypeGalaxy
		entry.Galaxy = r.GalaxyName
		entry.Source = r.Server
		if entry.Source == "" {
			entry.Source = cfg.Server
		}
		entry.Repository = loc.URL
		return entry, nil
	}
	entry.Type = lockfile.RoleTypeGit
	entry.Source = loc.URL
	return entry, nil
}

// urlRoleLockfileEntry renders a url role's pin: the tarball URL as its
// source and the origin bytes' sha256, which a url entry requires where a
// git entry's is refused.
func urlRoleLockfileEntry(r resolvedRole) (lockfile.RoleEntry, error) {
	loc, err := urlsource.ParseLocator(r.Source)
	if err != nil {
		return lockfile.RoleEntry{}, fmt.Errorf("lockfile: role %s: %w", r.Name, err)
	}
	if !loc.Pinned() {
		return lockfile.RoleEntry{}, fmt.Errorf("lockfile: role %s: %w: role is not pinned to a sha256", r.Name, helpers.ErrInvalidURLLocator)
	}
	return lockfile.RoleEntry{
		Name:    r.Name,
		Type:    lockfile.RoleTypeURL,
		Version: r.Version,
		Source:  loc.URL,
		SHA256:  loc.SHA256,
		Deps:    slices.Clone(r.Deps),
	}, nil
}

// resolveRolesFromLockfile is resolveFromLockfile for the roles list: every
// requirement must be locked as written, and every entry becomes a resolved
// role; no network is touched here.
func resolveRolesFromLockfile(lf *lockfile.File, roots []requirements.RoleRequirement) (roleResolution, error) {
	byName := make(map[string]lockfile.RoleEntry, len(lf.Roles))
	for _, e := range lf.Roles {
		byName[e.Name] = e
	}
	for _, root := range roots {
		if err := verifyRoleRootAgainstLockfile(root, byName); err != nil {
			return roleResolution{}, err
		}
	}
	res := roleResolution{roles: make(map[string]resolvedRole, len(lf.Roles)), order: make([]string, 0, len(lf.Roles))}
	for _, e := range lf.Roles {
		res.roles[e.Name] = resolvedRoleFromLockfile(e)
		res.order = append(res.order, e.Name)
	}
	return res, nil
}

// resolvedRoleFromLockfile materializes one locked role: a url entry
// rebuilds its url locator from the URL and sha256, a git or Galaxy entry
// its git locator from the repository and commit.
func resolvedRoleFromLockfile(e lockfile.RoleEntry) resolvedRole {
	if e.IsURL() {
		return resolvedRole{
			Deps:    slices.Clone(e.Deps),
			Name:    e.Name,
			Source:  urlsource.Locator{URL: e.Source, SHA256: e.SHA256}.String(),
			Version: e.Version,
			Kind:    requirements.TypeURL,
		}
	}
	kind := requirements.TypeGit
	if !e.IsGit() {
		kind = requirements.TypeGalaxy
	}
	role := resolvedRole{
		Deps:       slices.Clone(e.Deps),
		Name:       e.Name,
		Source:     gitsource.Locator{URL: e.RepositoryURL(), Commit: e.Commit}.String(),
		Repository: e.RepositoryURL(),
		Ref:        e.Ref,
		Version:    e.Version,
		GalaxyName: e.Galaxy,
		Kind:       kind,
	}
	if !e.IsGit() {
		role.Server = e.Source
	}
	return role
}

// verifyRoleRootAgainstLockfile checks one roles: entry, as written, against
// the lockfile: the install name must be locked from the same source, and a
// ref change is a mismatch even at the same commit.
func verifyRoleRootAgainstLockfile(root requirements.RoleRequirement, byName map[string]lockfile.RoleEntry) error {
	entry, ok := byName[root.Name]
	if !ok {
		return fmt.Errorf("%w: role %s missing", helpers.ErrLockfileMismatch, root.Name)
	}
	switch {
	case root.IsURL():
		return verifyURLRoleRoot(root, entry)
	case root.IsGit():
		return verifyGitRoleRoot(root, entry)
	default:
		return verifyGalaxyRoleRoot(root, entry)
	}
}

// verifyURLRoleRoot checks a url roles: entry against its locked form: same
// URL, and the same version label when the root spelled one.
func verifyURLRoleRoot(root requirements.RoleRequirement, entry lockfile.RoleEntry) error {
	if !entry.IsURL() || entry.Source != root.Src {
		return fmt.Errorf("%w: role %s locked from %s, requirements ask for %s",
			helpers.ErrLockfileMismatch, root.Name, roleEntryOrigin(entry), helpers.URLForMessage(root.Src))
	}
	if root.Version != "" && entry.Version != root.Version {
		return fmt.Errorf("%w: role %s locked at %q, requirements ask for %q",
			helpers.ErrLockfileMismatch, root.Name, entry.Version, root.Version)
	}
	return nil
}

// verifyGitRoleRoot checks a git roles: entry against its locked form: same
// repository and the same ref, a ref change being a mismatch even at the
// same commit.
func verifyGitRoleRoot(root requirements.RoleRequirement, entry lockfile.RoleEntry) error {
	if !entry.IsGit() || entry.Source != root.Src {
		return fmt.Errorf("%w: role %s locked from %s, requirements ask for %s",
			helpers.ErrLockfileMismatch, root.Name, roleEntryOrigin(entry), helpers.URLForMessage(root.Src))
	}
	if entry.Ref != root.Version {
		return fmt.Errorf("%w: role %s locked from ref %q, requirements ask for %q",
			helpers.ErrLockfileMismatch, root.Name, entry.Ref, root.Version)
	}
	return nil
}

// verifyGalaxyRoleRoot checks a Galaxy roles: entry against its locked
// form: same Galaxy name, and the version asked for when one was.
func verifyGalaxyRoleRoot(root requirements.RoleRequirement, entry lockfile.RoleEntry) error {
	if entry.IsGit() || entry.IsURL() || entry.Galaxy != root.Src {
		return fmt.Errorf("%w: role %s locked from %s, requirements ask for Galaxy role %s",
			helpers.ErrLockfileMismatch, root.Name, roleEntryOrigin(entry), root.Src)
	}
	if root.Version != "" && entry.Version != root.Version {
		return fmt.Errorf("%w: role %s locked at %q, requirements ask for %q",
			helpers.ErrLockfileMismatch, root.Name, entry.Version, root.Version)
	}
	return nil
}

// roleEntryOrigin renders where a locked role came from, for a message.
func roleEntryOrigin(e lockfile.RoleEntry) string {
	if e.IsGit() || e.IsURL() {
		return helpers.URLForMessage(e.Source)
	}
	return "Galaxy role " + e.Galaxy
}
