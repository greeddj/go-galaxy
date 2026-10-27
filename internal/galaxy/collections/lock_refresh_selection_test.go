package collections

// This file pins how --collection and --role narrow lock --refresh: what a
// selection sets aside, the names it refuses, the pins kept and the
// announcement.

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
)

const (
	selectionLockPath   = "/proj/galaxy.lock"
	selectionOtherTar   = "https://dl.example/acme-zookeeper-3.0.0.tar.gz"
	selectionAnnounced  = "Setting aside the pins of a.b, c.d, role x"
	selectionKafkaLabel = "0.24.0"
)

// releaseLockfile locks the chain acme.a, acme.b, acme.c, a lone acme.d, the
// cycle acme.x and acme.y, and roles web (Galaxy acme.web), base and common
// in a chain, beside a lone other.
func releaseLockfile() *lockfile.File {
	return &lockfile.File{
		Collections: []lockfile.Entry{
			{Name: "acme.a", Version: testVersion100, Deps: []string{"acme.b"}},
			{Name: "acme.b", Version: testVersion100, Deps: []string{"acme.c"}},
			{Name: "acme.c", Version: testVersion100},
			{Name: "acme.d", Version: testVersion100},
			{Name: "acme.x", Version: testVersion100, Deps: []string{"acme.y"}},
			{Name: "acme.y", Version: testVersion100, Deps: []string{"acme.x"}},
		},
		Roles: []lockfile.RoleEntry{
			{Name: "web", Type: lockfile.RoleTypeGalaxy, Galaxy: "acme.web", Deps: []string{"base"}},
			{Name: "base", Type: lockfile.RoleTypeGit, Deps: []string{"common"}},
			{Name: "common", Type: lockfile.RoleTypeGit},
			{Name: "other", Type: lockfile.RoleTypeGit},
		},
	}
}

// releasedNames lists, in file order, the entries of lf that release sets
// aside, so a test sees what a selection does to the file's own pins.
func releasedNames(release refreshRelease, lf *lockfile.File) ([]string, []string) {
	var collections, roles []string
	for _, e := range lf.Collections {
		if release.releasesCollection(e.Name) {
			collections = append(collections, e.Name)
		}
	}
	for _, e := range lf.Roles {
		if release.releasesRole(e.Name) {
			roles = append(roles, e.Name)
		}
	}
	return collections, roles
}

// TestNewRefreshReleaseClosesOverLockedDeps pins what a selection sets aside:
// named entries and what their galaxy.lock deps reach, cycles included, roles
// by install or Galaxy name, absent names nothing.
func TestNewRefreshReleaseClosesOverLockedDeps(t *testing.T) {
	t.Parallel()
	lf := releaseLockfile()
	for _, tc := range []struct {
		name            string
		collections     []string
		roles           []string
		wantCollections []string
		wantRoles       []string
	}{
		{name: "a chain from its head", collections: []string{"acme.a"}, wantCollections: []string{"acme.a", "acme.b", "acme.c"}},
		{name: "a chain from its middle", collections: []string{"acme.b"}, wantCollections: []string{"acme.b", "acme.c"}},
		{name: "a chain's tail", collections: []string{"acme.c"}, wantCollections: []string{"acme.c"}},
		{name: "a cycle", collections: []string{"acme.y"}, wantCollections: []string{"acme.x", "acme.y"}},
		{name: "two names", collections: []string{"acme.b", "acme.d"}, wantCollections: []string{"acme.b", "acme.c", "acme.d"}},
		{name: "a collection the file lacks", collections: []string{"acme.absent"}},
		{name: "a collection name never a role", collections: []string{"acme.web"}},
		{name: "a role by install name", roles: []string{"web"}, wantRoles: []string{"web", "base", "common"}},
		{name: "a role by Galaxy name", roles: []string{"acme.web"}, wantRoles: []string{"web", "base", "common"}},
		{name: "a role dependency", roles: []string{"base"}, wantRoles: []string{"base", "common"}},
		{name: "a role the file lacks", roles: []string{"absent"}},
		{name: "a role name never a collection", roles: []string{"acme.a"}},
		{
			name: "both kinds", collections: []string{"acme.d"}, roles: []string{"other"},
			wantCollections: []string{"acme.d"}, wantRoles: []string{"other"},
		},
	} {
		release := newRefreshRelease(&config.Config{RefreshCollections: tc.collections, RefreshRoles: tc.roles}, lf, nil)
		collections, roles := releasedNames(release, lf)
		if !slices.Equal(collections, tc.wantCollections) || !slices.Equal(roles, tc.wantRoles) {
			t.Errorf("%s: released collections %v and roles %v, want %v and %v",
				tc.name, collections, roles, tc.wantCollections, tc.wantRoles)
		}
	}
}

