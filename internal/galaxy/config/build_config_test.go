package config

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// isolateBuildEnv unsets every variable a flag or a credential loader may read,
// restoring them when the test ends, and moves HOME and the cwd under t.TempDir.
func isolateBuildEnv(t *testing.T) string {
	t.Helper()
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "GO_GALAXY_") && !strings.HasPrefix(key, "ANSIBLE_") && !strings.HasPrefix(key, "AWS_") {
			continue
		}
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

// buildInstallConfig runs BuildCollectionConfig from an install-shaped
// subcommand under a root carrying cliflags.CommonFlags, the tree main.go builds.
func buildInstallConfig(t *testing.T, args []string) (*Config, error) {
	t.Helper()
	flags := cliflags.CollectionFlags()
	flags = append(flags, cliflags.SignatureFlags()...)
	flags = append(flags, cliflags.S3Flags()...)
	var gotCfg *Config
	var gotErr error
	app := &cli.Command{
		Name:  "go-galaxy",
		Flags: cliflags.CommonFlags(),
		Commands: []*cli.Command{{
			Name:  "install",
			Flags: flags,
			Action: func(_ context.Context, c *cli.Command) error {
				gotCfg, gotErr = BuildCollectionConfig(c)
				return nil
			},
		}},
	}
	if err := app.Run(context.Background(), append([]string{"go-galaxy", "install"}, args...)); err != nil {
		t.Fatalf("app.Run() error = %v, want nil", err)
	}
	return gotCfg, gotErr
}

// writeBuildFile writes content to path, failing the test on error.
func writeBuildFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// buildLadderState is the mutable fixture of the precedence ladder: the
// argument list, keyed by flag so a fix can drop or replace one.
type buildLadderState struct {
	args map[string]string
	dir  string
}

func (s *buildLadderState) argv() []string {
	out := make([]string, 0, len(s.args))
	for _, key := range slices.Sorted(maps.Keys(s.args)) {
		out = append(out, "--"+key+"="+s.args[key])
	}
	return out
}

// buildLadderStep is one broken source: the sentinel it must be reported
// as while every later source is broken too, and the fix that clears it.
type buildLadderStep struct {
	want error
	fix  func(t *testing.T, s *buildLadderState)
	name string
}

// buildLadderSteps lists the sources in the order BuildCollectionConfig must
// report them, so a refactor that reorders the checks fails here.
func buildLadderSteps() []buildLadderStep {
	return []buildLadderStep{
		{name: "requirements file name", want: helpers.ErrRequirementsFileName, fix: func(_ *testing.T, s *buildLadderState) {
			s.args["requirements-file"] = filepath.Join(s.dir, "galaxy.toml")
		}},
		{name: "unset ${VAR} in galaxy.toml", want: helpers.ErrProjectFileEnvUnset, fix: func(t *testing.T, _ *buildLadderState) {
			t.Helper()
			t.Setenv("GG_LADDER_LOCK", "locks/pinned.lock")
		}},
		{name: "--timeout", want: helpers.ErrInvalidTimeout, fix: func(_ *testing.T, s *buildLadderState) {
			delete(s.args, "timeout")
		}},
		{name: "--ansible-config names no file", want: helpers.ErrAnsibleConfigNotFound, fix: func(t *testing.T, s *buildLadderState) {
			t.Helper()
			writeBuildFile(t, s.args["ansible-config"], "[galaxy]\nserver_timeout = never\n")
		}},
		{name: "--server", want: helpers.ErrInvalidGalaxyServerURL, fix: func(_ *testing.T, s *buildLadderState) {
			s.args["server"] = "https://galaxy.example"
		}},
		{name: "git credential list", want: helpers.ErrGitCredentialInvalid, fix: func(t *testing.T, _ *buildLadderState) {
			t.Helper()
			t.Setenv("GO_GALAXY_GIT_CREDENTIALS", "")
		}},
		{name: "url credential list", want: helpers.ErrURLCredentialInvalid, fix: func(t *testing.T, _ *buildLadderState) {
			t.Helper()
			t.Setenv("GO_GALAXY_URL_CREDENTIALS", "")
		}},
		{name: "S3 bucket without keys", want: helpers.ErrS3EmptyCreds, fix: func(_ *testing.T, s *buildLadderState) {
			s.args["s3-access-key"] = "AKIALADDER"
			s.args["s3-secret-key"] = "ladder-secret"
		}},
		{name: "empty --keyring", want: helpers.ErrEmptySignatureValue, fix: func(_ *testing.T, s *buildLadderState) {
			delete(s.args, "keyring")
		}},
		{name: "ansible.cfg server_timeout", want: helpers.ErrInvalidTimeout, fix: func(t *testing.T, s *buildLadderState) {
			t.Helper()
			writeBuildFile(t, s.args["ansible-config"], "[galaxy]\nserver_timeout = 45\n")
		}},
		{name: "S3 cache under --offline", want: helpers.ErrS3CacheOffline, fix: func(_ *testing.T, s *buildLadderState) {
			delete(s.args, "offline")
		}},
	}
}

