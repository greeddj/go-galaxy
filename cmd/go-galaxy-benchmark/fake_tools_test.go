package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeToolScript stands in for a measured tool: it answers --version, logs
// whether its cache was warm and its install tree empty, then fills both.
const fakeToolScript = `#!/bin/sh
if [ "$1" = "--version" ]; then
	printf '\n%[1]s 9.9.9\nbuilt for a test\n'
	exit 0
fi
install='' req='' deps=resolved
while [ $# -gt 0 ]; do
	case "$1" in
	-p) install=$2; shift ;;
	-r) req=$2; shift ;;
	--no-deps) deps=flat ;;
	esac
	shift
done
[ -d "$TMPDIR" ] || { echo 'no temp tree' >&2; exit 4; }
cache=$%[2]s
state=cold; [ -e "$cache/primed" ] && state=warm
tree=empty; [ -e "$install/installed" ] && tree=dirty
echo "%[1]s $state $tree $deps ${req##*/}" >>'%[3]s'
if [ %[4]d -ne 0 ]; then echo '%[1]s: resolver said no' >&2; exit %[4]d; fi
: >"$cache/primed"
: >"$install/installed"
`

// fakeBench is a measurement over fake tools: options ready for measure, and
// the log every fake invocation appends one line to.
type fakeBench struct {
	log  string
	opts options
}

// newFakeBench builds both fake tools, requirements files for sizes 1 and 10,
// and options measuring cold then warm at size 1, twice each.
func newFakeBench(t *testing.T, goGalaxyExit int) fakeBench {
	t.Helper()

	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	reqDir := filepath.Join(root, "requirements")
	log := filepath.Join(root, "invocations.log")

	for _, dir := range []string{bin, reqDir} {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}

	for _, size := range []int{1, 10} {
		path := filepath.Join(reqDir, fmt.Sprintf("requirements-%d.yml", size))
		if err := os.WriteFile(path, []byte("collections: []\n"), fileMode); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	return fakeBench{
		log: log,
		opts: options{
			ansibleGalaxy:   writeFakeTool(t, bin, "ansible-galaxy", "ANSIBLE_GALAXY_CACHE_DIR", log, 0),
			goGalaxy:        writeFakeTool(t, bin, "go-galaxy", "GO_GALAXY_CACHE_DIR", log, goGalaxyExit),
			workDir:         filepath.Join(root, "work"),
			requirementsDir: reqDir,
			scenarios:       []string{scenarioCold, scenarioWarm},
			sizes:           []int{1},
			runs:            2,
			resolveDeps:     true,
		},
	}
}

// writeFakeTool writes fakeToolScript as dir/name, reading its cache from
// cacheVar and exiting with exitCode after logging the invocation.
func writeFakeTool(t *testing.T, dir, name, cacheVar, log string, exitCode int) string {
	t.Helper()

	return writeExecutable(t, dir, name, fmt.Sprintf(fakeToolScript, name, cacheVar, log, exitCode))
}

// writeExecutable writes an executable file and returns its path.
func writeExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // a test fixture must be executable.
		t.Fatalf("writing %s: %v", path, err)
	}

	return path
}

// readLog returns the lines the fake tools logged, or nil when none ran.
func readLog(t *testing.T, path string) []string {
	t.Helper()

	payload, err := os.ReadFile(path) //nolint:gosec // the path is this test's own temporary directory.
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	return strings.Split(strings.TrimSuffix(string(payload), "\n"), "\n")
}

// captureOutput points *stream at a temporary file while fn runs and returns
// what fn wrote there. Callers must not run in parallel: the stream is global.
func captureOutput(t *testing.T, stream **os.File, fn func()) string {
	t.Helper()

	file, err := os.CreateTemp(t.TempDir(), "stream")
	if err != nil {
		t.Fatalf("creating capture file: %v", err)
	}

	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("closing capture file: %v", err)
		}
	}()

	saved := *stream
	*stream = file

	defer func() { *stream = saved }()

	fn()

	payload, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatalf("reading capture file: %v", err)
	}

	return string(payload)
}

// clearBenchEnv unsets every GGB_ variable for the test, so the operator's
// shell cannot answer a flag the test expects at its default.
func clearBenchEnv(t *testing.T) {
	t.Helper()

	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, envPrefix) {
			continue
		}

		t.Setenv(key, "")

		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unsetting %s: %v", key, err)
		}
	}
}
