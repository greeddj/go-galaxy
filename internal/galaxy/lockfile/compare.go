package lockfile

import (
	"slices"
	"strings"
)

// Field-name constants for FieldChange.Field. Fixed strings rather than an
// enum, since FieldChange is meant to be rendered directly by a caller (see
// internal/galaxy/collections/lock.go's renderFieldChanges).
const (
	fieldVersion = "version"
	fieldSource  = "source"
	// fieldDownloadURL belongs to a Galaxy entry alone.
	fieldDownloadURL = "download_url"
	fieldType        = "type"
	fieldRef         = "ref"
	fieldCommit      = "commit"
	fieldSubdir      = "subdir"
	fieldSHA256      = "sha256"
	fieldDeps        = "deps"
	fieldServer      = "server"
	// fieldGalaxy and fieldRepository belong to a role entry alone.
	fieldGalaxy     = "galaxy"
	fieldRepository = "repository"
)

// comparedFieldCount is the number of per-entry fields Change.Fields can
// report, which pre-sizes its result; Server is file-level, on Diff.Server.
const comparedFieldCount = 9

// Diff is what Compare(before, after) found: which collections after would
// add, update, or remove relative to before, plus whether the file-level
// Server field itself changed.
type Diff struct {
	// Server is set only when both files are non-nil and their Server fields
	// differ.
	Server  *FieldChange
	Added   []Entry
	Updated []Change
	Removed []Entry
	// RolesAdded, RolesUpdated and RolesRemoved are the roles list's half of
	// the diff, keyed by install name exactly as the collections are keyed
	// by fqdn.
	RolesAdded   []RoleEntry
	RolesUpdated []RoleChange
	RolesRemoved []RoleEntry
}

// Change is one collection in both files whose pinned fields differ. From
// and To carry whole entries; Fields reports which fields differ.
type Change struct {
	From Entry
	To   Entry
}

// FieldChange names one differing field and its old and new values: a
// per-entry field via Change.Fields, or "server" via Diff.Server.
type FieldChange struct {
	Field string
	From  string
	To    string
}

// Compare reports how after differs from before, keyed by name and blind to
// order, never mutating either; a nil file has no entries. Diff.Empty must
// equal Hash equality, so a field Hash covers must be compared here.
func Compare(before, after *File) Diff {
	beforeIdx := indexByName(before)
	afterIdx := indexByName(after)
	diff := diffIndexes(beforeIdx, afterIdx)
	diff.Server = serverFieldChange(before, after)
	diff.RolesAdded, diff.RolesUpdated, diff.RolesRemoved = diffRoles(indexRolesByName(before), indexRolesByName(after))
	return diff
}

// diffIndexes builds Added/Updated/Removed in one pass over the sorted union
// of the two indexes' names.
func diffIndexes(beforeIdx, afterIdx map[string]Entry) Diff {
	var diff Diff
	for _, name := range unionSortedNames(beforeIdx, afterIdx) {
		a, inAfter := afterIdx[name]
		b, inBefore := beforeIdx[name]
		switch {
		case inAfter && !inBefore:
			diff.Added = append(diff.Added, a)
		case inAfter && inBefore:
			if !sameEntry(a, b) {
				diff.Updated = append(diff.Updated, Change{From: b, To: a})
			}
		case !inAfter && inBefore:
			diff.Removed = append(diff.Removed, b)
		}
	}
	return diff
}

