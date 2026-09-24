// Package cliflags declares the shared urfave/cli flag sets: names, defaults
// and environment sources. Turning a parsed command into configuration belongs
// to internal/galaxy/config, so nothing here reads a value back.
package cliflags

import (
	"os"
	"path/filepath"
	"runtime"

	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// CommonFlags defines shared CLI flags for all commands.
func CommonFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{
			Name:    "verbose",
			Usage:   "Verbose output",
			Sources: cli.EnvVars("GO_GALAXY_VERBOSE"),
		},
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "Suppress progress and log lines; results, warnings and errors still print. Ignored when --verbose is also set",
			Sources: cli.EnvVars("GO_GALAXY_QUIET"),
		},
		&cli.BoolFlag{
			Name:    "dry-run",
			Usage:   "Enable dry-run mode",
			Sources: cli.EnvVars("GO_GALAXY_DRY_RUN"),
		},
		&cli.StringFlag{
			Name:    "cache-dir",
			Usage:   "Local cache directory",
			Value:   defaultCacheDir(),
			Sources: cli.EnvVars("GO_GALAXY_CACHE_DIR", "ANSIBLE_GALAXY_CACHE_DIR"),
		},
	}
}

// CollectionFlags defines CLI flags for collection install behavior.
func CollectionFlags() []cli.Flag {
	flags := collectionPathFlags()
	flags = append(flags, collectionBehaviorFlags()...)
	flags = append(flags, lockfileAndMetricsFlags()...)
	return flags
}

func collectionPathFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			// ANSIBLE_GALAXY_SERVER is read in internal/galaxy/config: ansible
			// treats it as the [galaxy] server fallback, but as a source here
			// urfave's IsSet would report it like --server and outrank server_list.
			Name:    "server",
			Usage:   "Galaxy server URL",
			Value:   defaultServerURL,
			Sources: cli.EnvVars("GO_GALAXY_SERVER"),
		},
		&cli.StringFlag{
			Name: "token",
			Usage: "Galaxy API token for the configured server; only valid when a single server is in effect " +
				"(configure per-server tokens in [galaxy_server.<id>] when using server_list)",
			Sources: cli.EnvVars("GO_GALAXY_TOKEN"),
		},
		&cli.StringFlag{
			Name: "timeout",
			// Names the semantics: a no-progress budget read as a whole-transfer
			// cap gets set far too low for a large collection.
			Usage: "No-progress budget: wait for response headers, and gap between body reads. " +
				"Not a total-transfer cap. Seconds (e.g. 60) or Go duration (e.g. 90s, 1m30s)",
			Value: defaultTimeout.String(),
			// Source order is precedence (urfave takes the first set name), so
			// reordering is a behavior change. GO_GALAXY_TIMEOUT sits behind the
			// name that shipped first and ahead of the ANSIBLE_ spelling.
			Sources: cli.EnvVars("GO_GALAXY_SERVER_TIMEOUT", "GO_GALAXY_TIMEOUT", "ANSIBLE_GALAXY_SERVER_TIMEOUT"),
		},
		&cli.StringFlag{
			Name:    "download-path",
			Aliases: []string{"p"},
			Usage:   "Path to download collections to",
			Value:   defaultCollectionsPath,
			// Source order is precedence, for the reason given on the timeout
			// flag above.
			Sources: cli.EnvVars("GO_GALAXY_COLLECTIONS_PATH", "GO_GALAXY_DOWNLOAD_PATH", "ANSIBLE_COLLECTIONS_PATH"),
		},
		&cli.StringFlag{
			Name:  "roles-path",
			Usage: "Path to install roles to",
			Value: defaultRolesPath,
			// Source order is precedence, for the reason given on the timeout
			// flag above.
			Sources: cli.EnvVars("GO_GALAXY_ROLES_PATH", "ANSIBLE_ROLES_PATH"),
		},
		RequirementsFileFlag(),
		&cli.StringFlag{
			Name: "ansible-config",
			Usage: "Path to ansible.cfg file; if unset, discovered in ansible's order " +
				"($ANSIBLE_CONFIG, ./ansible.cfg, ~/.ansible.cfg, /etc/ansible/ansible.cfg)",
			Sources: cli.EnvVars("GO_GALAXY_ANSIBLE_CONFIG"),
		},
	}
}

