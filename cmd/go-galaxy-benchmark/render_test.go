package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// relativeLuminance is WCAG 2.1's definition for an sRGB hex color. It is
// spelled out here rather than approximated so the guard below states the
// property chartInk exists to satisfy instead of restating its hex digits.
func relativeLuminance(t *testing.T, color string) float64 {
	t.Helper()

	var channel [3]float64

	for i := range channel {
		raw, err := strconv.ParseUint(color[1+2*i:3+2*i], 16, 8)
		if err != nil {
			t.Fatalf("parsing %q: %v", color, err)
		}

		value := float64(raw) / 255
		if value <= 0.04045 {
			channel[i] = value / 12.92
		} else {
			channel[i] = math.Pow((value+0.055)/1.055, 2.4)
		}
	}

	return 0.2126*channel[0] + 0.7152*channel[1] + 0.0722*channel[2]
}

// contrastRatio is WCAG 2.1's ratio between two sRGB hex colors.
func contrastRatio(t *testing.T, a, b string) float64 {
	t.Helper()

	high, low := relativeLuminance(t, a), relativeLuminance(t, b)
	if high < low {
		high, low = low, high
	}

	return (high + 0.05) / (low + 0.05)
}

// TestChartInkReadsOnBothCanvases pins chartInk's contrast on both a white
// page and GitHub dark's #0d1117 at a floor just under the 4.35:1 ceiling, so
// only a shade readable in both themes passes.
func TestChartInkReadsOnBothCanvases(t *testing.T) {
	const (
		floor     = 4.3
		lightPage = "#ffffff"
		darkPage  = "#0d1117"
	)

	for _, canvas := range []string{lightPage, darkPage} {
		if got := contrastRatio(t, chartInk, canvas); got < floor {
			t.Fatalf("contrast of chartInk %s on %s = %.2f, want at least %.2f",
				chartInk, canvas, got, floor)
		}
	}
}

// TestEmitSVGPaintsEveryGlyphInOneInk pins that text tiers differ by size and
// weight, not color: a second fill would spend chartInk's contrast budget at
// the expense of the smallest text.
func TestEmitSVGPaintsEveryGlyphInOneInk(t *testing.T) {
	svg := emitSVG("t", "s", []chartPanel{{
		title: "Cold cache",
		rows:  []chartRow{{label: "1 collection", detail: "2.0s -> 1.0s", value: 2}},
	}})

	_, rest, found := strings.Cut(svg, "<style>")
	if !found {
		t.Fatalf("SVG carries no style block:\n%s", svg)
	}

	style, _, found := strings.Cut(rest, "</style>")
	if !found {
		t.Fatalf("SVG style block is unterminated:\n%s", svg)
	}

	if got := strings.Count(style, "fill:"); got != 1 {
		t.Fatalf("style block declares %d text fills, want exactly one:\n%s", got, style)
	}

	if !strings.Contains(style, "text { fill: "+chartInk+"; }") {
		t.Fatalf("style block does not paint text in chartInk %s:\n%s", chartInk, style)
	}
}

// TestLogScaleEndsTheLongestBarAtTheColumnEdge pins that the largest ratio's
// bar fills the bar column exactly, whatever it is, and a ratio ten times
// smaller is exactly one decade shorter.
func TestLogScaleEndsTheLongestBarAtTheColumnEdge(t *testing.T) {
	for _, peak := range []float64{3.08, 21.1, 281.2, 5000} {
		scale := newLogScale([]chartPanel{{rows: []chartRow{{value: peak}}}})

		if got := scale.width(peak); math.Abs(got-barColumnW) > 0.001 {
			t.Fatalf("width(%v) = %v, want the full bar column %d", peak, got, barColumnW)
		}
	}
}

// TestLogScaleSpacesDecadesEvenly is the other half of what makes the axis a
// logarithmic one: every tenfold step is the same distance, wherever on the
// axis it is taken.
func TestLogScaleSpacesDecadesEvenly(t *testing.T) {
	scale := newLogScale([]chartPanel{{rows: []chartRow{{value: 281.2}}}})

	for _, pair := range [][2]float64{{10, 1}, {100, 10}, {281.2, 28.12}} {
		if got := scale.width(pair[0]) - scale.width(pair[1]); math.Abs(got-scale.unitsPerDecade) > 0.001 {
			t.Fatalf("the step from %v to %v measured %v, want one decade (%v)",
				pair[1], pair[0], got, scale.unitsPerDecade)
		}
	}
}

