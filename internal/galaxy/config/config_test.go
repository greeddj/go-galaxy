package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// Defaults used for the CLI flags built in newApplyAnsibleConfigCmd. They
// only need to be distinguishable from the ansible.cfg-sourced values used
// in the test cases below.
const (
	testDefaultDownloadPath = "/default/collections"
	testDefaultRolesPath    = "/default/roles"
	testDefaultCacheDir     = "/default/cache"
	testDefaultServer       = "https://default.example"
	testAnsibleConfigPath   = "path"
)

// newApplyAnsibleConfigCmd runs a command exposing only the four flags
// applyAnsibleConfig reads (download-path, roles-path, cache-dir, server)
// and returns the *cli.Command its action captured.
func newApplyAnsibleConfigCmd(t *testing.T, args []string) *cli.Command {
	t.Helper()

	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "download-path", Value: testDefaultDownloadPath},
			&cli.StringFlag{Name: "roles-path", Value: testDefaultRolesPath},
			&cli.StringFlag{Name: "cache-dir", Value: testDefaultCacheDir},
			&cli.StringFlag{Name: "server", Value: testDefaultServer},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}

	fullArgs := append([]string{"go-galaxy"}, args...)
	if err := cmd.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// applyAnsibleConfigWant is the expected shape of *Config after
// applyAnsibleConfig runs: the three mapped fields, their "came from
// ansible.cfg" flags, and the recorded ansible.cfg path.
type applyAnsibleConfigWant struct {
	downloadPath      string
	cacheDir          string
	server            string
	ansibleConfigPath string
	collectionsUsed   bool
	cacheDirUsed      bool
	serverUsed        bool
}

// runApplyAnsibleConfig drives applyAnsibleConfig with the given CLI args
// and ansible.cfg values, returning the resulting *Config for assertion.
func runApplyAnsibleConfig(t *testing.T, args []string, ansCfg ansibleConfig) *Config {
	t.Helper()
	c := newApplyAnsibleConfigCmd(t, args)
	cfg := &Config{}
	applyAnsibleConfig(cfg, c, ansCfg, testAnsibleConfigPath)
	return cfg
}

// assertApplyAnsibleConfig checks got against want field by field, so a
// mismatch on any one mapping is reported without masking the others.
func assertApplyAnsibleConfig(t *testing.T, got *Config, want applyAnsibleConfigWant) {
	t.Helper()
	if got.DownloadPath != want.downloadPath {
		t.Errorf("DownloadPath = %q, want %q", got.DownloadPath, want.downloadPath)
	}
	if got.AnsibleCollectionsPathUsed != want.collectionsUsed {
		t.Errorf("AnsibleCollectionsPathUsed = %v, want %v", got.AnsibleCollectionsPathUsed, want.collectionsUsed)
	}
	if got.CacheDir != want.cacheDir {
		t.Errorf("CacheDir = %q, want %q", got.CacheDir, want.cacheDir)
	}
	if got.AnsibleCacheDirUsed != want.cacheDirUsed {
		t.Errorf("AnsibleCacheDirUsed = %v, want %v", got.AnsibleCacheDirUsed, want.cacheDirUsed)
	}
	if got.Server != want.server {
		t.Errorf("Server = %q, want %q", got.Server, want.server)
	}
	if got.AnsibleServerUsed != want.serverUsed {
		t.Errorf("AnsibleServerUsed = %v, want %v", got.AnsibleServerUsed, want.serverUsed)
	}
	if got.AnsibleConfigPath != want.ansibleConfigPath {
		t.Errorf("AnsibleConfigPath = %q, want %q", got.AnsibleConfigPath, want.ansibleConfigPath)
	}
}

// TestApplyAnsibleConfigDownloadPath pins collections_path -> DownloadPath:
// ansible.cfg applies only when --download-path is unset, an empty value falls
// back to the flag default, and AnsibleConfigPath records the loaded file.
func TestApplyAnsibleConfigDownloadPath(t *testing.T) {
	t.Run("flag unset, ansible.cfg value present", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "/ansible/collections"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: "/ansible/collections", collectionsUsed: true,
			cacheDir: testDefaultCacheDir, server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})

	t.Run("flag set explicitly", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "/ansible/collections"}}
		got := runApplyAnsibleConfig(t, []string{"--download-path=/explicit/collections"}, ansCfg)
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: "/explicit/collections",
			cacheDir:     testDefaultCacheDir, server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})

	t.Run("flag unset, ansible.cfg empty", func(t *testing.T) {
		got := runApplyAnsibleConfig(t, nil, ansibleConfig{})
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath,
			cacheDir:     testDefaultCacheDir, server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})
}

// assertCollectionsPathSplit checks got's DownloadPath and Warnings count
// against want, for TestCollectionsPathSplit's cases.
func assertCollectionsPathSplit(t *testing.T, got *Config, wantDownloadPath string, wantWarnings int) {
	t.Helper()
	if got.DownloadPath != wantDownloadPath {
		t.Errorf("DownloadPath = %q, want %q", got.DownloadPath, wantDownloadPath)
	}
	if len(got.Warnings) != wantWarnings {
		t.Errorf("Warnings = %v, want %d entries", got.Warnings, wantWarnings)
	}
}

