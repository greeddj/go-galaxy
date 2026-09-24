package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/projectfile"
	"github.com/urfave/cli/v3"
)

// psProjectPath is the Path a fixture projectSettings carries; the warning
// applyWorkers queues names it, so it is asserted literally below.
const psProjectPath = "galaxy.toml"

// The plaintexts a fixture [tool.go-galaxy] table supplies. Each must be
// readable through Reveal and must never surface in a rendering of Config.
const (
	psTOMLSecretKey    = "s3cr3t-toml-secret"  //nolint:gosec // a fixture plaintext, never a credential
	psTOMLSessionToken = "s3cr3t-toml-session" //nolint:gosec // a fixture plaintext, never a credential
	psTOMLServerToken  = "s3cr3t-toml-token"
)

// psS3BucketWithoutKeysText is the text helpers.ErrS3EmptyCreds carries now
// that a bucket may arrive from galaxy.toml as well as from the flags.
const psS3BucketWithoutKeysText = "s3 cache requires access and secret keys when an S3 bucket is configured"

// psWorkers is a projectSettings whose [tool.go-galaxy] carries workers = n.
func psWorkers(n int) projectSettings {
	return projectSettings{Path: psProjectPath, Workers: n, HasWorkers: true}
}

// psWorkersRow is one shape a workers value reaches applyWorkers in when a
// galaxy.toml is present: the flag's arguments, the file's table, whether the
// command mounts no --workers, and the count, warning and credit that result.
type psWorkersRow struct {
	name        string
	wantWarning string
	args        []string
	wantUsed    []string
	project     projectSettings
	wantWorkers int
	unmounted   bool
}

// psWorkersRows enumerates TestApplyWorkersProjectSettings' rows, every want
// hand-spelled at procs 8, where the accepted range is 1..8 and the default
// is 8. It is a function of its own to keep the test inside funlen's budget.
func psWorkersRows() []psWorkersRow {
	return []psWorkersRow{
		{
			name:    "an in-range value from galaxy.toml is taken",
			project: psWorkers(3), wantWorkers: 3, wantUsed: []string{"workers"},
		},
		{
			name:    "a value above the ceiling warns by file and takes the default",
			project: psWorkers(9), wantWorkers: 8,
			wantWarning: "[tool.go-galaxy] workers in galaxy.toml = 9 is outside 1..8, the range this machine accepts " +
				"(the ceiling is the CPU this process is permitted to use, at least 2); using 8 instead",
		},
		{
			name:    "zero from galaxy.toml lands on the same outcome",
			project: psWorkers(0), wantWorkers: 8,
			wantWarning: "[tool.go-galaxy] workers in galaxy.toml = 0 is outside 1..8, the range this machine accepts " +
				"(the ceiling is the CPU this process is permitted to use, at least 2); using 8 instead",
		},
		{
			name: "a set flag wins over galaxy.toml",
			args: []string{"--workers=3"}, project: psWorkers(5), wantWorkers: 3,
		},
		{
			name: "a refused flag value keeps the flag's own warning and ignores galaxy.toml",
			args: []string{"--workers=0"}, project: psWorkers(5), wantWorkers: 8,
			wantWarning: "--workers (or $GO_GALAXY_WORKERS) = 0 is outside 1..8, the range this machine accepts " +
				"(the ceiling is the CPU this process is permitted to use, at least 2); using 8 instead",
		},
		{
			name:    "no workers key in galaxy.toml takes the default silently",
			project: projectSettings{Path: psProjectPath}, wantWorkers: 8,
		},
		{
			name:    "an in-range value is not read where the flag is not mounted",
			project: psWorkers(3), unmounted: true, wantWorkers: 8,
		},
		{
			name:    "zero is not judged where the flag is not mounted",
			project: psWorkers(0), unmounted: true, wantWorkers: 8,
		},
	}
}

// psAssertWorkers checks the settled count, ProjectSettingsUsed, and the
// queued warnings: exactly the one row.wantWarning spells, or none at all.
func psAssertWorkers(t *testing.T, cfg *Config, row psWorkersRow) {
	t.Helper()
	if cfg.Workers != row.wantWorkers {
		t.Errorf("Workers = %d, want %d", cfg.Workers, row.wantWorkers)
	}
	if !slices.Equal(cfg.ProjectSettingsUsed, row.wantUsed) {
		t.Errorf("ProjectSettingsUsed = %q, want %q", cfg.ProjectSettingsUsed, row.wantUsed)
	}
	if row.wantWarning == "" {
		if len(cfg.Warnings) != 0 {
			t.Errorf("Warnings = %q, want none", cfg.Warnings)
		}
		return
	}
	if len(cfg.Warnings) != 1 || cfg.Warnings[0] != row.wantWarning {
		t.Errorf("Warnings = %q, want exactly [%q]", cfg.Warnings, row.wantWarning)
	}
}

