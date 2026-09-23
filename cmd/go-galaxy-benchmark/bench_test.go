package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMergeEnvReplacesInheritedKeys(t *testing.T) {
	base := []string{"PATH=/bin", "GO_GALAXY_CACHE_DIR=/inherited", "HOME=/home/user"}
	got := mergeEnv(base, []string{"GO_GALAXY_CACHE_DIR=/measured", "TMPDIR=/measured/tmp"})

	// The inherited value has to be gone rather than merely shadowed: which of
	// two duplicate keys a child sees is platform-dependent, and a benchmark
	// that silently used the operator's own cache would measure nothing.
	if hasEnv(got, "GO_GALAXY_CACHE_DIR=/inherited") {
		t.Fatalf("inherited value survived: %v", got)
	}

	if !hasEnv(got, "GO_GALAXY_CACHE_DIR=/measured") || !hasEnv(got, "TMPDIR=/measured/tmp") {
		t.Fatalf("overrides missing: %v", got)
	}

	if !hasEnv(got, "PATH=/bin") || !hasEnv(got, "HOME=/home/user") {
		t.Fatalf("unrelated variables were dropped: %v", got)
	}
}

func TestTargetCommandArgs(t *testing.T) {
	opts := options{workDir: t.TempDir(), ansibleGalaxy: "/bin/true", goGalaxy: "/bin/true"}

	cases := []struct {
		name        string
		target      target
		want        []string
		resolveDeps bool
	}{
		{
			name:   "ansible-galaxy resolving dependencies",
			target: ansibleTarget(opts), resolveDeps: true,
			want: []string{"collection", "install", "-r", "req.yml", "-p"},
		},
		{
			name:   "ansible-galaxy flat",
			target: ansibleTarget(opts), resolveDeps: false,
			want: []string{"collection", "install", "--no-deps", "-r", "req.yml", "-p"},
		},
		{
			name:   "go-galaxy resolving dependencies",
			target: goGalaxyTarget(opts), resolveDeps: true,
			want: []string{"install", "-r", "req.yml", "-p"},
		},
		{
			name:   "go-galaxy flat",
			target: goGalaxyTarget(opts), resolveDeps: false,
			want: []string{"install", "--no-deps", "-r", "req.yml", "-p"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tc.target.command(t.Context(), "req.yml", tc.resolveDeps)
			got := cmd.Args[1:]

			if len(got) != len(tc.want)+1 {
				t.Fatalf("args = %v, want %v plus the install path", got, tc.want)
			}

			for i, want := range tc.want {
				if got[i] != want {
					t.Fatalf("args = %v, want %v plus the install path", got, tc.want)
				}
			}

			if got[len(got)-1] != tc.target.install {
				t.Fatalf("install path = %q, want %q", got[len(got)-1], tc.target.install)
			}
		})
	}
}

// TestCommandGivesTheChildNoStdin pins a nil Stdin, which exec turns into
// /dev/null: a measured tool that prompts reads EOF and exits instead of
// blocking forever, which would be indistinguishable from a hang.
func TestCommandGivesTheChildNoStdin(t *testing.T) {
	opts := options{workDir: t.TempDir(), goGalaxy: "/bin/true"}

	cmd := goGalaxyTarget(opts).command(t.Context(), "req.yml", true)
	if cmd.Stdin != nil {
		t.Fatal("command set a Stdin; the child must read from /dev/null")
	}
}

// TestOnceCarriesTheFailureBackWithItsStderr covers the other half of the
// same design: a run that fails is data, so its exit status and the tail of
// what it complained about have to reach the report.
func TestOnceCarriesTheFailureBackWithItsStderr(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fail.sh")
	body := "#!/bin/sh\necho 'resolver said no' >&2\nexit 3\n"

	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // a test fixture must be executable.
		t.Fatalf("writing fixture: %v", err)
	}

	tgt := target{name: "fake", bin: script, install: t.TempDir()}

	elapsed, err := tgt.once(t.Context(), "req.yml", true)
	if err == nil {
		t.Fatal("once() returned no error for a command that exited 3")
	}

	if elapsed <= 0 {
		t.Fatalf("once() reported %v elapsed for a run that happened", elapsed)
	}

	if !strings.Contains(err.Error(), "resolver said no") {
		t.Fatalf("once() error = %q, want it to carry the stderr tail", err)
	}

	if !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("once() error = %q, want it to carry the exit status", err)
	}
}

func TestTailBufferKeepsOnlyTheTail(t *testing.T) {
	tail := &tailBuffer{limit: 8}

	for _, chunk := range []string{"aaaaaaaa", "bbbb", "cccc"} {
		if _, err := tail.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if got := tail.String(); got != "bbbbcccc" {
		t.Fatalf("String() = %q, want bbbbcccc", got)
	}
}

func TestTailBufferCollapsesWhitespaceIntoOneLine(t *testing.T) {
	tail := &tailBuffer{limit: stderrTailBytes}
	if _, err := tail.Write([]byte("first line\n\n  second line\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := tail.String(); got != "first line second line" {
		t.Fatalf("String() = %q, want a single line", got)
	}
}

func TestResetRecreatesDirectoriesEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(filepath.Join(dir, "leftover"), dirMode); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := reset(dir); err != nil {
		t.Fatalf("reset: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}

	if len(entries) != 0 {
		t.Fatalf("reset left %d entries behind", len(entries))
	}
}

func TestTargetsDoNotShareADirectory(t *testing.T) {
	opts := options{workDir: t.TempDir()}
	ansible, goGalaxy := ansibleTarget(opts), goGalaxyTarget(opts)

	pairs := [][2]string{
		{ansible.cache, goGalaxy.cache},
		{ansible.install, goGalaxy.install},
		{ansible.tempDir(), goGalaxy.tempDir()},
	}

	for _, pair := range pairs {
		if pair[0] == pair[1] {
			t.Fatalf("both tools were given %q; neither may warm the other", pair[0])
		}
	}
}

// hasEnv reports whether the environment carries an exact key=value pair.
func hasEnv(env []string, want string) bool {
	return slices.Contains(env, want)
}
