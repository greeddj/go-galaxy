package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"
)

// The environment prefix is GGB_ rather than GO_GALAXY_ on purpose: this
// process sets GO_GALAXY_* for the binary it measures, and one prefix would
// blur configuring the stopwatch with configuring the thing being timed.
const envPrefix = "GGB_"

// Defaults. Five runs is enough for the cold rows to show their spread
// without turning a 100-collection series into an afternoon.
const (
	runsDefault      = 5
	sizesDefault     = "1,10,100"
	scenariosDefault = scenarioCold + "," + scenarioWarm
	reportName       = "report.json"
	dirMode          = 0o755
)

// Scenario and format names, spelled once.
const (
	scenarioCold = "cold"
	scenarioWarm = "warm"
	formatTable  = "table"
	formatSVG    = "svg"
)

// options is the validated result of parsing the run flags. Every path in it
// is absolute, and every binary in it has been confirmed executable, so the
// measurement loop never has to decide what to do about a bad argument.
type options struct {
	ansibleGalaxy   string
	goGalaxy        string
	workDir         string
	requirementsDir string
	reportPath      string
	scenarios       []string
	sizes           []int
	runs            int
	resolveDeps     bool
	verbose         bool
	quiet           bool
}

// spinnerActive reports whether the progress printer will draw a spinner,
// mirroring internal/progress's rule: stdout is a terminal and the run is
// neither quiet nor verbose. The live timing line exists only there.
func (o options) spinnerActive() bool {
	if o.quiet || o.verbose {
		return false
	}

	info, err := os.Stdout.Stat()

	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// runFlags declares the flags `run` accepts, in two groups: what to measure
// with, and what to measure. The split keeps either group readable.
func runFlags() []cli.Flag {
	return append(subjectFlags(), measurementFlags()...)
}

// subjectFlags names the binaries and the directories the run works in.
func subjectFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "ansible-galaxy",
			Usage:   "Path to the ansible-galaxy binary; looked up in PATH when unset",
			Value:   "ansible-galaxy",
			Sources: cli.EnvVars(envPrefix + "ANSIBLE_GALAXY"),
		},
		&cli.StringFlag{
			Name:    "go-galaxy",
			Usage:   "Path to the go-galaxy binary; looked up in PATH when unset",
			Value:   "go-galaxy",
			Sources: cli.EnvVars(envPrefix + "GO_GALAXY"),
		},
		&cli.StringFlag{
			Name:    "work-dir",
			Usage:   "Directory holding both tools' caches, install targets, temp trees and the report",
			Sources: cli.EnvVars(envPrefix + "WORK_DIR"),
		},
		&cli.StringFlag{
			Name:    "requirements-dir",
			Usage:   "Directory holding requirements-<size>.yml",
			Value:   "testing",
			Sources: cli.EnvVars(envPrefix + "REQUIREMENTS_DIR"),
		},
	}
}

// measurementFlags shapes the measurement and its output.
func measurementFlags() []cli.Flag {
	return []cli.Flag{
		&cli.IntFlag{
			Name:    "runs",
			Usage:   "Measured runs per scenario and size",
			Value:   runsDefault,
			Sources: cli.EnvVars(envPrefix + "RUNS"),
		},
		&cli.StringFlag{
			Name:    "sizes",
			Usage:   "Comma-separated requirements sizes to measure",
			Value:   sizesDefault,
			Sources: cli.EnvVars(envPrefix + "SIZES"),
		},
		&cli.StringFlag{
			Name:    "scenarios",
			Usage:   "Comma-separated scenarios to measure: " + scenarioCold + ", " + scenarioWarm,
			Value:   scenariosDefault,
			Sources: cli.EnvVars(envPrefix + "SCENARIOS"),
		},
		&cli.BoolFlag{
			Name: "no-deps",
			Usage: "Pass --no-deps to both tools, measuring fetch plus extract over one flat set. " +
				"Off by default: the transitive graph is what a real install pays for, and both " +
				"resolvers agree on these sets",
			Sources: cli.EnvVars(envPrefix + "NO_DEPS"),
		},
		&cli.StringFlag{
			Name:    "report",
			Usage:   "Where to write the JSON report; defaults to " + reportName + " inside --work-dir",
			Sources: cli.EnvVars(envPrefix + "REPORT"),
		},
		&cli.BoolFlag{
			Name:    "verbose",
			Usage:   "Print every command before it runs",
			Sources: cli.EnvVars(envPrefix + "VERBOSE"),
		},
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "Suppress progress lines; the final table still prints",
			Sources: cli.EnvVars(envPrefix + "QUIET"),
		},
	}
}

