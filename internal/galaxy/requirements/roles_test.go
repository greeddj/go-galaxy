package requirements

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// roleAcceptedCase is one roles: entry the parser accepts and the
// requirement it must produce.
type roleAcceptedCase struct {
	name  string
	input string
	want  RoleRequirement
}

const acceptedRepo = "https://github.com/acme/ansible-role-app.git"

func roleAcceptedCases() []roleAcceptedCase {
	return append(galaxyRoleAcceptedCases(), gitRoleAcceptedCases()...)
}

// galaxyRoleAcceptedCases are the Galaxy-name spellings.
func galaxyRoleAcceptedCases() []roleAcceptedCase {
	return []roleAcceptedCase{
		{
			name:  "galaxy name string",
			input: "- geerlingguy.docker\n",
			want:  RoleRequirement{Name: "geerlingguy.docker", Src: "geerlingguy.docker", Type: TypeGalaxy},
		},
		{
			name:  "galaxy name with version and name",
			input: "- geerlingguy.docker,7.4.1,docker\n",
			want:  RoleRequirement{Name: "docker", Src: "geerlingguy.docker", Version: "7.4.1", Type: TypeGalaxy},
		},
		{
			name:  "galaxy mapping",
			input: "- src: geerlingguy.docker\n  version: v7.4.1\n",
			want:  RoleRequirement{Name: "geerlingguy.docker", Src: "geerlingguy.docker", Version: "v7.4.1", Type: TypeGalaxy},
		},
		{
			name:  "galaxy mapping with name",
			input: "- src: geerlingguy.docker\n  name: docker\n",
			want:  RoleRequirement{Name: "docker", Src: "geerlingguy.docker", Type: TypeGalaxy},
		},
		{
			name:  "old style role key",
			input: "- role: geerlingguy.docker\n  version: 7.4.1\n",
			want:  RoleRequirement{Name: "geerlingguy.docker", Src: "geerlingguy.docker", Version: "7.4.1", Type: TypeGalaxy},
		},
		{
			name:  "name only mapping is a galaxy name",
			input: "- name: geerlingguy.docker\n",
			want:  RoleRequirement{Name: "geerlingguy.docker", Src: "geerlingguy.docker", Type: TypeGalaxy},
		},
		{
			name:  "star version means unspecified",
			input: "- src: geerlingguy.docker\n  version: '*'\n",
			want:  RoleRequirement{Name: "geerlingguy.docker", Src: "geerlingguy.docker", Type: TypeGalaxy},
		},
		{
			name:  "numeric version reads as written",
			input: "- src: geerlingguy.docker\n  version: 7\n",
			want:  RoleRequirement{Name: "geerlingguy.docker", Src: "geerlingguy.docker", Version: "7", Type: TypeGalaxy},
		},
		{
			name:  "unquoted trailing zero reads as written",
			input: "- src: geerlingguy.docker\n  version: 1.10\n",
			want:  RoleRequirement{Name: "geerlingguy.docker", Src: "geerlingguy.docker", Version: "1.10", Type: TypeGalaxy},
		},
		{
			name:  "mixed case galaxy name",
			input: "- 6connect.Php-Versions\n",
			want:  RoleRequirement{Name: "6connect.Php-Versions", Src: "6connect.Php-Versions", Type: TypeGalaxy},
		},
	}
}