// TestLogScaleIsSharedAcrossPanels is the property a single set of gridlines
// depends on: the same ratio is the same width whichever panel carries it,
// so a warm bar and a cold bar can be read against each other.
func TestLogScaleIsSharedAcrossPanels(t *testing.T) {
	scale := newLogScale([]chartPanel{
		{title: "Cold cache", rows: []chartRow{{value: 3.08}, {value: 18.41}}},
		{title: "Warm cache", rows: []chartRow{{value: 18.41}, {value: 281.2}}},
	})

	if scale.width(18.41) != scale.width(18.41) || scale.width(281.2) <= scale.width(18.41) {
		t.Fatalf("scale is not monotone across panels: %v vs %v", scale.width(18.41), scale.width(281.2))
	}

	if got := scale.width(281.2); math.Abs(got-barColumnW) > 0.001 {
		t.Fatalf("width of the global peak = %v, want the full bar column %d", got, barColumnW)
	}
}

// TestLogScaleGivesNoBarToWhatIsNotFaster covers the shape a logarithm cannot
// draw: a ratio at or below 1x has no positive width, and rendering its
// logarithm directly would put the bar to the left of its own origin.
func TestLogScaleGivesNoBarToWhatIsNotFaster(t *testing.T) {
	scale := newLogScale([]chartPanel{{rows: []chartRow{{value: 0.5}, {value: 1}}}})

	for _, value := range []float64{0.1, 0.5, 1} {
		if got := scale.width(value); got != 0 {
			t.Fatalf("width(%v) = %v, want 0 - that is not a speedup", value, got)
		}
	}
}

// TestLogScaleDrawsOnlyTheDecadesABarReaches keeps the axis honest: a rule
// stands at every power of ten the longest bar passes, and at none beyond it.
func TestLogScaleDrawsOnlyTheDecadesABarReaches(t *testing.T) {
	cases := map[float64][]string{
		3.08:  {"1" + glyphTimes},
		18.41: {"1" + glyphTimes, "10" + glyphTimes},
		281.2: {"1" + glyphTimes, "10" + glyphTimes, "100" + glyphTimes},
	}

	for peak, want := range cases {
		lines := newLogScale([]chartPanel{{rows: []chartRow{{value: peak}}}}).gridlines()

		got := make([]string, 0, len(lines))
		for _, line := range lines {
			got = append(got, line.label)
		}

		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("gridlines for peak %v = %v, want %v", peak, got, want)
		}

		if lines[0].x != barColumnX {
			t.Fatalf("the 1%s rule stands at x=%d, want the bars' own origin %d", glyphTimes, lines[0].x, barColumnX)
		}
	}
}

func TestTrimFloat(t *testing.T) {
	cases := map[float64]string{25: "25", 3.1: "3.1", 3.14159: "3.14", 500: "500", 0.5: "0.5"}
	for value, want := range cases {
		if got := trimFloat(value); got != want {
			t.Fatalf("trimFloat(%v) = %q, want %q", value, got, want)
		}
	}
}

