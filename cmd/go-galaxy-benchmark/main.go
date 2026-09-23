// Package main is go-galaxy-benchmark: `run` times ansible-galaxy against
// go-galaxy and writes every run's wall clock to a JSON report, and `show`
// re-renders that report without touching the network or the measured tools.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/urfave/cli/v3"
)

// Sentinel errors, matched with errors.Is by the caller and by the tests.
var (
	errBinaryMissing       = errors.New("binary not found or not executable")
	errRequirementsMissing = errors.New("requirements file not found")
	errWorkDirRequired     = errors.New("--work-dir is required")
	errSizeInvalid         = errors.New("size must be a positive integer")
	errScenarioUnknown     = errors.New("unknown scenario")
	errFormatUnknown       = errors.New("unknown format")
	errReportEmpty         = errors.New("report contains no results")
)

// main is the CLI entry point.
func main() {
	os.Exit(run())
}

// run assembles the command tree and turns what comes back into an exit code.
// Two codes only: this is a measurement harness, and the caller's question is
// whether it produced a report, not which layer refused.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()

	app := &cli.Command{
		Name:  "go-galaxy-benchmark",
		Usage: "Time ansible-galaxy against go-galaxy over the testing/ requirements files",
		Commands: []*cli.Command{
			runCommand(),
			showCommand(),
		},
		HideHelpCommand: true,
	}

	if err := app.Run(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)

		return 1
	}

	return 0
}

// runCommand declares `run`: measure, write the report, print the table.
func runCommand() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "Measure both tools and write a JSON report",
		Flags: runFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			opts, err := optionsFromCLI(cmd)
			if err != nil {
				return err
			}

			report, err := runMeasurement(ctx, opts)
			if err != nil {
				return err
			}

			return renderTable(os.Stdout, report)
		},
	}
}

// runMeasurement owns the progress printer only while measuring: the table is
// written straight to stdout, so the spinner must be closed before the caller
// renders it or its last frame lands in the table's first line.
func runMeasurement(ctx context.Context, opts options) (*Report, error) {
	out := progress.New(opts.verbose, opts.quiet)
	defer out.Close()

	report, err := measure(ctx, out, opts)
	if err != nil {
		return nil, err
	}

	if err := saveReport(opts.reportPath, report); err != nil {
		return nil, err
	}

	out.Okf("report written to %s", opts.reportPath)

	return report, nil
}

// showCommand declares `show`: render a report that already exists.
func showCommand() *cli.Command {
	return &cli.Command{
		Name:  "show",
		Usage: "Render an existing JSON report as a table or an SVG chart",
		Flags: showFlags(),
		Action: func(_ context.Context, cmd *cli.Command) error {
			report, err := loadReport(cmd.String("report"))
			if err != nil {
				return err
			}

			return renderReport(report, cmd.String("format"), cmd.String("scenario"), cmd.String("out"))
		},
	}
}

// renderReport dispatches on the requested format. The table goes to stdout
// because that is where a shell pipeline expects it; the chart goes to a file
// because a terminal cannot show it.
func renderReport(report *Report, format, scenario, out string) error {
	switch format {
	case formatTable:
		return renderTable(os.Stdout, report)
	case formatSVG:
		return writeSVG(report, scenario, out)
	default:
		return fmt.Errorf("%w: %q (want %s or %s)", errFormatUnknown, format, formatTable, formatSVG)
	}
}