// TestNewRefreshReleaseClosesOverGitSiblingsAndSharedURLs pins the two
// releases beyond deps: the entries one git root owns move together with what
// they need, and url roles sharing one URL move together.
func TestNewRefreshReleaseClosesOverGitSiblingsAndSharedURLs(t *testing.T) {
	t.Parallel()
	commit, sha := strings.Repeat("a", 40), strings.Repeat("b", 64)
	two := prefsGitEntry("acme.two", "collections/two", "main", commit)
	two.Deps = []string{"acme.one", "acme.dep"}
	lf := &lockfile.File{
		Collections: []lockfile.Entry{
			prefsGitEntry("acme.one", "collections/one", "main", commit), two,
			{Name: "acme.dep", Version: testVersion100},
			{Name: "acme.lone", Version: testVersion100},
		},
		Roles: []lockfile.RoleEntry{
			{Name: "first", Type: lockfile.RoleTypeURL, Version: sha[:12], Source: prefsRoleTarURL, SHA256: sha},
			{Name: "second", Type: lockfile.RoleTypeURL, Version: sha[:12], Source: prefsRoleTarURL, SHA256: sha},
			{Name: "third", Type: lockfile.RoleTypeURL, Version: sha[:12], Source: selectionOtherTar, SHA256: sha},
		},
	}
	oneRoot := []collection{prefsGitRoot("collections", "main", "")}
	twoRoots := []collection{prefsGitRoot("collections/one", "main", ""), prefsGitRoot("collections/two", "main", "")}
	for _, tc := range []struct {
		name            string
		roots           []collection
		collections     []string
		roles           []string
		wantCollections []string
		wantRoles       []string
	}{
		{
			name: "one sibling named", roots: oneRoot, collections: []string{"acme.one"},
			wantCollections: []string{"acme.one", "acme.two", "acme.dep"},
		},
		{
			name: "the other sibling named", roots: oneRoot, collections: []string{"acme.two"},
			wantCollections: []string{"acme.one", "acme.two", "acme.dep"},
		},
		{name: "a sibling's dependency named", roots: oneRoot, collections: []string{"acme.dep"}, wantCollections: []string{"acme.dep"}},
		{name: "an unrelated collection named", roots: oneRoot, collections: []string{"acme.lone"}, wantCollections: []string{"acme.lone"}},
		{name: "each entry its own root", roots: twoRoots, collections: []string{"acme.one"}, wantCollections: []string{"acme.one"}},
		{name: "no root owning the entries", collections: []string{"acme.one"}, wantCollections: []string{"acme.one"}},
		{name: "a url role sharing a URL", roles: []string{"second"}, wantRoles: []string{"first", "second"}},
		{name: "a url role alone at its URL", roles: []string{"third"}, wantRoles: []string{"third"}},
	} {
		release := newRefreshRelease(&config.Config{RefreshCollections: tc.collections, RefreshRoles: tc.roles}, lf, tc.roots)
		collections, roles := releasedNames(release, lf)
		if !slices.Equal(collections, tc.wantCollections) || !slices.Equal(roles, tc.wantRoles) {
			t.Errorf("%s: released collections %v and roles %v, want %v and %v",
				tc.name, collections, roles, tc.wantCollections, tc.wantRoles)
		}
	}
	// A root asked at a commit never moves, so naming one entry releases no
	// sibling and none of a sibling's deps.
	atCommit := prefsGitEntry("acme.two", "collections/two", commit, commit)
	atCommit.Deps = []string{"acme.dep"}
	pinned := &lockfile.File{Collections: []lockfile.Entry{
		prefsGitEntry("acme.one", "collections/one", commit, commit), atCommit, {Name: "acme.dep", Version: testVersion100},
	}}
	release := newRefreshRelease(&config.Config{RefreshCollections: []string{"acme.one"}}, pinned,
		[]collection{prefsGitRoot("collections", commit, "")})
	if collections, _ := releasedNames(release, pinned); !slices.Equal(collections, []string{"acme.one"}) {
		t.Errorf("a root asked at a commit: released %v, want [acme.one]", collections)
	}
}

// absentLockfile is what readExistingLockfile holds when no file is there.
func absentLockfile(t *testing.T) existingLockfile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "galaxy.lock")
	lf, err := lockfile.Load(path)
	if !lockfile.IsNotExist(err) {
		t.Fatalf("lockfile.Load(%s) = %v, want absence", path, err)
	}
	return existingLockfile{file: lf, err: err, path: path}
}