// assertWarningMentions checks that got's sole warning mentions substr (the
// ignored collections_path entries).
func assertWarningMentions(t *testing.T, got *Config, substr string) {
	t.Helper()
	if len(got.Warnings) != 1 {
		t.Fatalf("len(Warnings) = %d, want 1 (Warnings = %v)", len(got.Warnings), got.Warnings)
	}
	if !strings.Contains(got.Warnings[0], substr) {
		t.Errorf("Warnings[0] = %q, want it to mention %q", got.Warnings[0], substr)
	}
}

// assertRoleWarningMentions is assertWarningMentions for RoleWarnings, and
// also requires Warnings to be empty: a role warning on the unconditional
// queue would be printed to every run.
func assertRoleWarningMentions(t *testing.T, got *Config, substr string) {
	t.Helper()
	if len(got.Warnings) != 0 {
		t.Fatalf("Warnings = %v, want none - a roles_path warning belongs on RoleWarnings", got.Warnings)
	}
	if len(got.RoleWarnings) != 1 {
		t.Fatalf("len(RoleWarnings) = %d, want 1 (RoleWarnings = %v)", len(got.RoleWarnings), got.RoleWarnings)
	}
	if !strings.Contains(got.RoleWarnings[0], substr) {
		t.Errorf("RoleWarnings[0] = %q, want it to mention %q", got.RoleWarnings[0], substr)
	}
}

// TestCollectionsPathSplit pins ansible's ":"-separated collections_path: the
// first entry is used and the rest are named in exactly one warning, while a
// single entry, a trailing separator or an empty value warn about nothing.
func TestCollectionsPathSplit(t *testing.T) {
	t.Run("multiple entries: first wins, rest warned about", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "a:b:c"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		assertCollectionsPathSplit(t, got, "a", 1)
		assertWarningMentions(t, got, "[b c]")
	})

	t.Run("single entry: no split, no warning", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "a"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		assertCollectionsPathSplit(t, got, "a", 0)
	})

	t.Run("trailing separator: empty segment filtered, no warning", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "a:"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		assertCollectionsPathSplit(t, got, "a", 0)
	})

	t.Run("empty ansible.cfg value: falls back to flag default, no warning", func(t *testing.T) {
		got := runApplyAnsibleConfig(t, nil, ansibleConfig{})
		assertCollectionsPathSplit(t, got, testDefaultDownloadPath, 0)
	})

	t.Run("explicit flag value also splits, matching ansible", func(t *testing.T) {
		got := runApplyAnsibleConfig(t, []string{"--download-path=a:b"}, ansibleConfig{})
		assertCollectionsPathSplit(t, got, "a", 1)
	})
}

// TestApplyAnsibleConfigCacheDir checks the cache_dir -> CacheDir mapping
// with the same three precedence scenarios as TestApplyAnsibleConfigDownloadPath.
func TestApplyAnsibleConfigCacheDir(t *testing.T) {
	t.Run("flag unset, ansible.cfg value present", func(t *testing.T) {
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{CacheDir: "/ansible/cache"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath, cacheDir: "/ansible/cache", cacheDirUsed: true,
			server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})

	t.Run("flag set explicitly", func(t *testing.T) {
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{CacheDir: "/ansible/cache"}}
		got := runApplyAnsibleConfig(t, []string{"--cache-dir=/explicit/cache"}, ansCfg)
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath, cacheDir: "/explicit/cache",
			server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})

	t.Run("flag unset, ansible.cfg empty", func(t *testing.T) {
		got := runApplyAnsibleConfig(t, nil, ansibleConfig{})
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath, cacheDir: testDefaultCacheDir,
			server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})
}

