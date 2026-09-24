package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
	"go.yaml.in/yaml/v3"
)

// testServerFlagDefault stands in for the real --server default in
// newServerCmd; it only needs to differ from every value under test.
const testServerFlagDefault = "https://default.example"

// newServerCmd builds a command with the "server" and "token" flags as cliflags
// declares them. ANSIBLE_GALAXY_SERVER is deliberately not a source: as one it
// would outrank server_list, a precedence the pairing tables depend on.
func newServerCmd(t *testing.T, args []string) *cli.Command {
	t.Helper()

	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "server",
				Value:   testServerFlagDefault,
				Sources: cli.EnvVars("GO_GALAXY_SERVER"),
			},
			&cli.StringFlag{
				Name:    "token",
				Sources: cli.EnvVars("GO_GALAXY_TOKEN"),
			},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}

	fullArgs := append([]string{"go-galaxy"}, args...)
	if err := cmd.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// runResolveServers seeds cfg.Server and its ansible.cfg provenance bits as
// applyAnsibleConfig would, which resolveServers requires, then runs it.
func runResolveServers(t *testing.T, c *cli.Command, ansCfg ansibleConfig) (*Config, error) {
	t.Helper()
	cfg := &Config{}
	serverValue, serverFromEnv := ansibleGalaxyServer(ansCfg.Galaxy.Server)
	cfg.Server, cfg.AnsibleServerUsed = pickConfigValue(c, "server", serverValue)
	cfg.AnsibleServerEnvUsed = cfg.AnsibleServerUsed && serverFromEnv
	err := resolveServers(cfg, c, ansCfg, projectSettings{})
	return cfg, err
}

// TestResolveServersImplicit pins that with no server_list anywhere, Servers
// holds one anonymous entry built from the already-resolved cfg.Server.
func TestResolveServersImplicit(t *testing.T) {
	t.Parallel()

	c := newServerCmd(t, nil)
	cfg, err := runResolveServers(t, c, ansibleConfig{})
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}

	want := []Server{{URL: "https://default.example"}}
	if !reflect.DeepEqual(cfg.Servers, want) {
		t.Errorf("Servers = %+v, want %+v", cfg.Servers, want)
	}
	if cfg.Server != cfg.Servers[0].URL {
		t.Errorf("Server = %q, want it to equal Servers[0].URL = %q", cfg.Server, cfg.Servers[0].URL)
	}
}

// TestResolveServersUnregisteredServerFlag pins that a command not registering
// "server" (cleanup) resolves to one empty-URL server instead of failing its
// config build.
func TestResolveServersUnregisteredServerFlag(t *testing.T) {
	t.Parallel()

	// No "server" flag registered at all, mirroring cleanup's command tree.
	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}
	if err := cmd.Run(context.Background(), []string{"go-galaxy"}); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}

	cfg, err := runResolveServers(t, captured, ansibleConfig{})
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].URL != "" {
		t.Errorf("Servers = %+v, want a single entry with an empty URL", cfg.Servers)
	}
	if cfg.Server != "" {
		t.Errorf("Server = %q, want empty", cfg.Server)
	}
}

// TestResolveServersAnsibleServerFallback pins precedence rule 3: with no
// server_list and no explicit --server, [galaxy] server is the sole server.
func TestResolveServersAnsibleServerFallback(t *testing.T) {
	t.Parallel()

	c := newServerCmd(t, nil)
	ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://ansible.example"}}
	cfg, err := runResolveServers(t, c, ansCfg)
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}

	want := []Server{{URL: "https://ansible.example", sourceFile: cwdAnsibleCfgName, urlFromFile: true}}
	if !reflect.DeepEqual(cfg.Servers, want) {
		t.Errorf("Servers = %+v, want %+v", cfg.Servers, want)
	}
}

// singleServerList returns an ansibleConfig listing one section, "prod", plus a
// [galaxy] server distractor a broken precedence chain would pick instead.
func singleServerList(kv map[string]string) ansibleConfig {
	return ansibleConfig{
		Galaxy:        ansibleGalaxyConfig{Server: "https://distractor.example", ServerList: "prod"},
		GalaxyServers: map[string]map[string]string{"prod": kv},
	}
}

// TestResolveServersListBeatsAnsibleServer checks precedence rule 2: a
// non-empty server_list wins over [galaxy] server.
func TestResolveServersListBeatsAnsibleServer(t *testing.T) {
	t.Parallel()

	c := newServerCmd(t, nil)
	ansCfg := singleServerList(map[string]string{"url": "https://prod.example"})
	cfg, err := runResolveServers(t, c, ansCfg)
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}

	// tokenFromFile reads true even with no token configured: it is
	// envOrIni's provenance bit negated, and tokenPairingOffense consults it
	// only when a token is set.
	want := []Server{{ID: "prod", URL: "https://prod.example", sourceFile: cwdAnsibleCfgName, urlFromFile: true, tokenFromFile: true}}
	if !reflect.DeepEqual(cfg.Servers, want) {
		t.Errorf("Servers = %+v, want %+v", cfg.Servers, want)
	}
	if cfg.Server != "https://prod.example" {
		t.Errorf("Server = %q, want %q", cfg.Server, "https://prod.example")
	}
}

// TestResolveServersExplicitByID checks precedence rule 1's id-match branch:
// --server naming a configured id selects that one server, with its own
// token and TLS setting, even though the list has other entries.
func TestResolveServersExplicitByID(t *testing.T) {
	t.Parallel()

	ansCfg := ansibleConfig{
		Galaxy: ansibleGalaxyConfig{ServerList: "prod,staging"},
		GalaxyServers: map[string]map[string]string{
			"prod":    {"url": "https://prod.example", "token": "prod-token", "validate_certs": "false"},
			"staging": {"url": "https://staging.example"},
		},
	}
	c := newServerCmd(t, []string{"--server=prod"})
	cfg, err := runResolveServers(t, c, ansCfg)
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}

	if len(cfg.Servers) != 1 {
		t.Fatalf("len(Servers) = %d, want 1", len(cfg.Servers))
	}
	got := cfg.Servers[0]
	if got.ID != "prod" || got.URL != "https://prod.example" {
		t.Errorf("Servers[0] = {ID: %q, URL: %q}, want {ID: %q, URL: %q}", got.ID, got.URL, "prod", "https://prod.example")
	}
	if !got.Token.IsSet() || got.Token.Reveal() != "prod-token" {
		t.Errorf("Servers[0].Token.Reveal() = %q, want %q", got.Token.Reveal(), "prod-token")
	}
	if !got.InsecureSkipTLSVerify {
		t.Error("Servers[0].InsecureSkipTLSVerify = false, want true (validate_certs = false)")
	}
}

// TestResolveServersExplicitURLIgnoresList pins precedence rule 1's URL branch:
// a --server naming no configured id is used verbatim, and server_list is never
// consulted, not even a section that would fail validation.
func TestResolveServersExplicitURLIgnoresList(t *testing.T) {
	t.Parallel()

	ansCfg := ansibleConfig{
		Galaxy:        ansibleGalaxyConfig{ServerList: "prod"},
		GalaxyServers: map[string]map[string]string{"prod": {"username": "not-supported"}}, // would hard-error if built
	}
	c := newServerCmd(t, []string{"--server=https://anon.example"})
	cfg, err := runResolveServers(t, c, ansCfg)
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil (list must be ignored entirely)", err)
	}

	want := []Server{{URL: "https://anon.example"}}
	if !reflect.DeepEqual(cfg.Servers, want) {
		t.Errorf("Servers = %+v, want %+v", cfg.Servers, want)
	}
}

// TestResolveServerListEnvBeatsIni pins that ANSIBLE_GALAXY_SERVER_LIST, once
// set at all, wins over [galaxy] server_list, a blank value meaning no list.
func TestResolveServerListEnvBeatsIni(t *testing.T) {
	t.Run("env overrides ini", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_LIST", "from-env")
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{ServerList: "from-ini"}}
		got := resolveServerList(ansCfg, nil)
		want := []string{"from-env"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("resolveServerList() = %v, want %v", got, want)
		}
	})

	t.Run("env set to whitespace means unset, not fall back to ini", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_LIST", "   ")
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{ServerList: "from-ini"}}
		got := resolveServerList(ansCfg, nil)
		if got != nil {
			t.Errorf("resolveServerList() = %v, want nil", got)
		}
	})
}

