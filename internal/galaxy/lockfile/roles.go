package lockfile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// SchemaVersionRoles is the schema of a file carrying at least one role
// entry. It differs from SchemaVersionGit only in that the file has a roles
// list; SchemaVersionFor picks it from the entries.
const SchemaVersionRoles = 3

// Role entry types. A role always has one: a Galaxy role was looked up by
// name on a Galaxy server, a git role was named by its repository, a url
// role by a tarball URL.
const (
	RoleTypeGalaxy = "galaxy"
	RoleTypeGit    = "git"
	RoleTypeURL    = "url"
)

// RoleEntry is one pinned role; Name is its directory under roles_path and
// Version is not semver, since a branch is a legal role version. A git or
// Galaxy role pins Commit with no SHA256, a url role the origin's SHA256.
type RoleEntry struct {
	Name       string   `yaml:"name"`
	Type       string   `yaml:"type"`
	Version    string   `yaml:"version"`
	Galaxy     string   `yaml:"galaxy,omitempty"`
	Source     string   `yaml:"source"`
	Repository string   `yaml:"repository,omitempty"`
	Ref        string   `yaml:"ref,omitempty"`
	Commit     string   `yaml:"commit,omitempty"`
	SHA256     string   `yaml:"sha256,omitempty"`
	Deps       []string `yaml:"deps,omitempty"`
}

// IsGit reports whether the role was named by its repository rather than
// looked up on a Galaxy server.
func (e RoleEntry) IsGit() bool { return e.Type == RoleTypeGit }

// IsURL reports whether the role was named by a tarball URL.
func (e RoleEntry) IsURL() bool { return e.Type == RoleTypeURL }

// RepositoryURL is the one repository a frozen install fetches the role
// from: Repository for a Galaxy role, Source for a git role.
func (e RoleEntry) RepositoryURL() string {
	if e.IsGit() {
		return e.Source
	}
	return e.Repository
}

// validateRoles judges every role entry's name, version, pin, sources and
// deps, and refuses a role in a file below schema 3, or a url role below 4:
// the schema is what tells an older binary to stop.
func (f *File) validateRoles() error {
	seen := make(map[string]struct{}, len(f.Roles))
	for _, e := range f.Roles {
		if !helpers.IsRoleInstallName(e.Name) {
			return fmt.Errorf("%w: role name %q is not a role install name", helpers.ErrLockfileInvalid, e.Name)
		}
		if _, dup := seen[e.Name]; dup {
			return fmt.Errorf("%w: duplicate role name %s", helpers.ErrLockfileInvalid, e.Name)
		}
		seen[e.Name] = struct{}{}
		if f.SchemaVersion < SchemaVersionRoles {
			return fmt.Errorf("%w: role %s: a role entry requires schema_version %d", helpers.ErrLockfileInvalid, e.Name, SchemaVersionRoles)
		}
		if e.IsURL() && f.SchemaVersion < SchemaVersionURL {
			return fmt.Errorf("%w: role %s: a url role entry requires schema_version %d",
				helpers.ErrLockfileInvalid, e.Name, SchemaVersionURL)
		}
		if reason := roleEntryProblem(e); reason != "" {
			return fmt.Errorf("%w: role %s: %s", helpers.ErrLockfileInvalid, e.Name, reason)
		}
	}
	return nil
}

// roleEntryProblem returns why a role entry is refused, or "" when every
// field is canonical. Sources are re-parsed: a lockfile is repository content.
func roleEntryProblem(e RoleEntry) string {
	if !helpers.IsRoleVersion(e.Version) {
		return fmt.Sprintf("version %q is not a role version", e.Version)
	}
	if dep, ok := firstBadDep(e.Deps); !ok {
		return fmt.Sprintf("dependency %q is not a role install name", dep)
	}
	switch e.Type {
	case RoleTypeGit:
		if reason := roleCommitPinProblem(e); reason != "" {
			return reason
		}
		return gitRoleEntryProblem(e)
	case RoleTypeGalaxy:
		if reason := roleCommitPinProblem(e); reason != "" {
			return reason
		}
		return galaxyRoleEntryProblem(e)
	case RoleTypeURL:
		return urlRoleEntryProblem(e)
	default:
		return fmt.Sprintf("unsupported role type %q", e.Type)
	}
}

// roleCommitPinProblem judges the pin a git or Galaxy role entry carries: a
// canonical ref, a full lowercase commit, and no sha256, which belongs to a
// url role alone.
func roleCommitPinProblem(e RoleEntry) string {
	if ref, err := gitsource.ParseRef(e.Ref); err != nil || e.Ref == "" || ref.Name != e.Ref {
		return fmt.Sprintf("ref %q is not a canonical git ref", e.Ref)
	}
	if !gitsource.IsCommitHash(e.Commit) {
		return fmt.Sprintf("commit %q is not a lowercase 40-hex commit", e.Commit)
	}
	if e.SHA256 != "" {
		return "a git or galaxy role entry carries no sha256"
	}
	return ""
}

// urlRoleEntryProblem judges the pin a url role entry carries: a canonical
// tarball URL as its source, a full lowercase sha256, and none of the fields
// that belong to the other types.
func urlRoleEntryProblem(e RoleEntry) string {
	if u, err := urlsource.ParseURL(e.Source); err != nil || u.String() != e.Source {
		return "source is not a canonical tarball URL"
	}
	if !helpers.IsSHA256Hex(e.SHA256) {
		return fmt.Sprintf("sha256 %q is not a lowercase 64-hex digest", e.SHA256)
	}
	if e.Ref != "" || e.Commit != "" {
		return "ref and commit belong to a git or galaxy role"
	}
	if e.Galaxy != "" || e.Repository != "" {
		return "galaxy and repository belong to a galaxy role"
	}
	return ""
}

