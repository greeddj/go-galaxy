package infra

import (
	"slices"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestWarnConfigDrainsEachQueueApart pins that WarnConfig prints Warnings
// alone and WarnRoleConfig RoleWarnings alone, each line verbatim, so a
// queued "%" is never read as a format verb.
func TestWarnConfigDrainsEachQueueApart(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Warnings:     []string{"first", "100%d done"},
		RoleWarnings: []string{"roles_path has 2 entries"},
	}
	printer := &recordingPrinter{}
	i := New(printer, nil)

	i.WarnConfig(cfg)
	if want := []string{"first", "100%d done"}; !slices.Equal(printer.warnLines, want) {
		t.Fatalf("after WarnConfig, warnings = %q, want %q", printer.warnLines, want)
	}
	i.WarnRoleConfig(cfg)
	if want := []string{"first", "100%d done", "roles_path has 2 entries"}; !slices.Equal(printer.warnLines, want) {
		t.Fatalf("after WarnRoleConfig, warnings = %q, want %q", printer.warnLines, want)
	}
	if len(printer.debugLines) != 0 {
		t.Errorf("debug lines = %q, want none from a warning drain", printer.debugLines)
	}
}

// TestWarnConfigNilSafe pins the nil guards a hand-built Infra relies on: a
// nil Infra, an Infra without a printer and a nil Config print nothing.
func TestWarnConfigNilSafe(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Warnings: []string{"w"}, RoleWarnings: []string{"r"}}
	var nilInfra *Infra
	nilInfra.WarnConfig(cfg)
	nilInfra.WarnRoleConfig(cfg)
	(&Infra{}).WarnConfig(cfg)
	(&Infra{}).WarnRoleConfig(cfg)

	printer := &recordingPrinter{}
	New(printer, nil).WarnConfig(nil)
	New(printer, nil).WarnRoleConfig(nil)
	if len(printer.warnLines) != 0 {
		t.Errorf("warnings = %q, want none for a nil Config", printer.warnLines)
	}
}

// TestDeadlineAccessorsFallBackAndHonorOverrides pins GitDeadline,
// MetadataDeadline and StateDeadline: a positive override verbatim, and the
// helpers constant for a nil Infra, a zero value or a negative override.
func TestDeadlineAccessorsFallBackAndHonorOverrides(t *testing.T) {
	t.Parallel()
	const override = 7 * time.Millisecond
	var nilInfra *Infra
	cases := []struct {
		get      func(*Infra) time.Duration
		set      func(*Infra, time.Duration)
		name     string
		fallback time.Duration
	}{
		{
			name: "GitDeadline", fallback: helpers.ArtifactDownloadDeadline,
			get: (*Infra).GitDeadline, set: func(i *Infra, d time.Duration) { i.GitFetchDeadline = d },
		},
		{
			name: "MetadataDeadline", fallback: helpers.MetadataFetchDeadline,
			get: (*Infra).MetadataDeadline, set: func(i *Infra, d time.Duration) { i.MetadataFetchDeadline = d },
		},
		{
			name: "StateDeadline", fallback: helpers.StateObjectDeadline,
			get: (*Infra).StateDeadline, set: func(i *Infra, d time.Duration) { i.StateObjectDeadline = d },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			negative, positive := &Infra{}, &Infra{}
			tc.set(negative, -1)
			tc.set(positive, override)
			for label, row := range map[string]struct {
				infra *Infra
				want  time.Duration
			}{
				"nil":      {infra: nilInfra, want: tc.fallback},
				"zero":     {infra: &Infra{}, want: tc.fallback},
				"New":      {infra: New(&recordingPrinter{}, nil), want: tc.fallback},
				"negative": {infra: negative, want: tc.fallback},
				"positive": {infra: positive, want: override},
			} {
				if got := tc.get(row.infra); got != row.want {
					t.Errorf("%s Infra: %s() = %s, want %s", label, tc.name, got, row.want)
				}
			}
		})
	}
}

