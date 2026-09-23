package collections

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
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