// TestApplyWorkersProjectSettings pins galaxy.toml's place under --workers:
// taken and credited when the flag is mounted and unset, range-checked with a
// warning naming the file, and outranked by any set flag. Not parallel: t.Setenv.
func TestApplyWorkersProjectSettings(t *testing.T) {
	// The fixture flag's Value stands in for the production flag's at procs 8;
	// only the exported-empty variable subtest reads it.
	fixtureValue := helpers.DefaultInstallWorkers(8)

	psUnsetEnv(t, "GO_GALAXY_WORKERS")
	for _, tt := range psWorkersRows() {
		t.Run(tt.name, func(t *testing.T) {
			c := newIntFlagCmd(t, "workers", "GO_GALAXY_WORKERS", fixtureValue, !tt.unmounted, tt.args)
			cfg := &Config{}
			applyWorkers(cfg, c, tt.project, 8)
			psAssertWorkers(t, cfg, tt)
		})
	}

	// urfave marks a declared-but-empty variable set, so the flag's own Value
	// outranks galaxy.toml exactly as a written value would.
	t.Run("an exported but empty GO_GALAXY_WORKERS outranks galaxy.toml", func(t *testing.T) {
		t.Setenv("GO_GALAXY_WORKERS", "")
		c := newIntFlagCmd(t, "workers", "GO_GALAXY_WORKERS", fixtureValue, true, nil)
		cfg := &Config{}
		applyWorkers(cfg, c, psWorkers(3), 8)
		psAssertWorkers(t, cfg, psWorkersRow{wantWorkers: 8})
	})
}

// psDownloadWorkersDefault stands in for the pool size newConfigFromCLI
// derives before applyDownloadWorkers runs; it only needs to differ from
// every value the rows supply.
const psDownloadWorkersDefault = 16

// TestApplyDownloadWorkersProjectSettings pins download_workers: it applies
// only where the flag is mounted, it and its variable unset and the value
// positive; anything else passes silently. Not parallel: it clears the variable.
func TestApplyDownloadWorkersProjectSettings(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantUsed  []string
		project   projectSettings
		want      int
		unmounted bool
	}{
		{
			name:    "a positive value applies when the flag is unset",
			project: projectSettings{DownloadWorkers: 4, HasDownloadWorkers: true}, want: 4, wantUsed: []string{"download_workers"},
		},
		{
			name:    "zero is ignored silently",
			project: projectSettings{DownloadWorkers: 0, HasDownloadWorkers: true}, want: psDownloadWorkersDefault,
		},
		{
			name:    "a negative value is ignored silently",
			project: projectSettings{DownloadWorkers: -1, HasDownloadWorkers: true}, want: psDownloadWorkersDefault,
		},
		{name: "an absent key keeps the default", want: psDownloadWorkersDefault},
		{
			name: "a set flag wins over galaxy.toml",
			args: []string{"--download-workers=3"}, project: projectSettings{DownloadWorkers: 4, HasDownloadWorkers: true}, want: 3,
		},
		{
			name:    "a positive value is not read where the flag is not mounted",
			project: projectSettings{DownloadWorkers: 4, HasDownloadWorkers: true}, unmounted: true, want: psDownloadWorkersDefault,
		},
	}
	psUnsetEnv(t, "GO_GALAXY_DOWNLOAD_WORKERS")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newIntFlagCmd(t, "download-workers", "GO_GALAXY_DOWNLOAD_WORKERS", psDownloadWorkersDefault, !tt.unmounted, tt.args)
			// newConfigFromCLI's own step: an unmounted flag reads 0, which
			// becomes the default before applyDownloadWorkers runs.
			cfg := &Config{DownloadWorkers: c.Int("download-workers")}
			if cfg.DownloadWorkers < 1 {
				cfg.DownloadWorkers = psDownloadWorkersDefault
			}
			applyDownloadWorkers(cfg, c, tt.project)
			if cfg.DownloadWorkers != tt.want {
				t.Errorf("DownloadWorkers = %d, want %d", cfg.DownloadWorkers, tt.want)
			}
			if !slices.Equal(cfg.ProjectSettingsUsed, tt.wantUsed) {
				t.Errorf("ProjectSettingsUsed = %q, want %q", cfg.ProjectSettingsUsed, tt.wantUsed)
			}
			if len(cfg.Warnings) != 0 {
				t.Errorf("Warnings = %q, want none", cfg.Warnings)
			}
		})
	}
}