// showFlags declares the flags `show` accepts.
func showFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "report",
			Usage:   "Path to a JSON report written by `run`",
			Value:   reportName,
			Sources: cli.EnvVars(envPrefix + "REPORT"),
		},
		&cli.StringFlag{
			Name:    "format",
			Usage:   "Output format: " + formatTable + " or " + formatSVG,
			Value:   formatTable,
			Sources: cli.EnvVars(envPrefix + "FORMAT"),
		},
		&cli.StringFlag{
			Name:    "scenario",
			Usage:   "Render only this scenario; empty renders every scenario in the report",
			Sources: cli.EnvVars(envPrefix + "SCENARIO"),
		},
		&cli.StringFlag{
			Name:    "out",
			Usage:   "File to write the chart to",
			Value:   "benchmark.svg",
			Sources: cli.EnvVars(envPrefix + "OUT"),
		},
	}
}

// optionsFromCLI validates every argument before any measurement starts, so a
// typo costs a second rather than the hour a cold 100-collection series takes.
func optionsFromCLI(cmd *cli.Command) (options, error) {
	workDir := cmd.String("work-dir")
	if workDir == "" {
		return options{}, errWorkDirRequired
	}

	workDir, err := filepath.Abs(workDir)
	if err != nil {
		return options{}, fmt.Errorf("resolving --work-dir: %w", err)
	}

	opts := options{
		workDir:     workDir,
		runs:        cmd.Int("runs"),
		resolveDeps: !cmd.Bool("no-deps"),
		reportPath:  cmd.String("report"),
		verbose:     cmd.Bool("verbose"),
		quiet:       cmd.Bool("quiet"),
	}

	if opts.reportPath == "" {
		opts.reportPath = filepath.Join(workDir, reportName)
	}

	if opts.ansibleGalaxy, err = resolveBinary(cmd.String("ansible-galaxy")); err != nil {
		return options{}, err
	}

	if opts.goGalaxy, err = resolveBinary(cmd.String("go-galaxy")); err != nil {
		return options{}, err
	}

	if opts.requirementsDir, err = filepath.Abs(cmd.String("requirements-dir")); err != nil {
		return options{}, fmt.Errorf("resolving --requirements-dir: %w", err)
	}

	if opts.sizes, err = parseSizes(cmd.String("sizes")); err != nil {
		return options{}, err
	}

	if opts.scenarios, err = parseScenarios(cmd.String("scenarios")); err != nil {
		return options{}, err
	}

	return opts, opts.checkRequirements()
}

// resolveBinary turns a flag value into an absolute path to something
// executable, accepting both a bare name to look up in PATH and a path.
func resolveBinary(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", errBinaryMissing, name, err)
	}

	return filepath.Abs(path)
}

// parseSizes turns "1,10,100" into the sizes to measure.
func parseSizes(raw string) ([]int, error) {
	fields := splitList(raw)
	sizes := make([]int, 0, len(fields))

	for _, field := range fields {
		size, err := strconv.Atoi(field)
		if err != nil || size < 1 {
			return nil, fmt.Errorf("%w: %q", errSizeInvalid, field)
		}

		sizes = append(sizes, size)
	}

	if len(sizes) == 0 {
		return nil, fmt.Errorf("%w: empty list", errSizeInvalid)
	}

	return sizes, nil
}

// parseScenarios turns "cold,warm" into the scenarios to measure, refusing a
// name it does not implement rather than silently measuring less.
func parseScenarios(raw string) ([]string, error) {
	fields := splitList(raw)
	scenarios := make([]string, 0, len(fields))

	for _, field := range fields {
		if field != scenarioCold && field != scenarioWarm {
			return nil, fmt.Errorf("%w: %q (want %s or %s)", errScenarioUnknown, field, scenarioCold, scenarioWarm)
		}

		scenarios = append(scenarios, field)
	}

	if len(scenarios) == 0 {
		return nil, fmt.Errorf("%w: empty list", errScenarioUnknown)
	}

	return scenarios, nil
}

// splitList splits a comma-separated flag value, tolerating spaces and empty
// fields so "1, 10, 100" and "1,10,100," both mean the same thing.
func splitList(raw string) []string {
	fields := make([]string, 0, strings.Count(raw, ",")+1)

	for field := range strings.SplitSeq(raw, ",") {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			fields = append(fields, trimmed)
		}
	}

	return fields
}

// checkRequirements confirms every requirements file exists before the first
// measurement, for the same reason optionsFromCLI validates at all.
func (o options) checkRequirements() error {
	for _, size := range o.sizes {
		path := o.requirementsFile(size)
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%w: %s: %w", errRequirementsMissing, path, err)
		}
	}

	return nil
}

// requirementsFile names the requirements file for a size.
func (o options) requirementsFile(size int) string {
	return filepath.Join(o.requirementsDir, fmt.Sprintf("requirements-%d.yml", size))
}
