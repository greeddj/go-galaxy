package config

import (
	"bufio"
	"io"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ansibleBOM is a leading UTF-8 byte order mark. ansible refuses such a file
// ("File contains no section headers"); this reader strips the mark instead,
// the same leniency it shows every other line configparser refuses.
const ansibleBOM = "\uFEFF"

// signatureKeyNames are ansible's [galaxy] signature keys, recognized by name
// only: a file of unknown authorship must not relax verification.
//
//nolint:gochecknoglobals // a fixed, immutable name table, not mutable shared state.
var signatureKeyNames = [...]string{
	"gpg_keyring",
	"required_valid_signature_count",
	"ignore_signature_status_codes",
	"disable_gpg_verify",
}

// ansibleGalaxyConfig maps the [galaxy] section from ansible.cfg (INI).
type ansibleGalaxyConfig struct {
	CacheDir   string
	Server     string
	ServerList string
	// ServerTimeout is server_timeout exactly as written, judged only by
	// applyAnsibleTimeout, since only a command with a request budget uses it.
	ServerTimeout string
	// SignatureKeys names the signature keys this file carried and never a
	// value: storing no value is the security property, so no later change
	// can honor an ansible.cfg signature setting by accident.
	SignatureKeys []string
}

// ansibleDefaultsConfig maps the [defaults] section from ansible.cfg (INI).
type ansibleDefaultsConfig struct {
	CollectionsPath string
	RolesPath       string
}

// ansibleConfig represents the subset of ansible.cfg (INI) sections this
// tool understands: [defaults], [galaxy], and any [galaxy_server.<id>].
type ansibleConfig struct {
	GalaxyServers map[string]map[string]string
	Defaults      ansibleDefaultsConfig
	Galaxy        ansibleGalaxyConfig
}

// parseAnsibleConfig reads the keys this tool uses from an ansible.cfg,
// mirroring ansible's ConfigParser(inline_comment_prefixes=(';',)) for
// drop-in fidelity: a value keeps its quotes and any '#' that follows it.
func parseAnsibleConfig(r io.Reader) (ansibleConfig, error) {
	cfg := ansibleConfig{}
	section := ""

	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			line = strings.TrimPrefix(line, ansibleBOM)
			first = false
		}

		t := strings.TrimFunc(line, isINISpace)
		if t == "" || isCommentLine(t) {
			continue
		}
		t = stripInlineComment(t)

		if name, ok := sectionName(t); ok {
			section = name
			continue
		}

		if key, value, ok := splitKeyValue(t); ok {
			assignAnsibleValue(&cfg, section, key, value)
		} else if strings.HasPrefix(t, "[") {
			// A broken header closes the section, or a url written for one
			// [galaxy_server.<id>] would land in the section above it and be
			// sent that server's token.
			section = ""
		}
	}
	if err := sc.Err(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// isCommentLine reports whether t is a full-line comment. ansible.cfg
// accepts both '#' and ';' as comment markers.
func isCommentLine(t string) bool {
	return strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";")
}

// stripInlineComment cuts t at the first ';' that follows whitespace, as
// configparser does on every line, headers included; a glued ';' stays, so
// "token = abc;def" keeps its value. t must be trimmed and not a comment line.
func stripInlineComment(t string) string {
	for i, r := range t {
		if r != ';' || i == 0 {
			continue
		}
		if prev, _ := utf8.DecodeLastRuneInString(t[:i]); isINISpace(prev) {
			return strings.TrimRightFunc(t[:i], isINISpace)
		}
	}
	return t
}

// isINISpace reports whether r is whitespace to Python's str.isspace, which
// configparser uses both to trim and to find a ';' comment: unicode.IsSpace
// plus the information separators U+001C through U+001F.
func isINISpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= '\x1c' && r <= '\x1f')
}

// sectionName reports whether t is a section header, as configparser's SECTCRE
// matches one, and returns its name: it runs to the last ']' and is taken as
// written, neither trimmed nor case-folded; text after that ']' is ignored.
func sectionName(t string) (string, bool) {
	if !strings.HasPrefix(t, "[") {
		return "", false
	}
	end := strings.LastIndexByte(t, ']')
	if end < len("[x") {
		return "", false
	}
	return t[1:end], true
}

// splitKeyValue splits t at whichever of '=' or ':' comes first, so the
// colon inside "server = https://x" stays in the value; the key is lowercased.
func splitKeyValue(t string) (string, string, bool) {
	eq := strings.IndexByte(t, '=')
	colon := strings.IndexByte(t, ':')

	idx := eq
	switch {
	case eq == -1:
		idx = colon
	case colon != -1 && colon < eq:
		idx = colon
	}
	if idx == -1 {
		return "", "", false
	}

	key := strings.ToLower(strings.TrimFunc(t[:idx], isINISpace))
	value := strings.TrimFunc(t[idx+1:], isINISpace)
	return key, value, key != ""
}

// galaxyServerSectionPrefix is the fixed prefix of a per-server
// configuration section header, "[galaxy_server.<id>]"; everything after
// it is the server's id.
const galaxyServerSectionPrefix = "galaxy_server."

// assignAnsibleValue stores the (section, key) pairs this tool reads, last
// occurrence winning. A [galaxy_server.<id>] section is kept whole, since
// which of its keys are errors or warnings is the server resolver's decision.
func assignAnsibleValue(cfg *ansibleConfig, section, key, value string) {
	switch section {
	case "defaults":
		assignDefaultsValue(&cfg.Defaults, key, value)
	case "galaxy":
		switch key {
		case "cache_dir":
			cfg.Galaxy.CacheDir = value
		case "server":
			cfg.Galaxy.Server = value
		case "server_list":
			cfg.Galaxy.ServerList = value
		case "server_timeout":
			cfg.Galaxy.ServerTimeout = value
		default:
			recordSignatureKey(cfg, key)
		}
	default:
		if id, ok := strings.CutPrefix(section, galaxyServerSectionPrefix); ok {
			assignGalaxyServerValue(cfg, id, key, value)
		}
	}
}

// recordSignatureKey records, once, that [galaxy] named a signature key. It
// takes no value parameter, so no ansible.cfg signature value can reach
// Config through it.
func recordSignatureKey(cfg *ansibleConfig, key string) {
	if !slices.Contains(signatureKeyNames[:], key) {
		return
	}
	if slices.Contains(cfg.Galaxy.SignatureKeys, key) {
		return
	}
	cfg.Galaxy.SignatureKeys = append(cfg.Galaxy.SignatureKeys, key)
}

// assignGalaxyServerValue stores key/value for a [galaxy_server.<id>]
// section, allocating both maps lazily; a later occurrence wins.
func assignGalaxyServerValue(cfg *ansibleConfig, id, key, value string) {
	if cfg.GalaxyServers == nil {
		cfg.GalaxyServers = make(map[string]map[string]string)
	}
	if cfg.GalaxyServers[id] == nil {
		cfg.GalaxyServers[id] = make(map[string]string)
	}
	cfg.GalaxyServers[id][key] = value
}

// assignDefaultsValue records a [defaults] key this tool reads: the two
// install roots, each a search list ansible walks and this tool takes the
// first entry of.
func assignDefaultsValue(cfg *ansibleDefaultsConfig, key, value string) {
	switch key {
	case "collections_path":
		cfg.CollectionsPath = value
	case "roles_path":
		cfg.RolesPath = value
	}
}