// gitRoleAcceptedCases are the git spellings: pointer strings, scm: git,
// and ansible's github.com special case.
func gitRoleAcceptedCases() []roleAcceptedCase {
	const repo = acceptedRepo
	return []roleAcceptedCase{
		{
			name:  "git pointer string",
			input: "- git+" + repo + "\n",
			want:  RoleRequirement{Name: "ansible-role-app", Src: repo, Version: "HEAD", Type: TypeGit},
		},
		{
			name:  "git pointer string with ref and name",
			input: "- git+" + repo + ",v1.2.3,app\n",
			want:  RoleRequirement{Name: "app", Src: repo, Version: "v1.2.3", Type: TypeGit},
		},
		{
			name:  "git pointer mapping",
			input: "- src: git+" + repo + "\n  version: main\n  name: app\n",
			want:  RoleRequirement{Name: "app", Src: repo, Version: "main", Type: TypeGit},
		},
		{
			name:  "scm git with plain url",
			input: "- src: " + repo + "\n  scm: git\n  version: main\n",
			want:  RoleRequirement{Name: "ansible-role-app", Src: repo, Version: "main", Type: TypeGit},
		},
		{
			name:  "scm git with ssh url",
			input: "- src: ssh://git@example.com/acme/app.git\n  scm: git\n",
			want:  RoleRequirement{Name: "app", Src: "ssh://git@example.com/acme/app.git", Version: "HEAD", Type: TypeGit},
		},
		{
			name:  "scp-like pointer",
			input: "- git@example.com:acme/app.git\n",
			want:  RoleRequirement{Name: "app", Src: "git@example.com:acme/app.git", Version: "HEAD", Type: TypeGit},
		},
		{
			name:  "github https without scm is git",
			input: "- src: https://github.com/acme/ansible-role-app\n",
			want:  RoleRequirement{Name: "ansible-role-app", Src: "https://github.com/acme/ansible-role-app", Version: "HEAD", Type: TypeGit},
		},
		{
			name:  "commit as version",
			input: "- src: git+" + repo + "\n  version: 0123456789abcdef0123456789abcdef01234567\n",
			want: RoleRequirement{Name: "ansible-role-app", Src: repo,
				Version: "0123456789abcdef0123456789abcdef01234567", Type: TypeGit},
		},
		{
			name:  "qualified ref",
			input: "- src: git+" + repo + "\n  version: refs/tags/v1\n",
			want:  RoleRequirement{Name: "ansible-role-app", Src: repo, Version: "refs/tags/v1", Type: TypeGit},
		},
		{
			name:  "unquoted ref reads as written",
			input: "- src: git+" + repo + "\n  version: 1.10\n",
			want:  RoleRequirement{Name: "ansible-role-app", Src: repo, Version: "1.10", Type: TypeGit},
		},
		{
			name:  "uppercase scm prefix",
			input: "- GIT+" + repo + "\n",
			want:  RoleRequirement{Name: "ansible-role-app", Src: repo, Version: "HEAD", Type: TypeGit},
		},
	}
}

func TestParseRolesAccepted(t *testing.T) {
	t.Parallel()
	for _, tc := range roleAcceptedCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, err := Parse([]byte("roles:\n"+tc.input), "https://default")
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			if len(f.Roles) != 1 || f.Roles[0] != tc.want {
				t.Fatalf("Roles = %#v, want %#v", f.Roles, tc.want)
			}
			if f.Collections != nil || len(f.Warnings) != 0 {
				t.Fatalf("collections = %#v, warnings = %q, want none", f.Collections, f.Warnings)
			}
		})
	}
}

// roleRejectedCase is one roles: entry the parser refuses, the sentinel it
// refuses it with, and a text the message must not carry.
type roleRejectedCase struct {
	wantErr        error
	name           string
	input          string
	mustNotContain string
}