// TestBuildServerEnvBeatsIniPerKey pins that each of url, token and
// validate_certs is overridden by its own ANSIBLE_GALAXY_SERVER_<ID>_<KEY>.
func TestBuildServerEnvBeatsIniPerKey(t *testing.T) {
	kv := map[string]string{"url": "https://ini.example", "token": "ini-token", "validate_certs": "true"}

	t.Run("url", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_PROD_URL", "https://env.example")
		server, _, err := buildServer("prod", kv)
		if err != nil {
			t.Fatalf("buildServer() error = %v, want nil", err)
		}
		if server.URL != "https://env.example" {
			t.Errorf("URL = %q, want %q", server.URL, "https://env.example")
		}
	})

	t.Run("token", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_PROD_TOKEN", "env-token")
		server, _, err := buildServer("prod", kv)
		if err != nil {
			t.Fatalf("buildServer() error = %v, want nil", err)
		}
		if server.Token.Reveal() != "env-token" {
			t.Errorf("Token.Reveal() = %q, want %q", server.Token.Reveal(), "env-token")
		}
	})

	t.Run("validate_certs", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_PROD_VALIDATE_CERTS", "false")
		server, _, err := buildServer("prod", kv)
		if err != nil {
			t.Fatalf("buildServer() error = %v, want nil", err)
		}
		if !server.InsecureSkipTLSVerify {
			t.Error("InsecureSkipTLSVerify = false, want true (env overrides ini's true with false)")
		}
	})

	t.Run("a dash in the id is not translated", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_MY-HUB_URL", "https://env.example")
		server, _, err := buildServer("my-hub", map[string]string{"url": "https://ini.example"})
		if err != nil {
			t.Fatalf("buildServer() error = %v, want nil", err)
		}
		if server.URL != "https://env.example" {
			t.Errorf("URL = %q, want %q", server.URL, "https://env.example")
		}
	})
}

// TestBuildServerEnvIDIsUpperCased pins ansible's rule that <ID> in
// ANSIBLE_GALAXY_SERVER_<ID>_<KEY> is the id upper-cased. Only the negative
// subtest tells upper-casing apart from no translation at all.
func TestBuildServerEnvIDIsUpperCased(t *testing.T) {
	const (
		fromEnv = "https://env.example"
		fromIni = "https://ini.example"
	)
	ini := map[string]string{"url": fromIni}

	t.Run("the upper-cased name is read", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_MYHUB_URL", fromEnv)
		server, _, err := buildServer("myHub", ini)
		if err != nil {
			t.Fatalf("buildServer() error = %v, want nil", err)
		}
		if server.URL != fromEnv {
			t.Errorf("URL = %q, want %q", server.URL, fromEnv)
		}
	})

	t.Run("the as-written name is not read", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_myHub_URL", fromEnv)
		server, _, err := buildServer("myHub", ini)
		if err != nil {
			t.Fatalf("buildServer() error = %v, want nil", err)
		}
		if server.URL != fromIni {
			t.Errorf("URL = %q, want %q: the id as written must not name the variable", server.URL, fromIni)
		}
	})
}

// TestBuildServerMissingURL checks that a listed server with no url from
// either ini or env is a hard error.
func TestBuildServerMissingURL(t *testing.T) {
	t.Parallel()
	_, _, err := buildServer("prod", map[string]string{"token": "x"})
	if !errors.Is(err, helpers.ErrMissingGalaxyServerURL) {
		t.Errorf("error = %v, want helpers.ErrMissingGalaxyServerURL", err)
	}
}

// TestBuildServerHardErrorKeys checks that each of the four keys implying
// Basic auth or Keycloak/SSO token exchange is a hard error, naming the key.
func TestBuildServerHardErrorKeys(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"username", "password", "auth_url", "client_id"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			kv := map[string]string{"url": "https://x", key: "value"}
			_, _, err := buildServer("prod", kv)
			if !errors.Is(err, helpers.ErrUnsupportedGalaxyServerKey) {
				t.Errorf("error = %v, want helpers.ErrUnsupportedGalaxyServerKey", err)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error = %v, want it to name the key %q", err, key)
			}
		})
	}
}

// TestBuildServerNamesTheFirstHardErrorKeyInSortedOrder pins that of two
// hard-error keys in one section the error names the first in sorted order, not
// whichever key map iteration happened to yield.
func TestBuildServerNamesTheFirstHardErrorKeyInSortedOrder(t *testing.T) {
	t.Parallel()

	t.Run("sorted order decides which of two hard-error keys is named", func(t *testing.T) {
		t.Parallel()
		kv := map[string]string{"url": "https://x", "password": "p", "auth_url": "https://a"}
		// Map iteration order is randomized per range, so repeating the call
		// turns an unsorted collection into a near-certain failure.
		for range 64 {
			_, _, err := buildServer("prod", kv)
			if !errors.Is(err, helpers.ErrUnsupportedGalaxyServerKey) {
				t.Fatalf("error = %v, want helpers.ErrUnsupportedGalaxyServerKey", err)
			}
			if !strings.Contains(err.Error(), "auth_url") {
				t.Fatalf("error = %v, want it to name the first hard-error key in sorted order (%q)", err, "auth_url")
			}
		}
	})

	t.Run("positive control: the sole hard-error key is named", func(t *testing.T) {
		t.Parallel()
		kv := map[string]string{"url": "https://x", "password": "p"}
		_, _, err := buildServer("prod", kv)
		if !errors.Is(err, helpers.ErrUnsupportedGalaxyServerKey) {
			t.Fatalf("error = %v, want helpers.ErrUnsupportedGalaxyServerKey", err)
		}
		if !strings.Contains(err.Error(), "password") {
			t.Fatalf("error = %v, want it to name %q", err, "password")
		}
	})
}

// TestBuildServerAPIVersion checks that api_version = "v3" is accepted as a
// no-op and any other value is a hard error.
func TestBuildServerAPIVersion(t *testing.T) {
	t.Parallel()

	t.Run("v3 accepted", func(t *testing.T) {
		t.Parallel()
		_, _, err := buildServer("prod", map[string]string{"url": "https://x", "api_version": "v3"})
		if err != nil {
			t.Errorf("buildServer() error = %v, want nil", err)
		}
	})

	t.Run("other value errors", func(t *testing.T) {
		t.Parallel()
		_, _, err := buildServer("prod", map[string]string{"url": "https://x", "api_version": "v2"})
		if !errors.Is(err, helpers.ErrUnsupportedGalaxyServerAPIVersion) {
			t.Errorf("error = %v, want helpers.ErrUnsupportedGalaxyServerAPIVersion", err)
		}
	})
}