// psPathsCmd registers the flags applyAnsibleConfig and applyProjectSettings
// read between them; envSourced adds the GO_GALAXY_* sources of --lock-file
// and --metrics-file, which only the exported-empty variable test needs.
func psPathsCmd(t *testing.T, envSourced bool, args []string) *cli.Command {
	t.Helper()

	lockFile := &cli.StringFlag{Name: "lock-file"}
	metricsFile := &cli.StringFlag{Name: "metrics-file"}
	if envSourced {
		lockFile.Sources = cli.EnvVars("GO_GALAXY_LOCK_FILE")
		metricsFile.Sources = cli.EnvVars("GO_GALAXY_METRICS_FILE")
	}
	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "download-path", Value: testDefaultDownloadPath},
			&cli.StringFlag{Name: "roles-path", Value: testDefaultRolesPath},
			&cli.StringFlag{Name: "cache-dir", Value: testDefaultCacheDir},
			&cli.StringFlag{Name: "server", Value: testDefaultServer},
			lockFile,
			metricsFile,
		},
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}
	if err := cmd.Run(t.Context(), append([]string{"go-galaxy"}, args...)); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// psRunProjectPaths runs applyAnsibleConfig and then applyProjectSettings over
// one command, in BuildCollectionConfig's order, and returns the Config.
func psRunProjectPaths(t *testing.T, envSourced bool, args []string, ansCfg ansibleConfig, project projectSettings) *Config {
	t.Helper()
	c := psPathsCmd(t, envSourced, args)
	cfg := &Config{}
	applyAnsibleConfig(cfg, c, ansCfg, testAnsibleConfigPath)
	applyProjectSettings(cfg, c, project)
	return cfg
}

// psPathsWant is the shape of Config after applyProjectSettings has laid
// galaxy.toml's three paths over the flags and ansible.cfg.
type psPathsWant struct {
	cacheDir     string
	lockFile     string
	metricsFile  string
	used         []string
	cacheDirUsed bool
}

// psPathsRow is one precedence shape for the three paths: the flags, the
// ansible.cfg, the galaxy.toml table, and what must come out.
type psPathsRow struct {
	name    string
	args    []string
	want    psPathsWant
	ansCfg  ansibleConfig
	project projectSettings
}

// psPathsRows enumerates TestApplyProjectSettingsPaths' rows, a function of
// its own to keep the test inside funlen's budget.
func psPathsRows() []psPathsRow {
	ansibleCacheDir := ansibleConfig{Galaxy: ansibleGalaxyConfig{CacheDir: "/ansible/cache"}}
	tomlPaths := projectSettings{LockFile: "/project/galaxy.lock", MetricsFile: "/project/metrics.json"}
	return []psPathsRow{
		{
			name:   "cache_dir from galaxy.toml outranks [galaxy] cache_dir",
			ansCfg: ansibleCacheDir, project: projectSettings{CacheDir: "/project/cache"},
			want: psPathsWant{cacheDir: "/project/cache", used: []string{"cache_dir"}},
		},
		{
			name:   "--cache-dir outranks galaxy.toml",
			args:   []string{"--cache-dir=/explicit/cache"},
			ansCfg: ansibleCacheDir, project: projectSettings{CacheDir: "/project/cache"},
			want: psPathsWant{cacheDir: "/explicit/cache"},
		},
		{
			name:   "[galaxy] cache_dir stays credited when galaxy.toml has no cache_dir",
			ansCfg: ansibleCacheDir,
			want:   psPathsWant{cacheDir: "/ansible/cache", cacheDirUsed: true},
		},
		{
			name:    "lock_file and metrics_file from galaxy.toml apply when the flags are unset",
			project: tomlPaths,
			want: psPathsWant{
				cacheDir: testDefaultCacheDir, lockFile: "/project/galaxy.lock", metricsFile: "/project/metrics.json",
				used: []string{"lock_file", "metrics_file"},
			},
		},
		{
			name:    "--lock-file and --metrics-file outrank galaxy.toml",
			args:    []string{"--lock-file=/flag/galaxy.lock", "--metrics-file=/flag/metrics.json"},
			project: tomlPaths,
			want:    psPathsWant{cacheDir: testDefaultCacheDir, lockFile: "/flag/galaxy.lock", metricsFile: "/flag/metrics.json"},
		},
		{
			name:    "every key taken is listed in apply order",
			ansCfg:  ansibleCacheDir,
			project: projectSettings{CacheDir: "/project/cache", LockFile: "/project/galaxy.lock", MetricsFile: "/project/metrics.json"},
			want: psPathsWant{
				cacheDir: "/project/cache", lockFile: "/project/galaxy.lock", metricsFile: "/project/metrics.json",
				used: []string{"cache_dir", "lock_file", "metrics_file"},
			},
		},
		{
			name: "an empty table lists nothing and keeps the flag defaults",
			want: psPathsWant{cacheDir: testDefaultCacheDir},
		},
	}
}