// TestCheckRefreshSelectionRefusesEveryNameTheFileDoesNotPin pins the check
// before the resolve: no file or known names pass, a role by Galaxy name too,
// and one error lists every unknown name.
func TestCheckRefreshSelectionRefusesEveryNameTheFileDoesNotPin(t *testing.T) {
	t.Parallel()
	typo := &config.Config{RefreshCollections: []string{"acme.typo"}, RefreshRoles: []string{"typo"}}
	if err := checkRefreshSelection(typo, absentLockfile(t)); err != nil {
		t.Errorf("no file: checkRefreshSelection = %v, want nil", err)
	}
	existing := existingLockfile{file: releaseLockfile(), path: selectionLockPath}
	known := &config.Config{RefreshCollections: []string{"acme.a", "acme.y"}, RefreshRoles: []string{"acme.web", "common"}}
	if err := checkRefreshSelection(known, existing); err != nil {
		t.Errorf("known names: checkRefreshSelection = %v, want nil", err)
	}
	unknown := &config.Config{RefreshCollections: []string{"acme.a", "acme.typo"}, RefreshRoles: []string{"acme.a", "typo", "web"}}
	err := checkRefreshSelection(unknown, existing)
	if !errors.Is(err, helpers.ErrRefreshSelectionNotLocked) {
		t.Fatalf("unknown names: checkRefreshSelection = %v, want ErrRefreshSelectionNotLocked", err)
	}
	msg := err.Error()
	for _, want := range []string{selectionLockPath, "collection acme.typo", "role acme.a", "role typo"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
	for _, known := range []string{"collection acme.a", "role web"} {
		if strings.Contains(msg, known) {
			t.Errorf("error %q names the locked %q", msg, known)
		}
	}

	mixedCase := &lockfile.File{Collections: []lockfile.Entry{
		{Name: "Acme.Foo", Type: lockfile.TypeURL, Version: selectionKafkaLabel, Source: prefsTarURL, SHA256: strings.Repeat("d", 64)},
	}}
	named := &config.Config{RefreshCollections: []string{"Acme.Foo"}}
	if err := checkRefreshSelection(named, existingLockfile{file: mixedCase, path: selectionLockPath}); err != nil {
		t.Errorf("a mixed-case url collection: checkRefreshSelection = %v, want nil", err)
	}
}

// TestCheckRefreshSelectionRefusesAFileThatDoesNotLoad pins a selection over an
// unloadable lockfile: a usage error naming the file and its cause, not the
// file's own error; no selection passes.
func TestCheckRefreshSelectionRefusesAFileThatDoesNotLoad(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "galaxy.lock")
	mustWriteFile(t, path, []byte("schema_version: 99\n"))
	lf, cause := lockfile.Load(path)
	if !errors.Is(cause, helpers.ErrLockfileInvalid) {
		t.Fatalf("lockfile.Load = %v, want ErrLockfileInvalid", cause)
	}
	unreadable := existingLockfile{file: lf, err: cause, path: path}
	if err := checkRefreshSelection(&config.Config{Refresh: true}, unreadable); err != nil {
		t.Errorf("no selection: checkRefreshSelection = %v, want nil", err)
	}

	for _, cfg := range []*config.Config{
		{Refresh: true, RefreshCollections: []string{"acme.a"}},
		{Refresh: true, RefreshRoles: []string{"web"}},
	} {
		err := checkRefreshSelection(cfg, unreadable)
		if !errors.Is(err, helpers.ErrRefreshSelectionLockfileUnreadable) {
			t.Fatalf("checkRefreshSelection = %v, want ErrRefreshSelectionLockfileUnreadable", err)
		}
		if errors.Is(err, helpers.ErrLockfileInvalid) {
			t.Errorf("checkRefreshSelection = %v wraps ErrLockfileInvalid, whose exit class is the lockfile's", err)
		}
		for _, want := range []string{path, cause.Error()} {
			if msg := err.Error(); !strings.Contains(msg, want) {
				t.Errorf("error %q does not name %q", msg, want)
			}
		}
	}
}