// TestParseTimeout pins that parseTimeout accepts ansible's bare-integer
// seconds and Go durations, yields the default for an empty value, and refuses
// non-positive or unparsable input as helpers.ErrInvalidTimeout.
func TestParseTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "bare seconds", raw: "60", want: 60 * time.Second},
		{name: "bare seconds, other value", raw: "90", want: 90 * time.Second},
		{name: "go duration, minutes and seconds", raw: "1m30s", want: 90 * time.Second},
		{name: "go duration, seconds", raw: "45s", want: 45 * time.Second},
		{name: "empty falls back to default", raw: "", want: helpers.FetchDefaultTimeout},
		{name: "zero seconds is invalid", raw: "0", wantErr: true},
		{name: "negative seconds is invalid", raw: "-5", wantErr: true},
		{name: "not a number or duration is invalid", raw: "abc", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseTimeout(tt.raw)
			if tt.wantErr {
				if !errors.Is(err, helpers.ErrInvalidTimeout) {
					t.Errorf("parseTimeout(%q) error = %v, want helpers.ErrInvalidTimeout", tt.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTimeout(%q) error = %v, want nil", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("parseTimeout(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// newTimeoutCmd builds a command for TestApplyTimeout that registers --timeout
// as CollectionFlags does when registerFlag is true, and otherwise no such
// flag at all, like cleanup, so c.String("timeout") reads empty.
func newTimeoutCmd(t *testing.T, registerFlag bool, args []string) *cli.Command {
	t.Helper()

	var flags []cli.Flag
	if registerFlag {
		flags = []cli.Flag{&cli.StringFlag{
			Name:    "timeout",
			Value:   helpers.FetchDefaultTimeout.String(),
			Sources: cli.EnvVars("GO_GALAXY_SERVER_TIMEOUT", "GO_GALAXY_TIMEOUT", "ANSIBLE_GALAXY_SERVER_TIMEOUT"),
		}}
	}

	var captured *cli.Command
	cmd := &cli.Command{
		Name:  "go-galaxy",
		Flags: flags,
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}

	fullArgs := append([]string{"go-galaxy"}, args...)
	if err := cmd.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// TestApplyTimeout pins that applyTimeout honors an explicit --timeout or
// ANSIBLE_GALAXY_SERVER_TIMEOUT, and falls back to the default when the flag
// is unset or, as for cleanup, never registered.
func TestApplyTimeout(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		want         time.Duration
		registerFlag bool
	}{
		{
			name:         "small positive timeout is honored",
			registerFlag: true,
			args:         []string{"--timeout=5s"},
			want:         5 * time.Second,
		},
		{
			name:         "larger timeout passes through unchanged",
			registerFlag: true,
			args:         []string{"--timeout=45s"},
			want:         45 * time.Second,
		},
		{
			name:         "unset flag falls back to default",
			registerFlag: true,
			want:         helpers.FetchDefaultTimeout,
		},
		{
			name: "unregistered flag falls back to default",
			want: helpers.FetchDefaultTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTimeoutCmd(t, tt.registerFlag, tt.args)
			cfg := &Config{}
			if err := applyTimeout(cfg, c); err != nil {
				t.Fatalf("applyTimeout() error = %v, want nil", err)
			}
			if cfg.Timeout != tt.want {
				t.Errorf("Timeout = %v, want %v", cfg.Timeout, tt.want)
			}
		})
	}

	// t.Setenv forbids t.Parallel, so the env-driven case is its own
	// non-parallel subtest rather than part of the table above.
	t.Run("env var in ansible's bare-seconds form is honored", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_TIMEOUT", "60")
		c := newTimeoutCmd(t, true, nil)
		cfg := &Config{}
		if err := applyTimeout(cfg, c); err != nil {
			t.Fatalf("applyTimeout() error = %v, want nil", err)
		}
		if cfg.Timeout != 60*time.Second {
			t.Errorf("Timeout = %v, want %v", cfg.Timeout, 60*time.Second)
		}
	})
}

// newIntFlagCmd registers one IntFlag sourced from envName, or none at all
// (the cleanup shape). defaultValue mirrors the production flag's Value, which
// c.Int reads for a declared-but-empty variable.
func newIntFlagCmd(t *testing.T, flagName, envName string, defaultValue int, registerFlag bool, args []string) *cli.Command {
	t.Helper()

	var flags []cli.Flag
	if registerFlag {
		flags = []cli.Flag{&cli.IntFlag{
			Name:    flagName,
			Value:   defaultValue,
			Sources: cli.EnvVars(envName),
		}}
	}

	var captured *cli.Command
	cmd := &cli.Command{
		Name:  "go-galaxy",
		Flags: flags,
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}

	fullArgs := append([]string{"go-galaxy"}, args...)
	if err := cmd.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// workersRow is one shape a --workers value can arrive in: what the fixture
// registers and supplies, the worker count applyWorkers must settle on, and
// whether it must have queued a warning about the value it was handed.
type workersRow struct {
	name         string
	wantWarnHas  string
	args         []string
	procs        int
	wantWorkers  int
	registerFlag bool
	wantWarn     bool
}

// assertWorkersOutcome checks one row's settled worker count and the warning
// that must or must not sit beside it. It calls t.Helper, so every failure
// below is reported on the caller's line rather than on this function's own.
func assertWorkersOutcome(t *testing.T, cfg *Config, row workersRow) {
	t.Helper()

	if cfg.Workers != row.wantWorkers {
		t.Fatalf("cfg.Workers = %d, want %d", cfg.Workers, row.wantWorkers)
	}
	if !row.wantWarn {
		if len(cfg.Warnings) != 0 {
			t.Fatalf("len(cfg.Warnings) = %d, want 0", len(cfg.Warnings))
		}
		return
	}
	if len(cfg.Warnings) != 1 {
		t.Fatalf("len(cfg.Warnings) = %d, want 1", len(cfg.Warnings))
	}
	if !strings.Contains(cfg.Warnings[0], row.wantWarnHas) {
		t.Fatalf("warning does not name the supplied value %q: %q", row.wantWarnHas, cfg.Warnings[0])
	}
	if !strings.Contains(cfg.Warnings[0], "--workers") {
		t.Fatalf("warning does not name the flag: %q", cfg.Warnings[0])
	}
}

// workersRows enumerates the shapes TestApplyWorkers drives applyWorkers
// with. It is a function of its own rather than a literal inside that test
// only to keep the test itself inside funlen's budget.
func workersRows() []workersRow {
	return []workersRow{
		{
			name:  "a value inside the range is honored",
			procs: 8, registerFlag: true, args: []string{"--workers=3"}, wantWorkers: 3,
		},
		{
			name:  "the lower bound is inclusive",
			procs: 8, registerFlag: true, args: []string{"--workers=1"}, wantWorkers: 1,
		},
		{
			name:  "the upper bound is inclusive",
			procs: 8, registerFlag: true, args: []string{"--workers=8"}, wantWorkers: 8,
		},
		{
			name:  "zero is replaced with a warning",
			procs: 8, registerFlag: true, args: []string{"--workers=0"}, wantWorkers: 8,
			wantWarn: true, wantWarnHas: "= 0",
		},
		{
			name:  "a negative value lands on the same outcome",
			procs: 8, registerFlag: true, args: []string{"--workers=-1"}, wantWorkers: 8,
			wantWarn: true, wantWarnHas: "= -1",
		},
		{
			name:  "a single permitted cpu still accepts two workers",
			procs: 1, registerFlag: true, args: []string{"--workers=2"}, wantWorkers: 2,
		},
		{
			name:  "a single permitted cpu replaces five",
			procs: 1, registerFlag: true, args: []string{"--workers=5"}, wantWorkers: 2,
			wantWarn: true, wantWarnHas: "= 5",
		},
		{
			name:  "above the ceiling the substitute is the default, not the ceiling",
			procs: 64, registerFlag: true, args: []string{"--workers=65"}, wantWorkers: 16,
			wantWarn: true, wantWarnHas: "= 65",
		},
		{
			name:  "the ceiling is the permitted cpu, not the default's own cap",
			procs: 64, registerFlag: true, args: []string{"--workers=32"}, wantWorkers: 32,
		},
		{name: "an unregistered flag takes the default silently", procs: 8, wantWorkers: 8},
	}
}

// TestApplyWorkers pins which --workers values applyWorkers honors, which it
// replaces with the derived default plus a warning, and that an unregistered
// flag takes the default silently. Wants are hand-spelled, never computed.
func TestApplyWorkers(t *testing.T) {
	// Only the declared-but-empty subtest reads the fixture flag's Value, where
	// it stands in for the production flag's Value at procs 8.
	fixtureValue := helpers.DefaultInstallWorkers(8)

	tests := workersRows()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newIntFlagCmd(t, "workers", "GO_GALAXY_WORKERS", fixtureValue, tt.registerFlag, tt.args)
			cfg := &Config{}
			applyWorkers(cfg, c, tt.procs)
			assertWorkersOutcome(t, cfg, tt)
		})
	}

	// t.Setenv forbids t.Parallel, so the two environment-sourced shapes are
	// their own non-parallel subtests rather than rows in the table above.
	t.Run("a zero from the environment is replaced with a warning", func(t *testing.T) {
		t.Setenv("GO_GALAXY_WORKERS", "0")
		c := newIntFlagCmd(t, "workers", "GO_GALAXY_WORKERS", fixtureValue, true, nil)
		cfg := &Config{}
		applyWorkers(cfg, c, 8)
		assertWorkersOutcome(t, cfg, workersRow{wantWorkers: 8, wantWarn: true, wantWarnHas: "= 0"})
	})

	// urfave marks a declared-but-empty variable set while skipping its parse,
	// so it reaches the range check carrying the flag's own Value.
	t.Run("a declared but empty variable is honored on the flag's Value", func(t *testing.T) {
		t.Setenv("GO_GALAXY_WORKERS", "")
		c := newIntFlagCmd(t, "workers", "GO_GALAXY_WORKERS", fixtureValue, true, nil)
		cfg := &Config{}
		applyWorkers(cfg, c, 8)
		assertWorkersOutcome(t, cfg, workersRow{wantWorkers: 8})
	})
}

// TestDownloadWorkersDefault pins helpers.DefaultDownloadWorkers' clamp and
// newConfigFromCLI's silent fallback to it for a non-positive value; unlike
// --workers there is no ceiling, since that pool waits on the network.
func TestDownloadWorkersDefault(t *testing.T) {
	t.Run("derives from cpu count", func(t *testing.T) {
		tests := []struct {
			name string
			cpus int
			want int
		}{
			{name: "1 cpu floors to the minimum", cpus: 1, want: 8},
			{name: "2 cpus already equal the minimum", cpus: 2, want: 8},
			{name: "4 cpus scales linearly", cpus: 4, want: 16},
			{name: "8 cpus reaches the maximum exactly", cpus: 8, want: 32},
			{name: "16 cpus caps at the maximum", cpus: 16, want: 32},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := helpers.DefaultDownloadWorkers(tt.cpus); got != tt.want {
					t.Fatalf("DefaultDownloadWorkers(%d) = %d, want %d", tt.cpus, got, tt.want)
				}
			})
		}
	})

	t.Run("explicit positive value survives unoverridden", func(t *testing.T) {
		c := newIntFlagCmd(t, "download-workers", "GO_GALAXY_DOWNLOAD_WORKERS",
			helpers.DefaultDownloadWorkers(runtime.NumCPU()), true, []string{"--download-workers=3"})
		cfg := newConfigFromCLI(c)
		if cfg.DownloadWorkers != 3 {
			t.Fatalf("cfg.DownloadWorkers = %d, want 3", cfg.DownloadWorkers)
		}
	})

	t.Run("zero falls back to the default", func(t *testing.T) {
		c := newIntFlagCmd(t, "download-workers", "GO_GALAXY_DOWNLOAD_WORKERS",
			helpers.DefaultDownloadWorkers(runtime.NumCPU()), true, []string{"--download-workers=0"})
		cfg := newConfigFromCLI(c)
		want := helpers.DefaultDownloadWorkers(runtime.NumCPU())
		if cfg.DownloadWorkers != want {
			t.Fatalf("cfg.DownloadWorkers = %d, want %d (the default)", cfg.DownloadWorkers, want)
		}
	})

	t.Run("negative falls back to the default", func(t *testing.T) {
		c := newIntFlagCmd(t, "download-workers", "GO_GALAXY_DOWNLOAD_WORKERS",
			helpers.DefaultDownloadWorkers(runtime.NumCPU()), true, []string{"--download-workers=-1"})
		cfg := newConfigFromCLI(c)
		want := helpers.DefaultDownloadWorkers(runtime.NumCPU())
		if cfg.DownloadWorkers != want {
			t.Fatalf("cfg.DownloadWorkers = %d, want %d (the default)", cfg.DownloadWorkers, want)
		}
	})
}