// TestBuildServerUnknownKeyWarns checks that an unrecognized key warns
// (naming the key and the section) rather than failing, and does not
// prevent the server from being built.
func TestBuildServerUnknownKeyWarns(t *testing.T) {
	t.Parallel()

	server, warnings, err := buildServer("prod", map[string]string{"url": "https://x", "some_future_key": "y"})
	if err != nil {
		t.Fatalf("buildServer() error = %v, want nil", err)
	}
	if server.URL != "https://x" {
		t.Errorf("URL = %q, want %q", server.URL, "https://x")
	}
	if len(warnings) != 1 {
		t.Fatalf("len(warnings) = %d, want 1 (warnings = %v)", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "some_future_key") || !strings.Contains(warnings[0], "prod") {
		t.Errorf("warnings[0] = %q, want it to mention both the key and the server id", warnings[0])
	}
}

// TestBuildServerValidateCerts checks every ansible boolean spelling for
// validate_certs, case-insensitively, and that an unparseable value is
// always a hard error rather than a silent false.
func TestBuildServerValidateCerts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		raw          string
		wantInsecure bool
		wantErr      bool
	}{
		{name: "true", raw: "true", wantInsecure: false},
		{name: "True mixed case", raw: "True", wantInsecure: false},
		{name: "false", raw: "false", wantInsecure: true},
		{name: "False mixed case", raw: "False", wantInsecure: true},
		{name: "yes", raw: "yes", wantInsecure: false},
		{name: "no", raw: "no", wantInsecure: true},
		{name: "on", raw: "on", wantInsecure: false},
		{name: "off", raw: "off", wantInsecure: true},
		{name: "1", raw: "1", wantInsecure: false},
		{name: "0", raw: "0", wantInsecure: true},
		{name: "unset defaults to certs verified", raw: "", wantInsecure: false},
		{name: "unparseable is an error", raw: "maybe", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kv := map[string]string{"url": "https://x"}
			if tt.raw != "" {
				kv["validate_certs"] = tt.raw
			}
			server, _, err := buildServer("prod", kv)
			if tt.wantErr {
				if !errors.Is(err, helpers.ErrInvalidValidateCerts) {
					t.Errorf("error = %v, want helpers.ErrInvalidValidateCerts", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildServer() error = %v, want nil", err)
			}
			if server.InsecureSkipTLSVerify != tt.wantInsecure {
				t.Errorf("InsecureSkipTLSVerify = %v, want %v", server.InsecureSkipTLSVerify, tt.wantInsecure)
			}
		})
	}
}

// TestValidateServerIDs checks the id charset, plain duplicate, and
// case-collision invariants.
func TestValidateServerIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantErr error
		name    string
		ids     []string
	}{
		{name: "valid charset", ids: []string{"prod-1", "staging_2"}},
		{name: "dot is rejected", ids: []string{"bad.id"}, wantErr: helpers.ErrInvalidGalaxyServerID},
		{name: "space is rejected", ids: []string{"bad id"}, wantErr: helpers.ErrInvalidGalaxyServerID},
		{name: "exact duplicate is rejected", ids: []string{"prod", "prod"}, wantErr: helpers.ErrDuplicateGalaxyServerID},
		{name: "case-colliding ids are rejected", ids: []string{"Prod", "prod"}, wantErr: helpers.ErrDuplicateGalaxyServerID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateServerIDs(tt.ids)
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("validateServerIDs() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("validateServerIDs() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestResolveServersInvalidIDPropagates checks the same id-charset
// invariant end to end, through resolveServers's server_list branch.
func TestResolveServersInvalidIDPropagates(t *testing.T) {
	t.Parallel()
	c := newServerCmd(t, nil)
	ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{ServerList: "bad.id"}}
	_, err := runResolveServers(t, c, ansCfg)
	if !errors.Is(err, helpers.ErrInvalidGalaxyServerID) {
		t.Errorf("resolveServers() error = %v, want helpers.ErrInvalidGalaxyServerID", err)
	}
}

// TestBuildServerUserinfoRejected checks that a server URL embedding
// userinfo is a config error, both for a named server and (via
// buildImplicitServer) the implicit single-server case.
func TestBuildServerUserinfoRejected(t *testing.T) {
	t.Parallel()

	t.Run("named server", func(t *testing.T) {
		t.Parallel()
		// #nosec G101 -- test fixture literal, not a real credential
		_, _, err := buildServer("prod", map[string]string{"url": "https://user:pass@hub.example/"})
		if !errors.Is(err, helpers.ErrGalaxyServerURLUserinfo) {
			t.Errorf("error = %v, want helpers.ErrGalaxyServerURLUserinfo", err)
		}
		if strings.Contains(err.Error(), "pass") {
			t.Errorf("error = %v, must not echo the password", err)
		}
	})

	t.Run("implicit single server", func(t *testing.T) {
		t.Parallel()
		// #nosec G101 -- test fixture literal, not a real credential
		_, err := buildImplicitServer("https://user:pass@hub.example/", false)
		if !errors.Is(err, helpers.ErrGalaxyServerURLUserinfo) {
			t.Errorf("error = %v, want helpers.ErrGalaxyServerURLUserinfo", err)
		}
		if strings.Contains(err.Error(), "pass") {
			t.Errorf("error = %v, must not echo the password", err)
		}
	})
}

// TestBuildServerInsecureTokenTransport checks that a token configured for
// a plaintext (http) origin is rejected unless that origin is loopback.
func TestBuildServerInsecureTokenTransport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "plaintext non-loopback is rejected", url: "http://hub.example", wantErr: true},
		{name: "plaintext localhost is allowed", url: "http://localhost:8080"},
		{name: "plaintext 127.0.0.1 is allowed", url: "http://127.0.0.1:8080"},
		{name: "plaintext 127.9.9.9 (127.0.0.0/8) is allowed", url: "http://127.9.9.9:8080"},
		{name: "plaintext ::1 is allowed", url: "http://[::1]:8080"},
		{name: "https non-loopback is allowed", url: "https://hub.example"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := buildServer("prod", map[string]string{"url": tt.url, "token": "tok"})
			if tt.wantErr {
				if !errors.Is(err, helpers.ErrInsecureTokenTransport) {
					t.Errorf("error = %v, want helpers.ErrInsecureTokenTransport", err)
				}
				return
			}
			if err != nil {
				t.Errorf("buildServer() error = %v, want nil", err)
			}
		})
	}
}

// TestResolveServersOriginConflicts checks both same-origin conflict
// errors: two servers sharing an origin must agree on validate_certs and
// on their token.
func TestResolveServersOriginConflicts(t *testing.T) {
	t.Parallel()

	t.Run("conflicting validate_certs", func(t *testing.T) {
		t.Parallel()
		ansCfg := twoServerAnsCfg(
			map[string]string{"url": "https://hub.example", "validate_certs": "false"},
			map[string]string{"url": "https://hub.example:443", "validate_certs": "true"},
		)
		_, err := runResolveServers(t, newServerCmd(t, nil), ansCfg)
		if !errors.Is(err, helpers.ErrConflictingServerTLSPolicy) {
			t.Errorf("error = %v, want helpers.ErrConflictingServerTLSPolicy", err)
		}
	})

	t.Run("conflicting token", func(t *testing.T) {
		t.Parallel()
		ansCfg := twoServerAnsCfg(
			map[string]string{"url": "https://hub.example", "token": "tok-1"},
			map[string]string{"url": "https://hub.example", "token": "tok-2"},
		)
		_, err := runResolveServers(t, newServerCmd(t, nil), ansCfg)
		if !errors.Is(err, helpers.ErrConflictingServerToken) {
			t.Errorf("error = %v, want helpers.ErrConflictingServerToken", err)
		}
	})

	t.Run("same origin, one token set and one unset, also conflicts", func(t *testing.T) {
		t.Parallel()
		ansCfg := twoServerAnsCfg(
			map[string]string{"url": "https://hub.example", "token": "tok-1"},
			map[string]string{"url": "https://hub.example"},
		)
		_, err := runResolveServers(t, newServerCmd(t, nil), ansCfg)
		if !errors.Is(err, helpers.ErrConflictingServerToken) {
			t.Errorf("error = %v, want helpers.ErrConflictingServerToken", err)
		}
	})
}

// twoServerAnsCfg returns an ansibleConfig listing two servers, "a" and
// "b", with the given per-section key/value maps - the shared fixture for
// TestResolveServersOriginConflicts and TestResolveServersOriginNoConflict.
func twoServerAnsCfg(a, b map[string]string) ansibleConfig {
	return ansibleConfig{
		Galaxy:        ansibleGalaxyConfig{ServerList: "a,b"},
		GalaxyServers: map[string]map[string]string{"a": a, "b": b},
	}
}

// TestResolveServersOriginNoConflict pins that two servers sharing an origin
// (implicit vs explicit default port) with matching TLS and token settings are
// not a conflict.
func TestResolveServersOriginNoConflict(t *testing.T) {
	t.Parallel()

	ansCfg := twoServerAnsCfg(
		map[string]string{"url": "https://hub.example", "token": "same-token"},
		map[string]string{"url": "https://hub.example:443", "token": "same-token"},
	)
	cfg, err := runResolveServers(t, newServerCmd(t, nil), ansCfg)
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("len(Servers) = %d, want 2", len(cfg.Servers))
	}
}

