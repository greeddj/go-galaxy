package collections

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// TestVerifyRootsResolved pins the post-condition that fails closed with
// helpers.ErrMissingResolvedRoot when the solver drops a requested root.
func TestVerifyRootsResolved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		resolved map[string]collection
		name     string
		wantFQDN string
		roots    []collection
		wantErr  bool
	}{
		{
			name:  "all roots resolved",
			roots: []collection{{Namespace: "ns", Name: "a"}, {Namespace: "ns", Name: "b"}},
			resolved: map[string]collection{
				"ns.a": {Namespace: "ns", Name: "a", Version: "1.0.0"},
				"ns.b": {Namespace: "ns", Name: "b", Version: "2.0.0"},
			},
			wantErr: false,
		},
		{
			name:     "no roots",
			roots:    nil,
			resolved: map[string]collection{},
			wantErr:  false,
		},
		{
			name:     "a root missing from resolved",
			roots:    []collection{{Namespace: "ns", Name: "a"}, {Namespace: "ns", Name: "missing"}},
			resolved: map[string]collection{"ns.a": {Namespace: "ns", Name: "a", Version: "1.0.0"}},
			wantErr:  true,
			wantFQDN: "ns.missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := verifyRootsResolved(tt.roots, tt.resolved)

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("verifyRootsResolved() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, helpers.ErrMissingResolvedRoot) {
				t.Fatalf("verifyRootsResolved() error = %v, want ErrMissingResolvedRoot", err)
			}
			if !strings.Contains(err.Error(), tt.wantFQDN) {
				t.Fatalf("verifyRootsResolved() error = %v, want it to name %q", err, tt.wantFQDN)
			}
		})
	}
}

// TestPlanCollectionsRefusesWhatInstallCannotOrder pins the plan checks
// install and warm share: a root the resolution dropped and a dependency
// cycle are both refused, and an acyclic graph comes back in install levels.
func TestPlanCollectionsRefusesWhatInstallCannotOrder(t *testing.T) {
	t.Parallel()
	app := collection{Namespace: "ns", Name: "app", Version: "1.0.0"}
	lib := collection{Namespace: "ns", Name: "lib", Version: "1.0.0"}
	both := map[string]collection{"ns.app": app, "ns.lib": lib}

	tests := []struct {
		resolved map[string]collection
		graph    map[string][]string
		wantErr  error
		name     string
		roots    []collection
	}{
		{
			name:     "every root resolved, acyclic",
			roots:    []collection{{Namespace: "ns", Name: "app"}},
			resolved: both,
			graph:    map[string][]string{app.key(): {lib.key()}, lib.key(): nil},
		},
		{
			name:     "a root the resolution dropped",
			roots:    []collection{{Namespace: "ns", Name: "app"}, {Namespace: "ns", Name: "gone"}},
			resolved: both,
			graph:    map[string][]string{app.key(): {lib.key()}, lib.key(): nil},
			wantErr:  helpers.ErrMissingResolvedRoot,
		},
		{
			name:     "a dependency cycle",
			roots:    []collection{{Namespace: "ns", Name: "app"}},
			resolved: both,
			graph:    map[string][]string{app.key(): {lib.key()}, lib.key(): {app.key()}},
			wantErr:  helpers.ErrDependencyGraphHasACycle,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			collections, levels, err := planCollections(infra.New(noopPrinter{}, nil), tt.roots, tt.resolved, tt.graph)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("planCollections() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("planCollections() error = %v, want nil", err)
			}
			if len(collections) != len(tt.resolved) {
				t.Errorf("planCollections() folded %d collections, want %d", len(collections), len(tt.resolved))
			}
			want := [][]string{{lib.key()}, {app.key()}}
			if !slices.EqualFunc(levels, want, slices.Equal[[]string]) {
				t.Errorf("planCollections() levels = %v, want %v", levels, want)
			}
		})
	}
}

type prepareRootsRefusalCase struct {
	wantErr  error
	name     string
	wantText string
	roots    []collection
}