// TestInstallWorkersDefault pins helpers.DefaultInstallWorkers' clamp of the
// permitted CPU count at each boundary, and that the result always lies inside
// helpers.MaxAcceptedInstallWorkers' range. Wants are hand-spelled.
func TestInstallWorkersDefault(t *testing.T) {
	t.Run("derives from permitted cpu", func(t *testing.T) {
		tests := []struct {
			name  string
			procs int
			want  int
		}{
			{name: "1 permitted cpu floors to the minimum", procs: 1, want: 2},
			{name: "2 permitted cpus already equal the minimum", procs: 2, want: 2},
			{name: "3 permitted cpus pass through unclamped", procs: 3, want: 3},
			{name: "15 permitted cpus stay one below the maximum", procs: 15, want: 15},
			{name: "16 permitted cpus reach the maximum exactly", procs: 16, want: 16},
			{name: "17 permitted cpus cap at the maximum", procs: 17, want: 16},
			{name: "a 128-cpu node caps at the maximum", procs: 128, want: 16},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := helpers.DefaultInstallWorkers(tt.procs); got != tt.want {
					t.Fatalf("DefaultInstallWorkers(%d) = %d, want %d", tt.procs, got, tt.want)
				}
			})
		}
	})

	// An empty GO_GALAXY_WORKERS= reaches applyWorkers' range check carrying
	// the flag's Value, so a derived default outside the accepted range would
	// warn about a value nobody wrote.
	t.Run("the derived default is always inside the accepted range", func(t *testing.T) {
		tests := []struct {
			name        string
			procs       int
			wantCeiling int
		}{
			{name: "1 permitted cpu is floored to two", procs: 1, wantCeiling: 2},
			{name: "2 permitted cpus are the floor itself", procs: 2, wantCeiling: 2},
			{name: "3 permitted cpus pass through", procs: 3, wantCeiling: 3},
			{name: "4 permitted cpus pass through", procs: 4, wantCeiling: 4},
			{name: "12 permitted cpus pass through", procs: 12, wantCeiling: 12},
			{name: "15 permitted cpus stay one below the default's cap", procs: 15, wantCeiling: 15},
			{name: "16 permitted cpus meet the default's cap", procs: 16, wantCeiling: 16},
			{name: "17 permitted cpus pass the default's cap", procs: 17, wantCeiling: 17},
			{name: "64 permitted cpus are accepted in full", procs: 64, wantCeiling: 64},
			{name: "128 permitted cpus are accepted in full", procs: 128, wantCeiling: 128},
			{name: "1024 permitted cpus are accepted in full", procs: 1024, wantCeiling: 1024},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := helpers.MaxAcceptedInstallWorkers(tt.procs); got != tt.wantCeiling {
					t.Fatalf("MaxAcceptedInstallWorkers(%d) = %d, want %d", tt.procs, got, tt.wantCeiling)
				}
				got := helpers.DefaultInstallWorkers(tt.procs)
				if got < 1 || got > tt.wantCeiling {
					t.Fatalf("DefaultInstallWorkers(%d) = %d, want inside 1..%d", tt.procs, got, tt.wantCeiling)
				}
			})
		}
	})
}

