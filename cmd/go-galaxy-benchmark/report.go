package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// reportSchema is the version of the JSON document below. Bump it when a
// field changes meaning, so `show` can refuse a report it would misread
// rather than render it wrongly.
const reportSchema = 1

// filesystemProbeTimeout bounds the two commands describeHost shells out to.
// Either can block indefinitely on an unresponsive mount, and neither is
// worth stalling a measurement for.
const filesystemProbeTimeout = 3 * time.Second

// unknownValue is what a probe reports when it cannot answer. None of these
// fields is load-bearing, so a failure degrades the report rather than the
// measurement.
const unknownValue = "unknown"

// dfHeaderAndBody is the minimum line count a df listing must have before its
// last line can be read as a row rather than as the header.
const dfHeaderAndBody = 2

// Report is the whole document `run` writes and `show` reads. Raw per-run
// samples are stored rather than a mean, so a chart can be redrawn with
// different statistics without repeating the measurement.
type Report struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Tools       map[string]Tool `json:"tools"`
	Results     []Result        `json:"results"`
	Host        Host            `json:"host"`
	Schema      int             `json:"schema"`
	Runs        int             `json:"runs"`
	ResolveDeps bool            `json:"resolve_deps"`
}

// Host records what the numbers depend on but the harness does not control.
type Host struct {
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Filesystem string `json:"filesystem"`
	CPUs       int    `json:"cpus"`
}

// Tool records which build produced a column of numbers.
type Tool struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// Result is one tool, one scenario and one size. SamplesMS holds only the
// runs that exited zero; Failed counts the rest, and LastError carries the
// most recent failure's tail so a report explains itself without a log.
type Result struct {
	Scenario  string  `json:"scenario"`
	Tool      string  `json:"tool"`
	LastError string  `json:"last_error,omitempty"`
	SamplesMS []int64 `json:"samples_ms"`
	Size      int     `json:"size"`
	Failed    int     `json:"failed"`
}

// Stats is what a Result reduces to for rendering.
type Stats struct {
	MeanMS float64
	MinMS  int64
	MaxMS  int64
	Count  int
}

// stats reduces the samples. A Result with no successful run reports a zero
// Count, which every renderer treats as "no measurement" rather than as zero
// seconds.
func (r Result) stats() Stats {
	if len(r.SamplesMS) == 0 {
		return Stats{}
	}

	out := Stats{MinMS: r.SamplesMS[0], MaxMS: r.SamplesMS[0], Count: len(r.SamplesMS)}
	total := int64(0)

	for _, sample := range r.SamplesMS {
		total += sample

		if sample < out.MinMS {
			out.MinMS = sample
		}

		if sample > out.MaxMS {
			out.MaxMS = sample
		}
	}

	out.MeanMS = float64(total) / float64(len(r.SamplesMS))

	return out
}

// summarize renders one Result as the single line the progress output prints
// when a series finishes.
func summarize(r Result) string {
	stat := r.stats()
	if stat.Count == 0 {
		return fmt.Sprintf("no successful run out of %d", r.Failed)
	}

	if r.Failed > 0 {
		return fmt.Sprintf("mean %s over %d runs, %d failed", milliseconds(stat.MeanMS), stat.Count, r.Failed)
	}

	return fmt.Sprintf("mean %s over %d runs", milliseconds(stat.MeanMS), stat.Count)
}

// milliseconds renders a duration in seconds with three decimals, which is
// the resolution a subsecond warm install needs to be readable at all.
func milliseconds(ms float64) string {
	return fmt.Sprintf("%.3fs", ms/float64(time.Second/time.Millisecond))
}

// reportIndent is what the document is indented with. A report is read by
// people as often as by `show`, so it is written indented rather than dense.
const reportIndent = "  "

// saveReport writes the document, creating its directory. encoding/json/v2
// writes a series with no successful run as "samples_ms": [] where v1 wrote
// null, and still reads a v1-written report unchanged.
func saveReport(path string, report *Report) error {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return fmt.Errorf("creating report directory: %w", err)
	}

	payload, err := json.Marshal(report, jsontext.WithIndent(reportIndent))
	if err != nil {
		return fmt.Errorf("encoding report: %w", err)
	}

	if err := os.WriteFile(path, append(payload, '\n'), fileMode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

// loadReport reads a document written by saveReport.
func loadReport(path string) (*Report, error) {
	payload, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own argument.
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	report := &Report{}
	if err := json.Unmarshal(payload, report); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	if len(report.Results) == 0 {
		return nil, fmt.Errorf("%w: %s", errReportEmpty, path)
	}

	return report, nil
}

// scenarios lists the scenarios present in the report, in the order they were
// measured rather than alphabetically, so cold stays ahead of warm.
func (r *Report) scenarios() []string {
	seen := make(map[string]struct{}, len(r.Results))
	order := make([]string, 0, len(r.Results))

	for _, result := range r.Results {
		if _, ok := seen[result.Scenario]; !ok {
			seen[result.Scenario] = struct{}{}
			order = append(order, result.Scenario)
		}
	}

	return order
}

// sizes lists the sizes present in the report, in measurement order.
func (r *Report) sizes() []int {
	seen := make(map[int]struct{}, len(r.Results))
	order := make([]int, 0, len(r.Results))

	for _, result := range r.Results {
		if _, ok := seen[result.Size]; !ok {
			seen[result.Size] = struct{}{}
			order = append(order, result.Size)
		}
	}

	return order
}

// find returns the result for one scenario, size and tool.
func (r *Report) find(scenario string, size int, tool string) (Result, bool) {
	for _, result := range r.Results {
		if result.Scenario == scenario && result.Size == size && result.Tool == tool {
			return result, true
		}
	}

	return Result{}, false
}

// filesystemOf names the filesystem a directory sits on. GNU stat answers
// directly with a type; elsewhere the device name at least identifies the
// volume. Neither answer is load-bearing, so a failure reports "unknown".
func filesystemOf(ctx context.Context, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, filesystemProbeTimeout)
	defer cancel()

	// Both command names are literals; the only variable is the operator's
	// own working directory, which this process created.
	statCmd := exec.CommandContext(ctx, "stat", "-f", "-c", "%T", dir) //nolint:gosec
	if out, err := statCmd.Output(); err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return name
		}
	}

	dfCmd := exec.CommandContext(ctx, "df", "-P", dir) //nolint:gosec

	out, err := dfCmd.Output()
	if err != nil {
		return unknownValue
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < dfHeaderAndBody {
		return unknownValue
	}

	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) == 0 {
		return unknownValue
	}

	return fields[0]
}
