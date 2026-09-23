package requirements

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const gitTestCommit = "0123456789abcdef0123456789abcdef01234567"

// gitAcceptedCase is one accepted git shape: the input and the single
// requirement it must produce.
type gitAcceptedCase struct {
	name  string
	input string
	want  CollectionRequirement
}

func gitAcceptedCases() []gitAcceptedCase {
	return append(gitAcceptedStringCases(), gitAcceptedMapCases()...)
}

// gitAcceptedStringCases are the scalar list items ansible auto-detects as
// git sources.
func gitAcceptedStringCases() []gitAcceptedCase {
	return []gitAcceptedCase{
		{
			name:  "string git+https",
			input: "- git+https://github.com/acme/app.git\n",
			want:  CollectionRequirement{Type: TypeGit, Source: "https://github.com/acme/app.git", Ref: "HEAD"},
		},
		{
			name:  "string scp-like",
			input: "- git@github.com:acme/app.git\n",
			want:  CollectionRequirement{Type: TypeGit, Source: "git@github.com:acme/app.git", Ref: "HEAD"},
		},
		{
			name:  "string with comma ref and subdir",
			input: "- git+https://github.com/acme/mono.git#collections/app,v1.2.3\n",
			want:  CollectionRequirement{Type: TypeGit, Source: "https://github.com/acme/mono.git", Ref: "v1.2.3", Subdir: "collections/app"},
		},
		{
			// ansible's parse_scm cuts the comma first, so a fragment after the
			// ref stays in the ref ("main#sub") and fails as ansible's does.
			name:  "string ref then fragment keeps ansible's order",
			input: "- git+https://github.com/acme/app.git,main#sub\n",
			want:  CollectionRequirement{Type: TypeGit, Source: "https://github.com/acme/app.git", Ref: "main#sub"},
		},
	}
}

// gitAcceptedMapCases are the mapping items: type: git, a git pointer in
// name:, and the source:-plus-identity spellings.
func gitAcceptedMapCases() []gitAcceptedCase {
	return []gitAcceptedCase{
		{
			name:  "map type git with name url and version",
			input: "- name: https://github.com/acme/app.git\n  type: git\n  version: main\n",
			want:  CollectionRequirement{Type: TypeGit, Source: "https://github.com/acme/app.git", Ref: "main"},
		},
		{
			name:  "map git pointer without type",
			input: "- name: git+ssh://git@gitlab.example/group/app.git\n  version: refs/tags/v2\n",
			want:  CollectionRequirement{Type: TypeGit, Source: "ssh://git@gitlab.example/group/app.git", Ref: "refs/tags/v2"},
		},
		{
			name:  "map comma suffix wins over version",
			input: "- name: git+https://github.com/acme/app.git,dev\n  version: main\n",
			want:  CollectionRequirement{Type: TypeGit, Source: "https://github.com/acme/app.git", Ref: "dev"},
		},
		{
			name:  "map commit ref",
			input: "- name: https://github.com/acme/app.git\n  type: git\n  version: " + strings.ToUpper(gitTestCommit) + "\n",
			want:  CollectionRequirement{Type: TypeGit, Source: "https://github.com/acme/app.git", Ref: gitTestCommit},
		},
		{
			name:  "map star version is HEAD",
			input: "- name: https://github.com/acme/app.git\n  type: git\n  version: '*'\n",
			want:  CollectionRequirement{Type: TypeGit, Source: "https://github.com/acme/app.git", Ref: "HEAD"},
		},
		{
			name:  "explicit identity beside a git source",
			input: "- name: acme.app\n  type: git\n  source: https://github.com/acme/mono.git\n  version: main\n",
			want:  CollectionRequirement{Namespace: "acme", Name: "app", Type: TypeGit, Source: "https://github.com/acme/mono.git", Ref: "main"},
		},
		{
			name:  "explicit namespace and name beside a git source",
			input: "- namespace: acme\n  name: app\n  type: git\n  source: git@github.com:acme/mono.git#apps/app\n",
			want: CollectionRequirement{
				Namespace: "acme", Name: "app", Type: TypeGit, Source: "git@github.com:acme/mono.git", Ref: "HEAD", Subdir: "apps/app",
			},
		},
		{
			name:  "type case-insensitive and trimmed",
			input: "- name: https://github.com/acme/app.git\n  type: ' Git '\n",
			want:  CollectionRequirement{Type: TypeGit, Source: "https://github.com/acme/app.git", Ref: "HEAD"},
		},
	}
}

