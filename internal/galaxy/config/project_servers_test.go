package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/projectfile"
	"go.yaml.in/yaml/v3"
)

// The galaxy.toml fixtures every case here resolves against. The four tokens
// are plaintext that no refusal, warning or dump of the Config may ever show.
const (
	psrvTOMLPath      = "galaxy.toml"
	psrvHubID         = "hub"
	psrvHubURL        = "https://hub.example"
	psrvHubOrigin     = "https://hub.example:443"
	psrvPubID         = "pub"
	psrvPubURL        = "https://galaxy.ansible.com"
	psrvLiteralToken  = "s3cr3t-toml-token"
	psrvExpandedToken = "expanded-secret-token"
	psrvFlagToken     = "flag-secret-token"
	psrvEnvToken      = "env-secret-token"
)

// psrvCase is one galaxy.toml server scenario: the command line, environment
// and files it resolves against, its assertions over the outcome, and the
// ProjectSettingsUsed it leaves, error or not, since a refusal may follow the credit.
type psrvCase struct {
	check       func(t *testing.T, cfg *Config, err error)
	env         map[string]string
	ansCfg      ansibleConfig
	name        string
	ansiblePath string
	args        []string
	wantUsed    []string
	project     projectSettings
}

// run clears the server variables an ambient shell may export, exports tc.env,
// seeds cfg as runResolveServers does, hands resolveServers' outcome to tc.check
// and pins ProjectSettingsUsed. It calls t.Setenv, so no caller may be parallel.
func (tc psrvCase) run(t *testing.T) {
	t.Helper()
	psUnsetEnv(t, "ANSIBLE_GALAXY_SERVER_LIST", "ANSIBLE_GALAXY_SERVER", "GO_GALAXY_SERVER", "GO_GALAXY_TOKEN")
	for k, v := range tc.env {
		t.Setenv(k, v)
	}
	c := newServerCmd(t, tc.args)
	cfg := &Config{AnsibleConfigPath: tc.ansiblePath}
	serverValue, serverFromEnv := ansibleGalaxyServer(tc.ansCfg.Galaxy.Server)
	cfg.Server, cfg.AnsibleServerUsed = pickConfigValue(c, "server", serverValue)
	cfg.AnsibleServerEnvUsed = cfg.AnsibleServerUsed && serverFromEnv
	tc.check(t, cfg, resolveServers(cfg, c, tc.ansCfg, tc.project))
	if !slices.Equal(cfg.ProjectSettingsUsed, tc.wantUsed) {
		t.Errorf("ProjectSettingsUsed = %q, want %q", cfg.ProjectSettingsUsed, tc.wantUsed)
	}
}

