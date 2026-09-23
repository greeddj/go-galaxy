package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// Secret wraps a credential so fmt, encoding/json and yaml.v3 all render a
// redacted placeholder; the plaintext is reachable only through Reveal. No
// credential or value derived from one, not even a hash, may be persisted.
type Secret struct {
	value string
}

// NewSecret wraps value in a Secret. It is the only way to construct a
// non-zero Secret from outside this package, since the field is
// unexported.
func NewSecret(value string) Secret {
	return Secret{value: value}
}

// IsSet reports whether the secret carries a non-empty token.
func (s Secret) IsSet() bool {
	return s.value != ""
}

// Reveal returns the plaintext token. Call it only where the plaintext is
// needed, such as setting an Authorization header, never for a log line or
// anything persisted.
func (s Secret) Reveal() string {
	return s.value
}

// String implements fmt.Stringer, which fmt consults for %v, %s, %q, %x, %X
// and %+v, so a Secret never prints its plaintext.
func (s Secret) String() string {
	return s.redacted()
}

// GoString implements fmt.GoStringer, covering the one verb String does
// not: %#v. Without it, %#v would fall back to reflecting into the
// unexported value field regardless of exported-ness.
func (s Secret) GoString() string {
	return s.redacted()
}

// MarshalJSON implements json.Marshaler so a Secret embedded in any
// JSON-serialized structure renders the redacted placeholder instead of
// the plaintext.
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.redacted())
}

// MarshalYAML implements yaml.Marshaler (go.yaml.in/yaml/v3, already a
// project dependency) for the same reason as MarshalJSON, covering
// GALAXY.yml-shaped output.
func (s Secret) MarshalYAML() (any, error) {
	return s.redacted(), nil
}

// sameAs reports whether two secrets carry the same token, so config
// validation can compare credentials without calling Reveal.
func (s Secret) sameAs(other Secret) bool {
	return s.value == other.value
}

// redacted renders the single placeholder every redaction path shares,
// distinguishing "no token configured" from "token configured but hidden"
// without ever branching on the plaintext's actual content.
func (s Secret) redacted() string {
	if s.value == "" {
		return "[unset]"
	}
	return "[REDACTED]"
}

// Server is one configured Galaxy server: its normalized endpoint and the
// credential and TLS policy applied to it. ID is "" for the implicit single
// server and the server_list id as written otherwise.
type Server struct {
	ID                    string
	URL                   string
	Token                 Secret
	InsecureSkipTLSVerify bool
	// urlFromAnsibleConfig reports whether URL came from an ansible.cfg file
	// rather than an operator channel. Provenance never leaves this package;
	// tokenPairingOffense is the sole reader.
	urlFromAnsibleConfig bool
	// tokenFromAnsibleConfig reports whether Token came from a section's token
	// key; it also reads true when no token was set at all, which is
	// harmless only because tokenPairingOffense checks Token.IsSet() first.
	tokenFromAnsibleConfig bool
	// insecureFromAnsibleConfig is true only when InsecureSkipTLSVerify is true
	// and came from a validate_certs key rather than the environment, so a
	// server with no validate_certs key never reads as file-sourced.
	insecureFromAnsibleConfig bool
}

// galaxyServerHardErrorKeys are ansible's Basic auth and Keycloak keys, which
// this tool lacks; they fail config loading instead of causing a later 401.
//
//nolint:gochecknoglobals // a fixed, immutable lookup table, not mutable shared state
var galaxyServerHardErrorKeys = map[string]bool{
	"username":  true,
	"password":  true,
	"auth_url":  true,
	"client_id": true,
}

// galaxyServerKnownKeys are every [galaxy_server.<id>] key this resolver
// recognizes; any other key only warns, since ansible may understand it.
//
//nolint:gochecknoglobals // a fixed, immutable lookup table, not mutable shared state
var galaxyServerKnownKeys = map[string]bool{
	"url":            true,
	"token":          true,
	"validate_certs": true,
	"api_version":    true,
	"username":       true,
	"password":       true,
	"auth_url":       true,
	"client_id":      true,
}