// TestResolveServersTLSWarnings checks the two exact warning texts:
// certificate verification disabled, and - only when that server also
// carries a token - a token sent over that unverified connection.
func TestResolveServersTLSWarnings(t *testing.T) {
	t.Parallel()

	t.Run("insecure without token: one warning", func(t *testing.T) {
		t.Parallel()
		ansCfg := singleServerList(map[string]string{"url": "https://hub.example", "validate_certs": "false"})
		c := newServerCmd(t, nil)
		cfg, err := runResolveServers(t, c, ansCfg)
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		want := fmt.Sprintf(
			"TLS certificate verification is DISABLED for Galaxy server %q (%s); "+
				"this run cannot detect a man-in-the-middle on that host", "prod", "https://hub.example:443")
		if len(cfg.Warnings) != 1 {
			t.Fatalf("len(Warnings) = %d, want 1 (Warnings = %v)", len(cfg.Warnings), cfg.Warnings)
		}
		if cfg.Warnings[0] != want {
			t.Errorf("Warnings[0] = %q, want %q", cfg.Warnings[0], want)
		}
	})

	t.Run("insecure with token: two warnings", func(t *testing.T) {
		t.Parallel()
		// Every value here is file-sourced, so the pairing rule exempts this
		// row; an env-sourced token would make it a refusal and drop the only
		// coverage of the second warning.
		ansCfg := singleServerList(map[string]string{
			"url": "https://hub.example", "validate_certs": "false", "token": "tok",
		})
		c := newServerCmd(t, nil)
		cfg, err := runResolveServers(t, c, ansCfg)
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		wantTLS := fmt.Sprintf(
			"TLS certificate verification is DISABLED for Galaxy server %q (%s); "+
				"this run cannot detect a man-in-the-middle on that host", "prod", "https://hub.example:443")
		wantToken := fmt.Sprintf(
			"A Galaxy API token is being sent to server %q (%s) over a connection whose certificate is not "+
				"verified; the token can be captured by an on-path attacker", "prod", "https://hub.example:443")
		if len(cfg.Warnings) != 2 {
			t.Fatalf("len(Warnings) = %d, want 2 (Warnings = %v)", len(cfg.Warnings), cfg.Warnings)
		}
		if cfg.Warnings[0] != wantTLS {
			t.Errorf("Warnings[0] = %q, want %q", cfg.Warnings[0], wantTLS)
		}
		if cfg.Warnings[1] != wantToken {
			t.Errorf("Warnings[1] = %q, want %q", cfg.Warnings[1], wantToken)
		}
	})

	t.Run("verified TLS: no warnings", func(t *testing.T) {
		t.Parallel()
		ansCfg := singleServerList(map[string]string{"url": "https://hub.example", "token": "tok"})
		c := newServerCmd(t, nil)
		cfg, err := runResolveServers(t, c, ansCfg)
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		if len(cfg.Warnings) != 0 {
			t.Errorf("Warnings = %v, want none", cfg.Warnings)
		}
	})
}

// secretRedactionPlaintext is the fixture token shared by every
// TestSecretRedaction* function below: never expected to appear in any of
// Secret's redacted renderings.
const secretRedactionPlaintext = "super-secret-token-xyz"

// TestSecretRedactionFmtVerbs checks that every fmt verb fmt routes through
// Stringer or GoStringer (%v %s %q %x %X %+v %#v) never renders a Secret's
// plaintext.
func TestSecretRedactionFmtVerbs(t *testing.T) {
	t.Parallel()
	set := NewSecret(secretRedactionPlaintext)

	// %x and %X hex-encode the Stringer's output, so they are compared with the
	// hex encoding of "[REDACTED]" rather than searched for the word.
	hexRedacted := fmt.Sprintf("%x", "[REDACTED]")
	fmtChecks := []struct {
		verb        string
		got         string
		wantLiteral string // "" for the hex verbs, which are checked separately below
	}{
		{"%v", fmt.Sprintf("%v", set), "REDACTED"},
		{"%s", set.String(), "REDACTED"},
		{"%q", fmt.Sprintf("%q", set), "REDACTED"},
		{"%x", fmt.Sprintf("%x", set), ""},
		{"%X", fmt.Sprintf("%X", set), ""},
		{"%+v", fmt.Sprintf("%+v", set), "REDACTED"},
		{"%#v", fmt.Sprintf("%#v", set), "REDACTED"},
	}
	for _, c := range fmtChecks {
		t.Run(c.verb, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(c.got, secretRedactionPlaintext) {
				t.Errorf("%s rendered the plaintext: %q", c.verb, c.got)
			}
			if c.wantLiteral != "" && !strings.Contains(c.got, c.wantLiteral) {
				t.Errorf("%s = %q, want it to mention %s", c.verb, c.got, c.wantLiteral)
			}
		})
	}
	t.Run("%x is the hex encoding of the redacted placeholder", func(t *testing.T) {
		t.Parallel()
		if got := fmt.Sprintf("%x", set); got != hexRedacted {
			t.Errorf("%%x = %q, want %q", got, hexRedacted)
		}
	})
	t.Run("%X is the uppercase hex encoding of the redacted placeholder", func(t *testing.T) {
		t.Parallel()
		if got, want := fmt.Sprintf("%X", set), strings.ToUpper(hexRedacted); got != want {
			t.Errorf("%%X = %q, want %q", got, want)
		}
	})
}

// TestSecretRedactionUnsetAndReveal checks that a zero Secret always reads
// the unset placeholder, and that Reveal is the one way back to the
// plaintext.
func TestSecretRedactionUnsetAndReveal(t *testing.T) {
	t.Parallel()
	const wantUnset = "[unset]"

	var unset Secret
	if unset.IsSet() {
		t.Error("zero Secret.IsSet() = true, want false")
	}
	if got := fmt.Sprintf("%v", unset); got != wantUnset {
		t.Errorf("zero Secret %%v = %q, want %q", got, wantUnset)
	}
	if got := fmt.Sprintf("%#v", unset); got != wantUnset {
		t.Errorf("zero Secret %%#v = %q, want %q", got, wantUnset)
	}

	set := NewSecret(secretRedactionPlaintext)
	if got := set.Reveal(); got != secretRedactionPlaintext {
		t.Errorf("Reveal() = %q, want %q", got, secretRedactionPlaintext)
	}
}

// TestSecretRedactionMarshal checks that json.Marshal and yaml.Marshal both
// render the redacted placeholder rather than the plaintext, standalone and
// nested inside a Config.
func TestSecretRedactionMarshal(t *testing.T) {
	t.Parallel()
	set := NewSecret(secretRedactionPlaintext)

	t.Run("json.Marshal, bare", func(t *testing.T) {
		t.Parallel()
		b, err := json.Marshal(set)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v, want nil", err)
		}
		if strings.Contains(string(b), secretRedactionPlaintext) {
			t.Errorf("json.Marshal() = %s, must not contain the plaintext", b)
		}
		if string(b) != `"[REDACTED]"` {
			t.Errorf("json.Marshal() = %s, want %q", b, `"[REDACTED]"`)
		}
	})

	t.Run("yaml.Marshal, bare", func(t *testing.T) {
		t.Parallel()
		b, err := yaml.Marshal(set)
		if err != nil {
			t.Fatalf("yaml.Marshal() error = %v, want nil", err)
		}
		if strings.Contains(string(b), secretRedactionPlaintext) {
			t.Errorf("yaml.Marshal() = %s, must not contain the plaintext", b)
		}
		if !strings.Contains(string(b), "REDACTED") {
			t.Errorf("yaml.Marshal() = %s, want it to mention REDACTED", b)
		}
	})
}

