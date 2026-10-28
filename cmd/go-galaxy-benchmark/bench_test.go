package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestAnsibleTargetSeesOnlyItsInstallTree(t *testing.T) {
	tgt := ansibleTarget(options{workDir: t.TempDir()})

	for _, want := range []string{
		"ANSIBLE_COLLECTIONS_PATH=" + tgt.install,
		"ANSIBLE_COLLECTIONS_SCAN_SYS_PATH=false",
	} {
		if !hasEnv(tgt.env, want) {
			t.Fatalf("env = %v lacks %q; a collection found elsewhere turns the run into a no-op", tgt.env, want)
		}
	}
}

// hasEnv reports whether the environment carries an exact key=value pair.
func hasEnv(env []string, want string) bool {
	return slices.Contains(env, want)
}

// TestMeasureColdWipesTheCacheAndWarmKeepsIt reads the fake tools' log: a
// cold run must find no cache, a warm run the cache its unmeasured prime
// left, and every run an empty install tree.
func TestMeasureColdWipesTheCacheAndWarmKeepsIt(t *testing.T) {
	bench := newFakeBench(t, 0)

	report, err := measure(t.Context(), &recordingPrinter{}, bench.opts)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}

	want := []string{
		"ansible-galaxy cold empty resolved requirements-1.yml",
		"ansible-galaxy cold empty resolved requirements-1.yml",
		"go-galaxy cold empty resolved requirements-1.yml",
		"go-galaxy cold empty resolved requirements-1.yml",
		"ansible-galaxy cold empty resolved requirements-1.yml",
		"ansible-galaxy warm empty resolved requirements-1.yml",
		"ansible-galaxy warm empty resolved requirements-1.yml",
		"go-galaxy cold empty resolved requirements-1.yml",
		"go-galaxy warm empty resolved requirements-1.yml",
		"go-galaxy warm empty resolved requirements-1.yml",
	}

	if got := readLog(t, bench.log); !slices.Equal(got, want) {
		t.Fatalf("invocations =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	for _, result := range report.Results {
		if len(result.SamplesMS) != bench.opts.runs || result.Failed != 0 || result.LastError != "" {
			t.Fatalf("result %+v, want %d clean samples", result, bench.opts.runs)
		}
	}
}

func TestMeasureDescribesWhatItMeasured(t *testing.T) {
	bench := newFakeBench(t, 0)
	bench.opts.runs = 1

	report, err := measure(t.Context(), &recordingPrinter{}, bench.opts)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}

	header := *report
	header.GeneratedAt, header.Tools, header.Results, header.Host = time.Time{}, nil, nil, Host{}

	if want := (Report{Schema: reportSchema, Runs: 1, ResolveDeps: true}); !reflect.DeepEqual(header, want) {
		t.Fatalf("report header = %+v, want %+v", header, want)
	}

	if report.GeneratedAt.IsZero() || report.GeneratedAt.Location() != time.UTC {
		t.Fatalf("generated_at = %v, want a UTC timestamp", report.GeneratedAt)
	}

	wantHost := Host{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.GOMAXPROCS(0), Filesystem: report.Host.Filesystem}
	if report.Host != wantHost || report.Host.Filesystem == "" {
		t.Fatalf("host = %+v, want this process's platform and a filesystem name", report.Host)
	}

	// The version line is the first non-blank line, so a banner's second
	// line never leaks into the report header.
	wantTools := map[string]Tool{
		"ansible-galaxy": {Path: bench.opts.ansibleGalaxy, Version: "ansible-galaxy 9.9.9"},
		"go-galaxy":      {Path: bench.opts.goGalaxy, Version: "go-galaxy 9.9.9"},
	}
	if !maps.Equal(report.Tools, wantTools) {
		t.Fatalf("tools = %+v, want %+v", report.Tools, wantTools)
	}
}

// TestMeasureOrdersSeriesBySizeFirst pins the loop order the report relies on:
// sizes outermost in the order asked, then scenarios, then ansible-galaxy
// ahead of go-galaxy, so an interrupted run keeps whole sizes.
func TestMeasureOrdersSeriesBySizeFirst(t *testing.T) {
	bench := newFakeBench(t, 0)
	bench.opts.sizes = []int{10, 1}
	bench.opts.scenarios = []string{scenarioWarm, scenarioCold}
	bench.opts.runs = 1
	bench.opts.resolveDeps = false

	report, err := measure(t.Context(), &recordingPrinter{}, bench.opts)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}

	want := []string{
		"warm 10 ansible-galaxy", "warm 10 go-galaxy", "cold 10 ansible-galaxy", "cold 10 go-galaxy",
		"warm 1 ansible-galaxy", "warm 1 go-galaxy", "cold 1 ansible-galaxy", "cold 1 go-galaxy",
	}

	got := make([]string, 0, len(report.Results))
	for _, result := range report.Results {
		got = append(got, fmt.Sprintf("%s %d %s", result.Scenario, result.Size, result.Tool))
	}

	if !slices.Equal(got, want) {
		t.Fatalf("results = %v, want %v", got, want)
	}

	if report.ResolveDeps {
		t.Fatal("report claims dependencies were resolved for a --no-deps run")
	}

	for _, line := range readLog(t, bench.log) {
		if !strings.Contains(line, " flat ") {
			t.Fatalf("invocation %q did not receive --no-deps", line)
		}
	}
}