// serverIDPattern restricts server ids beyond ansible: a "." would make the
// section grammar ambiguous, and other characters cannot fold into an
// ANSIBLE_GALAXY_SERVER_<ID>_* variable name.
var serverIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// resolveServers computes cfg.Servers, never empty on success, and sets
// cfg.Server to its head. An explicit --server wins, then server_list, then
// the cfg.Server that applyAnsibleConfig resolved, so that must run first.
func resolveServers(cfg *Config, c *cli.Command, ansCfg ansibleConfig) error {
	ids := resolveServerList(ansCfg)

	servers, err := resolveServerCandidates(cfg, c, ids, ansCfg.GalaxyServers)
	if err != nil {
		return err
	}
	if err := applyTokenFlag(c, servers); err != nil {
		return err
	}
	// checkTokenPairing must follow applyTokenFlag, which is what assigns an
	// operator token to servers[0].
	if err := checkTokenPairing(servers); err != nil {
		return err
	}

	cfg.Servers = servers
	cfg.Server = servers[0].URL
	cfg.Warnings = append(cfg.Warnings, tlsWarnings(servers)...)
	return nil
}

// applyTokenFlag installs an explicitly set --token or GO_GALAXY_TOKEN as the
// single effective server's operator-sourced token (an empty value clears it);
// with several servers it is an error, since the token could reach any hub.
func applyTokenFlag(c *cli.Command, servers []Server) error {
	if !c.IsSet("token") {
		return nil
	}
	if len(servers) > 1 {
		return fmt.Errorf("%w: %d servers configured", helpers.ErrAmbiguousGalaxyToken, len(servers))
	}
	if len(servers) == 0 {
		return nil
	}

	token := NewSecret(strings.TrimSpace(c.String("token")))
	_, parsed, err := normalizeServerURL(servers[0].URL)
	if err != nil {
		return fmt.Errorf("%w: server %q", helpers.ErrInvalidGalaxyServerURL, servers[0].ID)
	}
	if err := checkTokenTransport(servers[0].ID, token, parsed); err != nil {
		return err
	}
	servers[0].Token = token
	servers[0].tokenFromAnsibleConfig = false
	return nil
}

// resolveServerCandidates applies the precedence chain of resolveServers and
// returns the origin-checked server list; non-fatal unknown-key warnings go
// straight to cfg.Warnings.
func resolveServerCandidates(
	cfg *Config, c *cli.Command, ids []string, sections map[string]map[string]string,
) ([]Server, error) {
	if c.IsSet("server") {
		server, warnings, err := resolveExplicitServer(c.String("server"), ids, sections)
		cfg.Warnings = append(cfg.Warnings, warnings...)
		if err != nil {
			return nil, err
		}
		return []Server{server}, nil
	}

	if len(ids) > 0 {
		if err := validateServerIDs(ids); err != nil {
			return nil, err
		}
		servers, warnings, err := buildServerList(ids, sections)
		cfg.Warnings = append(cfg.Warnings, warnings...)
		if err != nil {
			return nil, err
		}
		if err := checkOriginConflicts(servers); err != nil {
			return nil, err
		}
		return servers, nil
	}

	// The ansible.cfg file itself, not $ANSIBLE_GALAXY_SERVER, supplied
	// cfg.Server when AnsibleServerUsed holds and AnsibleServerEnvUsed does not.
	server, err := buildImplicitServer(cfg.Server, cfg.AnsibleServerUsed && !cfg.AnsibleServerEnvUsed)
	if err != nil {
		return nil, err
	}
	return []Server{server}, nil
}

// resolveExplicitServer selects the server_list entry whose id equals value
// exactly, built as the list path builds it; any other value is an anonymous
// URL, and the rest of server_list is never consulted or validated.
func resolveExplicitServer(value string, ids []string, sections map[string]map[string]string) (Server, []string, error) {
	for _, id := range ids {
		if id != value {
			continue
		}
		if !serverIDPattern.MatchString(id) {
			return Server{}, nil, fmt.Errorf("%w: %q", helpers.ErrInvalidGalaxyServerID, id)
		}
		return buildServer(id, sections[id])
	}
	// value is --server's own value or GO_GALAXY_SERVER - both operator
	// channels - so the resulting anonymous server's URL is never
	// ansible.cfg-sourced.
	server, err := buildImplicitServer(value, false)
	return server, nil, err
}

