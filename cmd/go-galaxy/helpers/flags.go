package helpers

import (
	"runtime"

	"github.com/urfave/cli/v2"
)

// CommonFlags defines shared CLI flags for all commands.
func CommonFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{
			Name:    "verbose",
			Usage:   "Verbose output",
			EnvVars: []string{"GO_GALAXY_VERBOSE"},
		},
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "Quiet mode, not working with verbose",
			EnvVars: []string{"GO_GALAXY_QUIET"},
		},
		&cli.BoolFlag{
			Name:  "dry-run",
			Usage: "Enable dry-run mode",
		},
		&cli.StringFlag{
			Name:    "cache-dir",
			Usage:   "Local cache directory",
			Value:   defaultCacheDir(),
			EnvVars: []string{"GO_GALAXY_CACHE_DIR", "ANSIBLE_GALAXY_CACHE_DIR"},
		},
	}
}

// CollectionFlags defines CLI flags for collection install behavior.
func CollectionFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "server",
			Usage:   "Galaxy server URL",
			Value:   defaultServerURL,
			EnvVars: []string{"GO_GALAXY_SERVER", "ANSIBLE_GALAXY_SERVER"},
		},
		&cli.DurationFlag{
			Name:    "timeout",
			Usage:   "Timeout duration",
			Value:   defaultTimeout,
			EnvVars: []string{"GO_GALAXY_SERVER_TIMEOUT", "ANSIBLE_GALAXY_SERVER_TIMEOUT"},
		},
		&cli.StringFlag{
			Name:    "download-path",
			Aliases: []string{"p"},
			Usage:   "Path to download collections to",
			Value:   defaultCollectionsPath,
			EnvVars: []string{"GO_GALAXY_COLLECTIONS_PATH", "ANSIBLE_COLLECTIONS_PATH"},
		},
		&cli.StringFlag{
			Name:    "requirements-file",
			Aliases: []string{"r"},
			Usage:   "Path to requirements.yml file",
			Value:   defaultRequirementsFilePath,
			EnvVars: []string{"GO_GALAXY_REQUIREMENTS_FILE", "ANSIBLE_GALAXY_REQUIREMENTS_FILE"},
		},
		&cli.StringFlag{
			Name:    "ansible-config",
			Usage:   "Path to ansible.cfg file",
			Value:   defaultAnsibleConfigPath,
			EnvVars: []string{"GO_GALAXY_ANSIBLE_CONFIG", "ANSIBLE_CONFIG"},
		},
		&cli.IntFlag{
			Name:    "workers",
			Usage:   "Number of concurrent workers",
			Value:   runtime.NumCPU(),
			EnvVars: []string{"GO_GALAXY_WORKERS"},
		},
		&cli.BoolFlag{
			Name:    "no-cache",
			Usage:   "Disable local caching",
			EnvVars: []string{"GO_GALAXY_NO_CACHE"},
		},
		&cli.BoolFlag{
			Name:    "refresh",
			Usage:   "Refresh all collections, ignoring cache",
			EnvVars: []string{"GO_GALAXY_REFRESH"},
		},
		&cli.BoolFlag{
			Name:    "clear-cache",
			Usage:   "Clear local cache before installing",
			EnvVars: []string{"GO_GALAXY_CLEAR_CACHE"},
		},
		&cli.BoolFlag{
			Name:    "no-deps",
			Usage:   "Do not install dependencies",
			EnvVars: []string{"GO_GALAXY_NO_DEPS"},
		},
	}
}

// S3Flags defines CLI flags for S3 cache configuration.
func S3Flags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "s3-bucket",
			Usage:   "S3 bucket name for caching, if defined enables S3 caching instead of local cache-dir",
			EnvVars: []string{"GO_GALAXY_S3_BUCKET"},
		},
		&cli.StringFlag{
			Name:    "s3-region",
			Usage:   "S3 region for caching",
			EnvVars: []string{"GO_GALAXY_S3_REGION"},
		},
		&cli.StringFlag{
			Name:    "s3-prefix",
			Usage:   "S3 prefix for caching",
			EnvVars: []string{"GO_GALAXY_S3_PREFIX"},
		},
		&cli.StringFlag{
			Name:    "s3-access-key",
			Usage:   "S3 access key for caching",
			EnvVars: []string{"GO_GALAXY_S3_ACCESS_KEY", "AWS_ACCESS_KEY_ID"},
		},
		&cli.StringFlag{
			Name:    "s3-secret-key",
			Usage:   "S3 secret key for caching",
			EnvVars: []string{"GO_GALAXY_S3_SECRET_KEY", "AWS_SECRET_ACCESS_KEY"},
		},
		&cli.StringFlag{
			Name:    "s3-endpoint",
			Usage:   "S3 endpoint for caching",
			EnvVars: []string{"GO_GALAXY_S3_ENDPOINT"},
		},
		&cli.StringFlag{
			Name:    "s3-session-token",
			Usage:   "S3 session token for caching",
			EnvVars: []string{"GO_GALAXY_S3_SESSION_TOKEN", "AWS_SESSION_TOKEN"},
		},
		&cli.BoolFlag{
			Name:    "s3-path-style-disabled",
			Usage:   "Path style addressing for S3",
			EnvVars: []string{"GO_GALAXY_S3_PATH_STYLE_DISABLED"},
		},
	}
}