func collectionBehaviorFlags() []cli.Flag {
	return []cli.Flag{
		&cli.IntFlag{
			Name: "workers",
			Usage: "Number of concurrent workers; accepted from 1 up to the CPU this process may use " +
				"(at least 2), and outside that range the derived default is used instead",
			// Value is load-bearing: urfave marks an exported empty variable as
			// set without parsing it, so applyWorkers range-checks this Value,
			// which is always inside 1..MaxAcceptedInstallWorkers.
			Value:   galaxyhelpers.DefaultInstallWorkers(runtime.GOMAXPROCS(0)),
			Sources: cli.EnvVars("GO_GALAXY_WORKERS"),
		},
		&cli.IntFlag{
			Name:    "download-workers",
			Usage:   "Number of concurrent artifact downloads and cache presence probes; these wait on the network, not the CPU",
			Value:   galaxyhelpers.DefaultDownloadWorkers(runtime.GOMAXPROCS(0)),
			Sources: cli.EnvVars("GO_GALAXY_DOWNLOAD_WORKERS"),
		},
		&cli.BoolFlag{
			Name:    "no-cache",
			Usage:   "Disable local caching",
			Sources: cli.EnvVars("GO_GALAXY_NO_CACHE"),
		},
		&cli.BoolFlag{
			Name: "refresh",
			Usage: "Re-resolve against the Galaxy servers instead of reusing cached metadata or the previous " +
				"resolution; a cached artifact and its own version-specific metadata are still reused even " +
				"if the server changed them - use --no-cache to force those too. Ignored with --offline, " +
				"and wherever resolution comes from the lockfile rather than the servers",
			Sources: cli.EnvVars("GO_GALAXY_REFRESH"),
		},
		&cli.BoolFlag{
			Name:    "clear-cache",
			Usage:   "Clear local cache before installing",
			Sources: cli.EnvVars("GO_GALAXY_CLEAR_CACHE"),
		},
		&cli.BoolFlag{
			Name:    "no-deps",
			Usage:   "Do not install dependencies",
			Sources: cli.EnvVars("GO_GALAXY_NO_DEPS"),
		},
		&cli.BoolFlag{
			Name:    "offline",
			Usage:   "Disallow any network access; only cached state may be used",
			Sources: cli.EnvVars("GO_GALAXY_OFFLINE"),
		},
	}
}

func lockfileAndMetricsFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "lock-file",
			Usage:   "Path to lockfile (default: galaxy.lock next to requirements file)",
			Sources: cli.EnvVars("GO_GALAXY_LOCK_FILE"),
		},
		&cli.BoolFlag{
			Name:    "frozen",
			Usage:   "Fail if lockfile is missing or does not match resolved requirements",
			Sources: cli.EnvVars("GO_GALAXY_FROZEN"),
		},
		&cli.StringFlag{
			Name:    "metrics-file",
			Usage:   "Write a JSON metrics report to this path on completion",
			Sources: cli.EnvVars("GO_GALAXY_METRICS_FILE"),
		},
	}
}

// LockInspectFlags defines the two flags shared by the read-only lockfile
// inspection commands (hash, tree, explain): where to find the requirements
// file and, optionally, an override lockfile path.
func LockInspectFlags() []cli.Flag {
	return []cli.Flag{
		RequirementsFileFlag(),
		&cli.StringFlag{
			Name:    "lock-file",
			Usage:   "Path to lockfile (default: galaxy.lock beside requirements file)",
			Sources: cli.EnvVars("GO_GALAXY_LOCK_FILE"),
		},
	}
}

// RequirementsFileFlag declares the one flag naming the requirements file,
// requirements.yml or galaxy.toml by extension; unset, config.RequirementsPath
// discovers galaxy.toml, then requirements.yml, which DefaultText states.
func RequirementsFileFlag() cli.Flag {
	return &cli.StringFlag{
		Name: "requirements-file",
		// --role-file is ansible-galaxy's own spelling of this flag for
		// the install command; one file names both collections and roles
		// there as it does here.
		Aliases: []string{"r", "role-file"},
		Usage: "Path to the requirements file: requirements.yml, or galaxy.toml by its .toml extension; " +
			"unset, ./galaxy.toml is read when present, else ./requirements.yml",
		DefaultText: "galaxy.toml if present, else requirements.yml",
		Sources:     cli.EnvVars("GO_GALAXY_REQUIREMENTS_FILE", envRequirementsFileAnsible),
	}
}