// resolveServerList returns the trimmed, non-empty ids of server_list, with
// ANSIBLE_GALAXY_SERVER_LIST winning whenever it is set, even to "". A
// whitespace-only value is treated as unset.
func resolveServerList(ansCfg ansibleConfig) []string {
	raw := ansCfg.Galaxy.ServerList
	if v, ok := os.LookupEnv("ANSIBLE_GALAXY_SERVER_LIST"); ok {
		raw = v
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var ids []string
	for p := range strings.SplitSeq(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			ids = append(ids, p)
		}
	}
	return ids
}

// validateServerIDs requires every id to match serverIDPattern and refuses
// two ids equal up to case, which would share one
// ANSIBLE_GALAXY_SERVER_<ID>_* environment prefix.
func validateServerIDs(ids []string) error {
	seen := make(map[string]string, len(ids))
	for _, id := range ids {
		if !serverIDPattern.MatchString(id) {
			return fmt.Errorf("%w: %q", helpers.ErrInvalidGalaxyServerID, id)
		}
		lower := strings.ToLower(id)
		if prior, ok := seen[lower]; ok {
			return fmt.Errorf("%w: %q and %q", helpers.ErrDuplicateGalaxyServerID, prior, id)
		}
		seen[lower] = id
	}
	return nil
}

// buildServerList builds every server_list id in order, collecting
// unknown-key warnings, and stops at the first hard error with the warnings
// gathered so far.
func buildServerList(ids []string, sections map[string]map[string]string) ([]Server, []string, error) {
	servers := make([]Server, 0, len(ids))
	var warnings []string
	for _, id := range ids {
		server, w, err := buildServer(id, sections[id])
		warnings = append(warnings, w...)
		if err != nil {
			return nil, warnings, err
		}
		servers = append(servers, server)
	}
	return servers, warnings, nil
}

// buildServer resolves one [galaxy_server.<id>] section into a Server. kv may
// be nil: an id with no ini section builds from its
// ANSIBLE_GALAXY_SERVER_<ID>_* environment overrides alone.
func buildServer(id string, kv map[string]string) (Server, []string, error) {
	keys := sortedKeys(kv)

	if err := checkHardErrorKeys(id, keys); err != nil {
		return Server{}, nil, err
	}
	if v, ok := kv["api_version"]; ok && v != "v3" {
		return Server{}, nil, fmt.Errorf("%w: server %q api_version %q", helpers.ErrUnsupportedGalaxyServerAPIVersion, id, v)
	}
	warnings := unknownKeyWarnings(id, keys)

	rawURL, urlFromEnv := envOrIni(id, "URL", kv["url"])
	normalized, parsed, err := resolveServerURL(id, rawURL)
	if err != nil {
		return Server{}, warnings, err
	}

	rawToken, tokenFromEnv := envOrIni(id, "TOKEN", kv["token"])
	token := NewSecret(rawToken)
	rawValidateCerts, validateCertsFromEnv := envOrIni(id, "VALIDATE_CERTS", kv["validate_certs"])
	insecure, err := resolveValidateCerts(id, rawValidateCerts)
	if err != nil {
		return Server{}, warnings, err
	}

	if err := checkTokenTransport(id, token, parsed); err != nil {
		return Server{}, warnings, err
	}

	return Server{
		ID: id, URL: normalized, Token: token, InsecureSkipTLSVerify: insecure,
		urlFromAnsibleConfig:      !urlFromEnv,
		tokenFromAnsibleConfig:    !tokenFromEnv,
		insecureFromAnsibleConfig: insecure && !validateCertsFromEnv,
	}, warnings, nil
}

// checkHardErrorKeys returns ErrUnsupportedGalaxyServerKey for the first
// hard-error key in keys, which are sorted so the outcome is deterministic.
func checkHardErrorKeys(id string, keys []string) error {
	for _, key := range keys {
		if galaxyServerHardErrorKeys[key] {
			return fmt.Errorf("%w: server %q key %q", helpers.ErrUnsupportedGalaxyServerKey, id, key)
		}
	}
	return nil
}

