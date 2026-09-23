package config

import (
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// S3CacheConfig defines configuration for S3 cache backend. SecretKey and
// SessionToken are Secret so no rendering prints them; AccessKey stays a plain
// string on purpose, as it rides in cleartext in every signed request anyway.
type S3CacheConfig struct {
	SecretKey    Secret
	SessionToken Secret
	Endpoint     string
	Region       string
	Bucket       string
	Prefix       string
	AccessKey    string
	Enabled      bool
	PathStyle    bool
}

// loadS3CacheConfig builds S3 cache config from CLI flags.
func loadS3CacheConfig(c *cli.Command) (S3CacheConfig, error) {
	cfg := S3CacheConfig{
		Bucket:       c.String("s3-bucket"),
		Prefix:       c.String("s3-prefix"),
		Endpoint:     c.String("s3-endpoint"),
		Region:       c.String("s3-region"),
		AccessKey:    c.String("s3-access-key"),
		SecretKey:    NewSecret(c.String("s3-secret-key")),
		SessionToken: NewSecret(c.String("s3-session-token")),
	}

	if cfg.Bucket == "" {
		return cfg, nil
	}
	cfg.Enabled = true

	if cfg.AccessKey == "" || !cfg.SecretKey.IsSet() {
		return cfg, helpers.ErrS3EmptyCreds
	}

	cfg.PathStyle = !c.Bool("s3-path-style-disabled")

	return cfg, nil
}

// checkS3CacheOffline refuses an enabled S3 cache under --offline: its bucket
// is reached only over the network, so the backend could never open.
func checkS3CacheOffline(cfg *Config) error {
	if cfg.Offline && cfg.S3Cache.Enabled {
		return helpers.ErrS3CacheOffline
	}
	return nil
}