func roleRejectedCases() []roleRejectedCase {
	return append([]roleRejectedCase{
		{name: "scalar roles value", input: "roles: yes\n", wantErr: helpers.ErrInvalidRolesList},
		{name: "mapping roles value", input: "roles:\n  a: b\n", wantErr: helpers.ErrInvalidRolesList},
		{name: "list entry", input: "roles:\n  - [a]\n", wantErr: helpers.ErrInvalidRoleEntry},
		{name: "empty string", input: "roles:\n  - ''\n", wantErr: helpers.ErrInvalidRoleEntry},
		{name: "too many commas", input: "roles:\n  - a.b,1,c,d\n", wantErr: helpers.ErrInvalidRoleEntry},
		{name: "old style role with comma", input: "roles:\n  - role: a.b,1\n", wantErr: helpers.ErrInvalidRoleEntry},
		{name: "mapping without src or name", input: "roles:\n  - version: 1\n", wantErr: helpers.ErrInvalidRoleEntry},
		{name: "source key", input: "roles:\n  - src: a.b\n    source: https://hub\n", wantErr: helpers.ErrInvalidRoleEntry},
		{name: "signatures key", input: "roles:\n  - src: a.b\n    signatures: [x]\n", wantErr: helpers.ErrInvalidRoleEntry},
		{name: "type key", input: "roles:\n  - src: a.b\n    type: galaxy\n", wantErr: helpers.ErrInvalidRoleEntry},
		{name: "include", input: "roles:\n  - include: other.yml\n", wantErr: helpers.ErrUnsupportedRoleInclude},
		{name: "scm hg", input: "roles:\n  - src: hg+https://example.com/r\n", wantErr: helpers.ErrUnsupportedRoleScm},
		{name: "scm key hg", input: "roles:\n  - src: https://example.com/r\n    scm: hg\n", wantErr: helpers.ErrUnsupportedRoleScm},
		{name: "tgz url", input: "roles:\n  - src: https://example.com/r.tgz\n", wantErr: helpers.ErrUnsupportedRoleSource},
		{name: "ftp tarball url", input: "roles:\n  - src: ftp://example.com/r.tar.gz\n",
			wantErr: helpers.ErrUnsupportedRoleSource},
		{name: "plain https url", input: "roles:\n  - src: https://example.com/a/r\n", wantErr: helpers.ErrUnsupportedRoleSource},
		{name: "local path", input: "roles:\n  - src: ./roles/local\n", wantErr: helpers.ErrUnsupportedRoleSource},
		{name: "absolute path", input: "roles:\n  - src: /srv/roles/x\n", wantErr: helpers.ErrUnsupportedRoleSource},
		{name: "home path", input: "roles:\n  - src: ~/roles/x\n", wantErr: helpers.ErrUnsupportedRoleSource},
		{name: "file url", input: "roles:\n  - src: file:///srv/r.tar.gz\n", wantErr: helpers.ErrUnsupportedRoleSource},
		{name: "no dot", input: "roles:\n  - docker\n", wantErr: helpers.ErrInvalidRoleName},
		{name: "three parts", input: "roles:\n  - a.b.c\n", wantErr: helpers.ErrInvalidRoleName},
		{name: "bad owner alphabet", input: "roles:\n  - -a.b\n", wantErr: helpers.ErrInvalidRoleName},
		{name: "plus in galaxy name", input: "roles:\n  - a+b.c\n", wantErr: helpers.ErrInvalidRoleName},
		{name: "newline in name", input: "roles:\n  - \"a.b\\nc\"\n", wantErr: helpers.ErrInvalidRoleName, mustNotContain: "\n"},
		{name: "bad galaxy version", input: "roles:\n  - src: a.b\n    version: 'a b'\n", wantErr: helpers.ErrInvalidRoleVersion},
		{name: "hidden install name", input: "roles:\n  - src: a.b\n    name: .hidden\n", wantErr: helpers.ErrInvalidRoleInstallName},
		{name: "collections install name", input: "roles:\n  - src: a.b\n    name: ansible_collections\n",
			wantErr: helpers.ErrInvalidRoleInstallName},
		{name: "slash in install name", input: "roles:\n  - src: a.b\n    name: x/y\n", wantErr: helpers.ErrInvalidRoleInstallName},
		{ //nolint:gosec // a fixture URL, not a credential
			name: "git url with userinfo", input: "roles:\n  - src: git+https://user:s3cret@example.com/a/r.git\n",
			wantErr: helpers.ErrGitURLUserinfo, mustNotContain: "s3cret",
		},
		{name: "git url with fragment", input: "roles:\n  - src: git+https://example.com/a/r.git#sub\n",
			wantErr: helpers.ErrUnsupportedRoleSource},
		{name: "git bad ref", input: "roles:\n  - src: git+https://example.com/a/r.git\n    version: 'a b'\n", wantErr: helpers.ErrInvalidGitRef},
		{name: "git abbreviated commit", input: "roles:\n  - src: git+https://example.com/a/r.git\n    version: '0123456'\n",
			wantErr: helpers.ErrGitAbbreviatedCommit},
		{name: "scm git with galaxy name", input: "roles:\n  - src: a.b\n    scm: git\n", wantErr: helpers.ErrInvalidGitURL},
		{name: "duplicate install name", input: "roles:\n  - a.b\n  - src: git+https://example.com/a/r.git\n    name: a.b\n",
			wantErr: helpers.ErrDuplicateRoleRequirement},
		{name: "duplicate install name by case", input: "roles:\n  - a.b\n  - A.B\n", wantErr: helpers.ErrDuplicateRoleRequirement},
	}, roleValueEchoRejectedCases()...)
}

