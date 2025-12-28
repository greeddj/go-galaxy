package infra

import (
	"net/http"
	"os"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
)

// Infra holds runtime dependencies such as IO and HTTP clients.
type Infra struct {
	Output  output.Printer
	HTTP    *http.Client
	Now     func() time.Time
	TempDir func() string
}

// New builds Infra with default helpers for time and temp paths.
func New(out output.Printer, httpClient *http.Client) *Infra {
	return &Infra{
		Output:  out,
		HTTP:    httpClient,
		Now:     time.Now,
		TempDir: os.TempDir,
	}
}

// DebugAnsibleConfig logs which settings were sourced from ansible.cfg.
func (i *Infra) DebugAnsibleConfig(cfg *config.Config) {
	if i == nil || i.Output == nil || cfg == nil || cfg.AnsibleConfigPath == "" {
		return
	}
	if cfg.AnsibleCollectionsPathUsed {
		i.Output.Debugf("ansible.cfg %s: defaults.collections_path=%s", cfg.AnsibleConfigPath, cfg.DownloadPath)
	}
	if cfg.AnsibleCacheDirUsed {
		i.Output.Debugf("ansible.cfg %s: galaxy.cache_dir=%s", cfg.AnsibleConfigPath, cfg.CacheDir)
	}
	if cfg.AnsibleServerUsed {
		i.Output.Debugf("ansible.cfg %s: galaxy.server=%s", cfg.AnsibleConfigPath, cfg.Server)
	}
}