// TestApplyAnsibleConfigServer checks the server -> Server mapping with the
// same three precedence scenarios as TestApplyAnsibleConfigDownloadPath.
func TestApplyAnsibleConfigServer(t *testing.T) {
	t.Run("flag unset, ansible.cfg value present", func(t *testing.T) {
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://ansible.example"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath, cacheDir: testDefaultCacheDir,
			server: "https://ansible.example", serverUsed: true, ansibleConfigPath: testAnsibleConfigPath,
		})
	})

	t.Run("flag set explicitly", func(t *testing.T) {
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://ansible.example"}}
		got := runApplyAnsibleConfig(t, []string{"--server=https://explicit.example"}, ansCfg)
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath, cacheDir: testDefaultCacheDir,
			server: "https://explicit.example", ansibleConfigPath: testAnsibleConfigPath,
		})
	})

	t.Run("flag unset, ansible.cfg empty", func(t *testing.T) {
		got := runApplyAnsibleConfig(t, nil, ansibleConfig{})
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath, cacheDir: testDefaultCacheDir,
			server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})
}

// newAnsibleConfigCmd declares --ansible-config as cliflags does, with no
// default and only GO_GALAXY_ANSIBLE_CONFIG as a source, since c.IsSet is what
// tells an explicit request from discovery, which reads ANSIBLE_CONFIG itself.
func newAnsibleConfigCmd(t *testing.T, args []string) *cli.Command {
	t.Helper()

	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "ansible-config", Sources: cli.EnvVars("GO_GALAXY_ANSIBLE_CONFIG")},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}

	fullArgs := append([]string{"go-galaxy"}, args...)
	if err := cmd.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// writeAnsibleCfg writes a minimal, distinguishable ansible.cfg to path so
