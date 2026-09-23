package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/output"
)

// stderrTailBytes bounds what is kept from a failing run's stderr. Enough to
// carry a resolver's complaint, small enough that a hundred failures cannot
// turn the report into a log file.
const stderrTailBytes = 2048

// installArgsCap is how many arguments command appends after the subcommand
// words: --no-deps, -r, its value, -p, its value.
const installArgsCap = 5

// workDirsPerTarget is how many directories prepareWorkDir creates for each
// target: cache, install tree and temporary tree.
const workDirsPerTarget = 3

// livePeriod is how often the spinner's suffix is repainted with the elapsed
// times. Fast enough that the tenths digit moves, slow enough that a run
// lasting minutes costs a few hundred string formats.
const livePeriod = 200 * time.Millisecond

// liveLine repaints the spinner's suffix with the current run's and the whole
// measurement's elapsed time. It ticks only where a spinner exists, since
// without one every update would be a fresh line in a CI log.
type liveLine struct {
	out     output.Printer
	started time.Time
	enabled bool
}

// track prints prefix, runs work, and while work is in flight keeps the line
// showing this run's elapsed time and the total since the measurement began.
func (l liveLine) track(prefix string, work func() (time.Duration, error)) (time.Duration, error) {
	l.out.Printf("%s", prefix)

	if !l.enabled {
		return work()
	}

	stop, stopped := make(chan struct{}), make(chan struct{})

	go l.repaint(prefix, stop, stopped)

	elapsed, err := work()

	// Closed and awaited rather than deferred: the repainting goroutine must
	// be finished before the caller writes its next line, or a stale frame
	// lands on top of it.
	close(stop)
	<-stopped

	return elapsed, err
}

// repaint is track's ticker, running until stop is closed.
func (l liveLine) repaint(prefix string, stop <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)

	ticker := time.NewTicker(livePeriod)
	defer ticker.Stop()

	runStart := time.Now()

	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			l.out.Printf("%s  %s (total %s)",
				prefix, humanDuration(now.Sub(runStart)), humanDuration(now.Sub(l.started)))
		}
	}
}

// humanDuration renders elapsed time for the live line: tenths below a
// minute, minutes and seconds above it, because a cold 100-collection run
// takes minutes and "372.4s" is harder to read at a glance than "6m12s".
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}

	return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
}

// runner carries what every series needs, so the scenario methods below take
// only what distinguishes one series from another.
type runner struct {
	out  output.Printer
	live liveLine
	opts options
}

// target is one measured tool: its binary, the environment that isolates it
// from the other tool, and the directories a scenario wipes.
type target struct {
	name        string
	bin         string
	install     string
	cache       string
	env         []string
	subcommands []string
}

// ansibleTarget describes ansible-galaxy. ANSIBLE_LOCAL_TEMP moves its
// download and unpack work out of ~/.ansible/tmp into the work directory, so
// both tools are measured on the same filesystem.
func ansibleTarget(opts options) target {
	root := filepath.Join(opts.workDir, "ag")
	install := filepath.Join(root, "collections")
	cache := filepath.Join(root, "cache")
	tmp := filepath.Join(root, "tmp")

	return target{
		name:        "ansible-galaxy",
		bin:         opts.ansibleGalaxy,
		install:     install,
		cache:       cache,
		subcommands: []string{"collection", "install"},
		env: []string{
			"ANSIBLE_GALAXY_CACHE_DIR=" + cache,
			"ANSIBLE_COLLECTIONS_PATH=" + install,
			"ANSIBLE_LOCAL_TEMP=" + tmp,
			"TMPDIR=" + tmp,
		},
	}
}

// goGalaxyTarget describes go-galaxy.
func goGalaxyTarget(opts options) target {
	root := filepath.Join(opts.workDir, "gg")
	install := filepath.Join(root, "collections")
	cache := filepath.Join(root, "cache")
	tmp := filepath.Join(root, "tmp")

	return target{
		name:        "go-galaxy",
		bin:         opts.goGalaxy,
		install:     install,
		cache:       cache,
		subcommands: []string{"install"},
		env: []string{
			"GO_GALAXY_CACHE_DIR=" + cache,
			"TMPDIR=" + tmp,
		},
	}
}

