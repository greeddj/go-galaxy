// Package infra holds Infra, the per-run container of runtime dependencies:
// printer, HTTP clients, git client, clock, metrics, and test-only deadline
// overrides read only through accessors that fall back to the helpers budgets.
package infra

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/metrics"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
)

// Infra holds runtime dependencies such as IO and HTTP clients.
type Infra struct {
	Output output.Printer
	Git    gitsource.Client
	HTTP   *http.Client
	// URLHTTP is the client url sources download over (fetch.NewURLDownload). A
	// nil one is a wiring defect the pipeline reports, never a fallback to HTTP,
	// which would re-attach Galaxy tokens to repository-authored URLs.
	URLHTTP                  *http.Client
	Now                      func() time.Time
	TempDir                  func() string
	Metrics                  *metrics.Counters
	GitCredentials           []gitsource.Credential
	ArtifactDownloadDeadline time.Duration
	MetadataFetchDeadline    time.Duration
	StateObjectDeadline      time.Duration
	SignatureFetchDeadline   time.Duration
	GitFetchDeadline         time.Duration
}

// New builds Infra with default helpers for time and temp paths.
func New(out output.Printer, httpClient *http.Client) *Infra {
	return &Infra{
		Output:                   out,
		HTTP:                     httpClient,
		Now:                      time.Now,
		TempDir:                  os.TempDir,
		Metrics:                  &metrics.Counters{},
		ArtifactDownloadDeadline: helpers.ArtifactDownloadDeadline,
		MetadataFetchDeadline:    helpers.MetadataFetchDeadline,
		StateObjectDeadline:      helpers.StateObjectDeadline,
		SignatureFetchDeadline:   helpers.SignatureFetchDeadline,
		GitFetchDeadline:         helpers.ArtifactDownloadDeadline,
	}
}

// GitDeadline returns the budget of one git acquisition, advertisement to last
// built artifact: GitFetchDeadline when positive, else
// helpers.ArtifactDownloadDeadline, since a pack is the artifact's wire form.
func (i *Infra) GitDeadline() time.Duration {
	if i == nil || i.GitFetchDeadline <= 0 {
		return helpers.ArtifactDownloadDeadline
	}
	return i.GitFetchDeadline
}

// ArtifactDeadline returns ArtifactDownloadDeadline when positive, else
// helpers.ArtifactDownloadDeadline, so a nil Infra, a zero value or a
// non-positive override falls back structurally rather than by convention.
func (i *Infra) ArtifactDeadline() time.Duration {
	if i == nil || i.ArtifactDownloadDeadline <= 0 {
		return helpers.ArtifactDownloadDeadline
	}
	return i.ArtifactDownloadDeadline
}

// MetadataDeadline returns the per-request Galaxy metadata fetch budget:
// MetadataFetchDeadline when positive, else helpers.MetadataFetchDeadline.
func (i *Infra) MetadataDeadline() time.Duration {
	if i == nil || i.MetadataFetchDeadline <= 0 {
		return helpers.MetadataFetchDeadline
	}
	return i.MetadataFetchDeadline
}

// StateDeadline returns the per-operation cache-state budget:
// StateObjectDeadline when positive, else helpers.StateObjectDeadline.
func (i *Infra) StateDeadline() time.Duration {
	if i == nil || i.StateObjectDeadline <= 0 {
		return helpers.StateObjectDeadline
	}
	return i.StateObjectDeadline
}

// SignatureDeadline returns the per-collection signature-phase budget:
// SignatureFetchDeadline when positive, else helpers.SignatureFetchDeadline.
func (i *Infra) SignatureDeadline() time.Duration {
	if i == nil || i.SignatureFetchDeadline <= 0 {
		return helpers.SignatureFetchDeadline
	}
	return i.SignatureFetchDeadline
}

// DebugConfigSources logs which settings came from ansible.cfg and which keys
// galaxy.toml supplied, then the resolved server list and credential bindings,
// whatever their source.
func (i *Infra) DebugConfigSources(cfg *config.Config) {
	if i == nil || i.Output == nil || cfg == nil {
		return
	}
	if len(cfg.ProjectSettingsUsed) > 0 {
		i.Output.Debugf("Galaxy.toml %s supplied: %s", cfg.RequirementsFile, strings.Join(cfg.ProjectSettingsUsed, ", "))
	}
	i.debugAnsibleSources(cfg)
	i.debugServerList(cfg.Servers)
	i.debugGitCredentials(cfg.GitCredentials)
	i.debugURLCredentials(cfg.URLCredentials)
}