// psAssertPaths checks got against want field by field, so a mismatch on one
// path is reported without masking the others.
func psAssertPaths(t *testing.T, got *Config, want psPathsWant) {
	t.Helper()
	if got.CacheDir != want.cacheDir {
		t.Errorf("CacheDir = %q, want %q", got.CacheDir, want.cacheDir)
	}
	if got.AnsibleCacheDirUsed != want.cacheDirUsed {
		t.Errorf("AnsibleCacheDirUsed = %v, want %v", got.AnsibleCacheDirUsed, want.cacheDirUsed)
	}
	if got.LockFile != want.lockFile {
		t.Errorf("LockFile = %q, want %q", got.LockFile, want.lockFile)
	}
	if got.MetricsFile != want.metricsFile {
		t.Errorf("MetricsFile = %q, want %q", got.MetricsFile, want.metricsFile)
	}
	if !slices.Equal(got.ProjectSettingsUsed, want.used) {
		t.Errorf("ProjectSettingsUsed = %q, want %q", got.ProjectSettingsUsed, want.used)
	}
}

// TestApplyProjectSettingsPaths pins cache_dir, lock_file and metrics_file
// after applyAnsibleConfig: a flag beats galaxy.toml, galaxy.toml beats
// [galaxy] cache_dir and withdraws its credit, and only keys taken are listed.
func TestApplyProjectSettingsPaths(t *testing.T) {
	t.Parallel()

	for _, tt := range psPathsRows() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := psRunProjectPaths(t, false, tt.args, tt.ansCfg, tt.project)
			psAssertPaths(t, got, tt.want)
		})
	}
}

// TestApplyProjectSettingsEmptyVariableOutranksProject pins that an exported
// but empty GO_GALAXY_LOCK_FILE counts as set: its empty value wins over
// galaxy.toml's lock_file, as a written value would. Not parallel: t.Setenv.
func TestApplyProjectSettingsEmptyVariableOutranksProject(t *testing.T) {
	t.Setenv("GO_GALAXY_LOCK_FILE", "")
	t.Setenv("GO_GALAXY_METRICS_FILE", "/env/metrics.json")
	project := projectSettings{LockFile: "/project/galaxy.lock", MetricsFile: "/project/metrics.json"}

	got := psRunProjectPaths(t, true, nil, ansibleConfig{}, project)

	psAssertPaths(t, got, psPathsWant{cacheDir: testDefaultCacheDir, metricsFile: "/env/metrics.json"})
}

// psUnsetEnv removes each name from this test's environment, so a row reads
// no ambient value; t.Setenv runs first only for the restore it registers,
// and an exported-empty value would still count as set.
func psUnsetEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("os.Unsetenv(%q) error = %v, want nil", name, err)
		}
	}
}

// psWriteProjectFile writes content as the galaxy.toml of a fresh temporary
// directory and returns the file's path.
func psWriteProjectFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "galaxy.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v, want nil", path, err)
	}
	return path
}

// psSettingsFixture is a galaxy.toml carrying every [tool.go-galaxy] key,
// relative and absolute paths among them, and no ${VAR} reference.
const psSettingsFixture = `[project]
collections = ["acme.app"]

[tool.go-galaxy]
lock_file = "locks/galaxy.lock"
cache_dir = "/var/cache/go-galaxy"
metrics_file = "metrics.json"
workers = 4
download_workers = 12

[tool.go-galaxy.s3]
bucket = "artifacts"
region = "eu-west-1"
prefix = "ci/"
endpoint = "https://s3.example"
access_key = "AKIAEXAMPLE"
secret_key = "s3cr3t-toml-secret"
session_token = "s3cr3t-toml-session"
path_style_disabled = true

[[tool.go-galaxy.servers]]
id = "hub"
url = "https://hub.example"
token = "s3cr3t-toml-token"
validate_certs = false
`

// psMinimalProjectFile wraps settings, the [tool.go-galaxy] body, in the
// smallest galaxy.toml that decodes.
func psMinimalProjectFile(settings string) string {
	return "[project]\ncollections = []\n\n[tool.go-galaxy]\n" + settings
}