// psrvRunCases runs every case as its own subtest, sequentially.
func psrvRunCases(t *testing.T, cases []psrvCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// psrvProject is the projectSettings of a galaxy.toml at psrvTOMLPath whose
// servers array holds entries in that order, as LoadSettings would return it.
func psrvProject(entries ...projectfile.ServerSetting) projectSettings {
	return projectSettings{Path: psrvTOMLPath, Servers: entries}
}

// psrvHubEntry is a hub entry carrying token as the file spelled it, expanded
// reporting that the spelling was a ${VAR} reference; no validate_certs key.
func psrvHubEntry(token string, expanded bool) projectfile.ServerSetting {
	return projectfile.ServerSetting{ID: psrvHubID, URL: psrvHubURL, Token: token, TokenExpanded: expanded}
}

// psrvHubAndPub is the two-entry fixture: hub then pub, no tokens, each url
// with a trailing slash that normalization must strip.
func psrvHubAndPub() projectSettings {
	return psrvProject(
		projectfile.ServerSetting{ID: psrvHubID, URL: psrvHubURL + "/"},
		projectfile.ServerSetting{ID: psrvPubID, URL: psrvPubURL + "/"},
	)
}

// psrvFileServer is the Server a token-less toml entry resolves to: the url
// and the absent token both credited to the file, as buildServer stamps them.
func psrvFileServer(id, url string) Server {
	return Server{ID: id, URL: url, sourceFile: psrvTOMLPath, urlFromFile: true, tokenFromFile: true}
}

// psrvTLSWarnings is what tlsWarnings queues for a server with certificate
// checks off and a token, verbatim; TestResolveServersTLSWarnings owns the
// wording, this file pins only that a toml entry reaches the same two lines.
func psrvTLSWarnings(id, origin string) []string {
	return []string{
		fmt.Sprintf("TLS certificate verification is DISABLED for Galaxy server %q (%s); "+
			"this run cannot detect a man-in-the-middle on that host", id, origin),
		fmt.Sprintf("A Galaxy API token is being sent to server %q (%s) over a connection whose certificate is not "+
			"verified; the token can be captured by an on-path attacker", id, origin),
	}
}

// psrvDestinationMsg is the whole ErrTokenDestinationFromFile message for
// server id at origin whose url came from file, as checkTokenPairing renders it.
func psrvDestinationMsg(id, origin, file string) string {
	return fmt.Sprintf("galaxy server token destination came from a configuration file: server %q (%s) in %s",
		id, origin, file)
}

// psrvNoPlaintext fails when text carries any fixture token: whatever refuses,
// warns about or dumps a Config must never show the credential it judged.
func psrvNoPlaintext(t *testing.T, what, text string) {
	t.Helper()
	for _, token := range []string{psrvLiteralToken, psrvExpandedToken, psrvFlagToken, psrvEnvToken} {
		if strings.Contains(text, token) {
			t.Errorf("%s = %q, must not contain the token plaintext %q", what, text, token)
		}
	}
}

// psrvWantWarnings asserts that cfg queued exactly want, in order, and that
// no warning shows a fixture token.
func psrvWantWarnings(t *testing.T, cfg *Config, want []string) {
	t.Helper()
	if !slices.Equal(cfg.Warnings, want) {
		t.Errorf("Warnings = %q, want %q", cfg.Warnings, want)
	}
	for _, w := range cfg.Warnings {
		psrvNoPlaintext(t, "warning", w)
	}
}

// psrvExactly asserts a successful resolve whose Servers equal want field by
// field, provenance bits included, with Server set to the head's URL.
func psrvExactly(want ...Server) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, cfg *Config, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		if !reflect.DeepEqual(cfg.Servers, want) {
			t.Errorf("Servers = %+v, want %+v", cfg.Servers, want)
		}
		if cfg.Server != want[0].URL {
			t.Errorf("Server = %q, want %q", cfg.Server, want[0].URL)
		}
	}
}

// psrvAccepted asserts a successful resolve of one server whose id, url,
// revealed token and TLS policy are want's, with exactly wantWarnings queued;
// provenance is psrvExactly's concern.
func psrvAccepted(want Server, wantWarnings ...string) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, cfg *Config, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		if len(cfg.Servers) != 1 {
			t.Fatalf("Servers = %+v, want exactly one", cfg.Servers)
		}
		got := cfg.Servers[0]
		if got.ID != want.ID || got.URL != want.URL {
			t.Errorf("Servers[0] = {ID: %q, URL: %q}, want {ID: %q, URL: %q}", got.ID, got.URL, want.ID, want.URL)
		}
		if got.Token.Reveal() != want.Token.Reveal() {
			t.Errorf("Servers[0].Token.Reveal() = %q, want %q", got.Token.Reveal(), want.Token.Reveal())
		}
		if got.InsecureSkipTLSVerify != want.InsecureSkipTLSVerify {
			t.Errorf("Servers[0].InsecureSkipTLSVerify = %v, want %v", got.InsecureSkipTLSVerify, want.InsecureSkipTLSVerify)
		}
		psrvWantWarnings(t, cfg, wantWarnings)
	}
}

