package main

import (
	"fmt"
	"html"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

// Table layout. tabwriter needs a minimum cell width, padding and a pad
// character; these three are the whole configuration.
const (
	tableMinWidth = 2
	tableTabWidth = 2
	tablePadding  = 2
)

// Chart layout, in user units of the viewBox. Every drop is measured from the
// top of the thing it belongs to (a bar, a legend swatch), not the document,
// so a panel can be placed by its own origin alone.
const (
	canvasWidth   = 706
	gutterX       = 4
	labelColumnX  = 148
	barColumnX    = 160
	barColumnW    = 391
	valueColumnX  = 576
	rightMargin   = 10
	titleDrop     = 24
	subtitleDrop  = 43
	firstLegendY  = 70
	legendSize    = 10
	legendRadius  = 3
	legendTextX   = 20
	legendDrop    = 9
	legendToBar   = 25
	panelGap      = 37
	rowPitch      = 42
	barHeight     = 14
	barRadius     = 7
	labelDrop     = 5
	detailDrop    = 20
	valueDrop     = 11
	axisOverhang  = 7
	axisLabelDrop = 16
	bottomMatter  = 14
	fileMode      = 0o644
	decadeStep    = 10
)

// chartInk is the one color of every glyph: at 4.35:1 against both #ffffff and
// GitHub dark's #0d1117 it is the best any single color reaches. chartInkFaint
// is opacity rather than a lighter color, so the decade rules suit both themes.
const (
	chartInk      = "#6f7b81"
	chartInkFaint = "0.5"
)

// barColor is the fill for the panel at index, one entry per scenario the
// benchmark defines and cycling if a report ever carries more. A function
// rather than a package-level palette so nothing can reassign it.
func barColor(index int) string {
	palette := []string{"#4493f8", "#3fb950"}

	return palette[index%len(palette)]
}

// Glyphs the chart draws that are not ASCII: the caption's middle dot, the
// multiplication sign after a ratio, and the arrow between two means. Spelled
// as escapes so this file stays ASCII.
const (
	glyphSeparator = "\u00b7"
	glyphTimes     = "\u00d7"
	glyphArrow     = "\u2192"
)

// tableWriter records the first write error so the row emitters below stay
// straight-line code. tabwriter buffers anyway, so a real I/O failure lands
// at Flush rather than at any one Fprintf.
type tableWriter struct {
	w   io.Writer
	err error
}

// printf writes one row unless an earlier write already failed.
func (t *tableWriter) printf(format string, args ...any) {
	if t.err != nil {
		return
	}

	_, t.err = fmt.Fprintf(t.w, format, args...)
}

// renderTable writes the report as a plain text table. Every scenario and
// size gets a speedup row, because the ratio is the number the whole exercise
// exists to produce and reading it off two adjacent means is needless work.
func renderTable(w io.Writer, report *Report) error {
	if err := renderHeader(w, report); err != nil {
		return err
	}

	aligned := tabwriter.NewWriter(w, tableMinWidth, tableTabWidth, tablePadding, ' ', 0)
	table := &tableWriter{w: aligned}

	table.printf("SCENARIO\tSIZE\tTOOL\tMEAN\tMIN\tMAX\tFAILED\n")

	for _, scenario := range report.scenarios() {
		for _, size := range report.sizes() {
			renderRows(table, report, scenario, size)
		}
	}

	if table.err != nil {
		return fmt.Errorf("writing table: %w", table.err)
	}

	if err := aligned.Flush(); err != nil {
		return fmt.Errorf("flushing table: %w", err)
	}

	return nil
}

// renderHeader writes the provenance block above the table: which builds were
// measured, on what, and under which flags.
func renderHeader(w io.Writer, report *Report) error {
	deps := "--no-deps"
	if report.ResolveDeps {
		deps = "dependencies resolved"
	}

	_, err := fmt.Fprintf(w,
		"ansible-galaxy  %s\ngo-galaxy       %s\nhost            %s/%s, %d cpus, %s\nmeasurement     %d runs, %s\n\n",
		report.Tools["ansible-galaxy"].Version,
		report.Tools["go-galaxy"].Version,
		report.Host.OS, report.Host.Arch, report.Host.CPUs, report.Host.Filesystem,
		report.Runs, deps,
	)
	if err != nil {
		return fmt.Errorf("writing report header: %w", err)
	}

	return nil
}

// renderRows writes both tools' rows for one scenario and size, followed by
// their ratio.
func renderRows(w *tableWriter, report *Report, scenario string, size int) {
	for _, tool := range []string{"ansible-galaxy", "go-galaxy"} {
		result, ok := report.find(scenario, size, tool)
		if !ok {
			continue
		}

		stat := result.stats()
		if stat.Count == 0 {
			w.printf("%s\t%d\t%s\tno successful run\t\t\t%d\n", scenario, size, tool, result.Failed)

			continue
		}

		w.printf("%s\t%d\t%s\t%s\t%s\t%s\t%d\n",
			scenario, size, tool,
			milliseconds(stat.MeanMS),
			milliseconds(float64(stat.MinMS)),
			milliseconds(float64(stat.MaxMS)),
			result.Failed)
	}

	if ratio, ok := speedup(report, scenario, size); ok {
		w.printf("%s\t%d\tspeedup\t%.1fx\t\t\t\n", scenario, size, ratio)
	}
}

// speedup is how many times faster go-galaxy was. It reports false when
// either side has no successful run, so a missing measurement never becomes a
// ratio against zero.
func speedup(report *Report, scenario string, size int) (float64, bool) {
	slow, okSlow := report.find(scenario, size, "ansible-galaxy")
	fast, okFast := report.find(scenario, size, "go-galaxy")

	if !okSlow || !okFast {
		return 0, false
	}

	slowStat, fastStat := slow.stats(), fast.stats()
	if slowStat.Count == 0 || fastStat.Count == 0 || fastStat.MeanMS <= 0 {
		return 0, false
	}

	return slowStat.MeanMS / fastStat.MeanMS, true
}

// chartRow is one bar: how many times faster, and the absolute pair the ratio
// came from.
type chartRow struct {
	label  string
	detail string
	value  float64
}

// chartPanel is one scenario's worth of bars.
type chartPanel struct {
	title string
	rows  []chartRow
}

// writeSVG renders the report as a bar chart and writes it to path. Bars carry
// the ratio, since seconds three orders of magnitude apart cannot share an
// axis; the absolute pair moves into the row's text.
func writeSVG(report *Report, scenario, path string) error {
	panels := buildPanels(report, scenario)
	if len(panels) == 0 {
		return fmt.Errorf("%w: nothing to chart for scenario %q", errReportEmpty, scenario)
	}

	subtitle := fmt.Sprintf("ansible-galaxy %s / go-galaxy %s "+glyphSeparator+" %s/%s "+
		glyphSeparator+" %s "+glyphSeparator+" %d runs",
		shortVersion(report.Tools["ansible-galaxy"].Version),
		shortVersion(report.Tools["go-galaxy"].Version),
		report.Host.OS, report.Host.Arch, report.Host.Filesystem, report.Runs)

	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return fmt.Errorf("creating chart directory: %w", err)
	}

	svg := emitSVG("go-galaxy vs ansible-galaxy - install speedup", subtitle, panels)

	if err := os.WriteFile(path, []byte(svg), fileMode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

// buildPanels turns the report into one panel per scenario, skipping any
// scenario the caller filtered out and any row without a ratio.
func buildPanels(report *Report, scenario string) []chartPanel {
	panels := make([]chartPanel, 0, len(report.scenarios()))

	for _, name := range report.scenarios() {
		if scenario != "" && name != scenario {
			continue
		}

		panel := chartPanel{title: strings.ToUpper(name[:1]) + name[1:] + " cache"}

		for _, size := range report.sizes() {
			ratio, ok := speedup(report, name, size)
			if !ok {
				continue
			}

			panel.rows = append(panel.rows, chartRow{
				label:  collectionCount(size),
				detail: chartDetail(report, name, size),
				value:  ratio,
			})
		}

		if len(panel.rows) > 0 {
			panels = append(panels, panel)
		}
	}

	return panels
}

// chartDetail renders the absolute pair a ratio came from.
func chartDetail(report *Report, scenario string, size int) string {
	slow, _ := report.find(scenario, size, "ansible-galaxy")
	fast, _ := report.find(scenario, size, "go-galaxy")

	return milliseconds(slow.stats().MeanMS) + " " + glyphArrow + " " + milliseconds(fast.stats().MeanMS)
}

// logScale is the chart's logarithmic horizontal axis: user units per decade
// of speedup, sized so the largest ratio ends at the bar column's right edge.
// Every panel shares it, which makes bars comparable across panels.
type logScale struct {
	unitsPerDecade float64
	decades        float64
}

// newLogScale sizes the axis from the largest ratio any panel carries.
func newLogScale(panels []chartPanel) logScale {
	peak := 1.0

	for _, panel := range panels {
		for _, row := range panel.rows {
			if row.value > peak {
				peak = row.value
			}
		}
	}

	// Nothing measured was faster, so there is no decade to span: one decade
	// keeps the axis drawable with every bar honestly at zero width.
	decades := math.Log10(peak)
	if decades <= 0 {
		decades = 1
	}

	return logScale{unitsPerDecade: barColumnW / decades, decades: decades}
}

// width is how wide value's bar is. A ratio at or below 1 is not a speedup;
// it gets no bar rather than a negative one, and its figure still prints.
func (s logScale) width(value float64) float64 {
	if value <= 1 {
		return 0
	}

	return math.Log10(value) * s.unitsPerDecade
}

// gridline is one decade of the axis: where its rule stands and what the
// tick under it reads.
type gridline struct {
	label string
	x     int
}

// gridlines returns one entry per power of ten the axis reaches, starting at
// 1x under the bars' own origin. A decade the longest bar does not reach is
// not drawn, so the rightmost rule is always one a bar could stand against.
func (s logScale) gridlines() []gridline {
	out := make([]gridline, 0, int(s.decades)+1)

	for decade := 0; float64(decade) <= s.decades; decade++ {
		out = append(out, gridline{
			label: trimFloat(math.Pow(decadeStep, float64(decade))) + glyphTimes,
			x:     barColumnX + int(math.Round(float64(decade)*s.unitsPerDecade)),
		})
	}

	return out
}

// panelPlacement is where one panel sits, and in which color. The whole stack
// is placed before anything is drawn, because the gridlines run its full height.
type panelPlacement struct {
	color   string
	panel   chartPanel
	legendY int
}

// placePanels stacks the panels top to bottom, returning each one's placement
// and the top of the last bar drawn.
func placePanels(panels []chartPanel) ([]panelPlacement, int) {
	placed := make([]panelPlacement, 0, len(panels))
	legendY := firstLegendY
	lastBarY := legendY + legendToBar

	for i, panel := range panels {
		placed = append(placed, panelPlacement{
			panel:   panel,
			color:   barColor(i),
			legendY: legendY,
		})
		lastBarY = legendY + legendToBar + (len(panel.rows)-1)*rowPitch
		legendY = lastBarY + panelGap
	}

	return placed, lastBarY
}

// emitSVG writes the document: two heading lines, the shared axis, then one
// legend-and-bars group per panel. The axis is drawn before the panels so a
// bar covers the rules it crosses rather than being crossed by them.
func emitSVG(title, subtitle string, panels []chartPanel) string {
	scale := newLogScale(panels)
	placed, lastBarY := placePanels(panels)

	axisTop := firstLegendY + legendToBar - axisOverhang
	axisBottom := lastBarY + barHeight + axisOverhang
	height := axisBottom + axisLabelDrop + bottomMatter

	var out strings.Builder

	writeSVGHead(&out, title, subtitle, height)
	writeSVGAxis(&out, scale, axisTop, axisBottom)

	for _, placement := range placed {
		writeSVGPanel(&out, placement, scale)
	}

	out.WriteString("</svg>\n")

	return out.String()
}

// writeSVGHead opens the document, declares the palette, and writes the two
// heading lines. Pixel width and height beside the viewBox keep the chart at
// its designed size instead of stretching to its container.
func writeSVGHead(out *strings.Builder, title, subtitle string, height int) {
	fmt.Fprintf(out, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" role="img"`+
		` font-family="-apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif">`+"\n",
		canvasWidth, height, canvasWidth, height)
	fmt.Fprintf(out, "  <title>%s</title>\n", html.EscapeString(title))
	out.WriteString("  <desc>Speedup factors on a shared logarithmic axis, one bar group per scenario.</desc>\n")
	fmt.Fprintf(out, "  <style>\n"+
		"    text { fill: %s; }\n"+
		"    .g { stroke: %s; stroke-opacity: %s; stroke-width: 1; }\n"+
		"    .t { font-size: 15px; font-weight: 600; }\n"+
		"    .s { font-size: 11px; }\n"+
		"    .p { font-size: 12.5px; font-weight: 600; }\n"+
		"    .l { font-size: 12.5px; }\n"+
		"    .v { font-size: 13px; font-weight: 600; }\n"+
		"    .a, .d { font-size: 10.5px; }\n"+
		"  </style>\n", chartInk, chartInk, chartInkFaint)
	fmt.Fprintf(out, "  <text class=\"t\" x=\"%d\" y=\"%d\">%s</text>\n", gutterX, titleDrop, html.EscapeString(title))
	fmt.Fprintf(out, "  <text class=\"s\" x=\"%d\" y=\"%d\">%s</text>\n", gutterX, subtitleDrop, html.EscapeString(subtitle))
}

// writeSVGAxis draws the decade rules across the whole stack and the ticks
// under them, followed by the caption naming the scale - without which a
// reader would take the bars for a linear comparison.
func writeSVGAxis(out *strings.Builder, scale logScale, axisTop, axisBottom int) {
	out.WriteString("\n")

	for _, line := range scale.gridlines() {
		fmt.Fprintf(out, "  <line class=\"g\" x1=\"%d\" y1=\"%d\" x2=\"%d\" y2=\"%d\"/>\n",
			line.x, axisTop, line.x, axisBottom)
	}

	tickY := axisBottom + axisLabelDrop

	for _, line := range scale.gridlines() {
		fmt.Fprintf(out, "  <text class=\"a\" x=\"%d\" y=\"%d\" text-anchor=\"middle\">%s</text>\n",
			line.x, tickY, line.label)
	}

	fmt.Fprintf(out, "  <text class=\"a\" x=\"%d\" y=\"%d\" text-anchor=\"end\">logarithmic scale</text>\n",
		canvasWidth-rightMargin, tickY)
}

// writeSVGPanel writes one panel: its legend swatch and heading, then a bar
// per row with the row's label and the two means above and below it.
func writeSVGPanel(out *strings.Builder, placement panelPlacement, scale logScale) {
	fmt.Fprintf(out, "\n  <rect x=\"%d\" y=\"%d\" width=\"%d\" height=\"%d\" rx=\"%d\" fill=\"%s\"/>\n",
		gutterX, placement.legendY, legendSize, legendSize, legendRadius, placement.color)
	fmt.Fprintf(out, "  <text class=\"p\" x=\"%d\" y=\"%d\">%s</text>\n",
		legendTextX, placement.legendY+legendDrop, html.EscapeString(placement.panel.title))

	for i, row := range placement.panel.rows {
		barY := placement.legendY + legendToBar + i*rowPitch

		fmt.Fprintf(out, "  <rect x=\"%d\" y=\"%d\" width=\"%s\" height=\"%d\" rx=\"%d\" fill=\"%s\"/>\n",
			barColumnX, barY, trimFloat(scale.width(row.value)), barHeight, barRadius, placement.color)
		fmt.Fprintf(out, "  <text class=\"l\" x=\"%d\" y=\"%d\" text-anchor=\"end\">%s</text>\n",
			labelColumnX, barY+labelDrop, html.EscapeString(row.label))
		fmt.Fprintf(out, "  <text class=\"d\" x=\"%d\" y=\"%d\" text-anchor=\"end\">%s</text>\n",
			labelColumnX, barY+detailDrop, html.EscapeString(row.detail))
		fmt.Fprintf(out, "  <text class=\"v\" x=\"%d\" y=\"%d\">%s%s</text>\n",
			valueColumnX, barY+valueDrop, trimFloat(row.value), glyphTimes)
	}
}

// shortVersion reduces a --version line to what a caption can carry: the core
// version from "ansible-galaxy [core 2.20.5]", or go-galaxy's first token.
func shortVersion(line string) string {
	if _, rest, found := strings.Cut(line, "[core "); found {
		if version, _, closed := strings.Cut(rest, "]"); closed {
			return version
		}
	}

	first, _, _ := strings.Cut(strings.TrimSpace(line), " ")

	return first
}

// collectionCount labels a row, in the singular where that is what it is.
func collectionCount(size int) string {
	if size == 1 {
		return "1 collection"
	}

	return fmt.Sprintf("%d collections", size)
}

// trimFloat prints a number without a trailing zero fraction, so 25 stays 25.
func trimFloat(value float64) string {
	text := fmt.Sprintf("%.2f", value)
	if strings.Contains(text, ".") {
		text = strings.TrimRight(text, "0")
		text = strings.TrimSuffix(text, ".")
	}

	return text
}
