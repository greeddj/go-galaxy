package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// savedReport writes sampleReport to a temporary file and returns its path.
func savedReport(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), reportName)
	if err := saveReport(path, sampleReport()); err != nil {
		t.Fatalf("saveReport: %v", err)
	}

	return path
}

// readChart returns the chart written at path.
func readChart(t *testing.T, path string) string {
	t.Helper()

	payload, err := os.ReadFile(path) //nolint:gosec // the path is this test's own temporary directory.
	if err != nil {
		t.Fatalf("reading chart: %v", err)
	}

	return string(payload)
}

// withArgs replaces os.Args for the test, since run reads the process's own.
func withArgs(t *testing.T, args ...string) {
	t.Helper()

	saved := os.Args
	os.Args = append([]string{"go-galaxy-benchmark"}, args...)

	t.Cleanup(func() { os.Args = saved })
}

func TestRenderReportDispatchesOnFormat(t *testing.T) {
	chart := filepath.Join(t.TempDir(), "charts", "bench.svg")

	var err error

	table := captureOutput(t, &os.Stdout, func() { err = renderReport(sampleReport(), formatTable, "", chart) })
	if err != nil {
		t.Fatalf("renderReport(table): %v", err)
	}

	if !strings.Contains(table, "SCENARIO  SIZE  TOOL") {
		t.Fatalf("renderReport(table) wrote no table to stdout:\n%s", table)
	}

	if _, statErr := os.Stat(chart); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("renderReport(table) touched the chart path: %v", statErr)
	}

	if err := renderReport(sampleReport(), formatSVG, scenarioCold, chart); err != nil {
		t.Fatalf("renderReport(svg): %v", err)
	}

	if svg := readChart(t, chart); !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, "Cold cache") {
		t.Fatalf("renderReport(svg) wrote no chart of the cold scenario:\n%s", svg)
	}

	err = renderReport(sampleReport(), "html", "", chart)
	if !errors.Is(err, errFormatUnknown) || !strings.Contains(err.Error(), `"html" (want table or svg)`) {
		t.Fatalf("renderReport(html) error = %v, want errFormatUnknown naming the choices", err)
	}
}

// TestShowCommandReadsFlagsAndEnvironment drives `show` end to end, once with
// flags and once with the GGB_ variables, which must mean the same thing.
func TestShowCommandReadsFlagsAndEnvironment(t *testing.T) {
	report := savedReport(t)

	cases := []struct {
		env  func(chart string) map[string]string
		args func(chart string) []string
		name string
	}{
		{
			name: "flags",
			args: func(chart string) []string {
				return []string{"--report", report, "--format", formatSVG, "--scenario", scenarioCold, "--out", chart}
			},
		},
		{
			name: "environment",
			env: func(chart string) map[string]string {
				return map[string]string{
					"GGB_REPORT": report, "GGB_FORMAT": formatSVG, "GGB_SCENARIO": scenarioCold, "GGB_OUT": chart,
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearBenchEnv(t)

			chart := filepath.Join(t.TempDir(), "bench.svg")
			args := []string{"show"}

			if tc.args != nil {
				args = append(args, tc.args(chart)...)
			}

			if tc.env != nil {
				for key, value := range tc.env(chart) {
					t.Setenv(key, value)
				}
			}

			if err := showCommand().Run(t.Context(), args); err != nil {
				t.Fatalf("show: %v", err)
			}

			if svg := readChart(t, chart); !strings.Contains(svg, "Cold cache") {
				t.Fatalf("show wrote no cold-cache chart:\n%s", svg)
			}
		})
	}
}

func TestShowCommandRefusesWhatItCannotRender(t *testing.T) {
	clearBenchEnv(t)

	report := savedReport(t)
	chart := filepath.Join(t.TempDir(), "bench.svg")

	cases := []struct {
		want error
		name string
		args []string
	}{
		{name: "missing report", args: []string{"--report", filepath.Join(t.TempDir(), "absent.json")}, want: fs.ErrNotExist},
		{name: "unknown format", args: []string{"--report", report, "--format", "png"}, want: errFormatUnknown},
		{
			name: "scenario not in the report",
			args: []string{"--report", report, "--format", formatSVG, "--scenario", scenarioWarm, "--out", chart},
			want: errReportEmpty,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := showCommand().Run(t.Context(), append([]string{"show"}, tc.args...)); !errors.Is(err, tc.want) {
				t.Fatalf("show error = %v, want %v", err, tc.want)
			}
		})
	}

	if _, err := os.Stat(chart); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused show still wrote a chart: %v", err)
	}
}

