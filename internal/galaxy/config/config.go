// Package config resolves one *Config per run from flags, the environment and
// ansible.cfg; BuildCollectionConfig alone decides precedence. It makes no
// network request, and every credential it yields is a redacting Secret.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// Config holds runtime settings for collection operations.
type Config struct {
	AnsibleConfigPath string
	RequirementsFile  string
	LockFile          string
	MetricsFile       string
	CacheDir          string
	DownloadPath      string
	// RolesPath is the directory roles install into, one role per child
	// directory named for the role - ansible's roles_path, first entry.
	RolesPath string
	// Server is the first effective server's URL, always Servers[0].URL once
	// resolveServers has run; anything dispatching credentials or TLS per
	// origin must read Servers instead.
	Server string
	// Warnings queues non-fatal configuration warnings, drained through
	// Infra.WarnConfig once a printer exists.
	Warnings []string
	// RoleWarnings queues the roles_path warnings, drained through
	// Infra.WarnRoleConfig only by a run whose requirements carry roles.
	RoleWarnings []string
	// AnsibleSignatureKeys names the signature keys the discovered ansible.cfg
	// carried, never their values, for AnsibleSignatureKeysWarning.
	AnsibleSignatureKeys []string
	// Servers is the resolved, non-empty server list in resolveServers' order;
	// with no server_list it holds one entry with ID "" and no token.
	Servers []Server
	// GitCredentials holds the GO_GALAXY_GIT_* host-bound credentials in list
	// order, already validated by loadGitCredentials; nil when none is declared.
	GitCredentials []GitCredential
	// URLCredentials holds the GO_GALAXY_URL_* origin-bound credentials in list
	// order, already validated by loadURLCredentials; nil when none is declared.
	URLCredentials []URLCredential
	S3Cache        S3CacheConfig
	// Signature is this run's resolved signature verification surface: the
	// keyring location, how many signatures must verify, which failure
	// statuses are tolerated, and whether verification is switched off.
	Signature SignatureConfig
	Timeout   time.Duration
	Workers   int
	// DownloadWorkers bounds the network-only download and cache-probe pool,
	// sized apart from Workers (see helpers.DefaultDownloadWorkers).
	DownloadWorkers            int
	Refresh                    bool
	NoCache                    bool
	NoDeps                     bool
	DryRun                     bool
	Verbose                    bool
	Quiet                      bool
	ClearCache                 bool
	Offline                    bool
	Frozen                     bool
	AnsibleCollectionsPathUsed bool
	AnsibleRolesPathUsed       bool
	AnsibleCacheDirUsed        bool
	AnsibleServerUsed          bool
	// AnsibleServerTimeoutUsed reports that Timeout came from ansible.cfg's
	// [galaxy] server_timeout, so debug output can credit the file for it.
	AnsibleServerTimeoutUsed bool
	// AnsibleServerEnvUsed reports that the ansible-side server came from
	// ANSIBLE_GALAXY_SERVER, so debug output does not credit the file.
	AnsibleServerEnvUsed bool
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

// IsOffline reports whether network access is forbidden.
func (c *Config) IsOffline() bool {
	if c == nil {
		return false
	}
	return c.Offline
}

// BuildCollectionConfig builds Config from flags, environment and ansible.cfg.
// An unregistered flag silently reads as its zero value, so a command must
// register every flag whose Config field it reads.
func BuildCollectionConfig(c *cli.Command) (*Config, error) {
	cfg := newConfigFromCLI(c)
	if err := applyTimeout(cfg, c); err != nil {
		return nil, err
	}
	applyWorkers(cfg, c, runtime.GOMAXPROCS(0))

	ansibleConfig, ansiblePath, ansibleWarnings, err := loadAnsibleConfigFromCLI(c)
	if err != nil {
		return nil, err
	}
	// Appended before applyAnsibleConfig runs, so the warning order follows
	// the order the events happened in: what discovery declined to read comes
	// ahead of what applying the file it did read had to say.
	cfg.Warnings = append(cfg.Warnings, ansibleWarnings...)
	applyAnsibleConfig(cfg, c, ansibleConfig, ansiblePath)

	if err := resolveServers(cfg, c, ansibleConfig); err != nil {
		return nil, err
	}

	// Git, then url credentials, after servers and before the S3 cache, so the
	// failure a configuration broken in several places reports first is stable.
	if err := loadGitCredentials(cfg); err != nil {
		return nil, err
	}
	if err := loadURLCredentials(cfg); err != nil {
		return nil, err
	}

	s3Cfg, err := loadS3CacheConfig(c)
	if err != nil {
		return nil, err
	}
	cfg.S3Cache = s3Cfg

	// Late, deliberately: every config error above keeps the precedence it
	// already had, so which failure a broken configuration reports first does
	// not change because a signature surface was added behind it.
	if err := applySignatureConfig(cfg, c); err != nil {
		return nil, err
	}

	// After the signature surface, for the same reason: a malformed
	// server_timeout became an error only once the file's value was read at
	// all, so it takes its place behind every failure that already had one.
	if err := applyAnsibleTimeout(cfg, c, ansibleConfig.Galaxy.ServerTimeout, ansiblePath); err != nil {
		return nil, err
	}

	// Last, so every failure above keeps its precedence. cleanup mounts no
	// --offline, so it reads false there and cleanup is never refused.
	if err := checkS3CacheOffline(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func newConfigFromCLI(c *cli.Command) *Config {
	cfg := &Config{
		DownloadWorkers: c.Int("download-workers"),
		LockFile:        c.String("lock-file"),
		MetricsFile:     c.String("metrics-file"),
		ClearCache:      c.Bool("clear-cache"),
		NoCache:         c.Bool("no-cache"),
		Refresh:         c.Bool("refresh"),
		NoDeps:          c.Bool("no-deps"),
		DryRun:          c.Bool("dry-run"),
		Offline:         c.Bool("offline"),
		Frozen:          c.Bool("frozen"),
		DownloadPath:    c.String("download-path"),
		RolesPath:       c.String("roles-path"),
	}
	// Discovery may decline ./galaxy.toml with a warning; queued first, ahead
	// of every later warning, because picking the file is the run's first event.
	requirementsPath, requirementsWarning := RequirementsPath(c)
	cfg.RequirementsFile = requirementsPath
	if requirementsWarning != "" {
		cfg.Warnings = append(cfg.Warnings, requirementsWarning)
	}

	// An unregistered or non-positive value becomes the default silently and
	// with no ceiling: unlike --workers, this pool waits on the network rather
	// than competing for CPU.
	if cfg.DownloadWorkers < 1 {
		cfg.DownloadWorkers = helpers.DefaultDownloadWorkers(runtime.GOMAXPROCS(0))
	}
	cfg.Verbose = c.Bool("verbose")
	cfg.Quiet = !cfg.Verbose && c.Bool("quiet")
	return cfg
}

// applyTimeout parses --timeout before ansible.cfg is loaded, so a bad flag is
// reported first; an unregistered flag reads "", which parseTimeout maps to
// the default, never to an unbounded client.
func applyTimeout(cfg *Config, c *cli.Command) error {
	timeout, err := parseTimeout(c.String("timeout"))
	if err != nil {
		return err
	}
	cfg.Timeout = timeout
	return nil
}

// applyAnsibleTimeout applies [galaxy] server_timeout when no --timeout source
// is set, as ansible orders them, and fails naming the file on a bad value; a
// command without --timeout (cleanup) never parses it.
func applyAnsibleTimeout(cfg *Config, c *cli.Command, raw, ansiblePath string) error {
	if raw == "" || c.IsSet("timeout") || c.String("timeout") == "" {
		return nil
	}
	timeout, err := parseTimeout(raw)
	if err != nil {
		return fmt.Errorf("%s: [galaxy] server_timeout: %w", ansiblePath, err)
	}
	cfg.Timeout = timeout
	cfg.AnsibleServerTimeoutUsed = true
	return nil
}

// applyWorkers alone sets cfg.Workers: a supplied value outside
// 1..helpers.MaxAcceptedInstallWorkers(procs) becomes the default with one
// warning, while an unset or unregistered flag takes the default silently.
func applyWorkers(cfg *Config, c *cli.Command, procs int) {
	fallback := helpers.DefaultInstallWorkers(procs)
	n := c.Int("workers")
	if !c.IsSet("workers") {
		// The cleanup shape: an unregistered flag name reads 0, which is the
		// absence of a value rather than a value to warn about.
		if n < 1 {
			n = fallback
		}
		cfg.Workers = n
		return
	}

	upper := helpers.MaxAcceptedInstallWorkers(procs)
	if n < 1 || n > upper {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
			"--workers (or $GO_GALAXY_WORKERS) = %d is outside 1..%d, the range this machine "+
				"accepts (the ceiling is the CPU this process is permitted to use, at least %d); "+
				"using %d instead",
			n, upper, helpers.MinDefaultInstallWorkers, fallback))
		n = fallback
	}
	cfg.Workers = n
}

// parseTimeout accepts whole seconds (ansible's form) or a Go duration; ""
// yields the default, and a non-positive value is refused, since a zero
// client timeout would mean no timeout at all.
func parseTimeout(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return helpers.FetchDefaultTimeout, nil
	}

	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("%w: %q", helpers.ErrInvalidTimeout, raw)
		}
		return time.Duration(n) * time.Second, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", helpers.ErrInvalidTimeout, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%w: %q", helpers.ErrInvalidTimeout, raw)
	}
	return d, nil
}