// psCheckYAMLPathReadsNothing pins that a requirements.yml path yields the
// zero value even when no such file exists, since only a .toml path is read.
func psCheckYAMLPathReadsNothing(t *testing.T) {
	for _, name := range []string{"requirements.yml", "requirements.yaml"} {
		path := filepath.Join(t.TempDir(), "absent", name)
		got, err := loadProjectSettings(path)
		if err != nil {
			t.Fatalf("loadProjectSettings(%q) error = %v, want nil", path, err)
		}
		if !reflect.DeepEqual(got, projectSettings{}) {
			t.Fatalf("loadProjectSettings(%q) = %+v, want the zero value", path, got)
		}
	}
}

// psCheckAbsentTOMLContributesNothing pins that a galaxy.toml path with no
// file behind it is the zero value and no error, so discovery's fallback holds.
func psCheckAbsentTOMLContributesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "galaxy.toml")
	got, err := loadProjectSettings(path)
	if err != nil {
		t.Fatalf("loadProjectSettings(%q) error = %v, want nil", path, err)
	}
	if !reflect.DeepEqual(got, projectSettings{}) {
		t.Fatalf("loadProjectSettings(%q) = %+v, want the zero value", path, got)
	}
}

// psCheckTOMLTableIsReturned pins that the whole table comes back with Path
// set and each relative path already joined under the file's directory.
func psCheckTOMLTableIsReturned(t *testing.T) {
	path := psWriteProjectFile(t, psSettingsFixture)
	dir := filepath.Dir(path)

	got, err := loadProjectSettings(path)
	if err != nil {
		t.Fatalf("loadProjectSettings(%q) error = %v, want nil", path, err)
	}

	validateCerts := false
	want := projectSettings{
		Path:        path,
		LockFile:    filepath.Join(dir, "locks", "galaxy.lock"),
		CacheDir:    "/var/cache/go-galaxy",
		MetricsFile: filepath.Join(dir, "metrics.json"),
		Servers: []projectfile.ServerSetting{
			{ID: "hub", URL: "https://hub.example", Token: psTOMLServerToken, ValidateCerts: &validateCerts},
		},
		S3: projectfile.S3Settings{
			Bucket: "artifacts", Region: "eu-west-1", Prefix: "ci/", Endpoint: "https://s3.example",
			AccessKey: "AKIAEXAMPLE", SecretKey: psTOMLSecretKey, SessionToken: psTOMLSessionToken, PathStyleDisabled: true,
		},
		Workers: 4, DownloadWorkers: 12, HasWorkers: true, HasDownloadWorkers: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadProjectSettings(%q) = %+v, want %+v", path, got, want)
	}
}

// psCheckMissingToolTableIsEmpty pins that a galaxy.toml without [tool] is
// empty settings carrying Path alone, Servers nil: expansion mutates the
// decoded slice in place and never builds one where the file named none.
func psCheckMissingToolTableIsEmpty(t *testing.T) {
	path := psWriteProjectFile(t, "[project]\ncollections = []\n")

	got, err := loadProjectSettings(path)
	if err != nil {
		t.Fatalf("loadProjectSettings(%q) error = %v, want nil", path, err)
	}
	if want := (projectSettings{Path: path}); !reflect.DeepEqual(got, want) {
		t.Fatalf("loadProjectSettings(%q) = %+v, want %+v", path, got, want)
	}
}

// psCheckVariableIsExpanded pins that a ${VAR} under [tool.go-galaxy] is
// replaced from the environment before the path is resolved, so an absolute
// value from a variable is kept as written.
func psCheckVariableIsExpanded(t *testing.T) {
	t.Setenv("PS_SET_VAR", "/from/env")
	path := psWriteProjectFile(t, psMinimalProjectFile("cache_dir = \"${PS_SET_VAR}/cache\"\n"))

	got, err := loadProjectSettings(path)
	if err != nil {
		t.Fatalf("loadProjectSettings(%q) error = %v, want nil", path, err)
	}
	if got.CacheDir != "/from/env/cache" {
		t.Fatalf("CacheDir = %q, want %q", got.CacheDir, "/from/env/cache")
	}
}

