package commands

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// writeCWDAnsibleConfig neutralizes discovery like neutralizeAnsibleDiscovery,
// then writes a real ./ansible.cfg into a 0o755 directory it chdirs into, so
// discovery finds it through the same candidate a checked-out repository uses.
func writeCWDAnsibleConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	// #nosec G302 -- the permission is the fixture: cwdCandidate must accept
	// this directory as a discovery source, which requires it not be
	// world-writable, so the mode is pinned rather than left to t.TempDir.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod cwd: %v", err)
	}
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "absent.cfg"))
	t.Setenv("HOME", t.TempDir())
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "ansible.cfg"), []byte(body), 0o600); err != nil {
		t.Fatalf("write ansible.cfg: %v", err)
	}
}

// bareGalaxyServerAnsibleCfg is the simplest shape the token pairing rule
// refuses: a bare [galaxy] server line a repository's own ansible.cfg can
// commit, with no server_list or section id for an operator to override.
const bareGalaxyServerAnsibleCfg = "[galaxy]\nserver = https://corp.example\n"

// TestTokenDestinationEndToEnd pins, through real cwd discovery and full config
// resolution, that a file-sourced server with GO_GALAXY_TOKEN is refused and
// that the documented remedy delivers the token to that origin via serverAuths.
func TestTokenDestinationEndToEnd(t *testing.T) {
	t.Run("refused: a bare file server plus GO_GALAXY_TOKEN", func(t *testing.T) {
		writeCWDAnsibleConfig(t, bareGalaxyServerAnsibleCfg)
		t.Setenv("GO_GALAXY_TOKEN", "leaked-if-this-ever-resolves")

		_, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
		if !errors.Is(err, helpers.ErrTokenDestinationFromAnsibleConfig) {
			t.Fatalf("BuildCollectionConfig() error = %v, want helpers.ErrTokenDestinationFromAnsibleConfig", err)
		}
	})

	t.Run("accepted: the documented remedy resolves and the token reaches the operator's own origin", func(t *testing.T) {
		writeCWDAnsibleConfig(t, bareGalaxyServerAnsibleCfg)
		t.Setenv("GO_GALAXY_TOKEN", "s3cr3t-operator-token")
		// The documented remedy: the same address through ansible's own env
		// channel for [galaxy] server, so only who supplied it changes.
		t.Setenv("ANSIBLE_GALAXY_SERVER", "https://corp.example")

		cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if len(cfg.Servers) != 1 || cfg.Servers[0].URL != "https://corp.example" {
			t.Fatalf("Servers = %+v, want one server at https://corp.example", cfg.Servers)
		}

		parsed, err := url.Parse(cfg.Servers[0].URL)
		if err != nil {
			t.Fatalf("url.Parse(%q) error = %v, want nil", cfg.Servers[0].URL, err)
		}
		wantOrigin := helpers.Origin(parsed)

		auths := serverAuths(cfg.Servers)
		if len(auths) != 1 {
			t.Fatalf("len(serverAuths()) = %d, want 1", len(auths))
		}
		if auths[0].Origin != wantOrigin {
			t.Errorf("serverAuths()[0].Origin = %q, want %q", auths[0].Origin, wantOrigin)
		}
		if auths[0].Token != "s3cr3t-operator-token" {
			t.Errorf("serverAuths()[0].Token = %q, want %q", auths[0].Token, "s3cr3t-operator-token")
		}
	})
}

// tlsPolicyGalaxyServerAnsibleCfg is the shape the TLS-policy half of the
// pairing rule refuses: a server_list section setting only validate_certs = no,
// its url and token left to the operator's environment.
const tlsPolicyGalaxyServerAnsibleCfg = "[galaxy]\nserver_list = corp\n\n[galaxy_server.corp]\nvalidate_certs = no\n"

// TestTokenTLSPolicyEndToEnd pins that a file-sourced validate_certs = no with
// the operator's token is refused, and that the env remedy yields exactly that
// token-over-unverified-TLS fetch.ServerAuth as the positive control.
func TestTokenTLSPolicyEndToEnd(t *testing.T) {
	t.Run("refused: a file validate_certs=no with the operator's own url and token", func(t *testing.T) {
		writeCWDAnsibleConfig(t, tlsPolicyGalaxyServerAnsibleCfg)
		t.Setenv("ANSIBLE_GALAXY_SERVER_CORP_URL", "https://real-hub.example")
		t.Setenv("ANSIBLE_GALAXY_SERVER_CORP_TOKEN", "leaked-if-this-ever-resolves")

		_, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
		if !errors.Is(err, helpers.ErrTokenTLSPolicyFromAnsibleConfig) {
			t.Fatalf("BuildCollectionConfig() error = %v, want helpers.ErrTokenTLSPolicyFromAnsibleConfig", err)
		}
	})

	t.Run("accepted: the documented remedy resolves, and the token still reaches an unverified connection", func(t *testing.T) {
		writeCWDAnsibleConfig(t, tlsPolicyGalaxyServerAnsibleCfg)
		t.Setenv("ANSIBLE_GALAXY_SERVER_CORP_URL", "https://real-hub.example")
		t.Setenv("ANSIBLE_GALAXY_SERVER_CORP_TOKEN", "s3cr3t-operator-token")
		// The documented remedy: the same validate_certs value from the
		// operator's environment, so only who supplied it changes.
		t.Setenv("ANSIBLE_GALAXY_SERVER_CORP_VALIDATE_CERTS", "no")

		cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if len(cfg.Servers) != 1 || cfg.Servers[0].URL != "https://real-hub.example" {
			t.Fatalf("Servers = %+v, want one server at https://real-hub.example", cfg.Servers)
		}

		// The combination the refusal keeps from being file-chosen, checked on
		// the fetch.ServerAuth that reaches the wire rather than on cfg.Servers.
		auths := serverAuths(cfg.Servers)
		if len(auths) != 1 {
			t.Fatalf("len(serverAuths()) = %d, want 1", len(auths))
		}
		if auths[0].Token != "s3cr3t-operator-token" {
			t.Errorf("serverAuths()[0].Token = %q, want %q", auths[0].Token, "s3cr3t-operator-token")
		}
		if !auths[0].InsecureTLS {
			t.Errorf("serverAuths()[0].InsecureTLS = false, want true")
		}
	})
}