func TestParseCollectionsAcceptsGitShapes(t *testing.T) {
	t.Parallel()
	for _, tc := range gitAcceptedCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, err := Parse([]byte(tc.input), "https://default")
			collections, rolesFound := f.Collections, len(f.Roles) > 0
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			if rolesFound || len(collections) != 1 {
				t.Fatalf("unexpected result: roles=%t collections=%#v", rolesFound, collections)
			}
			if got := collections[0]; !sameGitRequirement(got, tc.want) || !got.IsGit() {
				t.Fatalf("Parse = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// sameGitRequirement compares the fields a git requirement carries and
// insists the Galaxy-only ones (version, signatures) stay empty.
func sameGitRequirement(got, want CollectionRequirement) bool {
	return got.Namespace == want.Namespace && got.Name == want.Name && got.Type == want.Type &&
		got.Source == want.Source && got.Ref == want.Ref && got.Subdir == want.Subdir &&
		got.Version == "" && len(got.Signatures) == 0
}

func gitRejectedCases() []parseCollectionsRejectedCase {
	return []parseCollectionsRejectedCase{
		{ //nolint:gosec // a fixture URL, not a credential
			name: "credential in git url", input: "- git+https://ci:s3cret@github.com/acme/app.git\n",
			wantErr: helpers.ErrGitURLUserinfo, mustNotContain: "s3cret",
		},
		{ //nolint:gosec // a fixture URL, not a credential
			name: "credential in map git url", input: "- name: https://ci:s3cret@github.com/acme/app.git\n  type: git\n",
			wantErr: helpers.ErrGitURLUserinfo, mustNotContain: "s3cret",
		},
		{name: "git+file", input: "- git+file:///srv/repo\n", wantErr: helpers.ErrInvalidGitURL},
		{name: "git scheme", input: "- name: git://host.example/repo\n  type: git\n", wantErr: helpers.ErrInvalidGitURL},
		{name: "ssh without user", input: "- name: ssh://host.example/repo\n  type: git\n", wantErr: helpers.ErrInvalidGitURL},
		{name: "abbreviated commit", input: "- git+https://github.com/acme/app.git,0123abc\n", wantErr: helpers.ErrGitAbbreviatedCommit},
		{
			name: "invalid ref", input: "- name: https://github.com/acme/app.git\n  type: git\n  version: 'a..b'\n",
			wantErr: helpers.ErrInvalidGitRef,
		},
		{name: "unsafe subdir", input: "- git+https://github.com/acme/app.git#../etc\n", wantErr: helpers.ErrInvalidGitSubdir},
		{
			name:    "signatures on a git entry",
			input:   "- name: https://github.com/acme/app.git\n  type: git\n  signatures:\n    - https://sig.example/x.asc\n",
			wantErr: helpers.ErrInvalidCollectionEntry,
		},
		{
			name:    "name and source both repositories",
			input:   "- name: git+https://h.example/a.git\n  type: git\n  source: https://h.example/b.git\n",
			wantErr: helpers.ErrInvalidCollectionEntry,
		},
		{
			name: "namespace without source", input: "- namespace: acme\n  name: https://h.example/a.git\n  type: git\n",
			wantErr: helpers.ErrInvalidCollectionEntry,
		},
		{name: "type git without url", input: "- type: git\n  version: main\n", wantErr: helpers.ErrInvalidCollectionEntry},
		{
			name:    "explicit name outside the alphabet",
			input:   "- name: Acme.App\n  type: git\n  source: https://h.example/a.git\n",
			wantErr: helpers.ErrInvalidCollectionName,
		},
		{
			name: "explicit name without a dot", input: "- name: acme\n  type: git\n  source: https://h.example/a.git\n",
			wantErr: helpers.ErrInvalidCollectionName,
		},
		{
			name: "git pointer as a galaxy source", input: "- name: acme.app\n  source: git+https://h.example/a.git\n",
			wantErr: helpers.ErrUnsupportedCollectionSource,
		},
		{
			name:    "git locator as a galaxy source",
			input:   "- name: acme.app\n  type: galaxy\n  source: git+https://h.example/a.git#@" + gitTestCommit + "\n",
			wantErr: helpers.ErrUnsupportedCollectionSource,
		},
		{
			name: "ftp scheme stays a refused source", input: "- ftp://github.com/acme/app.tar.gz\n",
			wantErr: helpers.ErrUnsupportedCollectionSource,
		},
		{name: "type file", input: "- name: ./a.tar.gz\n  type: file\n", wantErr: helpers.ErrUnsupportedCollectionType},
		{name: "type dir", input: "- name: ./a\n  type: dir\n", wantErr: helpers.ErrUnsupportedCollectionType},
	}
}

func TestParseCollectionsRejectsGitShapes(t *testing.T) {
	t.Parallel()
	for _, tc := range gitRejectedCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.input), "https://default")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Parse error = %v, want %v", err, tc.wantErr)
			}
			if tc.mustNotContain != "" && strings.Contains(err.Error(), tc.mustNotContain) {
				t.Fatalf("error echoes the credential: %q", err.Error())
			}
		})
	}
}
