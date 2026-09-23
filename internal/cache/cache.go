// Package cache is the single factory for the cache backend: New returns the
// S3 backend when S3 caching is enabled and the local one otherwise, and no
// other package names a concrete backend.
package cache

import (
	"errors"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/cache/s3"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

var errHTTPClientNil = errors.New("http client is nil")

// New selects and constructs a cache backend based on configuration.
// A nil config is reported through helpers.ErrConfigIsNil so the failure
// classifies into the same exit code as every other nil-config refusal.
func New(cfg *config.Config, runtime *infra.Infra) (cacheManager.Backend, error) {
	if cfg == nil {
		return nil, helpers.ErrConfigIsNil
	}
	if cfg.S3Cache.Enabled {
		if runtime == nil || runtime.HTTP == nil {
			return nil, errHTTPClientNil
		}
		tempDir := ""
		if runtime.TempDir != nil {
			tempDir = runtime.TempDir()
		}
		return s3.New(cfg.S3Cache, runtime.HTTP, tempDir)
	}
	return local.New(cfg.CacheDir), nil
}