// loadAnsibleConfigFromCLI loads an explicitly named ansible.cfg strictly, a
// missing file being an error, or else the first discovered candidate; only
// discovery produces warnings.
func loadAnsibleConfigFromCLI(c *cli.Command) (ansibleConfig, string, []string, error) {
	if c.IsSet("ansible-config") {
		path := c.String("ansible-config")
		cfg, loadedPath, err := loadAnsibleConfig(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ansibleConfig{}, "", nil, fmt.Errorf("%w: %s", helpers.ErrAnsibleConfigNotFound, path)
			}
			return ansibleConfig{}, "", nil, err
		}
		return cfg, loadedPath, nil, nil
	}

	path, warnings := discoverAnsibleConfigPath()
	if path == "" {
		return ansibleConfig{}, "", warnings, nil
	}
	cfg, loadedPath, err := loadAnsibleConfig(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The candidate existed during discovery but vanished before we
			// could open it (e.g. a concurrent process removed it); treat
			// this exactly like "no config found" rather than erroring.
			return ansibleConfig{}, "", warnings, nil
		}
		return ansibleConfig{}, "", warnings, err
	}
	return cfg, loadedPath, warnings, nil
}

// maxAnsibleConfigCandidates bounds the discovery candidate list: env var,
// cwd, home, and the system-wide path.
const maxAnsibleConfigCandidates = 4