// unknownKeyWarnings returns one warning per key in keys that
// galaxyServerKnownKeys does not recognize, in sorted order for
// deterministic output.
func unknownKeyWarnings(id string, keys []string) []string {
	var warnings []string
	for _, key := range keys {
		if !galaxyServerKnownKeys[key] {
			warnings = append(warnings, fmt.Sprintf("unsupported key %q in [galaxy_server.%s] ignored", key, id))
		}
	}
	return warnings
}

// resolveServerURL requires and normalizes the url configured for server
// id, returning both the normalized string and its parsed form so callers
// need not reparse it for the userinfo and origin checks.
func resolveServerURL(id, raw string) (string, *url.URL, error) {
	if raw == "" {
		return "", nil, fmt.Errorf("%w: server %q", helpers.ErrMissingGalaxyServerURL, id)
	}
	normalized, parsed, err := normalizeServerURL(raw)
	if err != nil {
		return "", nil, fmt.Errorf("%w: server %q", helpers.ErrInvalidGalaxyServerURL, id)
	}
	if parsed.User != nil {
		return "", nil, fmt.Errorf("%w: server %q", helpers.ErrGalaxyServerURLUserinfo, id)
	}
	return normalized, parsed, nil
}

// resolveValidateCerts turns an env/ini-resolved validate_certs value into
// InsecureSkipTLSVerify: unset means certs verified, and an unparseable value
// is a hard error.
func resolveValidateCerts(id, raw string) (bool, error) {
	if raw == "" {
		return false, nil
	}
	validate, ok := parseAnsibleBool(raw)
	if !ok {
		return false, fmt.Errorf("%w: server %q value %q", helpers.ErrInvalidValidateCerts, id, raw)
	}
	return !validate, nil
}

// parseAnsibleBool parses one of ansible's boolean spellings,
// case-insensitively: true/false, yes/no, on/off, 1/0. ok is false for
// anything else.
func parseAnsibleBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "yes", "on", "1":
		return true, true
	case "false", "no", "off", "0":
		return false, true
	default:
		return false, false
	}
}

// envOrIni resolves one per-server key: ANSIBLE_GALAXY_SERVER_<ID>_<KEY>, with
// the id upper-cased as ansible composes it, wins when set at all, even to
// "". The bool reports that the environment supplied it, for the pairing rule.
func envOrIni(id, key, iniValue string) (string, bool) {
	if v, ok := os.LookupEnv("ANSIBLE_GALAXY_SERVER_" + strings.ToUpper(id) + "_" + key); ok {
		return v, true
	}
	return iniValue, false
}

// buildImplicitServer builds the anonymous server used when no server_list
// entry applies. An empty rawURL yields a zero Server rather than an error,
// since a command that registers no --server flag, such as cleanup, reads "".
func buildImplicitServer(rawURL string, urlFromAnsibleConfig bool) (Server, error) {
	if rawURL == "" {
		return Server{}, nil
	}
	normalized, parsed, err := normalizeServerURL(rawURL)
	if err != nil {
		return Server{}, helpers.ErrInvalidGalaxyServerURL
	}
	if parsed.User != nil {
		return Server{}, helpers.ErrGalaxyServerURLUserinfo
	}
	return Server{URL: normalized, urlFromAnsibleConfig: urlFromAnsibleConfig}, nil
}

// normalizeServerURL trims raw, strips one pair of surrounding double quotes
// that a value copied from a shell export often carries, trims trailing
// slashes, and requires an absolute URL, returned parsed as well.
func normalizeServerURL(raw string) (string, *url.URL, error) {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		v = v[1 : len(v)-1]
	}
	v = strings.TrimRight(v, "/")

	parsed, err := url.Parse(v)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", nil, helpers.ErrInvalidGalaxyServerURL
	}
	return v, parsed, nil
}

// checkTokenTransport refuses a token for a plaintext http origin unless the
// host is loopback: over http any on-path observer reads the credential. id
// is "" for the implicit single server.
func checkTokenTransport(id string, token Secret, parsed *url.URL) error {
	if !token.IsSet() || parsed == nil {
		return nil
	}
	if !strings.EqualFold(parsed.Scheme, "http") || isLoopbackHost(parsed.Hostname()) {
		return nil
	}
	return fmt.Errorf("%w: server %q (%s)", helpers.ErrInsecureTokenTransport, id, helpers.Origin(parsed))
}