// SignatureFlags defines the signature verification flags, mounted only by
// commands that verify. With their variables they are the whole surface: a
// setting that can relax verification never comes from an ansible.cfg.
func SignatureFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "keyring",
			Usage:   "Path to the OpenPGP keyring collection signatures are verified against; unset means no verification",
			Sources: cli.EnvVars("GO_GALAXY_KEYRING", "ANSIBLE_GALAXY_GPG_KEYRING"),
		},
		&cli.StringFlag{
			Name: "required-valid-signature-count",
			Usage: "How many signatures must verify: a non-negative count or 'all', optionally prefixed with '+' " +
				"to also require that at least one signature verified",
			Value: galaxyhelpers.DefaultRequiredValidSignatureCount,
			Sources: cli.EnvVars(
				"GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT",
				"ANSIBLE_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT",
			),
		},
		&cli.StringSliceFlag{
			Name:  "ignore-signature-status-code",
			Usage: "Signature failure status code to tolerate (repeatable), e.g. BADSIG or NO_PUBKEY",
			// The go-galaxy name is singular because it derives from this flag's
			// name, and the ansible one is plural because it is ansible's own.
			Sources: cli.EnvVars(
				"GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE",
				"ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES",
			),
		},
		&cli.BoolFlag{
			// resolveDisableGPGVerify reads ANSIBLE_GALAXY_DISABLE_GPG_VERIFY where
			// this flag is mounted, not as a source: urfave's bool source aborts
			// the command on the yes/no and on/off spellings ansible accepts.
			Name:    "disable-gpg-verify",
			Usage:   "Skip signature verification even when a keyring is configured",
			Sources: cli.EnvVars("GO_GALAXY_DISABLE_GPG_VERIFY"),
		},
	}
}

// S3Flags defines CLI flags for S3 cache configuration.
func S3Flags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "s3-bucket",
			Usage:   "S3 bucket name for caching, if defined enables S3 caching instead of local cache-dir",
			Sources: cli.EnvVars("GO_GALAXY_S3_BUCKET"),
		},
		&cli.StringFlag{
			Name:    "s3-region",
			Usage:   "S3 region for caching",
			Sources: cli.EnvVars("GO_GALAXY_S3_REGION"),
		},
		&cli.StringFlag{
			Name:    "s3-prefix",
			Usage:   "S3 prefix for caching",
			Sources: cli.EnvVars("GO_GALAXY_S3_PREFIX"),
		},
		&cli.StringFlag{
			Name:    "s3-access-key",
			Usage:   "S3 access key for caching",
			Sources: cli.EnvVars("GO_GALAXY_S3_ACCESS_KEY", "AWS_ACCESS_KEY_ID"),
		},
		&cli.StringFlag{
			Name:    "s3-secret-key",
			Usage:   "S3 secret key for caching",
			Sources: cli.EnvVars("GO_GALAXY_S3_SECRET_KEY", "AWS_SECRET_ACCESS_KEY"),
		},
		&cli.StringFlag{
			Name:    "s3-endpoint",
			Usage:   "S3 endpoint for caching",
			Sources: cli.EnvVars("GO_GALAXY_S3_ENDPOINT"),
		},
		&cli.StringFlag{
			Name:    "s3-session-token",
			Usage:   "S3 session token for caching",
			Sources: cli.EnvVars("GO_GALAXY_S3_SESSION_TOKEN", "AWS_SESSION_TOKEN"),
		},
		&cli.BoolFlag{
			Name: "s3-path-style-disabled",
			Usage: "Use virtual-hosted-style S3 addressing (<bucket>.<endpoint>/<key>) " +
				"instead of the default path style (<endpoint>/<bucket>/<key>)",
			Sources: cli.EnvVars("GO_GALAXY_S3_PATH_STYLE_DISABLED"),
		},
	}
}

// defaultCacheDir returns the default cache directory path.
func defaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(defaultHomeDir, dirSuffix)
	}
	return filepath.Join(home, dirSuffix)
}