// WarnConfig prints cfg.Warnings, queued while config was built before any
// printer existed. Role warnings wait for WarnRoleConfig, since no
// requirements file has been read yet and a run may have no roles.
func (i *Infra) WarnConfig(cfg *config.Config) {
	i.warn(cfg, func(c *config.Config) []string { return c.Warnings })
}

// WarnRoleConfig prints cfg.RoleWarnings (everything about roles_path). It is
// called only once a roles: block was found, so a run without roles never
// hears about a setting it does not read.
func (i *Infra) WarnRoleConfig(cfg *config.Config) {
	i.warn(cfg, func(c *config.Config) []string { return c.RoleWarnings })
}

// warn is the shared body of WarnConfig and WarnRoleConfig: the nil
// tolerance both need (a hand-built Infra in a test carries no printer) and
// the drain itself, with queue selecting which of cfg's two queues to print.
func (i *Infra) warn(cfg *config.Config, queue func(*config.Config) []string) {
	if i == nil || i.Output == nil || cfg == nil {
		return
	}
	for _, w := range queue(cfg) {
		i.Output.Warnf("%s", w)
	}
}

// debugAnsibleSources logs each value ansible.cfg supplied, then the one
// ANSIBLE_GALAXY_SERVER supplies whether or not a file was found at all,
// since crediting the file for it would name a source that did not provide it.
func (i *Infra) debugAnsibleSources(cfg *config.Config) {
	if cfg.AnsibleConfigPath != "" {
		i.debugAnsiblePaths(cfg)
		if cfg.AnsibleCacheDirUsed {
			i.Output.Debugf("Ansible.cfg %s: galaxy.cache_dir=%s", cfg.AnsibleConfigPath, cfg.CacheDir)
		}
		if cfg.AnsibleServerUsed && !cfg.AnsibleServerEnvUsed {
			i.Output.Debugf("Ansible.cfg %s: galaxy.server=%s", cfg.AnsibleConfigPath, cfg.Server)
		}
		if cfg.AnsibleServerTimeoutUsed {
			i.Output.Debugf("Ansible.cfg %s: galaxy.server_timeout=%s", cfg.AnsibleConfigPath, cfg.Timeout)
		}
	}
	if cfg.AnsibleServerEnvUsed {
		i.Output.Debugf("Env ANSIBLE_GALAXY_SERVER: galaxy.server=%s", cfg.Server)
	}
}

// debugServerList logs each resolved server's id, URL, TLS policy and token
// presence as a boolean; the token is never passed through Secret.Reveal, so
// raising verbosity cannot leak it.
func (i *Infra) debugServerList(servers []config.Server) {
	for _, s := range servers {
		i.Output.Debugf("Galaxy server %q: url=%s token=%t insecure_skip_tls_verify=%t",
			s.ID, s.URL, s.Token.IsSet(), s.InsecureSkipTLSVerify)
	}
}

// debugGitCredentials logs each git credential binding's id, URL prefix and
// kind; no password, key or passphrase is rendered, not even as presence.
func (i *Infra) debugGitCredentials(creds []config.GitCredential) {
	for _, c := range creds {
		kind := "basic"
		if c.Kind == config.GitCredentialSSHKey {
			kind = "ssh-key"
		}
		i.Output.Debugf("Git credential %q: url=%s kind=%s", c.ID, c.URL.String(), kind)
	}
}

// debugURLCredentials logs each url credential binding's id and URL prefix;
// the token is never rendered, not even as presence, since config refuses a
// binding without one.
func (i *Infra) debugURLCredentials(creds []config.URLCredential) {
	for _, c := range creds {
		i.Output.Debugf("Url credential %q: url=%s kind=bearer", c.ID, c.URL.String())
	}
}

// debugAnsiblePaths logs the two [defaults] install roots when ansible.cfg
// supplied them.
func (i *Infra) debugAnsiblePaths(cfg *config.Config) {
	if cfg.AnsibleCollectionsPathUsed {
		i.Output.Debugf("Ansible.cfg %s: defaults.collections_path=%s", cfg.AnsibleConfigPath, cfg.DownloadPath)
	}
	if cfg.AnsibleRolesPathUsed {
		i.Output.Debugf("Ansible.cfg %s: defaults.roles_path=%s", cfg.AnsibleConfigPath, cfg.RolesPath)
	}
}
