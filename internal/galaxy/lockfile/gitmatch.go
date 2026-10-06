package lockfile

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// GitRequirement is a git collection requirement as MatchGitRequirements reads
// it: the canonical repository URL, subdir and ref it asks for, and the fqdn
// it names, or "" when it names none.
type GitRequirement struct {
	URL    string
	Subdir string
	Ref    string
	FQDN   string
}

// GitMatch is what one requirement answers for, each list in name order: the
// entries it owns, and the entries no requirement owns that are charged to it
// as its ref mismatches.
type GitMatch struct {
	Requirement GitRequirement
	Owned       []Entry
	Mismatched  []Entry
}

// MatchGitRequirements gives each git entry to its nearest candidate asking for
// its ref, else as a mismatch to its nearest candidate, and returns one GitMatch
// per requirement in reqs order; docs/internals/lockfile-format.md has the rule.
func MatchGitRequirements(reqs []GitRequirement, entries []Entry) []GitMatch {
	matches := make([]GitMatch, len(reqs))
	for i, req := range reqs {
		matches[i].Requirement = req
	}
	byName := slices.SortedStableFunc(slices.Values(entries), func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	for _, entry := range byName {
		owner, candidate := gitEntryAssignees(reqs, entry)
		switch {
		case owner >= 0:
			matches[owner].Owned = append(matches[owner].Owned, entry)
		case candidate >= 0:
			matches[candidate].Mismatched = append(matches[candidate].Mismatched, entry)
		}
	}
	return matches
}

// gitEntryAssignees returns the indexes in reqs of entry's nearest owner, a
// candidate asking for its ref, and of its nearest candidate, -1 for none; of
// two equally near, the earlier wins.
func gitEntryAssignees(reqs []GitRequirement, entry Entry) (int, int) {
	owner, candidate := -1, -1
	for i, req := range reqs {
		if !isGitCandidate(req, entry) {
			continue
		}
		if candidate < 0 || nearerGitCandidate(req, reqs[candidate], entry) {
			candidate = i
		}
		if req.Ref == entry.Ref && (owner < 0 || nearerGitCandidate(req, reqs[owner], entry)) {
			owner = i
		}
	}
	return owner, candidate
}

// isGitCandidate reports whether req could have locked entry: a git entry from
// req's repository at req's subdir or an immediate child of it, and under req's
// fqdn when req names one.
func isGitCandidate(req GitRequirement, entry Entry) bool {
	return entry.IsGit() && entry.Source == req.URL && subdirWithin(entry.Subdir, req.Subdir) &&
		(req.FQDN == "" || req.FQDN == entry.Name)
}

// nearerGitCandidate reports whether a sits at entry's own subdir and b at its
// parent, the only two places a candidate can sit.
func nearerGitCandidate(a, b GitRequirement, entry Entry) bool {
	return a.Subdir == entry.Subdir && b.Subdir != entry.Subdir
}

// subdirWithin reports whether an entry's subdir is the requirement's own
// subdir or an immediate child of it.
func subdirWithin(entrySubdir, reqSubdir string) bool {
	if entrySubdir == reqSubdir {
		return true
	}
	parent := path.Dir(entrySubdir)
	if parent == "." {
		parent = ""
	}
	return parent == reqSubdir
}

// Err is the requirement's verdict, a helpers.ErrLockfileMismatch naming its
// first mismatch by name, else, when it owns no entry, that it has none; nil
// when it owns one and is charged none.
func (m GitMatch) Err() error {
	display := helpers.URLForMessage(m.Requirement.URL)
	switch {
	case len(m.Mismatched) > 0:
		return fmt.Errorf("%w: git root %s locked from ref %q, requirements ask for %q",
			helpers.ErrLockfileMismatch, display, m.Mismatched[0].Ref, m.Requirement.Ref)
	case len(m.Owned) > 0:
		return nil
	case m.Requirement.FQDN != "":
		return fmt.Errorf("%w: git root %s has no lockfile entry for %s",
			helpers.ErrLockfileMismatch, display, m.Requirement.FQDN)
	default:
		return fmt.Errorf("%w: git root %s has no lockfile entry", helpers.ErrLockfileMismatch, display)
	}
}