// tempDir names the target's temporary tree, which reset has to create along
// with the rest so a wiped run does not start by failing on a missing path.
func (t target) tempDir() string {
	return filepath.Join(filepath.Dir(t.cache), "tmp")
}

// command builds one measured invocation.
func (t target) command(ctx context.Context, req string, resolveDeps bool) *exec.Cmd {
	args := make([]string, 0, len(t.subcommands)+installArgsCap)
	args = append(args, t.subcommands...)

	if !resolveDeps {
		args = append(args, "--no-deps")
	}

	args = append(args, "-r", req, "-p", t.install)

	// Running the binaries named on the command line is this tool's entire
	// purpose, and both were confirmed executable during argument parsing.
	cmd := exec.CommandContext(ctx, t.bin, args...) //nolint:gosec
	cmd.Env = mergeEnv(os.Environ(), t.env)

	return cmd
}

// once runs the command once and times it, fork and startup included. stdin
// stays nil (/dev/null), so a tool that prompts exits on EOF instead of looking
// hung; copying its output pipes is inside the window but costs microseconds.
func (t target) once(ctx context.Context, req string, resolveDeps bool) (time.Duration, error) {
	cmd := t.command(ctx, req, resolveDeps)
	tail := &tailBuffer{limit: stderrTailBytes}
	cmd.Stdout = io.Discard
	cmd.Stderr = tail

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	if err != nil {
		return elapsed, fmt.Errorf("%s: %w: %s", t.name, err, tail.String())
	}

	return elapsed, nil
}

// mergeEnv returns base with every key named in overrides removed and the
// overrides appended, so a variable set in the caller's shell cannot survive
// alongside the value this harness chose for it.
func mergeEnv(base, overrides []string) []string {
	drop := make(map[string]struct{}, len(overrides))

	for _, kv := range overrides {
		if key, _, ok := strings.Cut(kv, "="); ok {
			drop[key] = struct{}{}
		}
	}

	merged := make([]string, 0, len(base)+len(overrides))

	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, dropped := drop[key]; !dropped {
			merged = append(merged, kv)
		}
	}

	return append(merged, overrides...)
}

// reset wipes and recreates directories. A scenario calls it with what that
// scenario considers cold.
func reset(dirs ...string) error {
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("wiping %s: %w", dir, err)
		}

		if err := os.MkdirAll(dir, dirMode); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	return nil
}

// measure runs every requested scenario over every requested size for both
// tools and returns the report. Sizes are the outer loop so that a run
// interrupted partway still holds complete data for the sizes it finished.
func measure(ctx context.Context, out output.Printer, opts options) (*Report, error) {
	targets := []target{ansibleTarget(opts), goGalaxyTarget(opts)}

	if err := prepareWorkDir(opts, targets); err != nil {
		return nil, err
	}

	run := runner{
		out:  out,
		opts: opts,
		live: liveLine{out: out, started: time.Now(), enabled: opts.spinnerActive()},
	}

	report := &Report{
		Schema:      reportSchema,
		GeneratedAt: time.Now().UTC(),
		Host:        describeHost(ctx, opts.workDir),
		Tools:       describeTools(ctx, targets),
		Runs:        opts.runs,
		ResolveDeps: opts.resolveDeps,
	}

	for _, size := range opts.sizes {
		req := opts.requirementsFile(size)

		for _, scenario := range opts.scenarios {
			for _, tgt := range targets {
				result, err := run.series(ctx, tgt, scenario, size, req)
				if err != nil {
					return nil, err
				}

				report.Results = append(report.Results, result)
				out.Okf("%s %s size %d: %s", tgt.name, scenario, size, summarize(result))
			}
		}
	}

	return report, nil
}

// prepareWorkDir creates everything both targets will write to.
func prepareWorkDir(opts options, targets []target) error {
	dirs := make([]string, 0, 1+workDirsPerTarget*len(targets))
	dirs = append(dirs, opts.workDir)

	for _, tgt := range targets {
		dirs = append(dirs, tgt.cache, tgt.install, tgt.tempDir())
	}

	return reset(dirs...)
}