// cwdAnsibleCfgName is the current-directory candidate, kept relative
// exactly as ansible's own discovery keeps it: it resolves against the
// process's working directory at open time rather than at discovery time.
const cwdAnsibleCfgName = "ansible.cfg"

// worldWritablePerm is the permission bit that makes a directory writable by
// any principal on the machine. A directory carrying it cannot be trusted to
// hold a configuration file this process obeys.
const worldWritablePerm = 0o002

// discoverAnsibleConfigPath returns the first existing candidate in ansible's
// search order, or "", plus discovery warnings; like ansible it skips
// ./ansible.cfg when the current directory is world-writable.
func discoverAnsibleConfigPath() (string, []string) {
	var warnings []string
	candidates := make([]string, 0, maxAnsibleConfigCandidates)
	if envPath := os.Getenv("ANSIBLE_CONFIG"); envPath != "" {
		candidates = append(candidates, envPath)
	}
	cwdPath, cwdWarning := cwdCandidate()
	if cwdPath != "" {
		candidates = append(candidates, cwdPath)
	}
	if cwdWarning != "" {
		warnings = append(warnings, cwdWarning)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".ansible.cfg"))
	}
	candidates = append(candidates, "/etc/ansible/ansible.cfg")

	for _, path := range candidates {
		if fileExists(path) {
			return path, warnings
		}
	}
	return "", warnings
}

// cwdCandidate returns ./ansible.cfg, or "" and a warning when the cwd is
// world-writable (sticky bit included, as in ansible), since anyone could plant
// the file there; a failed Getwd or Stat keeps the candidate.
func cwdCandidate() (string, string) {
	cwd, err := os.Getwd()
	if err != nil {
		return cwdAnsibleCfgName, ""
	}
	// #nosec G703 -- cwd is this process's own working directory; this is a
	// read-only mode check that never opens or writes anything.
	info, err := os.Stat(cwd)
	if err != nil {
		return cwdAnsibleCfgName, ""
	}
	if info.Mode().Perm()&worldWritablePerm == 0 {
		return cwdAnsibleCfgName, ""
	}
	return "", fmt.Sprintf(
		"the current directory %q is world-writable; dropping ./ansible.cfg from the discovery search path - "+
			"discovery continues with its remaining candidates, and a path named explicitly by $ANSIBLE_CONFIG, "+
			"--ansible-config or $GO_GALAXY_ANSIBLE_CONFIG is still read, even one resolving to that same file",
		cwd,
	)
}

// fileExists reports whether path can be stat'd. path is a fixed candidate or
// $ANSIBLE_CONFIG, never a flag value, and is only stat'd, never opened.
func fileExists(path string) bool {
	// #nosec G703 -- path is the user's own ansible.cfg location (a fixed
	// candidate or a value they set via env/flag); this is a read-only
	// existence check that never opens or writes the file.
	_, err := os.Stat(path)
	return err == nil
}

