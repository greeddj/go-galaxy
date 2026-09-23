package collections

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
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