// psrvRefused asserts resolveServers failed with want, naming each of wantIn
// in its message, showing no fixture token, and never carrying the other
// pairing sentinel beside the one wanted.
func psrvRefused(want error, wantIn ...string) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, _ *Config, err error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("resolveServers() error = %v, want %v", err, want)
		}
		msg := err.Error()
		for _, s := range wantIn {
			if !strings.Contains(msg, s) {
				t.Errorf("error = %q, want it to contain %q", msg, s)
			}
		}
		psrvNoPlaintext(t, "error", msg)
		for _, other := range []error{helpers.ErrTokenDestinationFromFile, helpers.ErrTokenTLSPolicyFromFile} {
			if !errors.Is(want, other) && errors.Is(err, other) {
				t.Errorf("error = %v, must not also be %v", err, other)
			}
		}
	}
}

// psrvRefusedExactly is psrvRefused with the whole message pinned: the shape
// the pairing rule documents, '<sentinel>: server "<id>" (<origin>) in <file>'.
func psrvRefusedExactly(want error, wantMsg string) func(t *testing.T, cfg *Config, err error) {
	inner := psrvRefused(want)
	return func(t *testing.T, cfg *Config, err error) {
		t.Helper()
		inner(t, cfg, err)
		if got := err.Error(); got != wantMsg {
			t.Errorf("error = %q, want %q", got, wantMsg)
		}
	}
}

// TestProjectServersList pins how [[tool.go-galaxy.servers]] entries become
// the server list and what outranks them. It is not parallel: rows export
// ANSIBLE_GALAXY_SERVER_LIST through t.Setenv.
func TestProjectServersList(t *testing.T) {
	psrvRunCases(t, append(psrvListCases(), psrvListSelectionCases()...))
}

// psrvListCases covers the list itself: file order, ansible.cfg set aside,
// and ANSIBLE_GALAXY_SERVER_LIST outranking the entries.
func psrvListCases() []psrvCase {
	return []psrvCase{
		{
			// The plain shape: two entries, nothing else configured. The
			// trailing slashes prove the url goes through normalizeServerURL.
			name:     "two entries resolve in file order with normalized urls and no tokens",
			project:  psrvHubAndPub(),
			check:    psrvCheckFileOrder,
			wantUsed: []string{"servers"},
		},
		{
			// A section that would hard-error if built, and a [galaxy] server
			// and server_list that would win if consulted: none of them is read.
			name: "ansible.cfg server_list and sections are not read beside toml servers",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{Server: "https://distractor.example", ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"username": "unsupported"}},
			},
			project:  psrvHubAndPub(),
			check:    psrvExactly(psrvFileServer(psrvHubID, psrvHubURL), psrvFileServer(psrvPubID, psrvPubURL)),
			wantUsed: []string{"servers"},
		},
		{
			// The exported list outranks the file's order and picks by id, but
			// the entry itself is still built from the toml section.
			name:     "ANSIBLE_GALAXY_SERVER_LIST naming a toml id builds that entry alone",
			env:      map[string]string{"ANSIBLE_GALAXY_SERVER_LIST": psrvPubID},
			project:  psrvHubAndPub(),
			check:    psrvExactly(psrvFileServer(psrvPubID, psrvPubURL)),
			wantUsed: []string{"servers"},
		},
		{
			// With toml servers present an ansible.cfg section is never a
			// fallback: an id only that file knows has no url at all.
			name: "ANSIBLE_GALAXY_SERVER_LIST naming an ansible.cfg-only id finds no section",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"url": "https://corp.example"}},
			},
			env:     map[string]string{"ANSIBLE_GALAXY_SERVER_LIST": "corp"},
			project: psrvHubAndPub(),
			check:   psrvRefused(helpers.ErrMissingGalaxyServerURL, `"corp"`),
		},
		{
			// An exported empty list means no list: the toml entries are set
			// aside and the implicit server is the --server default.
			name:    "ANSIBLE_GALAXY_SERVER_LIST exported empty leaves the implicit server",
			env:     map[string]string{"ANSIBLE_GALAXY_SERVER_LIST": ""},
			project: psrvHubAndPub(),
			check:   psrvExactly(Server{URL: testServerFlagDefault}),
		},
	}
}

