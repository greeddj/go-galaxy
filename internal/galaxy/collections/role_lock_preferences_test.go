package collections

// This file pins the role half of the lockfile preference index: which role
// request finds its entry, which server a locked Galaxy role may name, and
// that a role's warn-once key never collides with a collection's.

import (
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
)

const (
	prefsRoleGitURL = "https://git.example/acme/app.git"
	prefsRoleTarURL = "https://dl.example/roles/tar.tar.gz"
)

// roleLockfile locks app (git at HEAD), pinned (git at a commit ref),
// acme.web (Galaxy at 1.0.0) and tar (url labeled 1.2.3).
func roleLockfile() *lockfile.File {
	commit := strings.Repeat("a", 40)
	return &lockfile.File{Roles: []lockfile.RoleEntry{
		{Name: "app", Type: lockfile.RoleTypeGit, Version: "main", Source: prefsRoleGitURL, Ref: "HEAD", Commit: commit},
		{Name: "pinned", Type: lockfile.RoleTypeGit, Version: commit, Source: prefsRoleGitURL, Ref: commit, Commit: commit},
		{
			Name: "acme.web", Type: lockfile.RoleTypeGalaxy, Version: testVersion100, Galaxy: "acme.web",
			Source: "https://galaxy.example", Repository: "https://github.com/acme/ansible-role-web", Ref: "refs/tags/1.0.0", Commit: commit,
		},
		{Name: "tar", Type: lockfile.RoleTypeURL, Version: "1.2.3", Source: prefsRoleTarURL, SHA256: strings.Repeat("b", 64)},
	}}
}

// TestLockPreferencesRoleMatchesWhatFrozenAccepts pins the per-kind rule a
// role request finds its entry by: the source and ref of a git role, the name
// and any version of a Galaxy role, the URL and any label of a url role.
func TestLockPreferencesRoleMatchesWhatFrozenAccepts(t *testing.T) {
	t.Parallel()
	prefs := newLockPreferences(&config.Config{}, roleLockfile())
	commit := strings.Repeat("a", 40)
	git := func(name, src, ref string) requirements.RoleRequirement {
		return requirements.RoleRequirement{Name: name, Src: src, Version: ref, Type: requirements.TypeGit}
	}
	galaxy := func(name, src, version string) requirements.RoleRequirement {
		return requirements.RoleRequirement{Name: name, Src: src, Version: version, Type: requirements.TypeGalaxy}
	}
	url := func(name, src, label string) requirements.RoleRequirement {
		return requirements.RoleRequirement{Name: name, Src: src, Version: label, Type: requirements.TypeURL}
	}
	for _, tc := range []struct {
		name string
		req  requirements.RoleRequirement
		want bool
	}{
		{name: "git at the locked ref", req: git("app", prefsRoleGitURL, "HEAD"), want: true},
		{name: "git at another ref", req: git("app", prefsRoleGitURL, "main")},
		{name: "git from another repository", req: git("app", "https://git.example/acme/other.git", "HEAD")},
		{name: "git at a commit ref", req: git("pinned", prefsRoleGitURL, commit)},
		{name: "Galaxy at no version", req: galaxy("acme.web", "acme.web", ""), want: true},
		{name: "Galaxy at the locked version", req: galaxy("acme.web", "acme.web", testVersion100), want: true},
		{name: "Galaxy at another version", req: galaxy("acme.web", "acme.web", testVersion200)},
		{name: "another Galaxy name", req: galaxy("acme.web", "acme.other", "")},
		{name: "url with no label", req: url("tar", prefsRoleTarURL, ""), want: true},
		{name: "url at the locked label", req: url("tar", prefsRoleTarURL, "1.2.3"), want: true},
		{name: "url at another label", req: url("tar", prefsRoleTarURL, "1.2.4")},
		{name: "url from another URL", req: url("tar", "https://dl.example/roles/other.tar.gz", "")},
		{name: "another kind under the name", req: git("acme.web", prefsRoleGitURL, "HEAD")},
		{name: "a name not locked", req: git("absent", prefsRoleGitURL, "HEAD")},
	} {
		entry, ok := prefs.role(tc.req)
		if ok != tc.want || (ok && entry.Name != tc.req.Name) {
			t.Errorf("%s: role = %+v, %v; want ok %v", tc.name, entry, ok, tc.want)
		}
	}
	var none *lockPreferences
	if _, ok := none.role(git("app", prefsRoleGitURL, "HEAD")); ok {
		t.Error("a nil index found a role entry")
	}
}

// TestLockedRoleServerAsked pins which server a locked Galaxy role may name:
// one of the run's list by id or origin, or with no list cfg.Server's origin.
func TestLockedRoleServerAsked(t *testing.T) {
	t.Parallel()
	listed := &config.Config{
		Server:  "https://galaxy.example/api/",
		Servers: []config.Server{{ID: "galaxy", URL: "https://galaxy.example/api/"}, {URL: "https://hub.example"}},
	}
	alone := &config.Config{Server: "https://galaxy.example/"}
	for _, tc := range []struct {
		cfg    *config.Config
		name   string
		source string
		want   bool
	}{
		{name: "listed origin", cfg: listed, source: "https://galaxy.example", want: true},
		{name: "second listed origin", cfg: listed, source: "https://hub.example/api", want: true},
		{name: "listed id", cfg: listed, source: "galaxy", want: true},
		{name: "unlisted origin", cfg: listed, source: "https://old-galaxy.example"},
		{name: "another port", cfg: listed, source: "https://galaxy.example:8443"},
		{name: "cfg.Server origin", cfg: alone, source: "https://galaxy.example", want: true},
		{name: "not cfg.Server", cfg: alone, source: "https://old-galaxy.example"},
		{name: "an id with no list", cfg: alone, source: "galaxy"},
		{name: "no configuration", source: "https://galaxy.example"},
	} {
		if got := lockedRoleServerAsked(tc.cfg, tc.source); got != tc.want {
			t.Errorf("%s: lockedRoleServerAsked(%q) = %v, want %v", tc.name, tc.source, got, tc.want)
		}
	}
}

// TestLockPreferencesWarnRolefKeepsRolesApartFromCollections pins that a
// role and a collection of one name each warn once, as their entries are
// keyed by kind as well as by name.
func TestLockPreferencesWarnRolefKeepsRolesApartFromCollections(t *testing.T) {
	t.Parallel()
	prefs := newLockPreferences(&config.Config{}, roleLockfile())
	printer := &capturingPrinter{}
	for range 3 {
		prefs.warnOncef(printer, lockEntryRef{name: "acme.web", kind: lockedCollection}, "collection %s", "acme.web")
		prefs.warnRolef(printer, "acme.web", "role %s", "acme.web")
	}
	if len(printer.warns) != 2 || printer.warnsEqual("collection acme.web") != 1 || printer.warnsEqual("role acme.web") != 1 {
		t.Fatalf("warns = %v, want one line for the collection and one for the role", printer.warns)
	}
}
