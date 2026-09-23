package collections_test

// This file pins the headline install, warm and outdated end with when
// entries fail: role failures are counted as roles, apart from collections,
// and a kind with no failure is left out of it.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// coldFrozenOfflineFixture locks requirements on a Galaxy role fixture, then
// points it at an empty cache under --frozen --offline, with the client the
// command wires for it, so every entry the lockfile pins fails as not cached.
func coldFrozenOfflineFixture(t *testing.T, requirements string, dryRun bool) *roleFixture {
	t.Helper()
	f := newGalaxyRoleFixture(t)
	f.writeRequirements(t, requirements)
	f.lockfile(t)
	f.cfg.CacheDir = t.TempDir()
	f.cfg.Frozen, f.cfg.Offline, f.cfg.DryRun = true, true, dryRun
	f.runtime.HTTP = fetch.NewOffline(f.cfg.Timeout)
	return f
}

// assertHeadline fails the test unless err's one-line message is want.
func assertHeadline(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("run succeeded, want the headline %q", want)
	}
	if got := err.Error(); got != want {
		t.Fatalf("headline = %q, want %q", got, want)
	}
}

// headlineRun is one command run of a headline table: the command, whether
// it previews, and the headline it must end with.
type headlineRun struct {
	run    func(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error
	name   string
	want   string
	dryRun bool
}

// TestRoleFailuresHeadlineNamesRoles pins that a run in which only roles
// fail says so, for install and warm, real and previewed alike.
func TestRoleFailuresHeadlineNamesRoles(t *testing.T) {
	t.Parallel()
	// docker, app and app's dependency base: three roles, no collection.
	const requirements = "roles:\n  - geerlingguy.docker\n  - src: git+" + roleAppURL + "\n    name: app\n"
	for _, tc := range []headlineRun{
		{name: "install", run: collections.Start, want: "installation failed for 3 roles"},
		{name: "install --dry-run", run: collections.Start, dryRun: true, want: "installation failed for 3 roles"},
		{name: "warm", run: collections.Warm, want: "installation failed: warm failed for 3 roles"},
		{name: "warm --dry-run", run: collections.Warm, dryRun: true, want: "installation failed: warm failed for 3 roles"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := coldFrozenOfflineFixture(t, requirements, tc.dryRun)
			assertHeadline(t, tc.run(context.Background(), f.cfg, f.runtime), tc.want)
		})
	}
}

// TestFailureHeadlineCountsCollectionsAndRolesApart pins that a preview
// failing a collection and a role names each, in the singular, while a real
// run, which skips the roles once a collection failed, names the collection.
func TestFailureHeadlineCountsCollectionsAndRolesApart(t *testing.T) {
	t.Parallel()
	const requirements = "collections:\n  - acme.lib\nroles:\n  - geerlingguy.docker\n"
	for _, tc := range []headlineRun{
		{name: "install", run: collections.Start, want: "installation failed for 1 collection"},
		{name: "install --dry-run", run: collections.Start, dryRun: true, want: "installation failed for 1 collection and 1 role"},
		{name: "warm", run: collections.Warm, want: "installation failed: warm failed for 1 collection"},
		{
			name: "warm --dry-run", run: collections.Warm, dryRun: true,
			want: "installation failed: warm failed for 1 collection and 1 role",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := coldFrozenOfflineFixture(t, requirements, tc.dryRun)
			assertHeadline(t, tc.run(context.Background(), f.cfg, f.runtime), tc.want)
		})
	}
}

// TestOutdatedHeadlineCountsRolesApart pins that outdated's headline counts
// a failed role lookup as a role, whether or not a collection failed too.
func TestOutdatedHeadlineCountsRolesApart(t *testing.T) {
	t.Parallel()
	roles := func(server string) []lockfile.RoleEntry {
		names := []string{"base", "web"}
		out := make([]lockfile.RoleEntry, 0, len(names))
		for _, name := range names {
			out = append(out, lockfile.RoleEntry{
				Name: "acme." + name, Type: lockfile.RoleTypeGalaxy, Version: "1.0.0", Galaxy: "acme." + name,
				Source: server, Repository: "https://github.com/acme/ansible-role-" + name,
				Ref: "refs/tags/1.0.0", Commit: fakeCommit("acme-" + name),
			})
		}
		return out
	}
	for _, tc := range []struct {
		name        string
		want        string
		collections []string
	}{
		{name: "roles only", want: "latest version lookup failed for 2 roles"},
		{
			name: "a collection and roles", collections: []string{"acme.missing"},
			want: "latest version lookup failed for 1 collection and 2 roles",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// No role is registered, so every v1 lookup fails, and no
			// collection either, so every collection lookup does.
			s := fakegalaxy.New(t)
			reqPath := filepath.Join(t.TempDir(), "requirements.yml")
			entries := make([]lockfile.Entry, 0, len(tc.collections))
			for _, name := range tc.collections {
				entries = append(entries, lockfile.Entry{Name: name, Version: "1.0.0", Source: s.URL()})
			}
			locked := roles(s.URL())
			lf := &lockfile.File{
				SchemaVersion: lockfile.SchemaVersionFor(entries, locked),
				Server:        s.URL(),
				Collections:   entries,
				Roles:         locked,
			}
			if err := lockfile.Save(lockfile.ResolveDefaultPath(reqPath, ""), lf); err != nil {
				t.Fatalf("save lockfile: %v", err)
			}
			cfg := &config.Config{Server: s.URL(), RequirementsFile: reqPath, Workers: 1}
			err := collections.Outdated(context.Background(), cfg, infra.New(noopPrinter{}, s.Client()))
			assertHeadline(t, err, tc.want)
		})
	}
}