// psrvListSelectionCases covers --server and --token against the toml list:
// the id branch, the url branch and the ambiguity refusal.
func psrvListSelectionCases() []psrvCase {
	hubWithToken := psrvHubEntry(psrvLiteralToken, false)
	pub := projectfile.ServerSetting{ID: psrvPubID, URL: psrvPubURL}
	return []psrvCase{
		{
			// --server=<id> selects the entry as the list path builds it,
			// its literal token included, and pub is never built.
			name:     "--server naming a toml id selects that entry with its literal token",
			args:     []string{"--server=" + psrvHubID},
			project:  psrvProject(hubWithToken, pub),
			check:    psrvAccepted(Server{ID: psrvHubID, URL: psrvHubURL, Token: NewSecret(psrvLiteralToken)}),
			wantUsed: []string{"servers"},
		},
		{
			// A --server that matches no id is an anonymous url and the
			// list is not consulted, so neither entry's token can reach it.
			name:    "--server naming a url ignores the toml list",
			args:    []string{"--server=https://anon.example"},
			project: psrvProject(hubWithToken, pub),
			check:   psrvExactly(Server{URL: "https://anon.example"}),
		},
		{
			// --token names no server, and with two in effect guessing could
			// send hub's credential to pub.
			name:     "--token with two toml servers is ambiguous",
			args:     []string{"--token=" + psrvFlagToken},
			project:  psrvProject(hubWithToken, pub),
			check:    psrvRefused(helpers.ErrAmbiguousGalaxyToken, "2 servers configured"),
			wantUsed: []string{"servers"},
		},
	}
}

// psrvCheckFileOrder asserts the plain two-entry shape: both servers in file
// order with the file credited for each, and nothing to warn about.
func psrvCheckFileOrder(t *testing.T, cfg *Config, err error) {
	t.Helper()
	psrvExactly(psrvFileServer(psrvHubID, psrvHubURL), psrvFileServer(psrvPubID, psrvPubURL))(t, cfg, err)
	psrvWantWarnings(t, cfg, nil)
}

// TestProjectServersIDs pins that toml ids and origins are judged by the
// rules an ansible.cfg server_list is held to, once the decoder's own
// exact-duplicate check has let them through.
func TestProjectServersIDs(t *testing.T) {
	psrvRunCases(t, []psrvCase{
		{
			// A dot would make ANSIBLE_GALAXY_SERVER_<ID>_* unspellable; the
			// decoder accepts it, so the refusal must come from here.
			name:    "an id with a dot is refused",
			project: psrvProject(projectfile.ServerSetting{ID: "bad.id", URL: psrvHubURL}),
			check:   psrvRefused(helpers.ErrInvalidGalaxyServerID, `"bad.id"`),
		},
		{
			// The decoder refuses only exact repeats; two ids equal up to case
			// would share one environment prefix and are refused here.
			name: "two ids differing only by case are refused",
			project: psrvProject(
				projectfile.ServerSetting{ID: "Hub", URL: psrvHubURL},
				projectfile.ServerSetting{ID: psrvHubID, URL: psrvPubURL},
			),
			check: psrvRefused(helpers.ErrDuplicateGalaxyServerID, `"Hub" and "hub"`),
		},
		{
			// The HTTP client sets TLS policy per origin, so two entries on
			// one origin cannot disagree about it whichever file names them.
			name: "two entries on one origin disagreeing on validate_certs are refused",
			project: psrvProject(
				projectfile.ServerSetting{ID: "a", URL: psrvHubURL, ValidateCerts: new(false)},
				projectfile.ServerSetting{ID: "b", URL: psrvHubOrigin, ValidateCerts: new(true)},
			),
			check: psrvRefused(helpers.ErrConflictingServerTLSPolicy, `"a" and "b" share origin `+psrvHubOrigin),
		},
	})
}

// psrvMyHub is the entry the environment-override rows start from: every
// key set in the file, so each override has a file value to displace.
func psrvMyHub() projectSettings {
	return psrvProject(projectfile.ServerSetting{
		ID: "myHub", URL: "https://toml.example", Token: psrvLiteralToken, ValidateCerts: new(true),
	})
}