// TestBuildCollectionConfigReportsBrokenSourcesInOrder breaks every source,
// then fixes one per run: each run reports the earliest broken source, and the
// last builds a Config crediting each layer.
func TestBuildCollectionConfigReportsBrokenSourcesInOrder(t *testing.T) {
	dir := isolateBuildEnv(t)
	writeBuildFile(t, filepath.Join(dir, "galaxy.toml"),
		"[project]\ncollections = []\n\n[tool.go-galaxy]\nlock_file = \"${GG_LADDER_LOCK}\"\n")
	t.Setenv("GO_GALAXY_GIT_CREDENTIALS", "a,,b")
	t.Setenv("GO_GALAXY_URL_CREDENTIALS", "a,,b")
	state := &buildLadderState{dir: dir, args: map[string]string{
		"requirements-file": filepath.Join(dir, "requirements.txt"),
		"timeout":           "soon",
		"ansible-config":    filepath.Join(dir, "ansible.cfg"),
		"server":            "not-a-url",
		"s3-bucket":         "ladder",
		"keyring":           "",
		"offline":           "true",
	}}

	for _, step := range buildLadderSteps() {
		cfg, err := buildInstallConfig(t, state.argv())
		if !errors.Is(err, step.want) {
			t.Fatalf("%s: BuildCollectionConfig() = (%v, %v), want errors.Is match with %v", step.name, cfg, err, step.want)
		}
		if cfg != nil {
			t.Fatalf("%s: BuildCollectionConfig() returned a Config beside its error", step.name)
		}
		step.fix(t, state)
	}

	cfg, err := buildInstallConfig(t, state.argv())
	if err != nil {
		t.Fatalf("BuildCollectionConfig() with every source fixed: %v", err)
	}
	assertLadderConfig(t, dir, cfg)
}

// assertLadderConfig pins what the fully fixed ladder resolves to: each value
// and the source credited for it.
func assertLadderConfig(t *testing.T, dir string, cfg *Config) {
	t.Helper()
	if cfg.Timeout != 45*time.Second || !cfg.AnsibleServerTimeoutUsed {
		t.Errorf("Timeout = %s (from ansible.cfg %t), want 45s from ansible.cfg", cfg.Timeout, cfg.AnsibleServerTimeoutUsed)
	}
	if want := filepath.Join(dir, "ansible.cfg"); cfg.AnsibleConfigPath != want {
		t.Errorf("AnsibleConfigPath = %q, want %q", cfg.AnsibleConfigPath, want)
	}
	if want := filepath.Join(dir, "locks", "pinned.lock"); cfg.LockFile != want {
		t.Errorf("LockFile = %q, want %q joined under the galaxy.toml directory", cfg.LockFile, want)
	}
	if !slices.Contains(cfg.ProjectSettingsUsed, "lock_file") {
		t.Errorf("ProjectSettingsUsed = %q, want lock_file credited", cfg.ProjectSettingsUsed)
	}
	assertLadderRemotes(t, cfg)
}

// assertLadderRemotes pins the remote half of the fixed ladder: the one
// --server origin, the S3 cache with its keys, and no credential binding.
func assertLadderRemotes(t *testing.T, cfg *Config) {
	t.Helper()
	if cfg.Server != "https://galaxy.example" || len(cfg.Servers) != 1 || cfg.Servers[0].URL != cfg.Server {
		t.Errorf("Server = %q, Servers = %+v, want the one --server origin", cfg.Server, cfg.Servers)
	}
	if !cfg.S3Cache.Enabled || cfg.S3Cache.Bucket != "ladder" || cfg.S3Cache.SecretKey.Reveal() != "ladder-secret" {
		t.Errorf("S3Cache = %+v, want the ladder bucket enabled with its keys", cfg.S3Cache)
	}
	if cfg.Offline || cfg.GitCredentials != nil || cfg.URLCredentials != nil {
		t.Errorf("Offline = %t, GitCredentials = %v, URLCredentials = %v, want none", cfg.Offline, cfg.GitCredentials, cfg.URLCredentials)
	}
}