// isLoopbackHost reports whether host is "localhost" or a loopback IP literal.
// It is a syntactic check: DNS is never consulted, so a name that resolves to
// loopback through /etc/hosts does not count.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// checkOriginConflicts requires servers sharing a normalized origin to agree
// on validate_certs and token, since the HTTP client resolves TLS policy and
// credentials per origin, not per server id.
func checkOriginConflicts(servers []Server) error {
	seen := make(map[string]Server, len(servers))
	for _, s := range servers {
		parsed, err := url.Parse(s.URL)
		if err != nil {
			// s.URL was already normalized and validated by buildServer; an
			// already-valid absolute URL string always reparses cleanly.
			return fmt.Errorf("%w: server %q", helpers.ErrInvalidGalaxyServerURL, s.ID)
		}
		origin := helpers.Origin(parsed)

		prior, ok := seen[origin]
		if !ok {
			seen[origin] = s
			continue
		}
		if prior.InsecureSkipTLSVerify != s.InsecureSkipTLSVerify {
			return fmt.Errorf("%w: %q and %q share origin %s", helpers.ErrConflictingServerTLSPolicy, prior.ID, s.ID, origin)
		}
		if !prior.Token.sameAs(s.Token) {
			return fmt.Errorf("%w: %q and %q share origin %s", helpers.ErrConflictingServerToken, prior.ID, s.ID, origin)
		}
	}
	return nil
}

// tokenPairingOffense reports which pairing violation s commits, if any: an
// operator token sent to a file-sourced URL, checked first as the graver
// fault, or over a file-disabled TLS check. A section's own token is exempt.
func tokenPairingOffense(s Server) error {
	if !s.Token.IsSet() || s.tokenFromAnsibleConfig {
		return nil
	}
	if s.urlFromAnsibleConfig {
		return helpers.ErrTokenDestinationFromAnsibleConfig
	}
	if s.insecureFromAnsibleConfig {
		return helpers.ErrTokenTLSPolicyFromAnsibleConfig
	}
	return nil
}

// checkTokenPairing refuses an operator-supplied token paired with a server
// URL or relaxed TLS policy an ansible.cfg file chose, since a checked-out
// repository could otherwise pick where the credential goes.
func checkTokenPairing(servers []Server) error {
	for _, s := range servers {
		offense := tokenPairingOffense(s)
		if offense == nil {
			continue
		}
		parsed, err := url.Parse(s.URL)
		if err != nil {
			// s.URL was already normalized and validated by buildServer or
			// buildImplicitServer; an already-valid absolute URL string
			// always reparses cleanly.
			return fmt.Errorf("%w: server %q", helpers.ErrInvalidGalaxyServerURL, s.ID)
		}
		return fmt.Errorf("%w: server %q (%s)", offense, s.ID, helpers.Origin(parsed))
	}
	return nil
}

// tlsWarnings returns one warning per server with certificate verification
// disabled, plus one more when that server also carries a token. Tests
// assert the wording verbatim.
func tlsWarnings(servers []Server) []string {
	var warnings []string
	for _, s := range servers {
		if !s.InsecureSkipTLSVerify {
			continue
		}
		parsed, err := url.Parse(s.URL)
		if err != nil {
			continue // s.URL was already validated when the server was built.
		}
		origin := helpers.Origin(parsed)
		warnings = append(warnings, fmt.Sprintf(
			"TLS certificate verification is DISABLED for Galaxy server %q (%s); "+
				"this run cannot detect a man-in-the-middle on that host", s.ID, origin))
		if s.Token.IsSet() {
			warnings = append(warnings, fmt.Sprintf(
				"A Galaxy API token is being sent to server %q (%s) over a connection whose certificate is not "+
					"verified; the token can be captured by an on-path attacker", s.ID, origin))
		}
	}
	return warnings
}

// sortedKeys returns kv's keys sorted, so key errors and warnings are
// deterministic; kv may be nil.
func sortedKeys(kv map[string]string) []string {
	return slices.Sorted(maps.Keys(kv))
}
