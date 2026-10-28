package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResultStats(t *testing.T) {
	cases := []struct {
		name    string
		samples []int64
		want    Stats
	}{
		{name: "no successful run", samples: nil, want: Stats{}},
		{name: "single sample", samples: []int64{500}, want: Stats{MeanMS: 500, MinMS: 500, MaxMS: 500, Count: 1}},
		{
			name:    "spread",
			samples: []int64{300, 100, 200},
			want:    Stats{MeanMS: 200, MinMS: 100, MaxMS: 300, Count: 3},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Result{SamplesMS: tc.samples}.stats()
			if got != tc.want {
				t.Fatalf("stats() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSummarizeReportsFailuresRatherThanHidingThem(t *testing.T) {
	quiet := summarize(Result{SamplesMS: []int64{1000, 2000}})
	if !strings.Contains(quiet, "mean 1.500s over 2 runs") || strings.Contains(quiet, "failed") {
		t.Fatalf("summarize(clean) = %q", quiet)
	}

	noisy := summarize(Result{SamplesMS: []int64{1000}, Failed: 2})
	if !strings.Contains(noisy, "2 failed") {
		t.Fatalf("summarize(with failures) = %q, want it to count them", noisy)
	}

	dead := summarize(Result{Failed: 3})
	if !strings.Contains(dead, "no successful run out of 3") {
		t.Fatalf("summarize(all failed) = %q", dead)
	}
}

func TestSaveLoadReportRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", reportName)
	want := sampleReport()

	if err := saveReport(path, want); err != nil {
		t.Fatalf("saveReport: %v", err)
	}

	got, err := loadReport(path)
	if err != nil {
		t.Fatalf("loadReport: %v", err)
	}

	if len(got.Results) != len(want.Results) {
		t.Fatalf("loadReport returned %d results, want %d", len(got.Results), len(want.Results))
	}

	if got.Results[0].SamplesMS[0] != want.Results[0].SamplesMS[0] {
		t.Fatalf("first sample = %d, want %d", got.Results[0].SamplesMS[0], want.Results[0].SamplesMS[0])
	}

	if got.Tools["go-galaxy"].Version != want.Tools["go-galaxy"].Version {
		t.Fatalf("tool version did not survive the round trip: %+v", got.Tools)
	}
}

// TestSaveReportWritesAnEmptyArrayForNoSamples pins that a series with no
// successful run is written as "samples_ms": [] (encoding/json/v2), not the
// null v1 wrote for a nil slice.
func TestSaveReportWritesAnEmptyArrayForNoSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), reportName)
	report := sampleReport()
	report.Results = append(report.Results, Result{
		Scenario: scenarioCold, Tool: "go-galaxy", Size: 100, Failed: 2,
	})

	if err := saveReport(path, report); err != nil {
		t.Fatalf("saveReport: %v", err)
	}

	payload, err := os.ReadFile(path) //nolint:gosec // the path is this test's own temporary directory.
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}

	if !strings.Contains(string(payload), `"samples_ms": []`) {
		t.Fatalf("report does not carry an empty array for a series with no samples:\n%s", payload)
	}
}

func TestLoadReportRefusesADocumentWithNoResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), reportName)
	if err := os.WriteFile(path, []byte(`{"schema":1,"results":[]}`), fileMode); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	if _, err := loadReport(path); !errors.Is(err, errReportEmpty) {
		t.Fatalf("loadReport error = %v, want errReportEmpty", err)
	}
}

// TestReportOrderFollowsMeasurement guards the reason scenarios and sizes are
// collected rather than sorted: cold has to stay ahead of warm, and 100 after
// 10, neither of which alphabetical or numeric ordering would give for free.
func TestReportOrderFollowsMeasurement(t *testing.T) {
	report := &Report{Results: []Result{
		{Scenario: scenarioCold, Size: 10},
		{Scenario: scenarioCold, Size: 100},
		{Scenario: scenarioWarm, Size: 10},
		{Scenario: scenarioWarm, Size: 100},
	}}

	if got := report.scenarios(); len(got) != 2 || got[0] != scenarioCold {
		t.Fatalf("scenarios() = %v, want cold first", got)
	}

	if got := report.sizes(); len(got) != 2 || got[0] != 10 || got[1] != 100 {
		t.Fatalf("sizes() = %v, want [10 100]", got)
	}
}

func TestFindLocatesOneCell(t *testing.T) {
	report := sampleReport()

	if _, ok := report.find(scenarioCold, 1, "go-galaxy"); !ok {
		t.Fatal("find did not locate a result that is present")
	}

	if _, ok := report.find(scenarioWarm, 999, "go-galaxy"); ok {
		t.Fatal("find located a result that is not present")
	}
}