// TestSecretRedactionMarshalNestedInConfig pins json and yaml redaction of a
// Secret nested inside a Config, as it sits in real runtime state.
func TestSecretRedactionMarshalNestedInConfig(t *testing.T) {
	t.Parallel()
	cfg := Config{Servers: []Server{{ID: "prod", Token: NewSecret(secretRedactionPlaintext)}}}

	t.Run("json.Marshal", func(t *testing.T) {
		t.Parallel()
		// Config and Server carry no json tags: they are runtime types, not a
		// serialization contract; this checks redaction survives reflection.
		b, err := json.Marshal(cfg) //nolint:musttag
		if err != nil {
			t.Fatalf("json.Marshal() error = %v, want nil", err)
		}
		if strings.Contains(string(b), secretRedactionPlaintext) {
			t.Errorf("json.Marshal() = %s, must not contain the plaintext", b)
		}
	})

	t.Run("yaml.Marshal", func(t *testing.T) {
		t.Parallel()
		b, err := yaml.Marshal(cfg) //nolint:musttag // see the json.Marshal case above
		if err != nil {
			t.Fatalf("yaml.Marshal() error = %v, want nil", err)
		}
		if strings.Contains(string(b), secretRedactionPlaintext) {
			t.Errorf("yaml.Marshal() = %s, must not contain the plaintext", b)
		}
	})
}

// TestSecretRedactionDumpNestedInPointerConfig pins that a *Config with a
// nested token redacts through every fmt verb and marshaler a careless debug
// line or crash dump could reach for.
func TestSecretRedactionDumpNestedInPointerConfig(t *testing.T) {
	t.Parallel()
	cfg := &Config{Servers: []Server{{ID: "prod", URL: "https://hub.example", Token: NewSecret(secretRedactionPlaintext)}}}

	fmtChecks := []struct {
		verb string
		got  string
	}{
		{"%v", fmt.Sprintf("%v", cfg)},
		{"%+v", fmt.Sprintf("%+v", cfg)},
		{"%#v", fmt.Sprintf("%#v", cfg)},
	}
	for _, c := range fmtChecks {
		t.Run(c.verb, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(c.got, secretRedactionPlaintext) {
				t.Errorf("%s of *Config rendered the plaintext: %q", c.verb, c.got)
			}
		})
	}

	t.Run("json.Marshal", func(t *testing.T) {
		t.Parallel()
		b, err := json.Marshal(cfg) //nolint:musttag // see TestSecretRedactionMarshalNestedInConfig
		if err != nil {
			t.Fatalf("json.Marshal() error = %v, want nil", err)
		}
		if strings.Contains(string(b), secretRedactionPlaintext) {
			t.Errorf("json.Marshal() = %s, must not contain the plaintext", b)
		}
	})

	t.Run("yaml.Marshal", func(t *testing.T) {
		t.Parallel()
		b, err := yaml.Marshal(cfg) //nolint:musttag // see TestSecretRedactionMarshalNestedInConfig
		if err != nil {
			t.Fatalf("yaml.Marshal() error = %v, want nil", err)
		}
		if strings.Contains(string(b), secretRedactionPlaintext) {
			t.Errorf("yaml.Marshal() = %s, must not contain the plaintext", b)
		}
	})
}

// TestTokenFlagCases pins what --token does to the resolved server list. It is
// not parallel: rows call t.Setenv, which panics under a parallel test.
func TestTokenFlagCases(t *testing.T) {
	for _, tc := range tokenFlagCases() {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c := newServerCmd(t, tc.args)
			cfg, err := runResolveServers(t, c, tc.ansCfg)
			tc.check(t, cfg, err)
		})
	}
}

// tokenFlagCase is one row of TestTokenFlagCases: the command line the row
// runs, the ansible.cfg and environment it resolves against, and the row's
// own assertions over whatever resolveServers returned.
type tokenFlagCase struct {
	check  func(t *testing.T, cfg *Config, err error)
	env    map[string]string
	ansCfg ansibleConfig
	name   string
	args   []string
}

// hubWithINIToken is the ansible.cfg the rows about an already-configured
// credential resolve against: one server_list entry whose section carries a
// token, so the flag has something to override, to clear, or to leave alone.
func hubWithINIToken() ansibleConfig {
	return ansibleConfig{
		Galaxy:        ansibleGalaxyConfig{ServerList: "hub"},
		GalaxyServers: map[string]map[string]string{"hub": {"url": "https://hub.example.com", "token": "from-ini"}},
	}
}

// tokenFlagCases lists the --token shapes that hand over, override or clear a
// credential, those refused, and the one that leaves a token untouched; the
// plaintext rule has two rows so one outcome cannot hide the other.
func tokenFlagCases() []tokenFlagCase {
	return []tokenFlagCase{
		{
			// checks the case --token exists for: one effective server,
			// credential handed to it on the command line rather than through
			// an ansible.cfg section.
			name:  "applies to single server",
			args:  []string{"--server=https://hub.example.com", "--token=s3cr3t"},
			check: checkTokenAppliesToSingleServer,
		},
		{
			// an operator token wins over a section token when the url is the
			// operator's own too: the env url is what keeps this row out of the
			// destination refusal.
			name:   "overrides configured token when the url is the operator's own",
			args:   []string{"--token=from-flag"},
			ansCfg: hubWithINIToken(),
			env:    map[string]string{"ANSIBLE_GALAXY_SERVER_HUB_URL": "https://hub.example.com"},
			check:  checkTokenOverridesConfiguredToken,
		},
		{
			// the row above without the env url: a section's own token
			// authorizes only itself, so an operator token against its
			// file-sourced url is refused.
			name:   "refused: an operator token does not authorize itself against a file-sourced url",
			args:   []string{"--token=from-flag"},
			ansCfg: hubWithINIToken(),
			check:  checkTokenRefusedAgainstFileSourcedURL,
		},
		{
			// checks that exporting GO_GALAXY_TOKEN= forces an anonymous run
			// without editing any config, which is the point of treating an
			// explicitly empty value as "no token" rather than as "unset".
			name:   "clears when explicitly empty",
			args:   []string{"--token="},
			ansCfg: hubWithINIToken(),
			check:  checkTokenClearsWhenExplicitlyEmpty,
		},
		{
			// checks the ambiguity refusal: with two servers in effect the flag
			// names neither, and guessing could send a private hub's credential
			// to the public Galaxy.
			name: "rejected with multiple servers",
			args: []string{"--token=s3cr3t"},
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "hub,public"},
				GalaxyServers: map[string]map[string]string{
					"hub":    {"url": "https://hub.example.com"},
					"public": {"url": "https://galaxy.ansible.com"},
				},
			},
			check: checkTokenRejectedWithMultipleServers,
		},
		{
			// checks that --token is held to the same transport rule as a
			// configured token: a credential must not be sent in the clear to
			// anything but loopback.
			name:  "rejected over plaintext non-loopback",
			args:  []string{"--server=http://hub.example.com", "--token=s3cr3t"},
			check: checkTokenRejectedOverPlaintextNonLoopback,
		},
		{
			// the loopback half of that same transport rule: 127.0.0.1 is the
			// one plaintext destination a credential may reach, so this shape
			// resolves rather than being refused.
			name:  "allowed over plaintext loopback",
			args:  []string{"--server=http://127.0.0.1:8080", "--token=s3cr3t"},
			check: checkTokenAllowedOverPlaintextLoopback,
		},
		{
			// checks that a command which never sets the flag - including one
			// that does not register it - changes nothing, so the flag cannot
			// silently strip a configured credential.
			name:   "unset leaves servers untouched",
			args:   nil,
			ansCfg: hubWithINIToken(),
			check:  checkTokenUnsetLeavesServersUntouched,
		},
	}
}

// checkTokenAppliesToSingleServer asserts the "applies to single server" row:
// resolution succeeds, yields exactly one server, and that server carries the
// flag's value.
func checkTokenAppliesToSingleServer(t *testing.T, cfg *Config, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("len(Servers) = %d, want 1", len(cfg.Servers))
	}
	if got := cfg.Servers[0].Token.Reveal(); got != "s3cr3t" {
		t.Errorf("Servers[0].Token = %q, want %q", got, "s3cr3t")
	}
}

