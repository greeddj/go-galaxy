package cache

import (
	"errors"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/cache/s3"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

var (
	errConfigNil     = errors.New("config is nil")
	errHTTPClientNil = errors.New("http client is nil")
)

// New selects and constructs a cache backend based on configuration.
func New(cfg *config.Config, runtime *infra.Infra) (cacheManager.Backend, error) {
	if cfg == nil {
		return nil, errConfigNil
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
