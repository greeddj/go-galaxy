package collections

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// noteCommit is a hand-written commit, so a test sees which twelve digits the
// note keeps; the suite's fakeCommit values all start with zeros.
const (
	noteCommit     = "d4e99adaaa78873baaafbdb953a001315081c0f0"
	noteRepository = "https://git.example/acme/ansible-role-app.git"
)

// noteTime is a committer time in a non-UTC zone, so a test sees the note
// convert it.
func noteTime() time.Time {
	return time.Date(2026, time.October, 29, 16, 55, 0, 0, time.FixedZone("", 3*60*60))
}

func noteLocator() string {
	return gitsource.Locator{URL: noteRepository, Commit: noteCommit}.String()
}

// TestRefCanMove pins which canonical refs can move: HEAD, every refs/heads/
// branch and a name that is not version-like; a version-like name, a commit
// and an unparsable ref cannot.
func TestRefCanMove(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string]bool{
		"HEAD":                true,
		"main":                true,
		"release-1.x":         true,
		"2.x":                 true,
		"v1.0.0":              false,
		"1.0.0":               false,
		"1.2":                 false,
		"refs/heads/main":     true,
		"refs/heads/1.0.0":    true,
		"refs/tags/v1.0.0":    false,
		"refs/tags/release-1": true,
		noteCommit:            false,
		"refs/remotes/x":      false,
	} {
		if got := refCanMove(ref); got != want {
			t.Errorf("refCanMove(%q) = %v, want %v", ref, got, want)
		}
	}
}

// TestGitCommitNote pins the note's two spellings and every case that gets
// none: a ref that cannot move, and a source that is not a git commit.
func TestGitCommitNote(t *testing.T) {
	t.Parallel()
	urlLocator := "url+https://files.example/app.tar.gz#sha256:" + strings.Repeat("a", 64)
	commitless := gitsource.Locator{URL: noteRepository}.String()
	tests := []struct {
		when   time.Time
		name   string
		source string
		ref    string
		want   string
	}{
		{name: "moving ref with a time", source: noteLocator(), ref: "main", when: noteTime(), want: "(d4e99adaaa78, 2026-10-29T13:55:00Z)"},
		{name: "moving ref with no time", source: noteLocator(), ref: "HEAD", want: "(d4e99adaaa78)"},
		{name: "epoch time reads as none", source: noteLocator(), ref: "main", when: time.Unix(0, 0), want: "(d4e99adaaa78)"},
		{
			name: "time past RFC 3339 reads as none", source: noteLocator(), ref: "main",
			when: time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC), want: "(d4e99adaaa78)",
		},
		{name: "semver tag", source: noteLocator(), ref: "v1.0.0", when: noteTime()},
		{name: "commit ref", source: noteLocator(), ref: noteCommit, when: noteTime()},
		{name: "url source", source: urlLocator, ref: "", when: noteTime()},
		{name: "Galaxy source", source: "https://galaxy.example/api/", ref: "", when: noteTime()},
		{name: "git locator with no commit", source: commitless, ref: "main", when: noteTime()},
	}
	for _, tt := range tests {
		if got := gitCommitNote(tt.source, tt.ref, tt.when); got != tt.want {
			t.Errorf("%s: gitCommitNote = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestInstalledNotesReadTheMatchingRecord pins that a note's time comes only
// from an install record of the same source, so a record another source left
// under the key never dates this install.
func TestInstalledNotesReadTheMatchingRecord(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "app", Version: "1.2.3", Source: noteLocator(), Ref: "main", Type: typeGit}
	role := resolvedRole{Name: "app", Source: noteLocator(), Ref: "main", Version: "main"}
	const dated, undated = "(d4e99adaaa78, 2026-10-29T13:55:00Z)", "(d4e99adaaa78)"

	if got := installedCollectionNote(nil, col); got != undated {
		t.Fatalf("collection note with no store = %q, want %q", got, undated)
	}
	if got := installedRoleNote(nil, role); got != undated {
		t.Fatalf("role note with no store = %q, want %q", got, undated)
	}

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{Source: col.Source, CommitTime: noteTime()})
	st.SetInstalledRole(role.Name, store.InstalledRoleEntry{Source: role.Source, CommitTime: noteTime()})
	if got := installedCollectionNote(st, col); got != dated {
		t.Fatalf("collection note = %q, want %q", got, dated)
	}
	if got := installedRoleNote(st, role); got != dated {
		t.Fatalf("role note = %q, want %q", got, dated)
	}

	other := store.New()
	elsewhere := gitsource.Locator{URL: noteRepository, Commit: strings.Repeat("b", 40)}.String()
	other.SetInstalled(col.key(), store.InstalledEntry{Source: elsewhere, CommitTime: noteTime()})
	other.SetInstalledRole(role.Name, store.InstalledRoleEntry{Source: elsewhere, CommitTime: noteTime()})
	if got := installedCollectionNote(other, col); got != undated {
		t.Fatalf("collection note from another source's record = %q, want %q", got, undated)
	}
	if got := installedRoleNote(other, role); got != undated {
		t.Fatalf("role note from another source's record = %q, want %q", got, undated)
	}
}

// writeStampedArtifact writes a one-entry tar.gz stamped with when.
func writeStampedArtifact(t *testing.T, when time.Time) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "meta/main.yml", Mode: 0o644, ModTime: when}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	path := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return path
}

// TestGitArtifactCommitTime pins that only a git artifact is read, that an
// epoch or post-9999 stamp reads as no time, and that an unreadable one reads
// as none and logs why.
func TestGitArtifactCommitTime(t *testing.T) {
	t.Parallel()
	stamped := writeStampedArtifact(t, noteTime())
	out := &capturingPrinter{}

	if got := gitArtifactCommitTime(t.Context(), out, "role app", noteLocator(), stamped); !got.Equal(noteTime()) {
		t.Fatalf("git artifact time = %v, want %v", got, noteTime())
	}
	if got := gitArtifactCommitTime(t.Context(), out, "role app", "url+https://files.example/app.tar.gz#sha256:"+
		strings.Repeat("a", 64), stamped); !got.IsZero() {
		t.Fatalf("url artifact time = %v, want zero: only a git build stamps its commit time", got)
	}
	epoch := writeStampedArtifact(t, time.Unix(0, 0))
	if got := gitArtifactCommitTime(t.Context(), out, "role app", noteLocator(), epoch); !got.IsZero() {
		t.Fatalf("epoch-stamped artifact time = %v, want zero", got)
	}
	far := writeStampedArtifact(t, time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC))
	if got := gitArtifactCommitTime(t.Context(), out, "role app", noteLocator(), far); !got.IsZero() {
		t.Fatalf("post-9999 artifact time = %v, want zero: the snapshot's JSON cannot encode it", got)
	}
	if len(out.debugs) != 0 {
		t.Fatalf("readable artifacts logged %q, want nothing", out.debugs)
	}

	missing := filepath.Join(t.TempDir(), "gone.tar.gz")
	if got := gitArtifactCommitTime(t.Context(), out, "role app", noteLocator(), missing); !got.IsZero() {
		t.Fatalf("missing artifact time = %v, want zero", got)
	}
	if len(out.debugs) != 1 || !strings.HasPrefix(out.debugs[0], "role app: no commit time in the artifact: ") {
		t.Fatalf("missing artifact logged %q, want one debug line naming the role", out.debugs)
	}
}