// TestBuildCollectionConfigTimeoutSourceOutranksAnsibleCfg pins ansible's
// order: a set --timeout wins over [galaxy] server_timeout, which is then
// neither applied nor parsed, so a malformed one is no error.
func TestBuildCollectionConfigTimeoutSourceOutranksAnsibleCfg(t *testing.T) {
	dir := isolateBuildEnv(t)
	ansiblePath := filepath.Join(dir, "ansible.cfg")
	writeBuildFile(t, ansiblePath, "[galaxy]\nserver_timeout = never\n")
	reqPath := filepath.Join(dir, "requirements.yml")
	writeBuildFile(t, reqPath, "collections: []\n")

	cfg, err := buildInstallConfig(t, []string{"--ansible-config=" + ansiblePath, "-r", reqPath, "--timeout=7"})
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	if cfg.Timeout != 7*time.Second || cfg.AnsibleServerTimeoutUsed {
		t.Errorf("Timeout = %s (from ansible.cfg %t), want 7s from --timeout", cfg.Timeout, cfg.AnsibleServerTimeoutUsed)
	}

	_, err = buildInstallConfig(t, []string{"--ansible-config=" + ansiblePath, "-r", reqPath})
	if !errors.Is(err, helpers.ErrInvalidTimeout) || !strings.Contains(err.Error(), ansiblePath+": [galaxy] server_timeout") {
		t.Errorf("BuildCollectionConfig() without --timeout = %v, want ErrInvalidTimeout naming %s", err, ansiblePath)
	}
}

// TestBuildCollectionConfigQueuesDiscoveryWarningFirst pins that discovery's
// both-files warning is queued ahead of a later layer's warning, since picking
// the file is the run's first event.
func TestBuildCollectionConfigQueuesDiscoveryWarningFirst(t *testing.T) {
	dir := isolateBuildEnv(t)
	writeBuildFile(t, filepath.Join(dir, "galaxy.toml"), "[project]\ncollections = []\n\n[tool.go-galaxy]\nworkers = 100000\n")
	writeBuildFile(t, filepath.Join(dir, "requirements.yml"), "collections: []\n")
	ansiblePath := filepath.Join(dir, "ansible.cfg")
	writeBuildFile(t, ansiblePath, "[galaxy]\n")

	cfg, err := buildInstallConfig(t, []string{"--ansible-config=" + ansiblePath})
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	if cfg.RequirementsFile != helpers.RequirementsTOMLName {
		t.Errorf("RequirementsFile = %q, want %q", cfg.RequirementsFile, helpers.RequirementsTOMLName)
	}
	if len(cfg.Warnings) != 2 {
		t.Fatalf("Warnings = %q, want the discovery warning then the workers warning", cfg.Warnings)
	}
	if !strings.Contains(cfg.Warnings[0], "are both present in the current directory") {
		t.Errorf("Warnings[0] = %q, want the discovery warning", cfg.Warnings[0])
	}
	if !strings.Contains(cfg.Warnings[1], "[tool.go-galaxy] workers in galaxy.toml = 100000 is outside") {
		t.Errorf("Warnings[1] = %q, want the galaxy.toml workers warning", cfg.Warnings[1])
	}
	if slices.Contains(cfg.ProjectSettingsUsed, "workers") {
		t.Errorf("ProjectSettingsUsed = %q, want a refused workers value left out", cfg.ProjectSettingsUsed)
	}
}

// TestConfigPredicatesAreNilSafe pins that IsNoCache, IsRefresh and IsOffline
// report false on a nil Config and the field otherwise.
func TestConfigPredicatesAreNilSafe(t *testing.T) {
	t.Parallel()
	var nilCfg *Config
	if nilCfg.IsNoCache() || nilCfg.IsRefresh() || nilCfg.IsOffline() {
		t.Error("a nil Config reports a predicate as true")
	}
	if (&Config{}).IsNoCache() || (&Config{}).IsRefresh() || (&Config{}).IsOffline() {
		t.Error("a zero Config reports a predicate as true")
	}
	set := &Config{NoCache: true, Refresh: true, Offline: true}
	if !set.IsNoCache() || !set.IsRefresh() || !set.IsOffline() {
		t.Errorf("IsNoCache, IsRefresh, IsOffline = %t, %t, %t, want all true",
			set.IsNoCache(), set.IsRefresh(), set.IsOffline())
	}
}