// TestProjectServersEnvOverrides pins that ANSIBLE_GALAXY_SERVER_<ID>_URL,
// _TOKEN and _VALIDATE_CERTS displace a toml entry's keys one by one, <ID>
// being the id upper-cased. It is not parallel: rows call t.Setenv.
func TestProjectServersEnvOverrides(t *testing.T) {
	psrvRunCases(t, append(psrvEnvOverrideCases(), psrvEnvTokenOverrideCases()...))
}

// psrvEnvOverrideCases covers all three overrides at once, then the url and
// the validate_certs override each on its own.
func psrvEnvOverrideCases() []psrvCase {
	return []psrvCase{
		{
			// All three at once: the id "myHub" is read as MYHUB, and with
			// every half the operator's the pairing rule has nothing to say.
			name: "url, token and validate_certs override the entry, the id upper-cased",
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_MYHUB_URL":            "https://env.example",
				"ANSIBLE_GALAXY_SERVER_MYHUB_TOKEN":          psrvEnvToken,
				"ANSIBLE_GALAXY_SERVER_MYHUB_VALIDATE_CERTS": "false",
			},
			project: psrvMyHub(),
			check: psrvAccepted(
				Server{ID: "myHub", URL: "https://env.example", Token: NewSecret(psrvEnvToken), InsecureSkipTLSVerify: true},
				psrvTLSWarnings("myHub", "https://env.example:443")...,
			),
			wantUsed: []string{"servers"},
		},
		{
			// url alone: the literal token and the verified TLS policy stay.
			name:     "url alone leaves the literal token and validate_certs",
			env:      map[string]string{"ANSIBLE_GALAXY_SERVER_MYHUB_URL": "https://env.example"},
			project:  psrvMyHub(),
			check:    psrvAccepted(Server{ID: "myHub", URL: "https://env.example", Token: NewSecret(psrvLiteralToken)}),
			wantUsed: []string{"servers"},
		},
		{
			// validate_certs alone: the file's true gives way to false, and
			// the file's own literal token is exempt from the pairing rule.
			name:    "validate_certs alone relaxes what the file verified",
			env:     map[string]string{"ANSIBLE_GALAXY_SERVER_MYHUB_VALIDATE_CERTS": "false"},
			project: psrvMyHub(),
			check: psrvAccepted(
				Server{ID: "myHub", URL: "https://toml.example", Token: NewSecret(psrvLiteralToken), InsecureSkipTLSVerify: true},
				psrvTLSWarnings("myHub", "https://toml.example:443")...,
			),
			wantUsed: []string{"servers"},
		},
	}
}

// psrvEnvTokenOverrideCases covers the token override: refused against the
// file's url, since an environment token is the operator's, and carried once
// the url is exported too.
func psrvEnvTokenOverrideCases() []psrvCase {
	return []psrvCase{
		{
			// token alone displaces the literal and is the operator's, so the
			// file-sourced url it would reach is refused.
			name:    "token alone displaces the literal token and needs the url exported too",
			env:     map[string]string{"ANSIBLE_GALAXY_SERVER_MYHUB_TOKEN": psrvEnvToken},
			project: psrvMyHub(),
			check: psrvRefusedExactly(helpers.ErrTokenDestinationFromFile,
				psrvDestinationMsg("myHub", "https://toml.example:443", psrvTOMLPath)),
			wantUsed: []string{"servers"},
		},
		{
			// The remedy for the row above, and the proof that the env token
			// rather than the literal one is what the server carries.
			name: "token with the url exported overrides the literal token",
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_MYHUB_URL":   "https://env.example",
				"ANSIBLE_GALAXY_SERVER_MYHUB_TOKEN": psrvEnvToken,
			},
			project:  psrvMyHub(),
			check:    psrvAccepted(Server{ID: "myHub", URL: "https://env.example", Token: NewSecret(psrvEnvToken)}),
			wantUsed: []string{"servers"},
		},
	}
}