func TestMillisecondsKeepsSubsecondRunsReadable(t *testing.T) {
	if got := milliseconds(1242); got != "1.242s" {
		t.Fatalf("milliseconds(1242) = %q, want 1.242s", got)
	}

	if got := milliseconds(206); got != "0.206s" {
		t.Fatalf("milliseconds(206) = %q, want 0.206s", got)
	}
}

// sampleReport is a small but complete document the rendering and round-trip
// tests share.
func sampleReport() *Report {
	return &Report{
		Schema:      reportSchema,
		GeneratedAt: time.Unix(0, 0).UTC(),
		Host:        Host{OS: "linux", Arch: "amd64", Filesystem: "xfs", CPUs: 8},
		Tools: map[string]Tool{
			"ansible-galaxy": {Path: "/usr/bin/ansible-galaxy", Version: "ansible-galaxy [core 2.20.5]"},
			"go-galaxy":      {Path: "/usr/bin/go-galaxy", Version: "v1.0.2 (built by go) // go1.27.0"},
		},
		Runs:        2,
		ResolveDeps: true,
		Results: []Result{
			{Scenario: scenarioCold, Tool: "ansible-galaxy", Size: 1, SamplesMS: []int64{9000, 8980}},
			{Scenario: scenarioCold, Tool: "go-galaxy", Size: 1, SamplesMS: []int64{3000, 2860}},
		},
	}
}

// TestFilesystemOfPrefersStatThenFallsBackToDf drives both probes through fake
// stat and df binaries on PATH: GNU stat's type wins, otherwise df's device
// column, otherwise "unknown".
func TestFilesystemOfPrefersStatThenFallsBackToDf(t *testing.T) {
	const (
		gnuStat  = `[ "$1 $2 $3" = '-f -c %T' ] || exit 1; echo xfs`
		dfHeader = "echo 'Filesystem 512-blocks Used Available Capacity Mounted on'"
		dfRow    = "[ \"$1\" = -P ] || exit 1\n" + dfHeader + "\necho '/dev/sda1 100 50 50 50% /mnt/work'"
	)

	cases := []struct {
		name string
		stat string
		df   string
		want string
	}{
		{name: "GNU stat names the type", stat: gnuStat, df: "exit 1", want: "xfs"},
		{name: "blank stat answer falls back to df", stat: "echo '   '", df: dfRow, want: "/dev/sda1"},
		{name: "BSD stat refuses the flags", stat: "echo -c; exit 1", df: dfRow, want: "/dev/sda1"},
		{name: "no stat on PATH", df: dfRow, want: "/dev/sda1"},
		{name: "df prints only its header", stat: "exit 1", df: dfHeader, want: unknownValue},
		{name: "both probes fail", stat: "exit 1", df: "exit 1", want: unknownValue},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if tc.stat != "" {
				writeExecutable(t, bin, "stat", "#!/bin/sh\n"+tc.stat+"\n")
			}

			writeExecutable(t, bin, "df", "#!/bin/sh\n"+tc.df+"\n")
			t.Setenv("PATH", bin)

			if got := filesystemOf(t.Context(), t.TempDir()); got != tc.want {
				t.Fatalf("filesystemOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFilesystemOfGivesUpWithItsContext(t *testing.T) {
	bin := t.TempDir()
	writeExecutable(t, bin, "stat", "#!/bin/sh\necho xfs\n")
	writeExecutable(t, bin, "df", "#!/bin/sh\necho header; echo '/dev/sda1 1 1 0 100% /'\n")
	t.Setenv("PATH", bin)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if got := filesystemOf(ctx, t.TempDir()); got != unknownValue {
		t.Fatalf("filesystemOf(canceled) = %q, want %q", got, unknownValue)
	}
}

func TestLoadReportNamesTheFileItCannotUse(t *testing.T) {
	dir := t.TempDir()
	malformed := filepath.Join(dir, "malformed.json")

	if err := os.WriteFile(malformed, []byte(`{"schema": 1, "results": [`), fileMode); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	missing := filepath.Join(dir, "missing.json")

	if _, err := loadReport(missing); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "reading "+missing) {
		t.Fatalf("loadReport(missing) error = %v, want a read error naming the file", err)
	}

	if _, err := loadReport(malformed); err == nil || !strings.Contains(err.Error(), "parsing "+malformed) {
		t.Fatalf("loadReport(malformed) error = %v, want a parse error naming the file", err)
	}
}

func TestSaveReportNamesWhatItCannotWrite(t *testing.T) {
	dir := t.TempDir()

	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, fileMode); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	cases := []struct {
		name string
		path string
		want string
	}{
		{name: "directory under a file", path: filepath.Join(blocker, reportName), want: "creating report directory"},
		{name: "path is a directory", path: dir, want: "writing " + dir},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := saveReport(tc.path, sampleReport()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("saveReport(%s) error = %v, want %q", tc.path, err, tc.want)
			}
		})
	}
}