// TestRunCommandMeasuresWritesTheReportAndPrintsTheTable runs `run` over the
// fake tools: the report lands in the work dir and the table on stdout.
func TestRunCommandMeasuresWritesTheReportAndPrintsTheTable(t *testing.T) {
	clearBenchEnv(t)

	bench := newFakeBench(t, 0)
	args := []string{
		"run", "--work-dir", bench.opts.workDir,
		"--ansible-galaxy", bench.opts.ansibleGalaxy, "--go-galaxy", bench.opts.goGalaxy,
		"--requirements-dir", bench.opts.requirementsDir,
		"--sizes", "1", "--runs", "1", "--scenarios", scenarioCold, "--quiet",
	}

	var err error

	stdout := captureOutput(t, &os.Stdout, func() { err = runCommand().Run(t.Context(), args) })
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	report, err := loadReport(filepath.Join(bench.opts.workDir, reportName))
	if err != nil {
		t.Fatalf("loading the report run wrote: %v", err)
	}

	if len(report.Results) != 2 || report.Runs != 1 {
		t.Fatalf("report = %d results over %d runs, want 2 over 1", len(report.Results), report.Runs)
	}

	for _, want := range []string{
		"ansible-galaxy  ansible-galaxy 9.9.9", //nolint:dupword
		"measurement     1 runs, dependencies resolved",
		"SCENARIO  SIZE  TOOL",
		"cold      1     ansible-galaxy  ",
		"cold      1     go-galaxy       ",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("run printed no %q:\n%s", want, stdout)
		}
	}
}

func TestRunCommandStopsAtTheFirstRefusal(t *testing.T) {
	clearBenchEnv(t)

	bench := newFakeBench(t, 0)

	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, fileMode); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	cases := []struct {
		name    string
		mention string
		args    []string
	}{
		{name: "no work dir", args: []string{"--sizes", "1"}, mention: errWorkDirRequired.Error()},
		{
			name: "work dir under a file",
			args: []string{
				"--work-dir", filepath.Join(blocker, "work"), "--ansible-galaxy", bench.opts.ansibleGalaxy,
				"--go-galaxy", bench.opts.goGalaxy, "--requirements-dir", bench.opts.requirementsDir, "--sizes", "1", "-q",
			},
			mention: filepath.Join(blocker, "work"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error

			stdout := captureOutput(t, &os.Stdout, func() { err = runCommand().Run(t.Context(), append([]string{"run"}, tc.args...)) })
			if err == nil || !strings.Contains(err.Error(), tc.mention) {
				t.Fatalf("run error = %v, want one naming %q", err, tc.mention)
			}

			if strings.Contains(stdout, "SCENARIO") {
				t.Fatalf("run printed a table after a refusal:\n%s", stdout)
			}
		})
	}

	if got := readLog(t, bench.log); got != nil {
		t.Fatalf("a refused run still invoked a tool: %v", got)
	}
}

// TestRunMeasurementReportsAReportItCannotSave covers the last step: the
// measurement happened, but a report path it cannot write is still an error.
func TestRunMeasurementReportsAReportItCannotSave(t *testing.T) {
	bench := newFakeBench(t, 0)
	bench.opts.runs = 1
	bench.opts.scenarios = []string{scenarioCold}
	bench.opts.quiet = true

	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, fileMode); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	bench.opts.reportPath = filepath.Join(blocker, reportName)

	var (
		report *Report
		err    error
	)

	captureOutput(t, &os.Stdout, func() { report, err = runMeasurement(t.Context(), bench.opts) })

	if err == nil || !strings.Contains(err.Error(), "creating report directory") {
		t.Fatalf("runMeasurement error = %v, want the report directory refusal", err)
	}

	if report != nil {
		t.Fatalf("runMeasurement returned a report it did not save: %+v", report)
	}

	if got := len(readLog(t, bench.log)); got != 2 {
		t.Fatalf("fake tools ran %d times, want one cold run each", got)
	}
}

// TestRunTurnsTheOutcomeIntoTwoExitCodes pins the harness's whole exit-code
// contract: 0 when the command did its work, 1 with "error: " on stderr
// otherwise.
func TestRunTurnsTheOutcomeIntoTwoExitCodes(t *testing.T) {
	clearBenchEnv(t)

	report := savedReport(t)
	chart := filepath.Join(t.TempDir(), "bench.svg")
	missing := filepath.Join(t.TempDir(), "absent.json")

	cases := []struct {
		name   string
		stderr string
		args   []string
		want   int
	}{
		{name: "chart rendered", args: []string{"show", "--report", report, "--format", formatSVG, "--out", chart}},
		{name: "missing report", args: []string{"show", "--report", missing}, want: 1, stderr: "error: reading " + missing},
		{
			name: "unknown format", args: []string{"show", "--report", report, "--format", "html"},
			want: 1, stderr: `error: unknown format: "html"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withArgs(t, tc.args...)

			var code int

			stderr := captureOutput(t, &os.Stderr, func() { code = run() })
			if code != tc.want {
				t.Fatalf("run() = %d, want %d (stderr %q)", code, tc.want, stderr)
			}

			if tc.stderr == "" && stderr != "" {
				t.Fatalf("run() wrote %q to stderr on success", stderr)
			}

			if !strings.Contains(stderr, tc.stderr) {
				t.Fatalf("stderr = %q, want %q", stderr, tc.stderr)
			}
		})
	}

	if svg := readChart(t, chart); !strings.HasPrefix(svg, "<svg") {
		t.Fatalf("the successful run wrote no chart:\n%s", svg)
	}
}