// checkTokenOverridesConfiguredToken asserts the "overrides configured token
// when the url is the operator's own" row: the resolved server carries the
// flag's value rather than the section's.
func checkTokenOverridesConfiguredToken(t *testing.T, cfg *Config, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if got := cfg.Servers[0].Token.Reveal(); got != "from-flag" {
		t.Errorf("Servers[0].Token = %q, want the flag value %q", got, "from-flag")
	}
}

// checkTokenRefusedAgainstFileSourcedURL asserts that row's refusal:
// helpers.ErrTokenDestinationFromFile, naming server "hub".
func checkTokenRefusedAgainstFileSourcedURL(t *testing.T, _ *Config, err error) {
	t.Helper()
	if !errors.Is(err, helpers.ErrTokenDestinationFromFile) {
		t.Fatalf("resolveServers() error = %v, want helpers.ErrTokenDestinationFromFile", err)
	}
	if !strings.Contains(err.Error(), `"hub"`) {
		t.Errorf("error = %v, want it to name server %q", err, "hub")
	}
}

// checkTokenClearsWhenExplicitlyEmpty asserts the "clears when explicitly
// empty" row: the resolved server ends up with no token at all.
func checkTokenClearsWhenExplicitlyEmpty(t *testing.T, cfg *Config, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if cfg.Servers[0].Token.IsSet() {
		t.Errorf("Servers[0].Token is set, want it cleared by an explicitly empty --token")
	}
}

// checkTokenRejectedWithMultipleServers asserts the "rejected with multiple
// servers" row: resolution fails with ErrAmbiguousGalaxyToken.
func checkTokenRejectedWithMultipleServers(t *testing.T, _ *Config, err error) {
	t.Helper()
	if !errors.Is(err, helpers.ErrAmbiguousGalaxyToken) {
		t.Fatalf("resolveServers() error = %v, want ErrAmbiguousGalaxyToken", err)
	}
}

// checkTokenRejectedOverPlaintextNonLoopback asserts the "rejected over
// plaintext non-loopback" row: resolution fails with ErrInsecureTokenTransport.
func checkTokenRejectedOverPlaintextNonLoopback(t *testing.T, _ *Config, err error) {
	t.Helper()
	if !errors.Is(err, helpers.ErrInsecureTokenTransport) {
		t.Fatalf("resolveServers() error = %v, want ErrInsecureTokenTransport", err)
	}
}

// checkTokenAllowedOverPlaintextLoopback asserts the "allowed over plaintext
// loopback" row: resolution succeeds.
func checkTokenAllowedOverPlaintextLoopback(t *testing.T, _ *Config, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveServers() over loopback error = %v, want nil", err)
	}
}

// checkTokenUnsetLeavesServersUntouched asserts the "unset leaves servers
// untouched" row: the resolved server still carries the configured token.
func checkTokenUnsetLeavesServersUntouched(t *testing.T, cfg *Config, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if got := cfg.Servers[0].Token.Reveal(); got != "from-ini" {
		t.Errorf("Servers[0].Token = %q, want the configured %q", got, "from-ini")
	}
}

// TestTokenDestinationPairing pins the destination arm of tokenPairingOffense:
// an operator token never pairs with a server URL read from ansible.cfg. It is
// not parallel: rows call t.Setenv, which panics under a parallel test.
func TestTokenDestinationPairing(t *testing.T) {
	for _, tc := range tokenDestinationCases() {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c := newServerCmd(t, tc.args)
			cfg, err := runResolveServers(t, c, tc.ansCfg)
			tc.check(t, cfg, err)
		})
	}
}

// tokenDestinationCase is one row of TestTokenDestinationPairing: a full
// resolveServers run against its own ansible.cfg fixture, environment
// overrides, and command line, checked by its own assertions.
type tokenDestinationCase struct {
	check  func(t *testing.T, cfg *Config, err error)
	env    map[string]string
	ansCfg ansibleConfig
	name   string
	args   []string
}

// tokenDestinationRefused asserts ErrTokenDestinationFromFile naming
// wantID and wantOrigin but not wantToken, so a row's token must never be a
// substring of its own id or origin.
func tokenDestinationRefused(wantID, wantOrigin, wantToken string) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, _ *Config, err error) {
		t.Helper()
		if !errors.Is(err, helpers.ErrTokenDestinationFromFile) {
			t.Fatalf("resolveServers() error = %v, want helpers.ErrTokenDestinationFromFile", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, fmt.Sprintf("%q", wantID)) {
			t.Errorf("error = %q, want it to name server %q", msg, wantID)
		}
		if !strings.Contains(msg, wantOrigin) {
			t.Errorf("error = %q, want it to name origin %q", msg, wantOrigin)
		}
		if strings.Contains(msg, wantToken) {
			t.Errorf("error = %q, must not contain the token plaintext %q", msg, wantToken)
		}
	}
}

// tokenDestinationRefusedNotTLS adds to tokenDestinationRefused that the error
// is never also ErrTokenTLSPolicyFromFile: checkTokenPairing reports
// one sentinel or the other, never both.
func tokenDestinationRefusedNotTLS(wantID, wantOrigin, wantToken string) func(t *testing.T, cfg *Config, err error) {
	inner := tokenDestinationRefused(wantID, wantOrigin, wantToken)
	return func(t *testing.T, cfg *Config, err error) {
		t.Helper()
		inner(t, cfg, err)
		if errors.Is(err, helpers.ErrTokenTLSPolicyFromFile) {
			t.Errorf("resolveServers() error = %v, must not also be helpers.ErrTokenTLSPolicyFromFile", err)
		}
	}
}

// tokenDestinationAccepted builds a tokenDestinationCase assertion for an
// accepted pairing: resolveServers succeeds, and the resolved single
// server's URL and revealed token equal wantURL and wantToken.
func tokenDestinationAccepted(wantURL, wantToken string) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, cfg *Config, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		if len(cfg.Servers) != 1 {
			t.Fatalf("len(Servers) = %d, want 1", len(cfg.Servers))
		}
		if got := cfg.Servers[0].URL; got != wantURL {
			t.Errorf("Servers[0].URL = %q, want %q", got, wantURL)
		}
		if got := cfg.Servers[0].Token.Reveal(); got != wantToken {
			t.Errorf("Servers[0].Token.Reveal() = %q, want %q", got, wantToken)
		}
	}
}

// tokenDestinationAcceptedNoToken asserts an accepted pairing whose single
// server ends up with no token, never configured or cleared by --token=.
func tokenDestinationAcceptedNoToken(wantURL string) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, cfg *Config, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		if len(cfg.Servers) != 1 {
			t.Fatalf("len(Servers) = %d, want 1", len(cfg.Servers))
		}
		if got := cfg.Servers[0].URL; got != wantURL {
			t.Errorf("Servers[0].URL = %q, want %q", got, wantURL)
		}
		if cfg.Servers[0].Token.IsSet() {
			t.Errorf("Servers[0].Token is set, want it cleared or never configured")
		}
	}
}

// tokenDestinationCases lists the twelve reachable pairings: a file-sourced url
// with an operator token is refused on every channel, everything else is
// accepted. The three groups exist only for funlen.
func tokenDestinationCases() []tokenDestinationCase {
	cases := tokenDestinationCasesGroupOne()
	cases = append(cases, tokenDestinationCasesGroupTwo()...)
	return append(cases, tokenDestinationCasesGroupThree()...)
}