func TestShortVersion(t *testing.T) {
	cases := map[string]string{
		"ansible-galaxy [core 2.20.5]":                    "2.20.5",
		"v1.0.2 (built by go) // go1.27.0":                "v1.0.2",
		"v1.0.3-0.20260822031409-58c23c37177d (commit x)": "v1.0.3-0.20260822031409-58c23c37177d",
		"unknown": "unknown",
	}

	for line, want := range cases {
		if got := shortVersion(line); got != want {
			t.Fatalf("shortVersion(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestCollectionCount(t *testing.T) {
	if got := collectionCount(1); got != "1 collection" {
		t.Fatalf("collectionCount(1) = %q", got)
	}

	if got := collectionCount(10); got != "10 collections" {
		t.Fatalf("collectionCount(10) = %q", got)
	}
}

func TestSpeedupRefusesToDivideByAMissingMeasurement(t *testing.T) {
	report := sampleReport()

	ratio, ok := speedup(report, scenarioCold, 1)
	if !ok {
		t.Fatal("speedup reported nothing for a cell both tools measured")
	}

	if ratio < 3 || ratio > 3.1 {
		t.Fatalf("speedup = %v, want about 3.06", ratio)
	}

	// A series where every run failed carries no samples. Treating that as
	// zero seconds would print an infinite speedup.
	report.Results = append(report.Results,
		Result{Scenario: scenarioWarm, Tool: "ansible-galaxy", Size: 1, SamplesMS: []int64{5000}},
		Result{Scenario: scenarioWarm, Tool: "go-galaxy", Size: 1, Failed: 3},
	)

	if _, ok := speedup(report, scenarioWarm, 1); ok {
		t.Fatal("speedup produced a ratio against a series with no successful run")
	}

	if _, ok := speedup(report, scenarioCold, 999); ok {
		t.Fatal("speedup produced a ratio for a size nobody measured")
	}
}

func TestRenderTableGolden(t *testing.T) {
	var out strings.Builder
	if err := renderTable(&out, sampleReport()); err != nil {
		t.Fatalf("renderTable: %v", err)
	}

	got := out.String()

	// The provenance block names the tool and then quotes its --version line
	// verbatim, which is why the first field repeats.
	for _, want := range []string{
		"ansible-galaxy  ansible-galaxy [core 2.20.5]", //nolint:dupword
		"host            linux/amd64, 8 cpus, xfs",
		"measurement     2 runs, dependencies resolved",
		"SCENARIO  SIZE  TOOL            MEAN    MIN     MAX     FAILED",
		"cold      1     ansible-galaxy  8.990s  8.980s  9.000s  0",
		"cold      1     go-galaxy       2.930s  2.860s  3.000s  0",
		"cold      1     speedup         3.1x",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderTable output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderTableSaysWhenNothingSucceeded(t *testing.T) {
	report := sampleReport()
	report.Results = append(report.Results,
		Result{Scenario: scenarioWarm, Tool: "go-galaxy", Size: 1, Failed: 5})

	var out strings.Builder
	if err := renderTable(&out, report); err != nil {
		t.Fatalf("renderTable: %v", err)
	}

	if !strings.Contains(out.String(), "no successful run") {
		t.Fatalf("renderTable hid a series that never succeeded:\n%s", out.String())
	}
}

// TestEmitSVGPlacesRowsOnTheDesignedGrid pins the designed geometry: a fixed
// pixel size beside the viewBox, the first bar where the legend puts it, and
// each further bar one row pitch below the last.
func TestEmitSVGPlacesRowsOnTheDesignedGrid(t *testing.T) {
	panel := chartPanel{title: "Cold cache", rows: []chartRow{
		{label: "10 collections", detail: "152.28s -> 7.71s", value: 19.8},
		{label: "100 collections", detail: "458.10s -> 21.67s", value: 21.1},
	}}

	svg := emitSVG("title", "subtitle", []chartPanel{panel})

	firstBarY := firstLegendY + legendToBar

	for _, want := range []string{
		fmt.Sprintf(`width="%d" height="%d"`, canvasWidth, firstBarY+rowPitch+barHeight+axisOverhang+axisLabelDrop+bottomMatter),
		fmt.Sprintf(`<rect x="%d" y="%d" width="%s"`, barColumnX, firstBarY, trimFloat(newLogScale([]chartPanel{panel}).width(19.8))),
		fmt.Sprintf(`<rect x="%d" y="%d" width="%d" height="%d"`, barColumnX, firstBarY+rowPitch, barColumnW, barHeight),
		`>19.8` + glyphTimes + `</text>`,
	} {
		if !strings.Contains(svg, want) {
			t.Fatalf("SVG missing %q:\n%s", want, svg)
		}
	}
}

// TestEmitSVGStatesAPixelSizeRatherThanStretching pins that the chart carries
// no percentage width, which would stretch it across a whole README page.
func TestEmitSVGStatesAPixelSizeRatherThanStretching(t *testing.T) {
	svg := emitSVG("t", "s", []chartPanel{{title: "Cold cache", rows: []chartRow{{value: 2}}}})

	if strings.Contains(svg, `width="100%`) {
		t.Fatalf("SVG asks for the full width of its container:\n%s", svg)
	}

	if !strings.Contains(svg, fmt.Sprintf(`width="%d"`, canvasWidth)) {
		t.Fatalf("SVG does not state its designed pixel width:\n%s", svg)
	}
}

func TestEmitSVGEscapesWhatItDidNotAuthor(t *testing.T) {
	panel := chartPanel{title: "Cold cache", rows: []chartRow{
		{label: "1 collection", detail: "a & b <script>", value: 2},
	}}

	svg := emitSVG("t", `go-galaxy "v1" & co`, []chartPanel{panel})

	if strings.Contains(svg, "<script>") {
		t.Fatalf("SVG carries an unescaped tag:\n%s", svg)
	}

	if !strings.Contains(svg, "&amp;") {
		t.Fatalf("SVG did not escape an ampersand:\n%s", svg)
	}
}

func TestBuildPanelsFiltersByScenario(t *testing.T) {
	report := sampleReport()
	report.Results = append(report.Results,
		Result{Scenario: scenarioWarm, Tool: "ansible-galaxy", Size: 1, SamplesMS: []int64{5000}},
		Result{Scenario: scenarioWarm, Tool: "go-galaxy", Size: 1, SamplesMS: []int64{1000}},
	)

	if got := buildPanels(report, ""); len(got) != 2 {
		t.Fatalf("buildPanels(all) returned %d panels, want 2", len(got))
	}

	got := buildPanels(report, scenarioWarm)
	if len(got) != 1 || got[0].title != "Warm cache" {
		t.Fatalf("buildPanels(warm) = %+v, want one Warm cache panel", got)
	}
}

func TestWriteSVGCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "charts", "bench.svg")
	if err := writeSVG(sampleReport(), "", path); err != nil {
		t.Fatalf("writeSVG: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("chart was not written: %v", err)
	}
}

func TestWriteSVGRefusesAScenarioWithNothingInIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bench.svg")
	if err := writeSVG(sampleReport(), scenarioWarm, path); err == nil {
		t.Fatal("writeSVG rendered a chart for a scenario the report does not carry")
	}
}
