package collections

// This file pins the git and url half of the lockfile preference index: the
// commit each git root keeps, read from the entries it owns among every root,
// and the url entry a url root keeps, by the rules --frozen accepts them by.

import (
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

const (
	prefsGitURL = "https://git.example/acme/mono.git"
	prefsTarURL = "https://dl.example/acme-kafka-0.24.0.tar.gz"
)

// prefsGitEntry is a git entry of prefsGitURL for fqdn at subdir, ref and
// commit.
func prefsGitEntry(fqdn, subdir, ref, commit string) lockfile.Entry {
	return lockfile.Entry{
		Name: fqdn, Type: lockfile.TypeGit, Version: testVersion100, Source: prefsGitURL, Ref: ref, Commit: commit, Subdir: subdir,
	}
}

// prefsGitRoot is an unexpanded root of prefsGitURL at subdir and ref, naming
// fqdn unless it is empty.
func prefsGitRoot(subdir, ref, fqdn string) collection {
	root := collection{Source: gitsource.Locator{URL: prefsGitURL, Subdir: subdir}.String(), Type: typeGit, Ref: ref}
	if fqdn != "" {
		root.Namespace, root.Name, _ = strings.Cut(fqdn, ".")
	}
	return root
}

// TestLockPreferencesGitRootsKeepTheCommitOfOwnedEntries pins which commit a
// git root keeps: the one every entry it owns shares, its own entry for a named
// root, none for a commit ref, a changed ref or owned entries at two commits.
func TestLockPreferencesGitRootsKeepTheCommitOfOwnedEntries(t *testing.T) {
	t.Parallel()
	main, dev, other := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	prefs := newLockPreferences(&config.Config{}, &lockfile.File{Collections: []lockfile.Entry{
		prefsGitEntry("acme.one", "collections/one", "main", main),
		prefsGitEntry("acme.two", "collections/two", "main", main),
		prefsGitEntry("acme.three", "collections/three", "dev", dev),
		prefsGitEntry("acme.pinned", "pinned", other, other),
		prefsGitEntry("acme.x", "split/x", "main", main),
		prefsGitEntry("acme.y", "split/y", "main", other),
		{Name: "acme.galaxy", Version: testVersion100},
	}})
	galaxyRoot := collection{Namespace: "acme", Name: "galaxy", Type: typeGalaxy}
	for _, tc := range []struct {
		name  string
		roots []collection
		want  []lockedGitRoot
	}{
		{
			name:  "unnamed parent beside a child at another ref",
			roots: []collection{prefsGitRoot("collections", "main", ""), galaxyRoot, prefsGitRoot("collections/three", "dev", "")},
			want:  []lockedGitRoot{{commit: main, names: []string{"acme.one", "acme.two"}}, {}, {commit: dev, names: []string{"acme.three"}}},
		},
		{
			name:  "named parent beside an unnamed child",
			roots: []collection{prefsGitRoot("collections", "main", "acme.one"), prefsGitRoot("collections/three", "dev", "")},
			want:  []lockedGitRoot{{commit: main, names: []string{"acme.one"}}, {commit: dev, names: []string{"acme.three"}}},
		},
		{
			name:  "unnamed parent beside a named child",
			roots: []collection{prefsGitRoot("collections", "main", ""), prefsGitRoot("collections/three", "dev", "acme.three")},
			want:  []lockedGitRoot{{commit: main, names: []string{"acme.one", "acme.two"}}, {commit: dev, names: []string{"acme.three"}}},
		},
		{
			name:  "a parent charged with the child's entry keeps what it owns",
			roots: []collection{prefsGitRoot("collections", "main", "")},
			want:  []lockedGitRoot{{commit: main, names: []string{"acme.one", "acme.two"}}},
		},
		{name: "a changed ref owns nothing", roots: []collection{prefsGitRoot("collections/three", "main", "")}, want: []lockedGitRoot{{}}},
		{name: "a named root owning nothing", roots: []collection{prefsGitRoot("collections", "main", "acme.three")}, want: []lockedGitRoot{{}}},
		{name: "a commit ref is its own pin", roots: []collection{prefsGitRoot("pinned", other, "")}, want: []lockedGitRoot{{}}},
		{name: "owned entries at two commits", roots: []collection{prefsGitRoot("split", "main", "")}, want: []lockedGitRoot{{}}},
		{
			name:  "a locator that does not parse",
			roots: []collection{{Source: "git+", Type: typeGit, Ref: "main"}, prefsGitRoot("collections", "main", "")},
			want:  []lockedGitRoot{{}, {}},
		},
	} {
		got := prefs.gitRoots(tc.roots)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: gitRoots = %+v, want %+v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i].commit != tc.want[i].commit || strings.Join(got[i].names, ",") != strings.Join(tc.want[i].names, ",") {
				t.Errorf("%s: root %d locked at %+v, want %+v", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}

// TestLockPreferencesURLEntryMatchesWhatFrozenAccepts pins the url entry a
// url root keeps: the one locked from its URL, unless the root asserts a
// version other than the locked one.
func TestLockPreferencesURLEntryMatchesWhatFrozenAccepts(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("d", 64)
	prefs := newLockPreferences(&config.Config{}, &lockfile.File{Collections: []lockfile.Entry{
		{Name: "acme.kafka", Type: lockfile.TypeURL, Version: "0.24.0", Source: prefsTarURL, SHA256: sha},
		prefsGitEntry("acme.one", "collections/one", "main", strings.Repeat("a", 40)),
	}})
	for _, tc := range []struct {
		name, url, requested string
		want                 bool
	}{
		{name: "no version asserted", url: prefsTarURL, want: true},
		{name: "the locked version asserted", url: prefsTarURL, requested: "0.24.0", want: true},
		{name: "another version asserted", url: prefsTarURL, requested: "0.25.0"},
		{name: "another URL", url: "https://dl.example/acme-kafka-0.25.0.tar.gz"},
		{name: "a git entry's source", url: prefsGitURL},
	} {
		entry, ok := prefs.urlEntry(tc.url, tc.requested)
		if ok != tc.want || (ok && (entry.Name != "acme.kafka" || entry.SHA256 != sha)) {
			t.Errorf("%s: urlEntry = %+v, %v; want ok %v", tc.name, entry, ok, tc.want)
		}
	}
}

// TestLockPreferencesNilPinsNoSourceRoot pins that the nil index every command
// but lock carries pins no git root and finds no url entry.
func TestLockPreferencesNilPinsNoSourceRoot(t *testing.T) {
	t.Parallel()
	var none *lockPreferences
	if got := none.gitRoots([]collection{prefsGitRoot("collections", "main", "")}); len(got) != 1 || got[0].commit != "" {
		t.Errorf("a nil index pinned a git root: %+v", got)
	}
	if _, ok := none.urlEntry(prefsTarURL, ""); ok {
		t.Error("a nil index found a url entry")
	}
}
