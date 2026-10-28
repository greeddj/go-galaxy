package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestParseSizes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []int
		ok   bool
	}{
		{name: "plain list", raw: "1,10,100", want: []int{1, 10, 100}, ok: true},
		{name: "spaces and trailing comma", raw: " 1 , 10 ,", want: []int{1, 10}, ok: true},
		{name: "zero refused", raw: "0"},
		{name: "negative refused", raw: "-3"},
		{name: "not a number", raw: "ten"},
		{name: "empty list", raw: " , "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSizes(tc.raw)

			if !tc.ok {
				if !errors.Is(err, errSizeInvalid) {
					t.Fatalf("parseSizes(%q) error = %v, want errSizeInvalid", tc.raw, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseSizes(%q) = %v", tc.raw, err)
			}

			if len(got) != len(tc.want) {
				t.Fatalf("parseSizes(%q) = %v, want %v", tc.raw, got, tc.want)
			}

			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseSizes(%q) = %v, want %v", tc.raw, got, tc.want)
				}
			}
		})
	}
}

func TestParseScenariosRefusesWhatItCannotMeasure(t *testing.T) {
	got, err := parseScenarios("cold, warm")
	if err != nil {
		t.Fatalf("parseScenarios: %v", err)
	}

	if len(got) != 2 || got[0] != scenarioCold || got[1] != scenarioWarm {
		t.Fatalf("parseScenarios = %v, want [cold warm]", got)
	}

	// frozen is a scenario this binary does not implement. Accepting the name
	// and measuring nothing would be the worst of the three outcomes.
	if _, err := parseScenarios("cold,frozen"); !errors.Is(err, errScenarioUnknown) {
		t.Fatalf("parseScenarios(cold,frozen) error = %v, want errScenarioUnknown", err)
	}

	if _, err := parseScenarios(""); !errors.Is(err, errScenarioUnknown) {
		t.Fatalf("parseScenarios(empty) error = %v, want errScenarioUnknown", err)
	}
}

func TestCheckRequirementsNamesTheMissingFile(t *testing.T) {
	dir := t.TempDir()
	opts := options{requirementsDir: dir, sizes: []int{1, 10}}

	if err := os.WriteFile(filepath.Join(dir, "requirements-1.yml"), []byte("collections: []\n"), fileMode); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	err := opts.checkRequirements()
	if !errors.Is(err, errRequirementsMissing) {
		t.Fatalf("checkRequirements error = %v, want errRequirementsMissing", err)
	}

	// The point of validating up front is that the message says which file to
	// create, so a typo costs a second rather than a whole series.
	if want := "requirements-10.yml"; !strings.Contains(err.Error(), want) {
		t.Fatalf("checkRequirements error = %q, want it to name %q", err, want)
	}
}

func TestResolveBinaryRejectsWhatIsNotThere(t *testing.T) {
	_, err := resolveBinary(filepath.Join(t.TempDir(), "no-such-binary"))
	if !errors.Is(err, errBinaryMissing) {
		t.Fatalf("resolveBinary error = %v, want errBinaryMissing", err)
	}
}

// parseRunOptions parses args with the `run` flag set and returns what
// optionsFromCLI made of them.
func parseRunOptions(t *testing.T, args ...string) (options, error) {
	t.Helper()

	var (
		opts     options
		parseErr error
	)

	cmd := &cli.Command{
		Name:  "run",
		Flags: runFlags(),
		Action: func(_ context.Context, cmd *cli.Command) error {
			opts, parseErr = optionsFromCLI(cmd)

			return nil
		},
	}

	if err := cmd.Run(t.Context(), append([]string{"run"}, args...)); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}

	return opts, parseErr
}

// benchFixture is a directory holding fake binaries and requirements files,
// so optionsFromCLI's existence checks pass.
type benchFixture struct {
	root, bin, ansible, goGalaxy, requirements string
}