// psCheckUnsetVariableIsRefused pins that every unset ${VAR} in the table is
// reported once, by name and sorted, behind the file's path.
func psCheckUnsetVariableIsRefused(t *testing.T) {
	psUnsetEnv(t, "PS_UNSET_OTHER", "PS_UNSET_VAR")
	path := psWriteProjectFile(t, psMinimalProjectFile(
		"lock_file = \"${PS_UNSET_VAR}/galaxy.lock\"\ncache_dir = \"${PS_UNSET_OTHER}/cache\"\n"))

	_, err := loadProjectSettings(path)
	if !errors.Is(err, helpers.ErrProjectFileEnvUnset) {
		t.Fatalf("loadProjectSettings(%q) error = %v, want errors.Is helpers.ErrProjectFileEnvUnset", path, err)
	}
	want := path + ": project file references unset environment variables: PS_UNSET_OTHER, PS_UNSET_VAR"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

// psCheckBrokenTOMLNamesThePath pins that bytes which are not TOML are
// helpers.ErrInvalidRequirementsTOML with the path leading the message.
func psCheckBrokenTOMLNamesThePath(t *testing.T) {
	path := psWriteProjectFile(t, "[project\n")

	_, err := loadProjectSettings(path)
	if !errors.Is(err, helpers.ErrInvalidRequirementsTOML) {
		t.Fatalf("loadProjectSettings(%q) error = %v, want errors.Is helpers.ErrInvalidRequirementsTOML", path, err)
	}
	if !strings.HasPrefix(err.Error(), path+": ") {
		t.Fatalf("error = %q, want it to lead with %q", err, path)
	}
}

// psCheckSchemaViolationNamesTheKey pins that a schema violation is refused
// by table and key, never by value, behind the file's path.
func psCheckSchemaViolationNamesTheKey(t *testing.T) {
	path := psWriteProjectFile(t, psMinimalProjectFile("workers = \"4\"\n"))

	_, err := loadProjectSettings(path)
	if !errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) {
		t.Fatalf("loadProjectSettings(%q) error = %v, want errors.Is helpers.ErrUnsupportedRequirementsFormat", path, err)
	}
	want := path + ": " + helpers.ErrUnsupportedRequirementsFormat.Error() + ": [tool.go-galaxy] workers is not an integer"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

// psCheckDirectoryIsUnreadable pins that a galaxy.toml that exists but cannot
// be read as a file is helpers.ErrRequirementsUnreadable, not an absent file.
func psCheckDirectoryIsUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "galaxy.toml")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("os.Mkdir(%q) error = %v, want nil", path, err)
	}

	_, err := loadProjectSettings(path)
	if !errors.Is(err, helpers.ErrRequirementsUnreadable) {
		t.Fatalf("loadProjectSettings(%q) error = %v, want errors.Is helpers.ErrRequirementsUnreadable", path, err)
	}
	if !strings.HasPrefix(err.Error(), path+": ") {
		t.Fatalf("error = %q, want it to lead with %q", err, path)
	}
}

// TestLoadProjectSettings pins the wrapper over projectfile.LoadSettings: only
// a .toml path is read, and every failure leads with that path. Not parallel:
// the expansion subtests read the environment and one of them sets it.
func TestLoadProjectSettings(t *testing.T) {
	t.Run("a .yml path is the zero value and reads nothing", psCheckYAMLPathReadsNothing)
	t.Run("an absent galaxy.toml contributes nothing", psCheckAbsentTOMLContributesNothing)
	t.Run("a [tool.go-galaxy] table is returned with Path set", psCheckTOMLTableIsReturned)
	t.Run("a file without [tool] is empty settings with Path set", psCheckMissingToolTableIsEmpty)
	t.Run("a ${VAR} is expanded from the environment", psCheckVariableIsExpanded)
	t.Run("an unset ${VAR} is refused by name", psCheckUnsetVariableIsRefused)
	t.Run("broken TOML leads with the path", psCheckBrokenTOMLNamesThePath)
	t.Run("a schema violation names the key behind the path", psCheckSchemaViolationNamesTheKey)
	t.Run("a directory is unreadable", psCheckDirectoryIsUnreadable)
}

// psS3Settings is a projectSettings whose [tool.go-galaxy.s3] carries every
// string key, the two fixture plaintexts among them.
func psS3Settings() projectSettings {
	return projectSettings{Path: psProjectPath, S3: projectfile.S3Settings{
		Bucket: "toml-bucket", Region: "eu-west-1", Prefix: "ci/", Endpoint: "https://s3.example",
		AccessKey: "toml-access-key", SecretKey: psTOMLSecretKey, SessionToken: psTOMLSessionToken,
	}}
}

// psS3VirtualHost is psS3Settings with path_style_disabled = true.
func psS3VirtualHost() projectSettings {
	project := psS3Settings()
	project.S3.PathStyleDisabled = true
	return project
}