// tokenDestinationCasesGroupOne is the first four rows of
// tokenDestinationCases: the leaking shape and its remedy (R1, A2), and the
// bare [galaxy] server fallback refused and accepted (R3, A4).
func tokenDestinationCasesGroupOne() []tokenDestinationCase {
	return []tokenDestinationCase{
		{
			// the simplest leak: only ansible.cfg names "corp", the token is
			// ANSIBLE_GALAXY_SERVER_CORP_TOKEN; also pins that it is never the
			// TLS sentinel, the mirror of the TLS table's X1 row.
			name: "refused: file server_list url with an env token",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"url": "https://corp.example"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "r1-secret-token"},
			check: tokenDestinationRefusedNotTLS("corp", "https://corp.example:443", "r1-secret-token"),
		},
		{
			// the remedy: an env url moves the address onto the operator
			// channel. Both halves agree, so this row alone does not prove env
			// is the operator channel; the mixed-provenance rows do.
			name: "accepted: adding an env url remedies the row above",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"url": "https://corp.example"}},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "a2-secret-token",
				"ANSIBLE_GALAXY_SERVER_CORP_URL":   "https://corp-env.example",
			},
			check: tokenDestinationAccepted("https://corp-env.example", "a2-secret-token"),
		},
		{
			// [galaxy] server from the file (no server_list at all) with an
			// operator --token: the implicit single server's id is "",
			// which still has to be named correctly in the refusal.
			name:   "refused: file [galaxy] server with --token",
			ansCfg: ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://filecorp.example"}},
			args:   []string{"--token=r3-secret-token"},
			check:  tokenDestinationRefused("", "https://filecorp.example:443", "r3-secret-token"),
		},
		{
			// the same shape with the server value sourced from
			// $ANSIBLE_GALAXY_SERVER instead of the ansible.cfg file key:
			// an operator channel, so the pairing with --token is accepted.
			name:  "accepted: ANSIBLE_GALAXY_SERVER with --token",
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER": "https://opcorp.example"},
			args:  []string{"--token=a4-secret-token"},
			check: tokenDestinationAccepted("https://opcorp.example", "a4-secret-token"),
		},
	}
}

// tokenDestinationCasesGroupTwo covers --server naming an id (R5), file url
// with file token (A6), env url with file token (A7) and no token (A8).
func tokenDestinationCasesGroupTwo() []tokenDestinationCase {
	return []tokenDestinationCase{
		{
			// --server=corp selects the section but not the address: the
			// section's url is still file-sourced, so the env token is refused.
			name: "refused: --server naming a server_list id with an env token",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"url": "https://corp.example"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "r5-secret-token"},
			args:  []string{"--server=corp"},
			check: tokenDestinationRefused("corp", "https://corp.example:443", "r5-secret-token"),
		},
		{
			// section url + section token, nothing else: one author
			// supplied both halves, so the pairing is accepted unchanged.
			name: "accepted: section url with section token",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{
					"corp": {"url": "https://corp.example", "token": "a6-ini-token"},
				},
			},
			check: tokenDestinationAccepted("https://corp.example", "a6-ini-token"),
		},
		{
			// env url + section token: the url is operator-sourced, so the
			// pairing rule never even inspects the token's own provenance.
			name: "accepted: env url with section token",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"token": "a7-ini-token"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp-env.example"},
			check: tokenDestinationAccepted("https://corp-env.example", "a7-ini-token"),
		},
		{
			// section url, no token anywhere: exempt twice over, since
			// Token.IsSet() is false and tokenFromFile reads true for
			// an absent key.
			name: "accepted: section url with no token at all",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"url": "https://corp.example"}},
			},
			check: tokenDestinationAcceptedNoToken("https://corp.example"),
		},
	}
}

// tokenDestinationCasesGroupThree covers a later server_list entry (R9), forced
// anonymity (A10), and an operator --token over a section token against a file
// url (R11) and an env url (A12).
func tokenDestinationCasesGroupThree() []tokenDestinationCase {
	return []tokenDestinationCase{
		{
			// a two-entry server_list where only the second entry offends:
			// checkTokenPairing must walk the whole list rather than
			// stopping at (or only ever checking) the first entry.
			name: "refused: a later server_list entry offends, not the first",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "pub,corp"},
				GalaxyServers: map[string]map[string]string{
					"pub":  {},
					"corp": {"url": "https://corp.example"},
				},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_PUB_URL":    "https://pub.example",
				"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "r9-secret-token",
			},
			check: tokenDestinationRefused("corp", "https://corp.example:443", "r9-secret-token"),
		},
		{
			// section url + section token + an explicit --token= (empty):
			// forcing anonymity is never refused, even against a
			// file-sourced url - there is no destination left to pair.
			name: "accepted: an explicit empty --token forces anonymity",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{
					"corp": {"url": "https://corp.example", "token": "a10-ini-token"},
				},
			},
			args:  []string{"--token="},
			check: tokenDestinationAcceptedNoToken("https://corp.example"),
		},
		{
			// section url AND section token, overridden by an operator
			// --token: the section's own token authorizes only itself, not
			// a credential from an operator channel layered on top of it.
			name: "refused: an operator --token overrides a section token pairing",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{
					"corp": {"url": "https://corp.example", "token": "r11-ini-token"},
				},
			},
			args:  []string{"--token=from-flag"},
			check: tokenDestinationRefused("corp", "https://corp.example:443", "from-flag"),
		},
		{
			// env url + section token + operator --token: the url is the
			// operator's, so the override is accepted, as in
			// TestTokenFlagCases.
			name: "accepted: an operator --token against an env url overrides a section token",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"token": "a12-ini-token"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp-env.example"},
			args:  []string{"--token=from-flag"},
			check: tokenDestinationAccepted("https://corp-env.example", "from-flag"),
		},
	}
}

// tlsPolicyServerID is the [galaxy_server.<id>] section id every fixture in
// tokenTLSPolicyCases uses; see tlsPolicyRefused for why the TLS arm can
// only ever be reached through such a section.
const tlsPolicyServerID = "corp"

// tlsPolicyRefused mirrors tokenDestinationRefused for
// ErrTokenTLSPolicyFromFile. The id is fixed: only a section can reach
// the TLS arm, and every fixture names its section tlsPolicyServerID.
func tlsPolicyRefused(wantOrigin, wantToken string) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, _ *Config, err error) {
		t.Helper()
		if !errors.Is(err, helpers.ErrTokenTLSPolicyFromFile) {
			t.Fatalf("resolveServers() error = %v, want helpers.ErrTokenTLSPolicyFromFile", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, fmt.Sprintf("%q", tlsPolicyServerID)) {
			t.Errorf("error = %q, want it to name server %q", msg, tlsPolicyServerID)
		}
		if !strings.Contains(msg, wantOrigin) {
			t.Errorf("error = %q, want it to name origin %q", msg, wantOrigin)
		}
		if strings.Contains(msg, wantToken) {
			t.Errorf("error = %q, must not contain the token plaintext %q", msg, wantToken)
		}
	}
}

// tlsPolicyAccepted asserts an accepted pairing whose single server has
// wantURL, wantToken and wantInsecure, the TLS dimension the destination table
// has no need to check.
func tlsPolicyAccepted(wantURL, wantToken string, wantInsecure bool) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, cfg *Config, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		if len(cfg.Servers) != 1 {
			t.Fatalf("len(Servers) = %d, want 1", len(cfg.Servers))
		}
		if got := cfg.Servers[0].URL; got != wantURL {
			t.Errorf("Servers[0].URL = %q, want %q", got, wantURL)
		}
		if got := cfg.Servers[0].Token.Reveal(); got != wantToken {
			t.Errorf("Servers[0].Token.Reveal() = %q, want %q", got, wantToken)
		}
		if got := cfg.Servers[0].InsecureSkipTLSVerify; got != wantInsecure {
			t.Errorf("Servers[0].InsecureSkipTLSVerify = %v, want %v", got, wantInsecure)
		}
	}
}

// tlsPolicyAcceptedNoToken asserts an accepted pairing with no token left and
// the file's TLS policy unchanged, since without an operator token there is
// nothing for the pairing rule to protect.
func tlsPolicyAcceptedNoToken(wantURL string, wantInsecure bool) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, cfg *Config, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		if len(cfg.Servers) != 1 {
			t.Fatalf("len(Servers) = %d, want 1", len(cfg.Servers))
		}
		if got := cfg.Servers[0].URL; got != wantURL {
			t.Errorf("Servers[0].URL = %q, want %q", got, wantURL)
		}
		if cfg.Servers[0].Token.IsSet() {
			t.Errorf("Servers[0].Token is set, want it cleared or never configured")
		}
		if got := cfg.Servers[0].InsecureSkipTLSVerify; got != wantInsecure {
			t.Errorf("Servers[0].InsecureSkipTLSVerify = %v, want %v", got, wantInsecure)
		}
	}
}

