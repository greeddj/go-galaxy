package collections

import (
	"context"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// shortCommitLen is how many hex digits of a commit an install line shows,
// as many as a Go pseudo-version carries.
const shortCommitLen = 12

// maxCommitYear is the last year a commit time is shown and recorded for:
// RFC 3339 and the snapshot's JSON encoding both stop there.
const maxCommitYear = 9999

// refCanMove reports whether a canonical ref may name another commit later:
// HEAD, a refs/heads/ branch, or a name that does not read as a version. A
// commit never moves.
func refCanMove(ref string) bool {
	parsed, err := gitsource.ParseRef(ref)
	if err != nil {
		return false
	}
	switch parsed.Kind {
	case gitsource.RefHEAD:
		return true
	case gitsource.RefCommit:
		return false
	case gitsource.RefQualified:
		if strings.HasPrefix(parsed.Name, "refs/heads/") {
			return true
		}
		return !helpers.IsExactVersion(strings.TrimPrefix(parsed.Name, "refs/tags/"))
	case gitsource.RefName:
		return !helpers.IsExactVersion(parsed.Name)
	}
	return false
}

// gitCommitNote is the install-line note of a git source whose ref can move:
// "(<commit>, <time>)", or "(<commit>)" with no usable time. Any other source
// gets "".
func gitCommitNote(source, ref string, when time.Time) string {
	if !gitsource.IsLocator(source) || !refCanMove(ref) {
		return ""
	}
	loc, err := gitsource.ParseLocator(source)
	if err != nil || !gitsource.IsCommitHash(loc.Commit) {
		return ""
	}
	short := loc.Commit[:shortCommitLen]
	if !usableCommitTime(when) {
		return "(" + short + ")"
	}
	return "(" + short + ", " + when.UTC().Format(time.RFC3339) + ")"
}

// usableCommitTime reports whether when is after the Unix epoch, which url
// builds and unparsable git times stamp, and within the years RFC 3339 spells.
func usableCommitTime(when time.Time) bool {
	return when.Unix() > 0 && when.UTC().Year() <= maxCommitYear
}

// gitArtifactCommitTime reads the committer time a git build stamped on the
// artifact at path; zero otherwise. The artifact may come from a shared cache,
// so callers only print the time.
func gitArtifactCommitTime(ctx context.Context, out output.Printer, subject, source, path string) time.Time {
	if !gitsource.IsLocator(source) || path == "" {
		return time.Time{}
	}
	when, err := archive.FirstEntryModTime(ctx, path)
	if err != nil {
		out.Debugf("%s: no commit time in the artifact: %v", subject, err)
		return time.Time{}
	}
	if !usableCommitTime(when) {
		return time.Time{}
	}
	return when
}

// installedCollectionNote is col's install-line note, its time read from the
// install record, which both an install and a skip leave describing col.
func installedCollectionNote(st *store.Store, col collection) string {
	var when time.Time
	if entry, ok := st.GetInstalled(col.key()); ok && entry.Source == col.Source {
		when = entry.CommitTime
	}
	return gitCommitNote(col.Source, col.Ref, when)
}

// installedRoleNote is installedCollectionNote for a role.
func installedRoleNote(st *store.Store, r resolvedRole) string {
	var when time.Time
	if entry, ok := st.GetInstalledRole(r.Name); ok && entry.Source == r.Source {
		when = entry.CommitTime
	}
	return gitCommitNote(r.Source, r.Ref, when)
}
