package main

import (
	"errors"
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
