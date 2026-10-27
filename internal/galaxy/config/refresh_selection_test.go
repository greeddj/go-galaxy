package config

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// newRefreshSelectionCmd runs a command that registers lock's --collection
// and --role when register is true, and neither otherwise, as every other
// command; it returns the *cli.Command its action captured.
func newRefreshSelectionCmd(t *testing.T, register bool, args []string) *cli.Command {
	t.Helper()
	var flags []cli.Flag
	if register {
		flags = []cli.Flag{
			&cli.StringSliceFlag{Name: "collection", Aliases: []string{"c"}},
			&cli.StringSliceFlag{Name: "role"},
		}
	}
	var captured *cli.Command
	cmd := &cli.Command{
		Name:  "lock",
		Flags: flags,
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}
	if err := cmd.Run(context.Background(), append([]string{"lock"}, args...)); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// TestApplyRefreshSelectionAccepts pins the accepted shapes: unregistered
// flags, as on install, read as no selection even without --refresh, and a
// selection beside it is trimmed, sorted and unique.
func TestApplyRefreshSelectionAccepts(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name            string
		args            []string
		wantCollections []string
		wantRoles       []string
		register        bool
		refresh         bool
	}{
		{name: "flags not registered"},
		{name: "flags registered and unset", register: true},
		{
			name:     "a selection beside --refresh",
			register: true, refresh: true,
			args:            []string{"-c", "b.c, a.b", "--collection", "a.b", "--role", " x ,Owner.Role-1"},
			wantCollections: []string{"a.b", "b.c"},
			wantRoles:       []string{"Owner.Role-1", "x"},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{Refresh: row.refresh}
			if err := applyRefreshSelection(cfg, newRefreshSelectionCmd(t, row.register, row.args)); err != nil {
				t.Fatalf("applyRefreshSelection() error = %v, want nil", err)
			}
			if !slices.Equal(cfg.RefreshCollections, row.wantCollections) || !slices.Equal(cfg.RefreshRoles, row.wantRoles) {
				t.Errorf("selection = %q, %q, want %q, %q", cfg.RefreshCollections, cfg.RefreshRoles, row.wantCollections, row.wantRoles)
			}
		})
	}
}

// TestApplyRefreshSelectionRefuses pins each refusal and that it leaves cfg's
// selection unset; a refused value is quoted, so no control rune reaches a
// terminal, and cut to helpers.MessageValueMaxLen.
func TestApplyRefreshSelectionRefuses(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", helpers.MessageValueMaxLen+100)
	rows := []struct {
		want    error
		name    string
		notWant string
		args    []string
		refresh bool
	}{
		{name: "a selection without --refresh", args: []string{"--role", "x"}, want: helpers.ErrRefreshSelectionWithoutRefresh},
		{name: "an empty element", args: []string{"-c", "a.b,"}, refresh: true, want: helpers.ErrInvalidRefreshSelection},
		{
			name: "a control rune", args: []string{"-c", "a.b\x1b[2J"}, refresh: true,
			want: helpers.ErrInvalidRefreshSelection, notWant: "\x1b",
		},
		{name: "an overlong value", args: []string{"--role", long}, refresh: true, want: helpers.ErrInvalidRefreshSelection, notWant: long},
		{name: "a reserved role name", args: []string{"--role", "ansible_collections"}, refresh: true, want: helpers.ErrInvalidRefreshSelection},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{Refresh: row.refresh}
			err := applyRefreshSelection(cfg, newRefreshSelectionCmd(t, true, row.args))
			if !errors.Is(err, row.want) {
				t.Fatalf("applyRefreshSelection() error = %v, want errors.Is %v", err, row.want)
			}
			if row.notWant != "" && strings.Contains(err.Error(), row.notWant) {
				t.Errorf("error = %q, want it without %q", err, row.notWant)
			}
			if cfg.RefreshCollections != nil || cfg.RefreshRoles != nil {
				t.Errorf("selection = %q, %q after a refusal, want both nil", cfg.RefreshCollections, cfg.RefreshRoles)
			}
		})
	}
}