// tests can assert it was the one actually loaded.
func writeAnsibleCfg(t *testing.T, path, server string) {
	t.Helper()
	content := "[galaxy]\nserver = " + server + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v, want nil", path, err)
	}
}

// TestLoadAnsibleConfigFromCLIExplicit pins that an explicit --ansible-config
// is strict: an existing file is loaded, and a missing one is
// helpers.ErrAnsibleConfigNotFound where discovery would move on.
func TestLoadAnsibleConfigFromCLIExplicit(t *testing.T) {
	t.Run("existing path is loaded", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "custom.cfg")
		writeAnsibleCfg(t, path, "https://explicit.example")

		c := newAnsibleConfigCmd(t, []string{"--ansible-config=" + path})
		cfg, gotPath, _, err := loadAnsibleConfigFromCLI(c)
		if err != nil {
			t.Fatalf("loadAnsibleConfigFromCLI() error = %v, want nil", err)
		}
		if gotPath != path {
			t.Errorf("path = %q, want %q", gotPath, path)
		}
		if cfg.Galaxy.Server != "https://explicit.example" {
			t.Errorf("Galaxy.Server = %q, want %q", cfg.Galaxy.Server, "https://explicit.example")
		}
	})

	t.Run("missing path is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.cfg")

		c := newAnsibleConfigCmd(t, []string{"--ansible-config=" + path})
		_, _, _, err := loadAnsibleConfigFromCLI(c)
		if !errors.Is(err, helpers.ErrAnsibleConfigNotFound) {
			t.Errorf("error = %v, want helpers.ErrAnsibleConfigNotFound", err)
		}
	})
}

// TestLoadAnsibleConfigFromCLIDiscovery pins discovery without the flag:
// $ANSIBLE_CONFIG beats ./ansible.cfg, a missing $ANSIBLE_CONFIG falls through,
// and a world-writable cwd is skipped. Not parallel: t.Chdir and t.Setenv.
func TestLoadAnsibleConfigFromCLIDiscovery(t *testing.T) {
	t.Run("cwd ansible.cfg is discovered", func(t *testing.T) {
		dir := t.TempDir()
		writeAnsibleCfg(t, filepath.Join(dir, "ansible.cfg"), "https://cwd.example")
		t.Chdir(dir)

		c := newAnsibleConfigCmd(t, nil)
		cfg, gotPath, _, err := loadAnsibleConfigFromCLI(c)
		// The cwd candidate is the relative "ansible.cfg", resolved against
		// the working directory as ansible's own discovery does.
		assertAnsibleConfigLoaded(t, cfg, gotPath, err, "ansible.cfg", "https://cwd.example")
	})

	t.Run("ANSIBLE_CONFIG is discovered ahead of cwd", func(t *testing.T) {
		envDir := t.TempDir()
		envPath := filepath.Join(envDir, "env.cfg")
		writeAnsibleCfg(t, envPath, "https://env.example")
		t.Setenv("ANSIBLE_CONFIG", envPath)

		cwdDir := t.TempDir()
		writeAnsibleCfg(t, filepath.Join(cwdDir, "ansible.cfg"), "https://cwd.example")
		t.Chdir(cwdDir)

		c := newAnsibleConfigCmd(t, nil)
		cfg, gotPath, _, err := loadAnsibleConfigFromCLI(c)
		assertAnsibleConfigLoaded(t, cfg, gotPath, err, envPath, "https://env.example")
	})

	t.Run("missing ANSIBLE_CONFIG falls through to cwd, no error", func(t *testing.T) {
		t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "missing.cfg"))

		cwdDir := t.TempDir()
		writeAnsibleCfg(t, filepath.Join(cwdDir, "ansible.cfg"), "https://cwd.example")
		t.Chdir(cwdDir)

		c := newAnsibleConfigCmd(t, nil)
		cfg, gotPath, _, err := loadAnsibleConfigFromCLI(c)
		assertAnsibleConfigLoaded(t, cfg, gotPath, err, "ansible.cfg", "https://cwd.example")
	})

	t.Run("world-writable cwd is skipped with a warning", subtestWorldWritableCwdSkipped)
	t.Run("non-world-writable cwd is discovered", subtestNonWorldWritableCwdDiscovered)
	t.Run("world-writable cwd: a relative ANSIBLE_CONFIG still reads that file", subtestWorldWritableCwdEnvPathStillRead)

	t.Run("nothing found in cwd or ANSIBLE_CONFIG: falls through cleanly", func(t *testing.T) {
		// Whether ~/.ansible.cfg or /etc/ansible/ansible.cfg exists is up to
		// the host, so only no error, and a found path being one of those
		// two, can be asserted.
		t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "missing.cfg"))
		t.Chdir(t.TempDir())

		c := newAnsibleConfigCmd(t, nil)
		_, gotPath, _, err := loadAnsibleConfigFromCLI(c)
		assertDiscoveryFallsThroughCleanly(t, gotPath, err)
	})
}