// TestProjectServersTokenPairing pins the pairing rule over a galaxy.toml
// server: a literal token is the file's own, a ${VAR} token and --token are
// the operator's. It is not parallel: rows call t.Setenv.
func TestProjectServersTokenPairing(t *testing.T) {
	psrvRunCases(t, append(psrvPairingDestinationCases(), psrvPairingTLSCases()...))
}

// psrvPairingDestinationCases is the destination arm: the file's literal
// token accepted, a ${VAR} token and GO_GALAXY_TOKEN refused against the
// file's url and accepted once ANSIBLE_GALAXY_SERVER_HUB_URL repeats it.
func psrvPairingDestinationCases() []psrvCase {
	return []psrvCase{
		{
			// One author wrote both halves: allowed, not recommended.
			name:     "accepted: a literal token pairs with the file's own url",
			project:  psrvProject(psrvHubEntry(psrvLiteralToken, false)),
			check:    psrvAccepted(Server{ID: psrvHubID, URL: psrvHubURL, Token: NewSecret(psrvLiteralToken)}),
			wantUsed: []string{"servers"},
		},
		{
			// The secret belongs to whoever exported the variable, the url to
			// the file's author: the graver fault, so never the TLS sentinel.
			name:    "refused: a ${VAR} token against the file's url",
			project: psrvProject(psrvHubEntry(psrvExpandedToken, true)),
			check: psrvRefusedExactly(helpers.ErrTokenDestinationFromFile,
				psrvDestinationMsg(psrvHubID, psrvHubOrigin, psrvTOMLPath)),
			wantUsed: []string{"servers"},
		},
		{
			// The remedy: the same address on the operator's channel.
			name:     "accepted: the url exported beside the ${VAR} token",
			env:      map[string]string{"ANSIBLE_GALAXY_SERVER_HUB_URL": psrvHubURL},
			project:  psrvProject(psrvHubEntry(psrvExpandedToken, true)),
			check:    psrvAccepted(Server{ID: psrvHubID, URL: psrvHubURL, Token: NewSecret(psrvExpandedToken)}),
			wantUsed: []string{"servers"},
		},
		{
			// GO_GALAXY_TOKEN is the operator's too, judged after applyTokenFlag
			// hands it to the single server.
			name:    "refused: GO_GALAXY_TOKEN against a single toml server",
			env:     map[string]string{"GO_GALAXY_TOKEN": psrvFlagToken},
			project: psrvProject(psrvHubEntry("", false)),
			check: psrvRefusedExactly(helpers.ErrTokenDestinationFromFile,
				psrvDestinationMsg(psrvHubID, psrvHubOrigin, psrvTOMLPath)),
			wantUsed: []string{"servers"},
		},
		{
			name:     "accepted: GO_GALAXY_TOKEN once the url is exported",
			env:      map[string]string{"GO_GALAXY_TOKEN": psrvFlagToken, "ANSIBLE_GALAXY_SERVER_HUB_URL": psrvHubURL},
			project:  psrvProject(psrvHubEntry("", false)),
			check:    psrvAccepted(Server{ID: psrvHubID, URL: psrvHubURL, Token: NewSecret(psrvFlagToken)}),
			wantUsed: []string{"servers"},
		},
	}
}