// tokenTLSPolicyCase is one row of TestTokenTLSPolicyPairing, shaped like
// tokenDestinationCase.
type tokenTLSPolicyCase struct {
	check  func(t *testing.T, cfg *Config, err error)
	env    map[string]string
	ansCfg ansibleConfig
	name   string
	args   []string
}

// TestTokenTLSPolicyPairing pins the TLS arm of tokenPairingOffense: an
// operator token never pairs with a validate_certs relaxation read from
// ansible.cfg. It is not parallel: rows call t.Setenv.
func TestTokenTLSPolicyPairing(t *testing.T) {
	for _, tc := range tokenTLSPolicyCases() {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c := newServerCmd(t, tc.args)
			cfg, err := runResolveServers(t, c, tc.ansCfg)
			tc.check(t, cfg, err)
		})
	}
}

// tokenTLSPolicyCases lists the eleven reachable shapes: a file-sourced
// validate_certs relaxation with an operator token is refused, everything else
// is accepted. The four groups exist only for funlen.
func tokenTLSPolicyCases() []tokenTLSPolicyCase {
	cases := tokenTLSPolicyCasesGroupOne()
	cases = append(cases, tokenTLSPolicyCasesGroupTwo()...)
	cases = append(cases, tokenTLSPolicyCasesGroupThree()...)
	return append(cases, tokenTLSPolicyCasesGroupFour()...)
}

// tokenTLSPolicyCasesGroupOne is the first three rows of
// tokenTLSPolicyCases: the leaking shape and its remedy (R1, A1), and the
// --server-by-id refusal (R2).
func tokenTLSPolicyCasesGroupOne() []tokenTLSPolicyCase {
	return []tokenTLSPolicyCase{
		{
			// url and token both come from the operator's env, yet the
			// file-sourced validate_certs = no is refused: it is not part of
			// the destination pairing and is judged on its own.
			name: "refused: env url + env token + a file validate_certs",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "no"}},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_URL":   "https://real-hub.example",
				"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "r1-secret-token",
			},
			check: tlsPolicyRefused("https://real-hub.example:443", "r1-secret-token"),
		},
		{
			// the remedy: the same validate_certs through the env override
			// moves the relaxation onto the operator channel;
			// InsecureSkipTLSVerify stays true.
			name: "accepted: an env VALIDATE_CERTS override remedies the row above",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "no"}},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_URL":            "https://real-hub.example",
				"ANSIBLE_GALAXY_SERVER_CORP_TOKEN":          "a1-secret-token",
				"ANSIBLE_GALAXY_SERVER_CORP_VALIDATE_CERTS": "no",
			},
			check: tlsPolicyAccepted("https://real-hub.example", "a1-secret-token", true),
		},
		{
			// --server=corp and the url are both operator channels and only
			// validate_certs is file-sourced: the TLS check fires independently
			// of the destination one.
			name: "refused: --server by id, env url, env token, file validate_certs",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "no"}},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp.example",
				"GO_GALAXY_TOKEN":                "r2-secret-token",
			},
			args:  []string{"--server=corp"},
			check: tlsPolicyRefused("https://corp.example:443", "r2-secret-token"),
		},
	}
}

// tokenTLSPolicyCasesGroupTwo covers validate_certs = yes as no relaxation
// (A2), a section token with section validate_certs (A3), and no token (A4).
func tokenTLSPolicyCasesGroupTwo() []tokenTLSPolicyCase {
	return []tokenTLSPolicyCase{
		{
			// validate_certs = yes is still file-sourced but relaxes nothing,
			// which pins the predicate to the relaxation rather than the key's
			// presence.
			name: "accepted: validate_certs = yes is not a relaxation to pair against",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "yes"}},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp.example",
				"GO_GALAXY_TOKEN":                "a2-secret-token",
			},
			args:  []string{"--server=corp"},
			check: tlsPolicyAccepted("https://corp.example", "a2-secret-token", false),
		},
		{
			// a section token is exempt by tokenFromFile before the
			// TLS arm is reached: the file may pair its own credential with its
			// own TLS policy.
			name: "accepted: section token pairs with section validate_certs",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{
					"corp": {"token": "a3-ini-token", "validate_certs": "no"},
				},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp-env.example"},
			check: tlsPolicyAccepted("https://corp-env.example", "a3-ini-token", true),
		},
		{
			// no token anywhere: exempt by Token.IsSet(), so an unauthenticated
			// run keeps the file's validate_certs = no, as the drop-in promise
			// requires.
			name: "accepted: no token at all leaves a file validate_certs untouched",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "no"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp-env.example"},
			check: tlsPolicyAcceptedNoToken("https://corp-env.example", true),
		},
	}
}

// tokenTLSPolicyCasesGroupThree is the next three rows of
// tokenTLSPolicyCases: the --token refusal and its forced-anonymous
// sibling (R3, A5), and the later-server_list-entry refusal (R4).
func tokenTLSPolicyCasesGroupThree() []tokenTLSPolicyCase {
	return []tokenTLSPolicyCase{
		{
			// the R2 shape through --token instead of GO_GALAXY_TOKEN, and
			// through the implicit single-entry server_list instead of
			// --server=corp.
			name: "refused: env url + --token + file validate_certs",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "no"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp.example"},
			args:  []string{"--token=r3-secret-token"},
			check: tlsPolicyRefused("https://corp.example:443", "r3-secret-token"),
		},
		{
			// forced anonymity is never refused, even against a file
			// validate_certs, and leaves the relaxed policy as the file
			// configured it.
			name: "accepted: an explicit empty --token forces anonymity",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "no"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp.example"},
			args:  []string{"--token="},
			check: tlsPolicyAcceptedNoToken("https://corp.example", true),
		},
		{
			// a two-entry server_list where only the second entry offends:
			// tokenPairingOffense must be evaluated per server across the
			// whole walk, not only against the first entry.
			name: "refused: a later server_list entry offends, not the first",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "pub,corp"},
				GalaxyServers: map[string]map[string]string{
					"pub":  {"url": "https://pub.example"},
					"corp": {"validate_certs": "no"},
				},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_URL":   "https://corp.example",
				"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "r4-secret-token",
			},
			check: tlsPolicyRefused("https://corp.example:443", "r4-secret-token"),
		},
	}
}

// tokenTLSPolicyCasesGroupFour is the last two rows of
// tokenTLSPolicyCases: the sentinel-ordering row (X1) and the ordinary
// no-validate_certs shape that must not regress (A6).
func tokenTLSPolicyCasesGroupFour() []tokenTLSPolicyCase {
	return []tokenTLSPolicyCase{
		{
			// the destination table's first refusal plus validate_certs = no:
			// the destination sentinel fires first and the TLS one not at all.
			name: "refused (destination, not TLS): file url and file validate_certs, env token",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{
					"corp": {"url": "https://corp.example", "validate_certs": "no"},
				},
			},
			env: map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "x1-secret-token"},
			check: func(t *testing.T, _ *Config, err error) {
				t.Helper()
				if !errors.Is(err, helpers.ErrTokenDestinationFromFile) {
					t.Fatalf("resolveServers() error = %v, want helpers.ErrTokenDestinationFromFile", err)
				}
				if errors.Is(err, helpers.ErrTokenTLSPolicyFromFile) {
					t.Errorf("resolveServers() error = %v, must not also be helpers.ErrTokenTLSPolicyFromFile", err)
				}
			},
		},
		{
			// env url + env token with no validate_certs key: an absent key
			// leaves InsecureSkipTLSVerify false, so the TLS arm must stay
			// silent.
			name: "accepted: the ordinary env url + env token shape, no validate_certs",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {}},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_URL":   "https://corp-env.example",
				"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "a6-secret-token",
			},
			check: tlsPolicyAccepted("https://corp-env.example", "a6-secret-token", false),
		},
	}
}