// chmodDir sets dir's mode, failing the test if it cannot: a silently
// unchanged mode would turn the subtests below into tests of nothing, since
// t.TempDir is 0o700 on most systems and the mode is their whole subject.
func chmodDir(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	// #nosec G302 -- the permission is the fixture: these subtests exist to
	// drive discovery against a world-writable working directory.
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatalf("os.Chmod(%q, %#o) error = %v, want nil", dir, mode, err)
	}
}

// subtestWorldWritableCwdSkipped pins that ./ansible.cfg is not a discovery
// candidate in a world-writable working directory and that a warning says so;
// subtestNonWorldWritableCwdDiscovered is its positive control.
func subtestWorldWritableCwdSkipped(t *testing.T) {
	// The env candidate is pointed at a missing file so it cannot win and
	// mask what the cwd candidate did.
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "missing.cfg"))

	dir := t.TempDir()
	writeAnsibleCfg(t, filepath.Join(dir, "ansible.cfg"), "https://cwd.example")
	chmodDir(t, dir, 0o777)
	t.Chdir(dir)

	c := newAnsibleConfigCmd(t, nil)
	_, gotPath, warnings, err := loadAnsibleConfigFromCLI(c)
	if err != nil {
		t.Fatalf("loadAnsibleConfigFromCLI() error = %v, want nil", err)
	}
	if gotPath == "ansible.cfg" {
		t.Fatalf("path = %q, want the cwd candidate to have been skipped", gotPath)
	}
	if !warningMentions(warnings, "world-writable", dir) {
		t.Fatalf("warnings = %v, want one naming %q as world-writable", warnings, dir)
	}
}

// subtestNonWorldWritableCwdDiscovered is subtestWorldWritableCwdSkipped's
// positive control: the same fixture at mode 0o755 must be discovered with no
// warning, or "skipped" could not be told from never reached.
func subtestNonWorldWritableCwdDiscovered(t *testing.T) {
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "missing.cfg"))

	dir := t.TempDir()
	writeAnsibleCfg(t, filepath.Join(dir, "ansible.cfg"), "https://cwd.example")
	chmodDir(t, dir, 0o755)
	t.Chdir(dir)

	c := newAnsibleConfigCmd(t, nil)
	cfg, gotPath, warnings, err := loadAnsibleConfigFromCLI(c)
	assertAnsibleConfigLoaded(t, cfg, gotPath, err, "ansible.cfg", "https://cwd.example")
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

// subtestWorldWritableCwdEnvPathStillRead pins that the skip covers only the
// cwd candidate: a relative $ANSIBLE_CONFIG naming the same file still loads
// it, and the world-writable warning names $ANSIBLE_CONFIG as still read.
func subtestWorldWritableCwdEnvPathStillRead(t *testing.T) {
	dir := t.TempDir()
	writeAnsibleCfg(t, filepath.Join(dir, "ansible.cfg"), "https://cwd.example")
	chmodDir(t, dir, 0o777)
	t.Chdir(dir)
	// Relative on purpose: it names the same file without the assertion
	// depending on a temp path.
	t.Setenv("ANSIBLE_CONFIG", "ansible.cfg")

	c := newAnsibleConfigCmd(t, nil)
	cfg, gotPath, warnings, err := loadAnsibleConfigFromCLI(c)
	assertAnsibleConfigLoaded(t, cfg, gotPath, err, "ansible.cfg", "https://cwd.example")
	if !warningMentions(warnings, "world-writable", dir) {
		t.Fatalf("warnings = %v, want one naming %q as world-writable", warnings, dir)
	}
	if !warningMentions(warnings, "$ANSIBLE_CONFIG") {
		t.Fatalf("warnings = %v, want one naming $ANSIBLE_CONFIG as a path still read", warnings)
	}
}

