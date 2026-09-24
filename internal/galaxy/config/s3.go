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

// loadS3CacheConfig sets cfg.S3Cache: each key from its flag or variable when
// set, else from [tool.go-galaxy.s3]. A bucket from either source needs both
// keys from either source, and the two secrets are wrapped before anything else.
func loadS3CacheConfig(cfg *Config, c *cli.Command, project projectSettings) error {
	pick := projectPicker{c: c}
	s3 := S3CacheConfig{
		Bucket:       pick.value("s3-bucket", "s3.bucket", project.S3.Bucket),
		Prefix:       pick.value("s3-prefix", "s3.prefix", project.S3.Prefix),
		Endpoint:     pick.value("s3-endpoint", "s3.endpoint", project.S3.Endpoint),
		Region:       pick.value("s3-region", "s3.region", project.S3.Region),
		AccessKey:    pick.value("s3-access-key", "s3.access_key", project.S3.AccessKey),
		SecretKey:    NewSecret(pick.value("s3-secret-key", "s3.secret_key", project.S3.SecretKey)),
		SessionToken: NewSecret(pick.value("s3-session-token", "s3.session_token", project.S3.SessionToken)),
	}
	if s3.Bucket == "" {
		cfg.S3Cache = s3
		return nil
	}
	s3.Enabled = true

	if s3.AccessKey == "" || !s3.SecretKey.IsSet() {
		return helpers.ErrS3EmptyCreds
	}
	// Credited only for a cache that is on: a prefix or region the table
	// supplies for no bucket is read but decides nothing.
	cfg.ProjectSettingsUsed = append(cfg.ProjectSettingsUsed, pick.used...)

	pathStyleDisabled := c.Bool("s3-path-style-disabled")
	if !c.IsSet("s3-path-style-disabled") && project.S3.PathStyleDisabled {
		pathStyleDisabled = true
		cfg.useProjectSetting("s3.path_style_disabled")
	}
	s3.PathStyle = !pathStyleDisabled
	cfg.S3Cache = s3
	return nil
}

// checkS3CacheOffline refuses an enabled S3 cache under --offline: its bucket
// is reached only over the network, so the backend could never open.
func checkS3CacheOffline(cfg *Config) error {
	if cfg.Offline && cfg.S3Cache.Enabled {
		return helpers.ErrS3CacheOffline
	}
	return nil
}