// psrvPairingTLSCases is the TLS arm: validate_certs = false in the file
// with a ${VAR} token is refused even once the url is exported, and accepted
// once ANSIBLE_GALAXY_SERVER_HUB_VALIDATE_CERTS repeats the relaxation.
func psrvPairingTLSCases() []psrvCase {
	insecureHub := psrvHubEntry(psrvExpandedToken, true)
	insecureHub.ValidateCerts = new(false)
	return []psrvCase{
		{
			// The url is the operator's, so the destination arm is silent
			// and the file-sourced relaxation is what is refused.
			name:    "refused: validate_certs = false from the file with a ${VAR} token",
			env:     map[string]string{"ANSIBLE_GALAXY_SERVER_HUB_URL": psrvHubURL},
			project: psrvProject(insecureHub),
			check: psrvRefusedExactly(helpers.ErrTokenTLSPolicyFromFile,
				"galaxy server certificate verification was disabled by a configuration file for a token it did not supply: "+
					`server "hub" (https://hub.example:443) in galaxy.toml`),
			wantUsed: []string{"servers"},
		},
		{
			// The remedy: the relaxation on the operator's channel too. TLS
			// stays off, so both warnings are queued, the token one included.
			name: "accepted: validate_certs exported beside the url",
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_HUB_URL":            psrvHubURL,
				"ANSIBLE_GALAXY_SERVER_HUB_VALIDATE_CERTS": "false",
			},
			project: psrvProject(insecureHub),
			check: psrvAccepted(
				Server{ID: psrvHubID, URL: psrvHubURL, Token: NewSecret(psrvExpandedToken), InsecureSkipTLSVerify: true},
				psrvTLSWarnings(psrvHubID, psrvHubOrigin)...,
			),
			wantUsed: []string{"servers"},
		},
	}
}

// TestProjectServersAnsibleConfigPathNamed pins that a run with no toml
// servers still names the discovered ansible.cfg in a pairing refusal, so the
// file the message blames is always the one that supplied the url.
func TestProjectServersAnsibleConfigPathNamed(t *testing.T) {
	const ansiblePath = "/etc/ansible/ansible.cfg"
	psrvCase{
		ansCfg: ansibleConfig{
			Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
			GalaxyServers: map[string]map[string]string{"corp": {"url": "https://corp.example"}},
		},
		ansiblePath: ansiblePath,
		env:         map[string]string{"GO_GALAXY_TOKEN": psrvFlagToken},
		check: psrvRefusedExactly(helpers.ErrTokenDestinationFromFile,
			psrvDestinationMsg("corp", "https://corp.example:443", ansiblePath)),
	}.run(t)
}

// TestProjectServersConfigDumpRedacts pins that a Config resolved from a
// galaxy.toml with a literal token renders that token nowhere: not through
// any fmt verb, nor through json or yaml.
func TestProjectServersConfigDumpRedacts(t *testing.T) {
	psrvCase{
		project:  psrvProject(psrvHubEntry(psrvLiteralToken, false)),
		check:    psrvCheckDumpRedacts,
		wantUsed: []string{"servers"},
	}.run(t)
}

// psrvCheckDumpRedacts first proves the token reached Servers[0], or the
// checks that follow would pass on an empty Config, then renders the Config
// every way a debug line or crash dump could and searches each rendering.
func psrvCheckDumpRedacts(t *testing.T, cfg *Config, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if got := cfg.Servers[0].Token.Reveal(); got != psrvLiteralToken {
		t.Fatalf("Servers[0].Token.Reveal() = %q, want %q", got, psrvLiteralToken)
	}
	dumps := []struct{ what, text string }{
		{"%v", fmt.Sprintf("%v", cfg)},
		{"%+v", fmt.Sprintf("%+v", cfg)},
		{"%#v", fmt.Sprintf("%#v", cfg)},
		{"%v of the value", fmt.Sprintf("%v", *cfg)},
		{"%+v of the value", fmt.Sprintf("%+v", *cfg)},
		{"%#v of the value", fmt.Sprintf("%#v", *cfg)},
	}
	for _, d := range dumps {
		psrvNoPlaintext(t, d.what, d.text)
	}
	// Config carries no json or yaml tags: it is runtime state, not a
	// serialization contract, and redaction must survive reflection anyway.
	jsonDump, err := json.Marshal(cfg) //nolint:musttag // see the comment above
	if err != nil {
		t.Fatalf("json.Marshal() error = %v, want nil", err)
	}
	psrvNoPlaintext(t, "json.Marshal", string(jsonDump))
	yamlDump, err := yaml.Marshal(cfg) //nolint:musttag // see the comment above
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v, want nil", err)
	}
	psrvNoPlaintext(t, "yaml.Marshal", string(yamlDump))
}