func newBenchFixture(t *testing.T, sizes ...int) benchFixture {
	t.Helper()

	root := t.TempDir()
	fx := benchFixture{root: root, bin: filepath.Join(root, "bin"), requirements: filepath.Join(root, "testing")}

	for _, dir := range []string{fx.bin, fx.requirements} {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}

	fx.ansible = writeExecutable(t, fx.bin, "ansible-galaxy", "#!/bin/sh\n")
	fx.goGalaxy = writeExecutable(t, fx.bin, "go-galaxy", "#!/bin/sh\n")

	for _, size := range sizes {
		path := filepath.Join(fx.requirements, fmt.Sprintf("requirements-%d.yml", size))
		if err := os.WriteFile(path, []byte("collections: []\n"), fileMode); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	return fx
}

// TestOptionsFromCLIDefaults runs from the fixture with only --work-dir set:
// binaries come from PATH, requirements from ./testing, and every relative
// path is made absolute.
func TestOptionsFromCLIDefaults(t *testing.T) {
	clearBenchEnv(t)

	fx := newBenchFixture(t, 1, 10, 100)
	t.Chdir(fx.root)
	t.Setenv("PATH", fx.bin)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	got, err := parseRunOptions(t, "--work-dir", "work")
	if err != nil {
		t.Fatalf("optionsFromCLI: %v", err)
	}

	want := options{
		ansibleGalaxy:   fx.ansible,
		goGalaxy:        fx.goGalaxy,
		workDir:         filepath.Join(cwd, "work"),
		requirementsDir: filepath.Join(cwd, "testing"),
		reportPath:      filepath.Join(cwd, "work", reportName),
		scenarios:       []string{scenarioCold, scenarioWarm},
		sizes:           []int{1, 10, 100},
		runs:            runsDefault,
		resolveDeps:     true,
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("optionsFromCLI =\n%+v\nwant\n%+v", got, want)
	}
}

// TestOptionsFromCLIReadsFlagsAndEnvironment pins that every run flag is read,
// and that its GGB_ variable means the same as the flag.
func TestOptionsFromCLIReadsFlagsAndEnvironment(t *testing.T) {
	fx := newBenchFixture(t, 1, 10)
	work := filepath.Join(fx.root, "work")
	report := filepath.Join(fx.root, "out", "bench.json")

	want := options{
		ansibleGalaxy:   fx.ansible,
		goGalaxy:        fx.goGalaxy,
		workDir:         work,
		requirementsDir: fx.requirements,
		reportPath:      report,
		scenarios:       []string{scenarioWarm},
		sizes:           []int{10, 1},
		runs:            3,
		verbose:         true,
		quiet:           true,
	}

	cases := []struct {
		env  map[string]string
		name string
		args []string
	}{
		{
			name: "flags",
			args: []string{
				"--ansible-galaxy", fx.ansible, "--go-galaxy", fx.goGalaxy, "--work-dir", work,
				"--requirements-dir", fx.requirements, "--runs", "3", "--sizes", "10, 1", "--scenarios", "warm",
				"--no-deps", "--report", report, "--verbose", "-q",
			},
		},
		{
			name: "environment",
			env: map[string]string{
				"GGB_ANSIBLE_GALAXY": fx.ansible, "GGB_GO_GALAXY": fx.goGalaxy, "GGB_WORK_DIR": work,
				"GGB_REQUIREMENTS_DIR": fx.requirements, "GGB_RUNS": "3", "GGB_SIZES": "10, 1",
				"GGB_SCENARIOS": "warm", "GGB_NO_DEPS": "true", "GGB_REPORT": report,
				"GGB_VERBOSE": "true", "GGB_QUIET": "true",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearBenchEnv(t)

			for key, value := range tc.env {
				t.Setenv(key, value)
			}

			got, err := parseRunOptions(t, tc.args...)
			if err != nil {
				t.Fatalf("optionsFromCLI: %v", err)
			}

			if !reflect.DeepEqual(got, want) {
				t.Fatalf("optionsFromCLI =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

// TestOptionsFromCLIRefusesBeforeMeasuring walks every refusal and checks the
// message names the argument at fault, which is the point of validating early.
func TestOptionsFromCLIRefusesBeforeMeasuring(t *testing.T) {
	clearBenchEnv(t)

	fx := newBenchFixture(t, 1)
	missing := filepath.Join(fx.bin, "missing-galaxy")
	base := []string{
		"--ansible-galaxy", fx.ansible, "--go-galaxy", fx.goGalaxy,
		"--work-dir", filepath.Join(fx.root, "work"), "--requirements-dir", fx.requirements, "--sizes", "1",
	}

	cases := []struct {
		want    error
		name    string
		mention string
		args    []string
	}{
		{name: "no work dir", args: []string{"--sizes", "1"}, want: errWorkDirRequired, mention: "--work-dir"},
		{name: "ansible-galaxy missing", args: append(slices.Clone(base), "--ansible-galaxy", missing), want: errBinaryMissing, mention: missing},
		{name: "go-galaxy missing", args: append(slices.Clone(base), "--go-galaxy", missing), want: errBinaryMissing, mention: missing},
		{name: "size not a number", args: append(slices.Clone(base), "--sizes", "1,ten"), want: errSizeInvalid, mention: `"ten"`},
		{name: "scenario unknown", args: append(slices.Clone(base), "--scenarios", "frozen"), want: errScenarioUnknown, mention: `"frozen"`},
		{
			name: "requirements file missing", args: append(slices.Clone(base), "--sizes", "1,5"),
			want: errRequirementsMissing, mention: "requirements-5.yml",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRunOptions(t, tc.args...)
			if !errors.Is(err, tc.want) {
				t.Fatalf("optionsFromCLI error = %v, want %v", err, tc.want)
			}

			if !strings.Contains(err.Error(), tc.mention) {
				t.Fatalf("optionsFromCLI error = %q, want it to name %q", err, tc.mention)
			}
		})
	}
}

func TestResolveBinaryReturnsAnAbsolutePath(t *testing.T) {
	fx := newBenchFixture(t)
	t.Chdir(fx.root)
	t.Setenv("PATH", fx.bin)

	cases := []struct {
		name string
		arg  string
		want string
	}{
		{name: "bare name looked up in PATH", arg: "go-galaxy", want: fx.goGalaxy},
		{name: "relative path", arg: filepath.Join("bin", "ansible-galaxy"), want: fx.ansible},
		{name: "absolute path", arg: fx.ansible, want: fx.ansible},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveBinary(tc.arg)
			if err != nil {
				t.Fatalf("resolveBinary(%s): %v", tc.arg, err)
			}

			if !filepath.IsAbs(got) || !sameFile(t, got, tc.want) {
				t.Fatalf("resolveBinary(%s) = %q, want an absolute path to %s", tc.arg, got, tc.want)
			}
		})
	}
}

func TestResolveBinaryRejectsWhatCannotRun(t *testing.T) {
	fx := newBenchFixture(t)

	plain := filepath.Join(fx.root, "plain")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\n"), fileMode); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	for _, path := range []string{plain, fx.bin} {
		if _, err := resolveBinary(path); !errors.Is(err, errBinaryMissing) || !strings.Contains(err.Error(), path) {
			t.Fatalf("resolveBinary(%s) error = %v, want errBinaryMissing naming the path", path, err)
		}
	}
}

// sameFile reports whether two paths name one file, so a symlinked temporary
// directory cannot fail a comparison of equal paths.
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()

	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)

	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}