// roleValueEchoRejectedCases are role entries whose refusal would quote a URL
// typed into a key: the message must not carry its userinfo.
func roleValueEchoRejectedCases() []roleRejectedCase {
	return []roleRejectedCase{
		{ //nolint:gosec // a fixture URL, not a credential
			name: "URL install name", input: "roles:\n  - src: acme.role\n    name: https://u:s3cret@h.example/x\n",
			wantErr: helpers.ErrInvalidRoleInstallName, mustNotContain: "s3cret",
		},
		{ //nolint:gosec // a fixture URL, not a credential
			name: "URL galaxy version", input: "roles:\n  - src: acme.role\n    version: https://u:s3cret@h.example/x\n",
			wantErr: helpers.ErrInvalidRoleVersion, mustNotContain: "s3cret",
		},
		{ //nolint:gosec // a fixture URL, not a credential
			name: "URL in a string with too many commas", input: "roles:\n  - \"https://u:s3cret@h.example/x,a,b,c\"\n",
			wantErr: helpers.ErrInvalidRoleEntry, mustNotContain: "s3cret",
		},
		{ //nolint:gosec // a fixture URL, not a credential
			name: "URL in an old style role with a comma", input: "roles:\n  - role: \"https://u:s3cret@h.example/x,1.0\"\n",
			wantErr: helpers.ErrInvalidRoleEntry, mustNotContain: "s3cret",
		},
		{ //nolint:gosec // a fixture URL, not a credential
			name:    "URL after a comma in a string with too many commas",
			input:   "roles:\n  - \"git+https://h.example/r.git,https://u:s3cret@h.example/x,a,b\"\n",
			wantErr: helpers.ErrInvalidRoleEntry, mustNotContain: "s3cret",
		},
		{ //nolint:gosec // a fixture URL, not a credential
			name:    "URL after a comma in an old style role",
			input:   "roles:\n  - role: \"git+https://h.example/r.git,https://u:s3cret@h.example/x\"\n",
			wantErr: helpers.ErrInvalidRoleEntry, mustNotContain: "s3cret",
		},
		{ //nolint:gosec // a fixture URL, not a credential
			name: "URL url role version", input: "roles:\n  - src: https://h.example/r.tar.gz\n    version: https://u:s3cret@h.example/x\n",
			wantErr: helpers.ErrInvalidRoleVersion, mustNotContain: "s3cret",
		},
		{ //nolint:gosec // a fixture URL, not a credential
			name: "URL scm", input: "roles:\n  - src: acme.role\n    scm: https://u:s3cret@h.example/x\n",
			wantErr: helpers.ErrUnsupportedRoleScm, mustNotContain: "s3cret",
		},
		{ //nolint:gosec // a fixture URL, not a credential
			name: "URL git role version", input: "roles:\n  - src: https://github.com/acme/role\n    version: https://u:s3cret@h.example/x\n",
			wantErr: helpers.ErrInvalidGitRef, mustNotContain: "s3cret",
		},
	}
}

func TestParseRolesRejected(t *testing.T) {
	t.Parallel()
	for _, tc := range roleRejectedCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.input), "https://default")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Parse error = %v, want %v", err, tc.wantErr)
			}
			if tc.mustNotContain != "" && strings.Contains(err.Error(), tc.mustNotContain) {
				t.Fatalf("error %q carries %q", err, tc.mustNotContain)
			}
		})
	}
}

func TestParseRolesUnknownKeyWarns(t *testing.T) {
	t.Parallel()
	f, err := Parse([]byte("roles:\n  - src: a.b\n    extra_vars: {x: 1}\n"), "https://default")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(f.Warnings) != 1 || !strings.Contains(f.Warnings[0], "extra_vars") {
		t.Fatalf("warnings = %q", f.Warnings)
	}
}