// TestMeasureCountsFailedRunsAsData covers the rule that a failing tool is a
// result rather than an abort: its runs are counted, its stderr kept, and the
// other tool's series still measured.
func TestMeasureCountsFailedRunsAsData(t *testing.T) {
	bench := newFakeBench(t, 3)
	bench.opts.scenarios = []string{scenarioWarm}
	printer := &recordingPrinter{}

	report, err := measure(t.Context(), printer, bench.opts)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}

	failed, _ := report.find(scenarioWarm, 1, "go-galaxy")
	if got := summarize(failed); got != "no successful run out of 2" {
		t.Fatalf("go-galaxy result = %+v (%s), want two failed runs and no samples", failed, got)
	}

	for _, want := range []string{"go-galaxy: resolver said no", "exit status 3"} {
		if !strings.Contains(failed.LastError, want) {
			t.Fatalf("last_error = %q, want it to carry %q", failed.LastError, want)
		}
	}

	if clean, _ := report.find(scenarioWarm, 1, "ansible-galaxy"); len(clean.SamplesMS) != 2 || clean.Failed != 0 {
		t.Fatalf("ansible-galaxy result = %+v, want two clean samples", clean)
	}

	printed := strings.Join(printer.lines, "\n")
	for _, want := range []string{
		"go-galaxy: priming run failed, measured runs will not be warm",
		"go-galaxy warm size 1: run 2/2 failed after ",
		"go-galaxy warm size 1: no successful run out of 2",
		"ansible-galaxy warm size 1: mean ",
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("progress output lacks %q:\n%s", want, printed)
		}
	}
}

func TestMeasureStopsWhenInterrupted(t *testing.T) {
	bench := newFakeBench(t, 0)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	report, err := measure(ctx, &recordingPrinter{}, bench.opts)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("measure error = %v, want an interruption wrapping context.Canceled", err)
	}

	if report != nil {
		t.Fatalf("measure returned a report for an interrupted run: %+v", report)
	}

	if got := readLog(t, bench.log); got != nil {
		t.Fatalf("a tool ran after the interruption: %v", got)
	}
}

func TestMeasureRefusesAWorkDirItCannotCreate(t *testing.T) {
	bench := newFakeBench(t, 0)

	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, fileMode); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	bench.opts.workDir = filepath.Join(blocker, "work")

	report, err := measure(t.Context(), &recordingPrinter{}, bench.opts)
	if err == nil || !strings.Contains(err.Error(), bench.opts.workDir) {
		t.Fatalf("measure error = %v, want one naming %s", err, bench.opts.workDir)
	}

	if report != nil || readLog(t, bench.log) != nil {
		t.Fatal("measure went on after failing to prepare its work dir")
	}
}

func TestResetNamesTheDirectoryItCannotPrepare(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, fileMode); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatalf("creating fixture: %v", err)
	}

	cases := []struct {
		name         string
		dir          string
		verb         string
		unprivileged bool
	}{
		{name: "parent is a file", dir: filepath.Join(blocker, "cache"), verb: "wiping "},
		{name: "parent is read-only", dir: filepath.Join(locked, "cache"), verb: "creating ", unprivileged: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unprivileged && os.Geteuid() == 0 {
				t.Skip("root writes through a read-only directory")
			}

			err := reset(tc.dir)
			if err == nil || !strings.Contains(err.Error(), tc.verb+tc.dir) {
				t.Fatalf("reset(%s) error = %v, want one naming the directory", tc.dir, err)
			}
		})
	}
}

func TestToolVersionReadsTheFirstLineOrSaysUnknown(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "banner", body: "printf '  \\nansible-galaxy [core 2.20.5]\\n  config file = None\\n'", want: "ansible-galaxy [core 2.20.5]"},
		{name: "single line", body: "echo v1.0.2", want: "v1.0.2"},
		{name: "refuses the flag", body: "echo 'unknown flag' >&2; exit 2", want: unknownValue},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := writeExecutable(t, dir, strings.ReplaceAll(tc.name, " ", "-"), "#!/bin/sh\n"+tc.body+"\n")

			if got := toolVersion(t.Context(), bin); got != tc.want {
				t.Fatalf("toolVersion = %q, want %q", got, tc.want)
			}
		})
	}

	if got := toolVersion(t.Context(), filepath.Join(dir, "absent")); got != unknownValue {
		t.Fatalf("toolVersion(absent) = %q, want %q", got, unknownValue)
	}
}