// psS3StringKeys lists the seven string keys in the order loadS3CacheConfig
// reads them, which is the order ProjectSettingsUsed must report them in.
func psS3StringKeys() []string {
	return []string{"s3.bucket", "s3.prefix", "s3.endpoint", "s3.region", "s3.access_key", "s3.secret_key", "s3.session_token"}
}

// psS3Want is the shape of S3CacheConfig after loadS3CacheConfig ran over a
// [tool.go-galaxy.s3] table: which source each credential came from, whether
// the cache is on, the addressing style, and the keys credited to the file.
type psS3Want struct {
	bucket       string
	accessKey    string
	secretKey    string
	sessionToken string
	used         []string
	enabled      bool
	pathStyle    bool
}

// psS3Row is one precedence shape for the S3 cache: the flags, the
// galaxy.toml table, and either the error or the config that must come out.
type psS3Row struct {
	wantErr error
	name    string
	args    []string
	want    psS3Want
	project projectSettings
}

// psS3Rows enumerates TestLoadS3CacheConfigProjectSettings' rows, a function
// of its own to keep the test inside funlen's budget.
func psS3Rows() []psS3Row {
	everyKey := psS3Want{
		bucket: "toml-bucket", accessKey: "toml-access-key", secretKey: psTOMLSecretKey, sessionToken: psTOMLSessionToken,
		used: psS3StringKeys(), enabled: true, pathStyle: true,
	}
	return []psS3Row{
		{
			name:    "a bucket from galaxy.toml alone is refused",
			project: projectSettings{S3: projectfile.S3Settings{Bucket: "toml-bucket"}},
			wantErr: helpers.ErrS3EmptyCreds,
		},
		{
			name:    "a bucket and access key from galaxy.toml without a secret key is refused",
			project: projectSettings{S3: projectfile.S3Settings{Bucket: "toml-bucket", AccessKey: "toml-access-key"}},
			wantErr: helpers.ErrS3EmptyCreds,
		},
		{
			name:    "a bucket from galaxy.toml takes its keys from the flags",
			args:    []string{"--s3-access-key=flag-access-key", "--s3-secret-key=flag-secret-key"},
			project: projectSettings{S3: projectfile.S3Settings{Bucket: "toml-bucket"}},
			want: psS3Want{
				bucket: "toml-bucket", accessKey: "flag-access-key", secretKey: "flag-secret-key",
				used: []string{"s3.bucket"}, enabled: true, pathStyle: true,
			},
		},
		{name: "every key from galaxy.toml", project: psS3Settings(), want: everyKey},
		{
			name:    "--s3-bucket outranks galaxy.toml's bucket",
			args:    []string{"--s3-bucket=flag-bucket"},
			project: psS3Settings(),
			want: psS3Want{
				bucket: "flag-bucket", accessKey: "toml-access-key", secretKey: psTOMLSecretKey, sessionToken: psTOMLSessionToken,
				used: psS3StringKeys()[1:], enabled: true, pathStyle: true,
			},
		},
		{
			name:    "path_style_disabled from galaxy.toml switches addressing",
			project: psS3VirtualHost(),
			want: psS3Want{
				bucket: "toml-bucket", accessKey: "toml-access-key", secretKey: psTOMLSecretKey, sessionToken: psTOMLSessionToken,
				used: append(psS3StringKeys(), "s3.path_style_disabled"), enabled: true,
			},
		},
		{
			name:    "--s3-path-style-disabled=false outranks galaxy.toml",
			args:    []string{"--s3-path-style-disabled=false"},
			project: psS3VirtualHost(),
			want:    everyKey,
		},
		{
			name: "keys without a bucket switch nothing on and are not credited",
			project: projectSettings{S3: projectfile.S3Settings{
				Region: "eu-west-1", Prefix: "ci/", AccessKey: "toml-access-key", SecretKey: psTOMLSecretKey,
			}},
			want: psS3Want{accessKey: "toml-access-key", secretKey: psTOMLSecretKey},
		},
	}
}