func TestParseRolesBesideCollections(t *testing.T) {
	t.Parallel()
	f, err := Parse([]byte("collections:\n  - community.general\nroles:\n  - a.b\n"), "https://default")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(f.Collections) != 1 || len(f.Roles) != 1 {
		t.Fatalf("collections = %d, roles = %d", len(f.Collections), len(f.Roles))
	}
}

func TestParseRolesNullValue(t *testing.T) {
	t.Parallel()
	for name, input := range map[string]string{"bare key": "roles:\n", "tilde": "roles: ~\n", "empty list": "roles: []\n"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, err := Parse([]byte(input), "https://default")
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			if len(f.Roles) != 0 || f.Collections != nil {
				t.Fatalf("file = %#v", f)
			}
		})
	}
}

// TestBareListStaysCollections pins the documented divergence: a bare
// top-level list is a collections list, and a role spelled into one is
// refused by name rather than installed as something else.
func TestBareListStaysCollections(t *testing.T) {
	t.Parallel()
	f, err := Parse([]byte("- community.general\n"), "https://default")
	if err != nil || len(f.Collections) != 1 || len(f.Roles) != 0 {
		t.Fatalf("bare list: f = %#v, err = %v", f, err)
	}
	for _, input := range []string{
		"- src: geerlingguy.docker\n",
		"collections:\n  - name: x.y\n    scm: git\n",
	} {
		_, err := Parse([]byte(input), "https://default")
		if !errors.Is(err, helpers.ErrInvalidCollectionEntry) || !strings.Contains(err.Error(), "roles key") {
			t.Fatalf("%q: error = %v, want ErrInvalidCollectionEntry naming the roles key", input, err)
		}
	}
}

func TestParseRoleDependency(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		wantErr  error
		dep      gitsource.RoleDependency
		want     RoleRequirement
		name     string
		wantSkip DependencySkip
	}{
		{name: "galaxy name", dep: gitsource.RoleDependency{Src: "geerlingguy.docker"},
			want: RoleRequirement{Name: "geerlingguy.docker", Src: "geerlingguy.docker", Type: TypeGalaxy}},
		{name: "role key with version", dep: gitsource.RoleDependency{Name: "geerlingguy.docker", Version: "7.4.1"},
			want: RoleRequirement{Name: "geerlingguy.docker", Src: "geerlingguy.docker", Version: "7.4.1", Type: TypeGalaxy}},
		{name: "comma string", dep: gitsource.RoleDependency{Src: "geerlingguy.docker,7.4.1,docker"},
			want: RoleRequirement{Name: "docker", Src: "geerlingguy.docker", Version: "7.4.1", Type: TypeGalaxy}},
		{name: "git src with name", dep: gitsource.RoleDependency{Src: "git+" + acceptedRepo, Version: "main", Name: "app"},
			want: RoleRequirement{Name: "app", Src: acceptedRepo, Version: "main", Type: TypeGit}},
		{name: "scm git", dep: gitsource.RoleDependency{Src: acceptedRepo, Scm: "git"},
			want: RoleRequirement{Name: "ansible-role-app", Src: acceptedRepo, Version: "HEAD", Type: TypeGit}},
		{name: "local role", dep: gitsource.RoleDependency{Src: "common"}, wantSkip: DependencyLocal},
		{name: "local role by name", dep: gitsource.RoleDependency{Name: "common"}, wantSkip: DependencyLocal},
		{name: "collection role", dep: gitsource.RoleDependency{Src: "ns.coll.role"}, wantSkip: DependencyCollection},
		{name: "tgz", dep: gitsource.RoleDependency{Src: "https://example.com/r.tgz"}, wantErr: helpers.ErrUnsupportedRoleSource},
		{name: "hg", dep: gitsource.RoleDependency{Src: "hg+https://example.com/r"}, wantErr: helpers.ErrUnsupportedRoleScm},
		{name: "bad alphabet", dep: gitsource.RoleDependency{Src: "-a.b"}, wantErr: helpers.ErrInvalidRoleName},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, skip, err := ParseRoleDependency(tt.dep)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if skip != tt.wantSkip || got != tt.want {
				t.Fatalf("= (%#v, %d), want (%#v, %d)", got, skip, tt.want, tt.wantSkip)
			}
		})
	}
}
