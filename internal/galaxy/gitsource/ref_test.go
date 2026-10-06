package gitsource

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

type refCase struct {
	wantErr  error
	name     string
	raw      string
	wantName string
	wantKind RefKind
}

func refCases() []refCase {
	return []refCase{
		{name: "empty is HEAD", raw: "", wantName: "HEAD", wantKind: RefHEAD},
		{name: "star is HEAD", raw: "*", wantName: "HEAD", wantKind: RefHEAD},
		{name: "HEAD", raw: "HEAD", wantName: "HEAD", wantKind: RefHEAD},
		{name: "branch", raw: "main", wantName: "main", wantKind: RefName},
		{name: "branch with slash", raw: "feature/x-1.2", wantName: "feature/x-1.2", wantKind: RefName},
		{name: "tag-like", raw: "v1.2.3", wantName: "v1.2.3", wantKind: RefName},
		{name: "short hex name below the abbreviation floor", raw: "beef1", wantName: "beef1", wantKind: RefName},
		{name: "commit", raw: testCommit, wantName: testCommit, wantKind: RefCommit},
		{name: "commit upper-cased", raw: strings.ToUpper(testCommit), wantName: testCommit, wantKind: RefCommit},
		{name: "qualified branch", raw: "refs/heads/deadbeef", wantName: "refs/heads/deadbeef", wantKind: RefQualified},
		{name: "qualified tag", raw: "refs/tags/1.0", wantName: "refs/tags/1.0", wantKind: RefQualified},
		{name: "abbreviated 7", raw: "deadbee", wantErr: helpers.ErrGitAbbreviatedCommit},
		{name: "abbreviated 39", raw: testCommit[:39], wantErr: helpers.ErrGitAbbreviatedCommit},
		{name: "other refs prefix", raw: "refs/remotes/origin/main", wantErr: helpers.ErrInvalidGitRef},
		{name: "double dot", raw: "a..b", wantErr: helpers.ErrInvalidGitRef},
		{name: "at brace", raw: "a@{1}", wantErr: helpers.ErrInvalidGitRef},
		{name: "leading dash", raw: "-x", wantErr: helpers.ErrInvalidGitRef},
		{name: "leading dash component", raw: "a/-x", wantErr: helpers.ErrInvalidGitRef},
		{name: "trailing lock", raw: "a.lock", wantErr: helpers.ErrInvalidGitRef},
		{name: "trailing slash", raw: "a/", wantErr: helpers.ErrInvalidGitRef},
		{name: "leading slash", raw: "/a", wantErr: helpers.ErrInvalidGitRef},
		{name: "double slash", raw: "a//b", wantErr: helpers.ErrInvalidGitRef},
		{name: "trailing dot", raw: "a.", wantErr: helpers.ErrInvalidGitRef},
		{name: "dot component", raw: "a/.b", wantErr: helpers.ErrInvalidGitRef},
		{name: "space", raw: "a b", wantErr: helpers.ErrInvalidGitRef},
		{name: "tilde", raw: "a~1", wantErr: helpers.ErrInvalidGitRef},
		{name: "caret", raw: "a^", wantErr: helpers.ErrInvalidGitRef},
		{name: "colon", raw: "a:b", wantErr: helpers.ErrInvalidGitRef},
		{name: "question", raw: "a?", wantErr: helpers.ErrInvalidGitRef},
		{name: "star inside", raw: "a*", wantErr: helpers.ErrInvalidGitRef},
		{name: "bracket", raw: "a[", wantErr: helpers.ErrInvalidGitRef},
		{name: "backslash", raw: `a\b`, wantErr: helpers.ErrInvalidGitRef},
		{name: "control rune", raw: "a\x01", wantErr: helpers.ErrInvalidGitRef},
		{name: "del rune", raw: "a\x7f", wantErr: helpers.ErrInvalidGitRef},
		{name: "bare at", raw: "@", wantErr: helpers.ErrInvalidGitRef},
		{name: "qualified with bad component", raw: "refs/heads/a..b", wantErr: helpers.ErrInvalidGitRef},
	}
}

func TestParseRef(t *testing.T) {
	t.Parallel()
	for _, tt := range refCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRef(tt.raw)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ParseRef(%q) error = %v, want %v", tt.raw, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRef(%q): %v", tt.raw, err)
			}
			if got.Name != tt.wantName || got.Kind != tt.wantKind {
				t.Fatalf("ParseRef(%q) = %+v, want name %q kind %d", tt.raw, got, tt.wantName, tt.wantKind)
			}
			if got.IsCommit() != (tt.wantKind == RefCommit) {
				t.Fatalf("IsCommit() = %t for kind %d", got.IsCommit(), got.Kind)
			}
		})
	}
}

// TestParseRefNamesNoURL pins that a URL typed as a ref, bare or behind a
// refs/ prefix, is refused without the message quoting it.
func TestParseRefNamesNoURL(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://u:s3cret@h.example/x",
		"refs/heads/https://u:s3cret@h.example/x",
		"refs/other/https://u:s3cret@h.example/x",
	} {
		_, err := ParseRef(raw)
		if !errors.Is(err, helpers.ErrInvalidGitRef) {
			t.Fatalf("ParseRef(%q) error = %v, want %v", raw, err, helpers.ErrInvalidGitRef)
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Fatalf("ParseRef(%q) error %q quotes the URL", raw, err)
		}
	}
}

func TestIsCommitHash(t *testing.T) {
	t.Parallel()
	if !IsCommitHash(testCommit) {
		t.Fatalf("IsCommitHash rejected a canonical commit")
	}
	for _, bad := range []string{"", testCommit[:39], testCommit + "0", strings.ToUpper(testCommit), strings.Repeat("g", 40)} {
		if IsCommitHash(bad) {
			t.Fatalf("IsCommitHash(%q) accepted", bad)
		}
	}
}

// TestRefAdmits pins which commit a pin recorded under a ref may name: any
// canonical hash under HEAD, a branch or a tag, under a commit ref that
// commit alone, and never a malformed one.
func TestRefAdmits(t *testing.T) {
	t.Parallel()
	const other = "fedcba9876543210fedcba9876543210fedcba98"
	for _, tc := range []struct {
		raw, commit string
		want        bool
	}{
		{raw: "HEAD", commit: testCommit, want: true},
		{raw: "main", commit: other, want: true},
		{raw: "refs/tags/1.0", commit: testCommit, want: true},
		{raw: testCommit, commit: testCommit, want: true},
		{raw: strings.ToUpper(testCommit), commit: testCommit, want: true},
		{raw: testCommit, commit: other, want: false},
		{raw: testCommit, commit: strings.ToUpper(testCommit), want: false},
		{raw: "main", commit: testCommit[:39], want: false},
		{raw: "main", commit: "", want: false},
	} {
		ref, err := ParseRef(tc.raw)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", tc.raw, err)
		}
		if got := ref.Admits(tc.commit); got != tc.want {
			t.Fatalf("ParseRef(%q).Admits(%q) = %t, want %t", tc.raw, tc.commit, got, tc.want)
		}
	}
}
