package config

import (
	"bufio"
	"io"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ansibleBOM is the leading UTF-8 byte order mark some ansible.cfg files
// carry (e.g. when authored by editors that default to BOM-prefixed UTF-8).
// ansible's own configparser-based reader tolerates it, so we strip it too.
const ansibleBOM = "\uFEFF"

// signatureKeyNames are the four [galaxy] keys ansible reads its signature
// policy from. This program deliberately reads none of their VALUES (see
// applySignatureConfig and cliflags.SignatureFlags for why a setting that can
// relax a verification check must not come from a file whose author this
// program cannot establish), so the array exists to recognize the names and
// nothing else.
//
// Adding a fifth key ansible learns is one edit here: assignAnsibleValue tests
// membership in this array and the warning renders it by filtering this same
// array, so neither has a list of its own to keep current.
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
	// ServerTimeout is server_timeout exactly as written; it is judged only
	// where it is applied, by applyAnsibleTimeout, since only a command with a
	// request budget has any use for it.
	ServerTimeout string
	// SignatureKeys names the signature keys this file carried, and holds no
	// value any of them was set to. Recording the NAME and never the value is
	// the security property rather than an economy: no ansible.cfg-sourced
	// signature value enters Config at all, so no later change can accidentally
	// honor one - it would first have to teach the parser to read a value it
	// currently never stores. What the names buy is the one thing silence costs
	// an operator: a run that verifies nothing while their keyring sits in
	// ~/.ansible.cfg gets told which keys were ignored.
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

// parseAnsibleConfig reads an ansible.cfg (INI-style) file and extracts the
// handful of keys this tool cares about. It deliberately mirrors CPython's
// configparser as ansible constructs it, ConfigParser(inline_comment_prefixes=
// (';',)), since ansible.cfg is not TOML and we aim for drop-in fidelity with
// how ansible itself reads it: values are not unquoted, and the only inline
// comment is the one stripInlineComment removes, so a '#' after a value stays
// part of it.
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
			// A line that opens like a header yet is neither a header nor a key
			// line - one never closed, or one whose ';' comment cut off its ']'
			// - is a line configparser refuses. Keeping the section it followed
			// would file every key below it there, so a url written for one
			// [galaxy_server.<id>] would land in another and be sent that
			// server's token. Closing the section leaves those keys under none.
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

// stripInlineComment removes an inline comment from t, the way configparser
// applies inline_comment_prefixes=(';',) to every line, section headers
// included: the comment is the first ';' that follows whitespace, and it runs
// to the end of the line, taking the whitespace before it along. A ';' glued
// to the text before it is not a comment, so "token = abc;def" keeps its whole
// value, exactly as ansible reads it.
//
// t must be a trimmed line isCommentLine does not match, which is what
// parseAnsibleConfig passes: a ';' opening the line, configparser's other
// inline case, is then a full-line comment already skipped, and the result is
// never empty, since t's first rune is not whitespace.
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

// isINISpace reports whether r is whitespace as configparser judges it, where
// both str.strip and the \s of its comment pattern follow Python's
// str.isspace. That is unicode.IsSpace plus the four information separators
// U+001C through U+001F, which Python counts as whitespace and Go does not;
// using one predicate for trimming and for recognizing a comment keeps the
// two in step, as they are in configparser.
func isINISpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= '\x1c' && r <= '\x1f')
}

// sectionName reports whether t is a "[section]" header and, if so, returns
// its trimmed inner name. Section names are matched case-sensitively.
func sectionName(t string) (string, bool) {
	if !strings.HasPrefix(t, "[") || !strings.HasSuffix(t, "]") {
		return "", false
	}
	name := strings.TrimSpace(t[1 : len(t)-1])
	return name, name != ""
}

// splitKeyValue splits t on the first '=' or ':' delimiter, whichever
// appears first in the line, into a (key, value, ok) triple. This matters
// for values that themselves contain a colon, e.g. "server = https://x"
// must split on '=', not on the colon inside the URL.
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

// assignAnsibleValue stores value into cfg for the known (section, key)
// pairs this tool consumes; anything else, including keys seen before any
// section header, is ignored. Later occurrences win over earlier ones. A
// section matching "galaxy_server.<id>" is captured in full via
// assignGalaxyServerValue rather than a fixed key whitelist, since the set
// of keys to recognize (and which ones are errors vs. warnings) is a
// concern of the config resolver, not this parser.
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

// recordSignatureKey records that the [galaxy] section named one of ansible's
// signature keys, so a later warning can say which. The VALUE is not a
// parameter here, which is what makes "no ansible.cfg-sourced signature value
// enters Config" a property of this function's signature rather than of its
// body.
//
// A file carrying none of them - the overwhelmingly common case - pays four
// string comparisons per unrecognized [galaxy] key and allocates nothing, since
// the slice stays nil. The dedupe is a linear scan because the slice can hold
// at most four elements, so a repeated key costs a scan of at most three.
func recordSignatureKey(cfg *ansibleConfig, key string) {
	if !slices.Contains(signatureKeyNames[:], key) {
		return
	}
	if slices.Contains(cfg.Galaxy.SignatureKeys, key) {
		return
	}
	cfg.Galaxy.SignatureKeys = append(cfg.Galaxy.SignatureKeys, key)
}

// assignGalaxyServerValue stores key/value into the per-id map for a
// "[galaxy_server.<id>]" section, allocating the outer and inner maps
// lazily. Later occurrences of the same key within the same id win, same
// as every other key this parser tracks.
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