// TestDebugConfigSourcesCreditsEveryAnsibleCfgValue pins the ansible.cfg lines
// in their order, paths before cache_dir before server_timeout, and that a
// value no ansible.cfg supplied is credited to none.
func TestDebugConfigSourcesCreditsEveryAnsibleCfgValue(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		AnsibleConfigPath:          "/etc/ansible/ansible.cfg",
		DownloadPath:               "/srv/collections",
		RolesPath:                  "/srv/roles",
		CacheDir:                   "/srv/cache",
		Timeout:                    45 * time.Second,
		AnsibleCollectionsPathUsed: true,
		AnsibleRolesPathUsed:       true,
		AnsibleCacheDirUsed:        true,
		AnsibleServerTimeoutUsed:   true,
	}
	printer := &recordingPrinter{}
	New(printer, nil).DebugConfigSources(cfg)
	want := []string{
		"Ansible.cfg /etc/ansible/ansible.cfg: defaults.collections_path=/srv/collections",
		"Ansible.cfg /etc/ansible/ansible.cfg: defaults.roles_path=/srv/roles",
		"Ansible.cfg /etc/ansible/ansible.cfg: galaxy.cache_dir=/srv/cache",
		"Ansible.cfg /etc/ansible/ansible.cfg: galaxy.server_timeout=45s",
	}
	if !slices.Equal(printer.debugLines, want) {
		t.Errorf("debug lines = %q, want %q", printer.debugLines, want)
	}

	cfg.AnsibleConfigPath = ""
	silent := &recordingPrinter{}
	New(silent, nil).DebugConfigSources(cfg)
	if len(silent.debugLines) != 0 {
		t.Errorf("debug lines without an ansible.cfg = %q, want none", silent.debugLines)
	}
}

// TestDebugConfigSourcesNamesGitCredentialsWithoutSecrets pins one line per
// git binding naming its id, prefix and kind, and that no password, key,
// passphrase or user name reaches the output.
func TestDebugConfigSourcesNamesGitCredentialsWithoutSecrets(t *testing.T) {
	t.Parallel()
	const (
		password   = "git-pa55word-must-not-print"
		keyPEM     = "-----BEGIN OPENSSH PRIVATE KEY-----must-not-print"
		passphrase = "git-passphrase-must-not-print"
		username   = "deploy-bot-must-not-print"
	)
	httpsPrefix := mustParseGitPrefix(t, "https://git.example/org")
	sshPrefix := mustParseGitPrefix(t, "ssh://git.example:2222/org")
	cfg := &config.Config{GitCredentials: []config.GitCredential{
		{ID: "web", URL: httpsPrefix, Kind: config.GitCredentialBasic, Username: username, Password: config.NewSecret(password)},
		{
			ID: "ops", URL: sshPrefix, Kind: config.GitCredentialSSHKey,
			SSHKeyPEM: config.NewSecret(keyPEM), SSHPassphrase: config.NewSecret(passphrase),
		},
	}}
	printer := &recordingPrinter{}
	New(printer, nil).DebugConfigSources(cfg)

	want := []string{
		`Git credential "web": url=https://git.example/org kind=basic`,
		`Git credential "ops": url=ssh://git.example:2222/org kind=ssh-key`,
	}
	if !slices.Equal(printer.debugLines, want) {
		t.Fatalf("debug lines = %q, want %q", printer.debugLines, want)
	}
	for _, secret := range []string{password, keyPEM, passphrase, username} {
		assertNoneContains(t, printer.debugLines, secret)
	}
}

// mustParseGitPrefix parses raw as a git credential binding prefix.
func mustParseGitPrefix(t *testing.T, raw string) gitsource.URL {
	t.Helper()
	u, err := gitsource.ParsePrefix(raw)
	if err != nil {
		t.Fatalf("gitsource.ParsePrefix(%q): %v", raw, err)
	}
	return u
}