// warningMentions reports whether any warning contains every one of parts,
// matching fragments so the test does not pin wording it has no reason to own.
func warningMentions(warnings []string, parts ...string) bool {
	for _, w := range warnings {
		matched := true
		for _, part := range parts {
			if !strings.Contains(w, part) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// assertAnsibleConfigLoaded checks that loadAnsibleConfigFromCLI succeeded
// and returned the expected path and Galaxy.Server value.
func assertAnsibleConfigLoaded(t *testing.T, cfg ansibleConfig, gotPath string, err error, wantPath, wantServer string) {
	t.Helper()
	if err != nil {
		t.Fatalf("loadAnsibleConfigFromCLI() error = %v, want nil", err)
	}
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
	if cfg.Galaxy.Server != wantServer {
		t.Errorf("Galaxy.Server = %q, want %q", cfg.Galaxy.Server, wantServer)
	}
}

// assertDiscoveryFallsThroughCleanly checks for no error and that any path
// found is one of the machine-level candidates (~/.ansible.cfg,
// /etc/ansible/ansible.cfg) this test cannot control.
func assertDiscoveryFallsThroughCleanly(t *testing.T, gotPath string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("loadAnsibleConfigFromCLI() error = %v, want nil", err)
	}
	if gotPath == "" {
		return
	}
	home, homeErr := os.UserHomeDir()
	wantHomePath := ""
	if homeErr == nil {
		wantHomePath = filepath.Join(home, ".ansible.cfg")
	}
	if gotPath != wantHomePath && gotPath != "/etc/ansible/ansible.cfg" {
		t.Errorf("path = %q, want %q, %q, or empty", gotPath, wantHomePath, "/etc/ansible/ansible.cfg")
	}
}

// TestAnsibleGalaxyServerEnv pins ANSIBLE_GALAXY_SERVER as the env spelling of
// [galaxy] server: it outranks that key, and an exported but empty value still
// hides the key and falls through to the flag default, not to an empty URL.
func TestAnsibleGalaxyServerEnv(t *testing.T) {
	const envServer = "https://env.example"

	t.Run("env beats the ansible.cfg key", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER", envServer)
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://ini.example"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		if got.Server != envServer {
			t.Fatalf("Server = %q, want %q", got.Server, envServer)
		}
		if !got.AnsibleServerUsed || !got.AnsibleServerEnvUsed {
			t.Fatalf("AnsibleServerUsed = %v, AnsibleServerEnvUsed = %v, want both true",
				got.AnsibleServerUsed, got.AnsibleServerEnvUsed)
		}
	})

	t.Run("env unset keeps the ansible.cfg key", func(t *testing.T) {
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://ini.example"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		if got.Server != "https://ini.example" {
			t.Fatalf("Server = %q, want %q", got.Server, "https://ini.example")
		}
		if !got.AnsibleServerUsed || got.AnsibleServerEnvUsed {
			t.Fatalf("AnsibleServerUsed = %v, AnsibleServerEnvUsed = %v, want true and false",
				got.AnsibleServerUsed, got.AnsibleServerEnvUsed)
		}
	})

	t.Run("env set empty falls through to the flag default", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER", "")
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://ini.example"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		if got.Server != testDefaultServer {
			t.Fatalf("Server = %q, want the flag default %q", got.Server, testDefaultServer)
		}
		if got.AnsibleServerUsed || got.AnsibleServerEnvUsed {
			t.Fatalf("AnsibleServerUsed = %v, AnsibleServerEnvUsed = %v, want both false",
				got.AnsibleServerUsed, got.AnsibleServerEnvUsed)
		}
	})
}

// TestApplyAnsibleConfigRolesPath checks the roles_path -> RolesPath mapping
// under the three precedence scenarios of TestApplyAnsibleConfigDownloadPath.
func TestApplyAnsibleConfigRolesPath(t *testing.T) {
	t.Run("flag unset, ansible.cfg value present", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{RolesPath: "/ansible/roles"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		if got.RolesPath != "/ansible/roles" || !got.AnsibleRolesPathUsed {
			t.Fatalf("RolesPath = %q (ansible used %t), want /ansible/roles from ansible.cfg", got.RolesPath, got.AnsibleRolesPathUsed)
		}
	})

	t.Run("flag set explicitly", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{RolesPath: "/ansible/roles"}}
		got := runApplyAnsibleConfig(t, []string{"--roles-path=/explicit/roles"}, ansCfg)
		if got.RolesPath != "/explicit/roles" || got.AnsibleRolesPathUsed {
			t.Fatalf("RolesPath = %q (ansible used %t), want the explicit flag", got.RolesPath, got.AnsibleRolesPathUsed)
		}
	})

	t.Run("flag unset, ansible.cfg empty", func(t *testing.T) {
		got := runApplyAnsibleConfig(t, nil, ansibleConfig{})
		if got.RolesPath != testDefaultRolesPath || got.AnsibleRolesPathUsed {
			t.Fatalf("RolesPath = %q (ansible used %t), want the flag default", got.RolesPath, got.AnsibleRolesPathUsed)
		}
		if len(got.Warnings) != 0 || len(got.RoleWarnings) != 0 {
			t.Fatalf("warnings = %q, role warnings = %q, want none of either", got.Warnings, got.RoleWarnings)
		}
	})
}

// TestRolesPathSplitAndOverlap pins roles_path's two warnings, a search list
// beyond its first entry and a directory shared with collections_path: both go
// to RoleWarnings, never Warnings, so a run with no roles never prints them.
func TestRolesPathSplitAndOverlap(t *testing.T) {
	t.Run("search list: first wins, rest warned about by name", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{RolesPath: "/r/a:/r/b"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		if got.RolesPath != "/r/a" {
			t.Fatalf("RolesPath = %q, want /r/a", got.RolesPath)
		}
		assertRoleWarningMentions(t, got, "roles_path lists multiple paths")
		assertRoleWarningMentions(t, got, "[/r/b]")
	})

	t.Run("same directory as collections_path warns", func(t *testing.T) {
		got := runApplyAnsibleConfig(t, []string{"--download-path=/shared/", "--roles-path=/shared"}, ansibleConfig{})
		assertRoleWarningMentions(t, got, "roles_path and collections_path are the same directory")
	})
}

// TestCollectionsPathSplitStaysUnconditional pins that collections_path's
// search-list warning stays on Warnings, since every run reads that setting.
func TestCollectionsPathSplitStaysUnconditional(t *testing.T) {
	t.Parallel()
	got := runApplyAnsibleConfig(t, []string{"--download-path=/c/a:/c/b"}, ansibleConfig{})
	if got.DownloadPath != "/c/a" {
		t.Fatalf("DownloadPath = %q, want /c/a", got.DownloadPath)
	}
	if len(got.RoleWarnings) != 0 {
		t.Fatalf("RoleWarnings = %q, want none - collections_path is not role-scoped", got.RoleWarnings)
	}
	assertWarningMentions(t, got, "collections_path lists multiple paths")
}
