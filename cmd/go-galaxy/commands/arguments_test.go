package commands

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// argumentsRoot is a root shaped as main.go builds it: install as the
// DefaultCommand, NoArguments as the validator, explain with its own. Each
// action records that it ran.
func argumentsRoot(ran *[]string) *cli.Command {
	record := func(name string) cli.ActionFunc {
		return func(context.Context, *cli.Command) error {
			*ran = append(*ran, name)
			return nil
		}
	}
	explain := Explain()
	explain.Action = record("explain")
	return &cli.Command{
		Name:           "go-galaxy",
		DefaultCommand: "install",
		ArgValidator:   NoArguments,
		Writer:         io.Discard,
		ErrWriter:      io.Discard,
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Commands: []*cli.Command{
			{Name: "install", Action: record("install")},
			{Name: "hash", Action: record("hash")},
			explain,
		},
	}
}

// TestNoArgumentsRefusesPositionalArguments pins the refusal text: each
// argument quoted so control bytes stay escaped, the count the command takes,
// and the default-command clause only for install.
func TestNoArgumentsRefusesPositionalArguments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		want    string
		notWant string
		args    []string
	}{
		{
			name: "install with a control byte",
			args: []string{"install", "ns.a", "x\x1b[31m"},
			want: `unexpected arguments "ns.a" "x\x1b[31m": install takes none, and is what runs when the first word names no command`,
		},
		{
			name:    "a command other than the default",
			args:    []string{"hash", "extra"},
			want:    `unexpected arguments "extra": hash takes none`,
			notWant: "first word",
		},
		{
			name: "explain past its one argument",
			args: []string{"explain", "ns.a", "ns.b", "ns.c"},
			want: `unexpected arguments "ns.b" "ns.c": explain takes one`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran []string
			err := argumentsRoot(&ran).Run(context.Background(), append([]string{"go-galaxy"}, tc.args...))
			if !errors.Is(err, helpers.ErrUnexpectedArguments) {
				t.Fatalf("Run() = %v, want errors.Is match with %v", err, helpers.ErrUnexpectedArguments)
			}
			msg := err.Error()
			if !strings.HasSuffix(msg, tc.want) {
				t.Errorf("message = %q, want it to end with %q", msg, tc.want)
			}
			if strings.ContainsRune(msg, '\x1b') || (tc.notWant != "" && strings.Contains(msg, tc.notWant)) {
				t.Errorf("message = %q carries a raw ESC or %q", msg, tc.notWant)
			}
			if len(ran) != 0 {
				t.Errorf("actions %q ran, want none after a refusal", ran)
			}
		})
	}
}

// TestNoArgumentsAdmitsACommandWithNone pins that a command named with no
// argument, and explain with its one, reach their actions.
func TestNoArgumentsAdmitsACommandWithNone(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"install"}, {"hash"}, {"explain", "ns.a"}} {
		var ran []string
		if err := argumentsRoot(&ran).Run(context.Background(), append([]string{"go-galaxy"}, args...)); err != nil {
			t.Fatalf("Run(%q) = %v, want nil", args, err)
		}
		if len(ran) != 1 || ran[0] != args[0] {
			t.Errorf("Run(%q) ran %q, want %q alone", args, ran, args[0])
		}
	}
}