// series measures one tool, one scenario and one size: cold wipes cache and
// install tree before every run, warm primes the cache once unmeasured and then
// wipes only the install tree. A failed run is counted, not sampled.
func (r runner) series(ctx context.Context, tgt target, scenario string, size int, req string) (Result, error) {
	result := Result{Scenario: scenario, Tool: tgt.name, Size: size}

	if scenario == scenarioWarm {
		if err := r.prime(ctx, tgt, req); err != nil {
			return result, err
		}
	}

	for i := 1; i <= r.opts.runs; i++ {
		if err := resetFor(scenario, tgt); err != nil {
			return result, err
		}

		prefix := fmt.Sprintf("%s %s size %d: run %d/%d", tgt.name, scenario, size, i, r.opts.runs)

		elapsed, err := r.live.track(prefix, func() (time.Duration, error) {
			return tgt.once(ctx, req, r.opts.resolveDeps)
		})
		if ctx.Err() != nil {
			return result, fmt.Errorf("interrupted: %w", ctx.Err())
		}

		if err != nil {
			result.Failed++
			result.LastError = err.Error()

			r.out.Warnf("%s failed after %s", prefix, humanDuration(elapsed))

			continue
		}

		result.SamplesMS = append(result.SamplesMS, elapsed.Milliseconds())
	}

	return result, nil
}

// prime performs the one unmeasured install that makes a warm run warm.
func (r runner) prime(ctx context.Context, tgt target, req string) error {
	if err := reset(tgt.cache, tgt.install, tgt.tempDir()); err != nil {
		return err
	}

	_, err := r.live.track(tgt.name+": priming cache", func() (time.Duration, error) {
		return tgt.once(ctx, req, r.opts.resolveDeps)
	})
	if err != nil {
		// A failed prime is not fatal: the measured runs that follow will
		// report the same failure, and reporting it there keeps every
		// outcome in the one place the report already describes.
		r.out.Warnf("%s: priming run failed, measured runs will not be warm: %v", tgt.name, err)
	}

	return nil
}

// resetFor wipes what the scenario considers cold before a measured run.
func resetFor(scenario string, tgt target) error {
	if scenario == scenarioCold {
		return reset(tgt.cache, tgt.install, tgt.tempDir())
	}

	return reset(tgt.install)
}

// describeHost records what the numbers depend on but the harness does not
// control, the filesystem above all: this workload is mostly inode creation,
// whose cost varies by an order of magnitude between filesystems.
func describeHost(ctx context.Context, workDir string) Host {
	return Host{
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		CPUs:       runtime.GOMAXPROCS(0),
		Filesystem: filesystemOf(ctx, workDir),
	}
}

// describeTools records each binary's path and the first line of its version
// output, so a report can be told from one taken against another build.
func describeTools(ctx context.Context, targets []target) map[string]Tool {
	tools := make(map[string]Tool, len(targets))

	for _, tgt := range targets {
		tools[tgt.name] = Tool{Path: tgt.bin, Version: toolVersion(ctx, tgt.bin)}
	}

	return tools
}

// toolVersion asks a binary for its version, reporting "unknown" rather than
// failing: an unreadable version is a worse report, not a broken measurement.
func toolVersion(ctx context.Context, bin string) string {
	// Same binary that optionsFromCLI already confirmed executable.
	cmd := exec.CommandContext(ctx, bin, "--version")

	output, err := cmd.Output()
	if err != nil {
		return unknownValue
	}

	line, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")

	return line
}

// tailBuffer keeps at most the last limit bytes written to it.
type tailBuffer struct {
	buf   []byte
	limit int
}

// Write implements io.Writer, discarding everything but the tail.
func (t *tailBuffer) Write(payload []byte) (int, error) {
	t.buf = append(t.buf, payload...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}

	return len(payload), nil
}

// String returns the retained tail as a single line.
func (t *tailBuffer) String() string {
	return strings.Join(strings.Fields(string(t.buf)), " ")
}