// unionSortedNames returns the sorted union of beforeIdx's and afterIdx's
// keys, each name once.
func unionSortedNames(beforeIdx, afterIdx map[string]Entry) []string {
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

// serverFieldChange returns the file-level Server FieldChange between before
// and after, or nil when either is nil (no pair of file-level values to
// compare) or their Server fields agree.
func serverFieldChange(before, after *File) *FieldChange {
	if before == nil || after == nil || before.Server == after.Server {
		return nil
	}
	return &FieldChange{Field: fieldServer, From: before.Server, To: after.Server}
}

// Empty reports whether d describes no difference at all: no collection or
// role added, updated, or removed, and no file-level server change.
func (d Diff) Empty() bool {
	return d.Server == nil && len(d.Added) == 0 && len(d.Updated) == 0 && len(d.Removed) == 0 &&
		len(d.RolesAdded) == 0 && len(d.RolesUpdated) == 0 && len(d.RolesRemoved) == 0
}

// HasRoles reports whether either side of the diff involved a role, for a
// report that names roles only when a run has any.
func (d Diff) HasRoles() bool {
	return len(d.RolesAdded) > 0 || len(d.RolesUpdated) > 0 || len(d.RolesRemoved) > 0
}

// Fields reports which of c's pinned fields differ, in a fixed order, each
// with its old and new value. It compares what sameEntry compares, so Updated
// and Fields cannot drift; Deps compare and render order-insensitively.
func (c Change) Fields() []FieldChange {
	fields := make([]FieldChange, 0, comparedFieldCount)
	if c.From.Version != c.To.Version {
		fields = append(fields, FieldChange{Field: fieldVersion, From: c.From.Version, To: c.To.Version})
	}
	if c.From.Source != c.To.Source {
		fields = append(fields, FieldChange{Field: fieldSource, From: c.From.Source, To: c.To.Source})
	}
	if c.From.DownloadURL != c.To.DownloadURL {
		fields = append(fields, FieldChange{Field: fieldDownloadURL, From: c.From.DownloadURL, To: c.To.DownloadURL})
	}
	if c.From.Type != c.To.Type {
		fields = append(fields, FieldChange{Field: fieldType, From: c.From.Type, To: c.To.Type})
	}
	if c.From.Ref != c.To.Ref {
		fields = append(fields, FieldChange{Field: fieldRef, From: c.From.Ref, To: c.To.Ref})
	}
	if c.From.Commit != c.To.Commit {
		fields = append(fields, FieldChange{Field: fieldCommit, From: c.From.Commit, To: c.To.Commit})
	}
	if c.From.Subdir != c.To.Subdir {
		fields = append(fields, FieldChange{Field: fieldSubdir, From: c.From.Subdir, To: c.To.Subdir})
	}
	if c.From.SHA256 != c.To.SHA256 {
		fields = append(fields, FieldChange{Field: fieldSHA256, From: c.From.SHA256, To: c.To.SHA256})
	}
	if !sameDeps(c.From.Deps, c.To.Deps) {
		fields = append(fields, FieldChange{Field: fieldDeps, From: renderDeps(c.From.Deps), To: renderDeps(c.To.Deps)})
	}
	return fields
}

// indexByName builds a name-keyed index of f's collections; a nil f yields an
// empty map, and a duplicate name resolves last-entry-wins like indexLockfile.
func indexByName(f *File) map[string]Entry {
	if f == nil {
		return map[string]Entry{}
	}
	idx := make(map[string]Entry, len(f.Collections))
	for _, e := range f.Collections {
		idx[e.Name] = e
	}
	return idx
}

// sameEntry reports whether a and b pin the same collection: every field but
// Name, which Compare has already matched.
func sameEntry(a, b Entry) bool {
	return a.Version == b.Version && a.Source == b.Source && a.DownloadURL == b.DownloadURL && a.Type == b.Type &&
		a.Ref == b.Ref && a.Commit == b.Commit && a.Subdir == b.Subdir &&
		a.SHA256 == b.SHA256 && sameDeps(a.Deps, b.Deps)
}

// sameDeps reports whether a and b are the same multiset of names, ignoring
// order but not duplicates, as Hash sorts and never deduplicates. Sorted
// clones run only when the length and slices.Equal checks cannot decide.
func sameDeps(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	if slices.Equal(a, b) {
		return true
	}
	ac := slices.Clone(a)
	bc := slices.Clone(b)
	slices.Sort(ac)
	slices.Sort(bc)
	return slices.Equal(ac, bc)
}

// renderDeps renders deps sorted and comma-joined with duplicates kept, as
// sameDeps compares them; no deps render as "", shown as "(none)" by the
// collections package's quoteEmpty.
func renderDeps(deps []string) string {
	if len(deps) == 0 {
		return ""
	}
	sorted := slices.Clone(deps)
	slices.Sort(sorted)
	return strings.Join(sorted, ",")
}