// selectionLockfile is roleLockfile, acme.web needing tar, beside Galaxy
// entries acme.named (needing acme.dep) and acme.kept, url entries acme.kafka
// and acme.zookeeper, and git entries acme.one and acme.two at one commit.
func selectionLockfile() *lockfile.File {
	lf := roleLockfile()
	for i := range lf.Roles {
		if lf.Roles[i].Name == "acme.web" {
			lf.Roles[i].Deps = []string{"tar"}
		}
	}
	sha, commit := strings.Repeat("d", 64), strings.Repeat("a", 40)
	lf.Collections = []lockfile.Entry{
		{Name: "acme.named", Version: testVersion100, Deps: []string{"acme.dep"}},
		{Name: "acme.dep", Version: testVersion100},
		{Name: "acme.kept", Version: testVersion100},
		{Name: "acme.kafka", Type: lockfile.TypeURL, Version: selectionKafkaLabel, Source: prefsTarURL, SHA256: sha},
		{Name: "acme.zookeeper", Type: lockfile.TypeURL, Version: "3.0.0", Source: selectionOtherTar, SHA256: sha},
		prefsGitEntry("acme.one", "collections/one", "main", commit),
		prefsGitEntry("acme.two", "collections/two", "main", commit),
	}
	return lf
}

// keptPins lists what prefs still pins of selectionLockfile, looked up the way
// each resolve asks: Galaxy by name, url by URL, role by request, git entries.
func keptPins(prefs *lockPreferences) []string {
	var kept []string
	for _, fqdn := range []string{"acme.named", "acme.dep", "acme.kept"} {
		if _, ok := prefs.galaxyVersion(fqdn); ok {
			kept = append(kept, fqdn)
		}
	}
	for _, rawURL := range []string{prefsTarURL, selectionOtherTar} {
		if entry, ok := prefs.urlEntry(rawURL, ""); ok {
			kept = append(kept, entry.Name)
		}
	}
	for _, req := range []requirements.RoleRequirement{
		{Name: "app", Src: prefsRoleGitURL, Version: "HEAD", Type: requirements.TypeGit},
		{Name: "acme.web", Src: "acme.web", Type: requirements.TypeGalaxy},
		{Name: "tar", Src: prefsRoleTarURL, Type: requirements.TypeURL},
	} {
		if _, ok := prefs.role(req); ok {
			kept = append(kept, "role "+req.Name)
		}
	}
	for _, e := range prefs.git {
		kept = append(kept, "git "+e.Name)
	}
	return kept
}