// psAssertS3 checks cfg.S3Cache and ProjectSettingsUsed against want field by
// field; the two secrets are compared through Reveal, never rendered.
func psAssertS3(t *testing.T, cfg *Config, want psS3Want) {
	t.Helper()
	got := cfg.S3Cache
	if got.Enabled != want.enabled {
		t.Errorf("Enabled = %v, want %v", got.Enabled, want.enabled)
	}
	if got.Bucket != want.bucket {
		t.Errorf("Bucket = %q, want %q", got.Bucket, want.bucket)
	}
	if got.AccessKey != want.accessKey {
		t.Errorf("AccessKey = %q, want %q", got.AccessKey, want.accessKey)
	}
	if got.SecretKey.Reveal() != want.secretKey || got.SecretKey.IsSet() != (want.secretKey != "") {
		t.Errorf("SecretKey does not carry the expected plaintext (IsSet = %v)", got.SecretKey.IsSet())
	}
	if got.SessionToken.Reveal() != want.sessionToken || got.SessionToken.IsSet() != (want.sessionToken != "") {
		t.Errorf("SessionToken does not carry the expected plaintext (IsSet = %v)", got.SessionToken.IsSet())
	}
	if got.PathStyle != want.pathStyle {
		t.Errorf("PathStyle = %v, want %v", got.PathStyle, want.pathStyle)
	}
	if !slices.Equal(cfg.ProjectSettingsUsed, want.used) {
		t.Errorf("ProjectSettingsUsed = %q, want %q", cfg.ProjectSettingsUsed, want.used)
	}
}

// TestLoadS3CacheConfigProjectSettings pins [tool.go-galaxy.s3] under the S3
// flags: each key is the flag when set, else the file; a bucket from either
// needs both keys from either; and only keys the file supplied are credited.
func TestLoadS3CacheConfigProjectSettings(t *testing.T) {
	t.Parallel()

	for _, tt := range psS3Rows() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{}
			err := loadS3CacheConfig(cfg, newS3Cmd(t, tt.args), tt.project)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("loadS3CacheConfig() error = %v, want errors.Is %v", err, tt.wantErr)
				}
				if err.Error() != psS3BucketWithoutKeysText {
					t.Fatalf("error = %q, want %q", err, psS3BucketWithoutKeysText)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadS3CacheConfig() error = %v, want nil", err)
			}
			psAssertS3(t, cfg, tt.want)
			if len(cfg.Warnings) != 0 {
				t.Errorf("Warnings = %q, want none", cfg.Warnings)
			}
		})
	}
}

// TestLoadS3CacheConfigProjectSecretsNeverPrint pins that the two plaintexts
// a [tool.go-galaxy.s3] table supplies reach Config only as Secret values: no
// fmt verb and no JSON encoding of the whole Config renders either of them.
func TestLoadS3CacheConfigProjectSecretsNeverPrint(t *testing.T) {
	t.Parallel()
	cfg := &Config{}
	if err := loadS3CacheConfig(cfg, newS3Cmd(t, nil), psS3Settings()); err != nil {
		t.Fatalf("loadS3CacheConfig() error = %v, want nil", err)
	}

	// Config carries no json tags: it is a runtime type, not a serialization
	// contract, and this checks that redaction survives reflection.
	jsonBytes, err := json.Marshal(cfg) //nolint:musttag
	if err != nil {
		t.Fatalf("json.Marshal() error = %v, want nil", err)
	}
	renderings := map[string]string{
		"%v":   fmt.Sprintf("%v", cfg),
		"%+v":  fmt.Sprintf("%+v", cfg),
		"%#v":  fmt.Sprintf("%#v", cfg),
		"json": string(jsonBytes),
	}
	for verb, out := range renderings {
		for _, plaintext := range []string{psTOMLSecretKey, psTOMLSessionToken} {
			if strings.Contains(out, plaintext) {
				t.Errorf("%s of Config renders the galaxy.toml plaintext %q", verb, plaintext)
			}
		}
	}

	// The positive control: the same secrets are held and readable through
	// Reveal, just not printable.
	if cfg.S3Cache.SecretKey.Reveal() != psTOMLSecretKey || cfg.S3Cache.SessionToken.Reveal() != psTOMLSessionToken {
		t.Errorf("Reveal() does not return the galaxy.toml plaintexts")
	}
}

// TestCheckS3CacheOfflineRefusesProjectBucket pins that a bucket switched on
// by galaxy.toml alone is refused under --offline with the same sentinel and
// text as one from the flags, since it is reached over the network all the same.
func TestCheckS3CacheOfflineRefusesProjectBucket(t *testing.T) {
	t.Parallel()
	cfg := &Config{Offline: true}
	if err := loadS3CacheConfig(cfg, newS3Cmd(t, nil), psS3Settings()); err != nil {
		t.Fatalf("loadS3CacheConfig() error = %v, want nil", err)
	}

	err := checkS3CacheOffline(cfg)
	if !errors.Is(err, helpers.ErrS3CacheOffline) {
		t.Fatalf("checkS3CacheOffline() error = %v, want errors.Is helpers.ErrS3CacheOffline", err)
	}
	const want = "--offline cannot be combined with an S3 cache bucket: the S3 cache is reached over the network"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}
