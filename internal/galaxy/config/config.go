package config

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v2"
)

// Config holds runtime settings for collection operations.
type Config struct {
	Verbose                    bool
	Quiet                      bool
	RequirementsFile           string
	CacheDir                   string
	DownloadPath               string
	Server                     string
	S3Cache                    S3CacheConfig
	ClearCache                 bool
	NoCache                    bool
	Refresh                    bool
	NoDeps                     bool
	DryRun                     bool
	Timeout                    time.Duration
	Workers                    int
	AnsibleConfigPath          string
	AnsibleCollectionsPathUsed bool
	AnsibleCacheDirUsed        bool
	AnsibleServerUsed          bool
}

// IsNoCache reports whether cache reads and writes are disabled.
func (c *Config) IsNoCache() bool {
	if c == nil {
		return false
	}
	return c.NoCache
}

// IsRefresh reports whether cache refresh is requested.
func (c *Config) IsRefresh() bool {
	if c == nil {
		return false
	}
	return c.Refresh
}

// CollectionOptions captures collection install options before normalization.
type CollectionOptions struct {
	Verbose             bool
	Quiet               bool
	RequirementsFile    string
	RequirementsFileSet bool
	DownloadPath        string
	DownloadPathSet     bool
	ClearCache          bool
	NoCache             bool
	Refresh             bool
	NoDeps              bool
	Timeout             time.Duration
	Server              string
	CacheDir            string
}

// BuildCollectionConfig builds Config from CLI flags and ansible.cfg.
func BuildCollectionConfig(c *cli.Context) (*Config, error) {
	cfg := newConfigFromCLI(c)
	applyTimeout(cfg, c)

	ansibleConfig, ansiblePath, err := loadAnsibleConfigFromCLI(c)
	if err != nil {
		return nil, err
	}
	applyAnsibleConfig(cfg, c, ansibleConfig, ansiblePath)

	s3Cfg, err := loadS3CacheConfig(c)
	if err != nil {
		return nil, err
	}
	cfg.S3Cache = s3Cfg

	return cfg, nil
}

func newConfigFromCLI(c *cli.Context) *Config {
	cfg := &Config{
		Workers:          c.Int("workers"),
		RequirementsFile: c.String("requirements-file"),
		ClearCache:       c.Bool("clear-cache"),
		NoCache:          c.Bool("no-cache"),
		Refresh:          c.Bool("refresh"),
		NoDeps:           c.Bool("no-deps"),
		DryRun:           c.Bool("dry-run"),
		DownloadPath:     c.String("download-path"),
	}

	if cfg.Workers < 1 {
		cfg.Workers = runtime.NumCPU()
	}
	cfg.Verbose = c.Bool("verbose")
	cfg.Quiet = !cfg.Verbose && c.Bool("quiet")
	return cfg
}

func applyTimeout(cfg *Config, c *cli.Context) {
	cfg.Timeout = c.Duration("timeout")
	cfg.Timeout = max(cfg.Timeout, helpers.FetchDefaultTimeout)
}

func loadAnsibleConfigFromCLI(c *cli.Context) (ansibleConfig, string, error) {
	ansibleConfig, ansiblePath, err := loadAnsibleConfig(c.String("ansible-config"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ansibleConfig, "", fmt.Errorf("failed to load ansible config: %w", err)
	}
	return ansibleConfig, ansiblePath, nil
}

func applyAnsibleConfig(cfg *Config, c *cli.Context, ansibleConfig ansibleConfig, ansiblePath string) {
	if ansiblePath != "" {
		cfg.AnsibleConfigPath = ansiblePath
	}
	if ansibleConfig.Defaults.CollectionsPath != "" {
		cfg.DownloadPath = ansibleConfig.Defaults.CollectionsPath
		cfg.AnsibleCollectionsPathUsed = true
	} else {
		cfg.DownloadPath = c.String("download-path")
	}
	if ansibleConfig.Galaxy.CacheDir != "" {
		cfg.CacheDir = ansibleConfig.Galaxy.CacheDir
		cfg.AnsibleCacheDirUsed = true
	} else {
		cfg.CacheDir = c.String("cache-dir")
	}
	if ansibleConfig.Galaxy.Server != "" {
		cfg.Server = ansibleConfig.Galaxy.Server
		cfg.AnsibleServerUsed = true
	} else {
		cfg.Server = c.String("server")
	}
}

/*
env: ANSIBLE_CONFIG (environment variable if set)
ansible.cfg (in the current directory)
~/.ansible.cfg (in the home directory)
/etc/ansible/ansible.cfg


[galaxy]
cache_dir // env:ANSIBLE_GALAXY_CACHE_DIR // default {{ ANSIBLE_HOME ~ "/galaxy_cache" }}
server // env:ANSIBLE_GALAXY_SERVER // default https://galaxy.ansible.com
*/

// ansibleGalaxyConfig maps the [galaxy] section from ansible.cfg.
type ansibleGalaxyConfig struct {
	CacheDir string `toml:"cache_dir"`
	Server   string `toml:"server"`
}

// ansibleDefaultsConfig maps the [defaults] section from ansible.cfg.
type ansibleDefaultsConfig struct {
	CollectionsPath string `toml:"collections_path"`
}

// ansibleConfig represents the parsed ansible.cfg structure.
type ansibleConfig struct {
	Defaults ansibleDefaultsConfig `toml:"defaults"`
	Galaxy   ansibleGalaxyConfig   `toml:"galaxy"`
}

// loadAnsibleConfig loads ansible.cfg if it exists.
func loadAnsibleConfig(configPath string) (ansibleConfig, string, error) {
	config := ansibleConfig{}
	if _, err := os.Stat(configPath); err != nil {
		return config, "", err
	}
	if _, err := toml.DecodeFile(configPath, &config); err != nil {
		return config, "", fmt.Errorf("failed parse ansible.cfg: %w", err)
	}
	return config, configPath, nil
}