// TestNewLockPreferencesSetsAsideOnlyTheRefreshSelection pins the index under a
// selection: released Galaxy, url and role entries pin nothing, git entries
// all stay for gitRoots to judge, and --offline keeps every pin.
func TestNewLockPreferencesSetsAsideOnlyTheRefreshSelection(t *testing.T) {
	t.Parallel()
	collections := []string{"acme.kafka", "acme.named", "acme.one"}
	roles := []string{"acme.web"}
	every := []string{
		"acme.named", "acme.dep", "acme.kept", "acme.kafka", "acme.zookeeper",
		"role app", "role acme.web", "role tar", "git acme.one", "git acme.two",
	}
	for _, tc := range []struct {
		cfg  *config.Config
		name string
		want []string
	}{
		{name: "no refresh", cfg: &config.Config{}, want: every},
		{
			name: "a selection",
			cfg:  &config.Config{Refresh: true, RefreshCollections: collections, RefreshRoles: roles},
			want: []string{"acme.kept", "acme.zookeeper", "role app", "git acme.one", "git acme.two"},
		},
		{
			name: "a selection under --offline",
			cfg:  &config.Config{Refresh: true, Offline: true, RefreshCollections: collections, RefreshRoles: roles},
			want: every,
		},
	} {
		prefs := newLockPreferences(tc.cfg, selectionLockfile(), nil)
		if prefs == nil {
			t.Errorf("%s: newLockPreferences = nil, want an index", tc.name)
			continue
		}
		if got := keptPins(prefs); !slices.Equal(got, tc.want) {
			t.Errorf("%s: kept %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestLockPreferencesGitRootsReleaseARootWholeForOneNamedCollection pins that
// one commit holds every collection of a git root: naming any one of them, or
// a collection depending on one, releases the root; naming neither keeps it.
func TestLockPreferencesGitRootsReleaseARootWholeForOneNamedCollection(t *testing.T) {
	t.Parallel()
	commit := strings.Repeat("a", 40)
	lf := &lockfile.File{Collections: []lockfile.Entry{
		prefsGitEntry("acme.one", "collections/one", "main", commit),
		prefsGitEntry("acme.two", "collections/two", "main", commit),
		{Name: "acme.galaxy", Version: testVersion100, Deps: []string{"acme.two"}},
		{Name: "acme.other", Version: testVersion100},
	}}
	roots := []collection{prefsGitRoot("collections", "main", ""), {Namespace: "acme", Name: "galaxy", Type: typeGalaxy}}
	kept := lockedGitRoot{commit: commit, names: []string{"acme.one", "acme.two"}}
	for _, tc := range []struct {
		name     string
		selected []string
		want     lockedGitRoot
	}{
		{name: "the first collection named", selected: []string{"acme.one"}},
		{name: "its sibling named", selected: []string{"acme.two"}},
		{name: "a collection depending on one named", selected: []string{"acme.galaxy"}},
		{name: "neither named", selected: []string{"acme.other"}, want: kept},
	} {
		prefs := newLockPreferences(&config.Config{Refresh: true, RefreshCollections: tc.selected}, lf, nil)
		got := prefs.gitRoots(roots)
		if len(got) != len(roots) || got[1].commit != "" {
			t.Fatalf("%s: gitRoots = %+v, want two roots, the Galaxy one unpinned", tc.name, got)
		}
		if got[0].commit != tc.want.commit || got[0].released != "" || !slices.Equal(got[0].names, tc.want.names) {
			t.Errorf("%s: git root locked at %+v, want %+v", tc.name, got[0], tc.want)
		}
	}
}

// TestRefreshReleaseAnnouncesOneLine pins the announcement: one report-tier
// line, which --quiet keeps, collections sorted before roles sorted,
// dependencies included, and no line when nothing is set aside.
func TestRefreshReleaseAnnouncesOneLine(t *testing.T) {
	t.Parallel()
	printer := &capturingPrinter{}
	refreshRelease{
		collections: map[string]struct{}{"c.d": {}, "a.b": {}},
		roles:       map[string]struct{}{"x": {}},
	}.announce(printer)
	if !slices.Equal(printer.persists, []string{selectionAnnounced}) {
		t.Errorf("persists = %q, want [%q]", printer.persists, selectionAnnounced)
	}
	if others := slices.Concat(printer.prints, printer.warns, printer.oks, printer.updates, printer.errs, printer.debugs); len(others) != 0 {
		t.Errorf("lines on other tiers: %q", others)
	}

	selected := &config.Config{Refresh: true, RefreshCollections: []string{"acme.a"}, RefreshRoles: []string{"acme.web"}}
	printer = &capturingPrinter{}
	newLockPreferences(selected, releaseLockfile(), nil).announceRelease(printer)
	want := "Setting aside the pins of acme.a, acme.b, acme.c, role base, role common, role web"
	if !slices.Equal(printer.persists, []string{want}) {
		t.Errorf("persists = %q, want [%q]", printer.persists, want)
	}

	silent := &capturingPrinter{}
	refreshRelease{}.announce(silent)
	var none *lockPreferences
	none.announceRelease(silent)
	newLockPreferences(&config.Config{}, releaseLockfile(), nil).announceRelease(silent)
	offline := &config.Config{Refresh: true, Offline: true, RefreshCollections: []string{"acme.a"}}
	newLockPreferences(offline, releaseLockfile(), nil).announceRelease(silent)
	if all := slices.Concat(silent.persists, silent.prints); len(all) != 0 {
		t.Errorf("lines %q, want none when nothing is set aside", all)
	}
}

// TestLockedPinReadable pins when a pin galaxy.lock keeps may replay a recorded
// pin: whenever the policy reads, and under --refresh unless --no-cache.
func TestLockedPinReadable(t *testing.T) {
	t.Parallel()
	reads := cacheManager.Policy{Read: true, Write: true}
	writesOnly := cacheManager.Policy{Write: true}
	for _, tc := range []struct {
		cfg    *config.Config
		name   string
		policy cacheManager.Policy
		want   bool
	}{
		{name: "a reading policy", cfg: &config.Config{}, policy: reads, want: true},
		{name: "a reading policy under --refresh --no-cache", cfg: &config.Config{Refresh: true, NoCache: true}, policy: reads, want: true},
		{name: "--refresh", cfg: &config.Config{Refresh: true}, policy: writesOnly, want: true},
		{name: "--refresh --no-cache", cfg: &config.Config{Refresh: true, NoCache: true}, policy: writesOnly},
		{name: "--no-cache", cfg: &config.Config{NoCache: true}, policy: writesOnly},
		{name: "a policy that does not read", cfg: &config.Config{}, policy: writesOnly},
		{name: "no configuration, a reading policy", policy: reads, want: true},
		{name: "no configuration", policy: writesOnly},
	} {
		if got := lockedPinReadable(collectionDeps{cfg: tc.cfg}, tc.policy); got != tc.want {
			t.Errorf("%s: lockedPinReadable = %v, want %v", tc.name, got, tc.want)
		}
	}
}