// prepareRootsRefusalCases pairs each root set prepareRoots refuses with the
// sentinel and the text the refusal must carry.
func prepareRootsRefusalCases() []prepareRootsRefusalCase {
	gitSource := gitsource.Locator{URL: "https://git.example/acme/app.git"}.String()
	urlSource := urlsource.Locator{URL: "https://dl.example/acme-app-1.0.0.tar.gz"}.String()
	return []prepareRootsRefusalCase{
		{
			name:     "unsupported type",
			roots:    []collection{{Name: "acme.app", Type: "file"}},
			wantErr:  helpers.ErrUnsupportedCollectionType,
			wantText: `"file"`,
		},
		{
			name:     "git type over a server source",
			roots:    []collection{{Name: "acme.app", Type: "git", Source: "https://hub.example/"}},
			wantErr:  helpers.ErrInvalidCollectionEntry,
			wantText: `type "git"`,
		},
		{
			name:     "galaxy type over a git locator",
			roots:    []collection{{Type: "Galaxy", Source: gitSource}},
			wantErr:  helpers.ErrInvalidCollectionEntry,
			wantText: `type "galaxy"`,
		},
		{
			name:     "url type over a git locator",
			roots:    []collection{{Type: "url", Source: gitSource}},
			wantErr:  helpers.ErrInvalidCollectionEntry,
			wantText: `type "url"`,
		},
		{
			name:     "name that is not an fqdn",
			roots:    []collection{{Name: "app"}},
			wantErr:  helpers.ErrInvalidCollectionName,
			wantText: `"app"`,
		},
		{
			name:     "same locator twice",
			roots:    []collection{{Type: "url", Source: urlSource}, {Type: " URL ", Source: urlSource}},
			wantErr:  helpers.ErrDuplicateCollectionRequirement,
			wantText: "type url vs url",
		},
		{
			name: "git root named like a galaxy root",
			roots: []collection{
				{Name: "acme.app"},
				{Namespace: "acme", Name: "app", Type: "git", Source: gitSource},
			},
			wantErr:  helpers.ErrDuplicateCollectionRequirement,
			wantText: "acme.app (type galaxy vs git)",
		},
		{
			name:     "named git root with a name that is not an fqdn",
			roots:    []collection{{Name: "app", Type: "git", Source: gitSource}},
			wantErr:  helpers.ErrInvalidCollectionName,
			wantText: `"app"`,
		},
	}
}

// TestPrepareRootsRefusesWhatDownstreamCannotKey pins prepareRoots' refusals:
// an unsupported type, a type that disagrees with its source, a name that is
// not an fqdn, and a duplicate by locator or by name.
func TestPrepareRootsRefusesWhatDownstreamCannotKey(t *testing.T) {
	t.Parallel()
	for _, tt := range prepareRootsRefusalCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := prepareRoots(tt.roots)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("prepareRoots() error = %v, want %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("prepareRoots() error = %v, want it to contain %s", err, tt.wantText)
			}
			if got != nil {
				t.Fatalf("prepareRoots() = %v on a refusal, want nil", got)
			}
		})
	}
}

// TestPrepareRootsSplitsAndTypesRoots pins the accepted shapes: a dotted name
// is split and typed galaxy, a mixed-case type is canonicalized, and an
// unnamed locator root is kept for discovery to name.
func TestPrepareRootsSplitsAndTypesRoots(t *testing.T) {
	t.Parallel()
	gitSource := gitsource.Locator{URL: "https://git.example/acme/app.git"}.String()

	got, err := prepareRoots([]collection{
		{Name: "acme.app", Constraint: ">=1.0.0"},
		{Namespace: "acme", Name: "lib", Type: " GALAXY "},
		{Type: "Git", Source: gitSource},
	})
	if err != nil {
		t.Fatalf("prepareRoots() error = %v", err)
	}
	want := []collection{
		{Namespace: "acme", Name: "app", Constraint: ">=1.0.0", Type: typeGalaxy},
		{Namespace: "acme", Name: "lib", Type: typeGalaxy},
		{Type: typeGit, Source: gitSource},
	}
	if len(got) != len(want) {
		t.Fatalf("prepareRoots() returned %d roots, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Namespace != want[i].Namespace || got[i].Name != want[i].Name ||
			got[i].Type != want[i].Type || got[i].Source != want[i].Source || got[i].Constraint != want[i].Constraint {
			t.Errorf("prepareRoots()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
