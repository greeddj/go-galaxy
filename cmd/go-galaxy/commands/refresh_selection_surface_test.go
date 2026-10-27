package commands

import (
	"errors"
	"slices"
	"strings"
	"testing"

	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// refreshSelectionRow is one accepted lock invocation and the selection
// Config must carry for it; nil means no selection.
type refreshSelectionRow struct {
	name            string
	args            []string
	wantCollections []string
	wantRoles       []string
}

// refreshSelectionRows are TestLockRefreshSelectionConfigSurface's accepted rows.
func refreshSelectionRows() []refreshSelectionRow {
	return []refreshSelectionRow{
		{
			name:            "repeated and comma-separated",
			args:            []string{"--refresh", "-c", "a.b,c.d", "-c", "e.f"},
			wantCollections: []string{"a.b", "c.d", "e.f"},
		},
		{
			name:            "a space after a comma is trimmed, then sorted",
			args:            []string{"--refresh", "--collection", "c.d, a.b"},
			wantCollections: []string{"a.b", "c.d"},
		},
		{
			name:            "duplicates collapse",
			args:            []string{"--refresh", "-c", "a.b,a.b", "--collection", " a.b "},
			wantCollections: []string{"a.b"},
		},
		{
			name:      "roles by install or Galaxy name, sorted",
			args:      []string{"--refresh", "--role", "geerlingguy.docker,foo"},
			wantRoles: []string{"foo", "geerlingguy.docker"},
		},
		{
			name:            "a url collection's mixed-case name",
			args:            []string{"--refresh", "-c", "Acme.Foo"},
			wantCollections: []string{"Acme.Foo"},
		},
		{
			name:            "both flags at once",
			args:            []string{"--refresh", "-c", "a.b", "--role", "foo"},
			wantCollections: []string{"a.b"},
			wantRoles:       []string{"foo"},
		},
		{name: "--refresh with no selection", args: []string{"--refresh"}},
		{name: "neither flag", args: nil},
	}
}

// assertRefreshSelection checks cfg's selection against row's.
func assertRefreshSelection(t *testing.T, name string, collections, roles []string, row refreshSelectionRow) {
	t.Helper()
	if !slices.Equal(collections, row.wantCollections) {
		t.Errorf("%s: RefreshCollections = %q, want %q", name, collections, row.wantCollections)
	}
	if !slices.Equal(roles, row.wantRoles) {
		t.Errorf("%s: RefreshRoles = %q, want %q", name, roles, row.wantRoles)
	}
}

// TestLockRefreshSelectionConfigSurface pins how lock's --collection (-c) and
// --role reach Config: split on commas, trimmed, sorted and unique, the url
// alphabet admitted. Not parallel: t.Setenv.
func TestLockRefreshSelectionConfigSurface(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	projUnsetEnv(t, "GO_GALAXY_REFRESH", "GO_GALAXY_COLLECTION", "GO_GALAXY_ROLE")
	for _, row := range refreshSelectionRows() {
		cfg, err := buildConfigFor(t, "lock", Lock().Flags, row.args)
		if err != nil {
			t.Errorf("%s: BuildCollectionConfig() error = %v, want nil", row.name, err)
			continue
		}
		assertRefreshSelection(t, row.name, cfg.RefreshCollections, cfg.RefreshRoles, row)
	}
}

// TestLockRefreshSelectionTakesRefreshFromItsVariable pins that
// GO_GALAXY_REFRESH satisfies the --refresh a selection needs, as the flag
// does. Not parallel: t.Setenv.
func TestLockRefreshSelectionTakesRefreshFromItsVariable(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	projUnsetEnv(t, "GO_GALAXY_REFRESH", "GO_GALAXY_COLLECTION", "GO_GALAXY_ROLE")
	t.Setenv("GO_GALAXY_REFRESH", "true")

	cfg, err := buildConfigFor(t, "lock", Lock().Flags, []string{"-c", "a.b"})
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	assertConfigField(t, "Refresh", cfg.Refresh, true)
	assertRefreshSelection(t, "GO_GALAXY_REFRESH", cfg.RefreshCollections, cfg.RefreshRoles,
		refreshSelectionRow{wantCollections: []string{"a.b"}})
}

// TestLockRefreshSelectionHasNoVariable pins that the flags read no variable:
// an export named like one selects nothing, so no scheduled lock --check
// --refresh is narrowed by it. Not parallel: t.Setenv.
func TestLockRefreshSelectionHasNoVariable(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	projUnsetEnv(t, "GO_GALAXY_REFRESH", "GO_GALAXY_COLLECTION", "GO_GALAXY_ROLE")
	for _, key := range []string{"GO_GALAXY_COLLECTION", "GO_GALAXY_ROLE", "GO_GALAXY_REFRESH_COLLECTION", "GO_GALAXY_REFRESH_ROLE"} {
		t.Setenv(key, "a.b")
	}

	for _, args := range [][]string{{"--refresh"}, nil} {
		cfg, err := buildConfigFor(t, "lock", Lock().Flags, args)
		if err != nil {
			t.Errorf("args %q: BuildCollectionConfig() error = %v, want nil", args, err)
			continue
		}
		assertRefreshSelection(t, strings.Join(args, " "), cfg.RefreshCollections, cfg.RefreshRoles, refreshSelectionRow{})
	}
}

// refreshSelectionRefusal is one refused lock invocation: the sentinel and a
// fragment the message must carry, empty for none.
type refreshSelectionRefusal struct {
	want     error
	name     string
	wantText string
	args     []string
}

// refreshSelectionRefusals are TestLockRefreshSelectionRefusals' rows.
func refreshSelectionRefusals() []refreshSelectionRefusal {
	without, invalid := galaxyhelpers.ErrRefreshSelectionWithoutRefresh, galaxyhelpers.ErrInvalidRefreshSelection
	return []refreshSelectionRefusal{
		{name: "-c without --refresh", args: []string{"-c", "a.b"}, want: without},
		{name: "--role without --refresh", args: []string{"--role", "foo"}, want: without},
		{name: "--refresh=false", args: []string{"--refresh=false", "-c", "a.b"}, want: without},
		{name: "an empty list element", args: []string{"--refresh", "-c", "a.b,,c.d"}, want: invalid, wantText: `--collection ""`},
		{name: "an empty value", args: []string{"--refresh", "--collection="}, want: invalid, wantText: `--collection ""`},
		{name: "a blank value", args: []string{"--refresh", "-c", "  "}, want: invalid, wantText: `--collection "  "`},
		{name: "a name with no dot", args: []string{"--refresh", "-c", "nodot"}, want: invalid, wantText: `--collection "nodot"`},
		{name: "a name with two dots", args: []string{"--refresh", "-c", "a.b.c"}, want: invalid, wantText: `"a.b.c"`},
		{name: "a hyphen in a collection", args: []string{"--refresh", "-c", "a-b.c"}, want: invalid, wantText: `"a-b.c"`},
		{name: "a slash in a role", args: []string{"--refresh", "--role", "bad/name"}, want: invalid, wantText: `--role "bad/name"`},
		{name: "a dot-dot role", args: []string{"--refresh", "--role", ".."}, want: invalid, wantText: `--role ".."`},
		{name: "an empty role element", args: []string{"--refresh", "--role", "foo,"}, want: invalid, wantText: `--role ""`},
	}
}

// TestLockRefreshSelectionRefusals pins every refusal as a config error: a
// selection without --refresh, and any element, empty ones included, that
// names no collection or role install name. Not parallel: t.Setenv.
func TestLockRefreshSelectionRefusals(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	projUnsetEnv(t, "GO_GALAXY_REFRESH", "GO_GALAXY_COLLECTION", "GO_GALAXY_ROLE")
	for _, row := range refreshSelectionRefusals() {
		cfg, err := buildConfigFor(t, "lock", Lock().Flags, row.args)
		if !errors.Is(err, row.want) {
			t.Errorf("%s: BuildCollectionConfig() error = %v, want errors.Is %v", row.name, err, row.want)
			continue
		}
		if cfg != nil {
			t.Errorf("%s: cfg = %+v, want nil beside the error", row.name, cfg)
		}
		if !strings.Contains(err.Error(), row.wantText) {
			t.Errorf("%s: error = %q, want it to carry %q", row.name, err, row.wantText)
		}
	}
}