// firstBadDep returns the first dependency name outside the role install
// alphabet, or ok when every one is inside it.
func firstBadDep(deps []string) (string, bool) {
	for _, dep := range deps {
		if !helpers.IsRoleInstallName(dep) {
			return dep, false
		}
	}
	return "", true
}

func gitRoleEntryProblem(e RoleEntry) string {
	if u, err := gitsource.ParseURL(e.Source); err != nil || u.String() != e.Source {
		return "source is not a canonical git repository URL"
	}
	if e.Galaxy != "" || e.Repository != "" {
		return "galaxy and repository belong to a galaxy role"
	}
	return ""
}

func galaxyRoleEntryProblem(e RoleEntry) string {
	if !helpers.IsRoleName(e.Galaxy) {
		return fmt.Sprintf("galaxy %q is not owner.role", e.Galaxy)
	}
	if u, err := gitsource.ParseURL(e.Repository); err != nil || u.String() != e.Repository {
		return "repository is not a canonical git repository URL"
	}
	if sourceHasUserinfo(e.Source) {
		return helpers.ErrGalaxyServerURLUserinfo.Error()
	}
	return ""
}

// cloneRoles deep-copies the roles list and each entry's Deps, for
// canonicalClone.
func cloneRoles(roles []RoleEntry) []RoleEntry {
	if roles == nil {
		return nil
	}
	out := make([]RoleEntry, len(roles))
	copy(out, roles)
	for i := range out {
		if len(roles[i].Deps) == 0 {
			continue
		}
		out[i].Deps = slices.Clone(roles[i].Deps)
	}
	return out
}

// canonicalizeRoles sorts the roles by name and each entry's Deps.
func canonicalizeRoles(roles []RoleEntry) {
	slices.SortFunc(roles, func(a, b RoleEntry) int { return strings.Compare(a.Name, b.Name) })
	for i := range roles {
		slices.Sort(roles[i].Deps)
	}
}

// RoleChange is one role present in both before and after whose pinned
// fields differ; see Change.
type RoleChange struct {
	From RoleEntry
	To   RoleEntry
}

// comparedRoleFieldCount is the number of per-role fields Fields can report.
const comparedRoleFieldCount = 9

// Fields reports which of c's pinned fields differ between From and To, in
// a fixed order (type, version, galaxy, source, repository, ref, commit,
// sha256, deps); see Change.Fields.
func (c RoleChange) Fields() []FieldChange {
	fields := make([]FieldChange, 0, comparedRoleFieldCount)
	add := func(name, from, to string) {
		if from != to {
			fields = append(fields, FieldChange{Field: name, From: from, To: to})
		}
	}
	add(fieldType, c.From.Type, c.To.Type)
	add(fieldVersion, c.From.Version, c.To.Version)
	add(fieldGalaxy, c.From.Galaxy, c.To.Galaxy)
	add(fieldSource, c.From.Source, c.To.Source)
	add(fieldRepository, c.From.Repository, c.To.Repository)
	add(fieldRef, c.From.Ref, c.To.Ref)
	add(fieldCommit, c.From.Commit, c.To.Commit)
	add(fieldSHA256, c.From.SHA256, c.To.SHA256)
	if !sameDeps(c.From.Deps, c.To.Deps) {
		fields = append(fields, FieldChange{Field: fieldDeps, From: renderDeps(c.From.Deps), To: renderDeps(c.To.Deps)})
	}
	return fields
}

// sameRoleEntry reports whether a and b pin the same role; Name is not
// compared, as in sameEntry.
func sameRoleEntry(a, b RoleEntry) bool {
	return a.Type == b.Type && a.Version == b.Version && a.Galaxy == b.Galaxy && a.Source == b.Source &&
		a.Repository == b.Repository && a.Ref == b.Ref && a.Commit == b.Commit && a.SHA256 == b.SHA256 &&
		sameDeps(a.Deps, b.Deps)
}

// indexRolesByName builds a name-keyed index of f's roles; a nil f yields
// an empty map, as indexByName does.
func indexRolesByName(f *File) map[string]RoleEntry {
	if f == nil {
		return map[string]RoleEntry{}
	}
	idx := make(map[string]RoleEntry, len(f.Roles))
	for _, e := range f.Roles {
		idx[e.Name] = e
	}
	return idx
}

// diffRoles builds RolesAdded/RolesUpdated/RolesRemoved over the sorted
// name union of the two indexes, as diffIndexes does for collections.
func diffRoles(beforeIdx, afterIdx map[string]RoleEntry) ([]RoleEntry, []RoleChange, []RoleEntry) {
	names := unionSortedRoleNames(beforeIdx, afterIdx)
	var added, removed []RoleEntry
	var updated []RoleChange
	for _, name := range names {
		a, inAfter := afterIdx[name]
		b, inBefore := beforeIdx[name]
		switch {
		case inAfter && !inBefore:
			added = append(added, a)
		case inAfter && inBefore:
			if !sameRoleEntry(a, b) {
				updated = append(updated, RoleChange{From: b, To: a})
			}
		case !inAfter && inBefore:
			removed = append(removed, b)
		}
	}
	return added, updated, removed
}

// unionSortedRoleNames is unionSortedNames for the role indexes.
func unionSortedRoleNames(beforeIdx, afterIdx map[string]RoleEntry) []string {
	names := make([]string, 0, len(beforeIdx)+len(afterIdx))
	for name := range afterIdx {
		names = append(names, name)
	}
	for name := range beforeIdx {
		if _, ok := afterIdx[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