func applyAnsibleConfig(cfg *Config, c *cli.Command, ansibleConfig ansibleConfig, ansiblePath string) {
	if ansiblePath != "" {
		cfg.AnsibleConfigPath = ansiblePath
	}
	cfg.AnsibleSignatureKeys = ansibleConfig.Galaxy.SignatureKeys
	cfg.DownloadPath, cfg.AnsibleCollectionsPathUsed = pickConfigValue(c, "download-path", ansibleConfig.Defaults.CollectionsPath)
	cfg.RolesPath, cfg.AnsibleRolesPathUsed = pickConfigValue(c, "roles-path", ansibleConfig.Defaults.RolesPath)
	cfg.CacheDir, cfg.AnsibleCacheDirUsed = pickConfigValue(c, "cache-dir", ansibleConfig.Galaxy.CacheDir)
	serverValue, serverFromEnv := ansibleGalaxyServer(ansibleConfig.Galaxy.Server)
	cfg.Server, cfg.AnsibleServerUsed = pickConfigValue(c, "server", serverValue)
	cfg.AnsibleServerEnvUsed = cfg.AnsibleServerUsed && serverFromEnv

	// collections_path is a ":"-separated search list in ansible: the first
	// entry is kept, whichever source resolved it, and a dropped entry warns.
	var warning string
	cfg.DownloadPath, warning = firstSearchPathEntry("collections_path", cfg.DownloadPath)
	if warning != "" {
		cfg.Warnings = append(cfg.Warnings, warning)
	}
	// roles_path gets the same treatment, its warnings queued on RoleWarnings
	// so a run without roles never hears them.
	cfg.RolesPath, warning = firstSearchPathEntry("roles_path", cfg.RolesPath)
	if warning != "" {
		cfg.RoleWarnings = append(cfg.RoleWarnings, warning)
	}
	if cfg.RolesPath != "" && filepath.Clean(cfg.RolesPath) == filepath.Clean(cfg.DownloadPath) {
		cfg.RoleWarnings = append(cfg.RoleWarnings,
			fmt.Sprintf("roles_path and collections_path are the same directory %q; roles install beside ansible_collections", cfg.RolesPath))
	}
}

// firstSearchPathEntry keeps the first entry of a ":"-separated search path,
// returning a warning naming setting when others were dropped, for the caller
// to queue on Warnings or RoleWarnings.
func firstSearchPathEntry(setting, value string) (string, string) {
	first, rest := splitSearchPath(value)
	if len(rest) == 0 {
		return first, ""
	}
	return first, fmt.Sprintf("%s lists multiple paths; using %q and ignoring the rest: %v", setting, first, rest)
}

// splitSearchPath splits a POSIX ":"-separated path into its first entry and
// the remaining non-empty ones; Windows drive letters are not recognized.
func splitSearchPath(value string) (string, []string) {
	parts := strings.Split(value, ":")
	first := parts[0]

	var rest []string
	for _, p := range parts[1:] {
		if p != "" {
			rest = append(rest, p)
		}
	}
	return first, rest
}

// ansibleGalaxyServer returns ANSIBLE_GALAXY_SERVER when set at all, else the
// [galaxy] server key, and whether it came from the environment. It fills the
// file's slot in resolveServers' precedence, below server_list and --server.
func ansibleGalaxyServer(ini string) (string, bool) {
	if v, ok := os.LookupEnv("ANSIBLE_GALAXY_SERVER"); ok {
		return v, true
	}
	return ini, false
}

// pickConfigValue picks a string config value with precedence:
// explicit CLI/ENV (IsSet) > ansible.cfg > CLI default. The bool reports
// whether the value came from ansible.cfg.
func pickConfigValue(c *cli.Command, flag, ansibleValue string) (string, bool) {
	if c.IsSet(flag) {
		return c.String(flag), false
	}
	if ansibleValue != "" {
		return ansibleValue, true
	}
	return c.String(flag), false
}

// The ansible.cfg keys read here, their ANSIBLE_* environment spellings and
// the discovery order are tabulated in docs/configuration.md.

// loadAnsibleConfig loads and parses ansible.cfg. Absence stays a bare
// fs.ErrNotExist for the caller to judge; any other open, read or scan
// failure wraps helpers.ErrAnsibleConfigUnreadable.
func loadAnsibleConfig(configPath string) (ansibleConfig, string, error) {
	config := ansibleConfig{}

	f, err := os.Open(configPath) // #nosec G304 -- user-provided path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config, "", err
		}
		return config, "", fmt.Errorf("%w: %w", helpers.ErrAnsibleConfigUnreadable, err)
	}
	defer func() { _ = f.Close() }()

	config, err = parseAnsibleConfig(f)
	if err != nil {
		return config, "", fmt.Errorf("%w: %s: %w", helpers.ErrAnsibleConfigUnreadable, configPath, err)
	}
	return config, configPath, nil
}
