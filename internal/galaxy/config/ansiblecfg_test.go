package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// parseAnsibleConfigCase is one table-driven case shared by the
// TestParseAnsibleConfig* functions below.
type parseAnsibleConfigCase struct {
	name  string
	input string
	want  ansibleConfig
}

// runParseAnsibleConfigCases feeds each case's input through
// parseAnsibleConfig and compares the result to want.
func runParseAnsibleConfigCases(t *testing.T, cases []parseAnsibleConfigCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseAnsibleConfig(strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("parseAnsibleConfig() error = %v, want nil", err)
			}
			// ansibleConfig carries GalaxyServers, a map field, so it is not
			// comparable with !=; reflect.DeepEqual is the equivalent
			// structural check.
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseAnsibleConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestParseAnsibleConfigDelimiters checks that both '=' and ':' are
// accepted as key/value delimiters and that the first delimiter in the
// line wins, so a URL value's own colon is never mistaken for one.
func TestParseAnsibleConfigDelimiters(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "unquoted value",
			input: "[defaults]\ncollections_path = ./collections",
			want:  ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "./collections"}},
		},
		{
			name:  "equals separator",
			input: "[galaxy]\nserver = https://x",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "colon separator",
			input: "[galaxy]\nserver : https://x",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "url keeps its colon",
			input: "[galaxy]\nserver = https://example.com:8080/path",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://example.com:8080/path"}},
		},
	})
}

// TestParseAnsibleConfigValueFidelity checks that values are stored as
// ansible reads them: quotes are kept and a trailing '#' is part of the value.
// ansible.cfg is read by CPython's configparser, not a TOML parser, so
// drop-in fidelity requires reproducing that behavior rather than
// "helpfully" cleaning it up. The one thing configparser does strip, a ';'
// comment, is TestParseAnsibleConfigInlineSemicolonComment's.
func TestParseAnsibleConfigValueFidelity(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "quoted value preserved verbatim",
			input: `[defaults]` + "\n" + `collections_path = "./c"`,
			want:  ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: `"./c"`}},
		},
		{
			name:  "inline trailing hash comment preserved",
			input: "[galaxy]\nserver = https://x # prod",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x # prod"}},
		},
	})
}

// TestParseAnsibleConfigInlineSemicolonComment pins the inline comment ansible
// configures its parser with, inline_comment_prefixes=(';',): a ';' that
// follows whitespace starts a comment running to the end of the line, on a key
// line and on a section header alike, while a ';' glued to the text before it
// stays in the value. Every want below is what CPython 3.14's
// ConfigParser(inline_comment_prefixes=(';',)) returned for the same input.
//
// The "glued" rows are the positive control for the stripping rows: a parser
// that cut at every ';' would pass the first rows and fail these, which is why
// a URL query or a token carrying ';' is here beside the comments.
//
// KILLING MUTATION, run and reverted, in parseAnsibleConfig (ansiblecfg.go) -
// delete the stripInlineComment call. Every stripping row fails, the glued
// rows pass:
//
//	ansiblecfg_test.go:35: parseAnsibleConfig() = {GalaxyServers:map[]
//	Defaults:{CollectionsPath: RolesPath:} Galaxy:{CacheDir:/c ; note Server:
//	ServerList: ServerTimeout: SignatureKeys:[]}}, want {GalaxyServers:map[]
//	Defaults:{CollectionsPath: RolesPath:} Galaxy:{CacheDir:/c Server:
//	ServerList: ServerTimeout: SignatureKeys:[]}}
//
// KILLING MUTATION, run and reverted, in isINISpace (ansiblecfg.go) - return
// unicode.IsSpace(r) alone. Only "an information separator counts as
// whitespace" fails, since U+001C is the one kind of whitespace Python and Go
// disagree on (it sits unprinted between "x" and ";" in the got value):
//
//	ansiblecfg_test.go:35: parseAnsibleConfig() = {GalaxyServers:map[]
//	Defaults:{CollectionsPath: RolesPath:} Galaxy:{CacheDir: Server:https://x;prod
//	ServerList: ServerTimeout: SignatureKeys:[]}}, want {GalaxyServers:map[]
//	Defaults:{CollectionsPath: RolesPath:} Galaxy:{CacheDir: Server:https://x
//	ServerList: ServerTimeout: SignatureKeys:[]}}
func TestParseAnsibleConfigInlineSemicolonComment(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "semicolon after a space starts a comment",
			input: "[galaxy]\ncache_dir = /c ; note",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{CacheDir: "/c"}},
		},
		{
			name:  "semicolon after a tab starts a comment",
			input: "[galaxy]\nserver = https://x\t;prod",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "an information separator counts as whitespace",
			input: "[galaxy]\nserver = https://x\x1c;prod",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "the first semicolon after whitespace wins",
			input: "[galaxy]\nserver_list = a;b c ;d ;e",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{ServerList: "a;b c"}},
		},
		{
			name:  "a hash before the comment stays in the value",
			input: "[galaxy]\nserver = https://x # prod ; note",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x # prod"}},
		},
		{
			name:  "comment on a section header",
			input: "[galaxy] ; main section\nserver = https://x",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "comment after a galaxy_server url",
			input: "[galaxy_server.prod]\nurl = https://prod.example ; primary hub",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": "https://prod.example"},
			}},
		},
		{
			name:  "glued semicolon stays in a token",
			input: "[galaxy_server.prod]\ntoken = abc;def",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"token": "abc;def"},
			}},
		},
		{
			name:  "glued semicolons stay in a url query",
			input: "[galaxy_server.prod]\nurl = https://x/api/?a=1;b=2",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": "https://x/api/?a=1;b=2"},
			}},
		},
	})
}

// TestParseAnsibleConfigCommentsAndBlanks checks that full-line comments
// (both '#' and ';' markers, indented or not) and blank lines are skipped
// without affecting subsequently parsed keys.
func TestParseAnsibleConfigCommentsAndBlanks(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "full line hash comment skipped",
			input: "[galaxy]\n# server = https://x\nserver = https://y",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://y"}},
		},
		{
			name:  "full line semicolon comment skipped",
			input: "[galaxy]\n; server = https://x\nserver = https://y",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://y"}},
		},
		{
			name:  "indented full line comment skipped",
			input: "[galaxy]\n   # server = https://x\nserver = https://y",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://y"}},
		},
		{
			name:  "blank lines ignored",
			input: "[galaxy]\n\n\nserver = https://x\n\n",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
	})
}

// TestParseAnsibleConfigSections checks section- and key-scoping rules:
// keys before any section header, unknown sections, and unknown keys
// within a known section are all ignored; duplicate keys resolve to the
// last occurrence; and section names are matched case-sensitively.
func TestParseAnsibleConfigSections(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "key before any section ignored",
			input: "server = https://x\n[galaxy]\n",
			want:  ansibleConfig{},
		},
		{
			name:  "unknown section ignored",
			input: "[colors]\nhighlight = yes\n",
			want:  ansibleConfig{},
		},
		{
			name:  "unknown key in known section ignored",
			input: "[galaxy]\ntoken = secret\n",
			want:  ansibleConfig{},
		},
		{
			name:  "duplicate key last wins",
			input: "[galaxy]\nserver = https://x\nserver = https://y\n",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://y"}},
		},
		{
			// Section names are matched case-sensitively; "[Defaults]" is
			// not the same section as "[defaults]".
			name:  "section case is not normalized",
			input: "[Defaults]\ncollections_path = x",
			want:  ansibleConfig{},
		},
		{
			name:  "roles_path beside collections_path",
			input: "[defaults]\ncollections_path = /c\nroles_path = /r:/r2\n",
			want:  ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "/c", RolesPath: "/r:/r2"}},
		},
	})
}

// TestParseAnsibleConfigSectionHeaderGrammar pins sectionName to configparser's
// SECTCRE, `\[(?P<header>.+)\]` applied with re.match: whatever follows the
// last ']' is ignored, the name runs to that last ']' rather than the first,
// and the name is not trimmed. Every want is what CPython 3.14's
// ConfigParser(inline_comment_prefixes=(';',)) returned for the same input, so
// the rows that leave a tracked section unread are fidelity too: ansible does
// not read "[ galaxy ]" as [galaxy] either.
//
// KILLING MUTATION, run and reverted, in sectionName (ansiblecfg.go) - require
// the line to end with ']', as the parser once did. The five rows with text
// after the ']' fail, among them:
//
//	ansiblecfg_test.go:35: parseAnsibleConfig() = {GalaxyServers:map[]
//	Defaults:{CollectionsPath: RolesPath:} Galaxy:{CacheDir: Server:
//	ServerList: ServerTimeout: SignatureKeys:[]}}, want {GalaxyServers:map[]
//	Defaults:{CollectionsPath: RolesPath:} Galaxy:{CacheDir: Server:https://x
//	ServerList: ServerTimeout: SignatureKeys:[]}}
//
// KILLING MUTATION, run and reverted, in sectionName (ansiblecfg.go) - trim the
// name with strings.TrimSpace. Only the two rows with spaces inside the
// brackets fail.
//
// KILLING MUTATION, run and reverted, in sectionName (ansiblecfg.go) - end the
// name at the first ']' instead of the last. Only "the last bracket ends the
// name" fails.
//
// KILLING MUTATION, run and reverted, in sectionName (ansiblecfg.go) - weaken
// the length guard to end < 1, letting an empty name through. Only "brackets
// with nothing between them are not a header" fails: its key line is read as a
// header, and the url below it lands under no section.
func TestParseAnsibleConfigSectionHeaderGrammar(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "a hash comment after the header is ignored",
			input: "[galaxy] # note\nserver = https://x",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "text glued after the header is ignored",
			input: "[galaxy]x\nserver = https://x",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "a glued semicolon after the header is ignored",
			input: "[galaxy];note\nserver = https://x",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "trailing text after a galaxy_server header is ignored",
			input: "[galaxy_server.prod] primary hub\nurl = https://x",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": "https://x"},
			}},
		},
		{
			name:  "a header line wins over the delimiter after it",
			input: "[galaxy_server.prod]\nurl = https://prod\n[galaxy] = oops\nserver = https://x",
			want: ansibleConfig{
				GalaxyServers: map[string]map[string]string{"prod": {"url": "https://prod"}},
				Galaxy:        ansibleGalaxyConfig{Server: "https://x"},
			},
		},
		{
			name:  "the last bracket ends the name",
			input: "[galaxy] [note]\nserver = https://x",
			want:  ansibleConfig{},
		},
		{
			name:  "brackets with nothing between them are not a header",
			input: "[galaxy_server.prod]\n[]: note\nurl = https://x",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"[]": "note", "url": "https://x"},
			}},
		},
		{
			name:  "spaces inside the brackets are part of the name",
			input: "[ galaxy ]\nserver = https://x",
			want:  ansibleConfig{},
		},
		{
			name:  "spaces inside a galaxy_server header are part of the id",
			input: "[galaxy_server. prod ]\nurl = https://x",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				" prod ": {"url": "https://x"},
			}},
		},
	})
}

// TestParseAnsibleConfigLexicalQuirks checks line-ending, BOM, whitespace,
// key-case, and empty-input handling.
func TestParseAnsibleConfigLexicalQuirks(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "crlf line endings",
			input: "[galaxy]\r\nserver = https://x\r\n",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "leading BOM stripped, a file ansible refuses",
			input: "\uFEFF[defaults]\ncollections_path=x",
			want:  ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "x"}},
		},
		{
			name:  "surrounding whitespace trimmed",
			input: "[defaults]\n  collections_path   =   ./c   ",
			want:  ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "./c"}},
		},
		{
			name:  "key case is normalized",
			input: "[defaults]\nCollections_Path = x",
			want:  ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "x"}},
		},
		{
			name:  "empty input",
			input: "",
			want:  ansibleConfig{},
		},
	})
}

// TestParseAnsibleConfigServerList checks that [galaxy] server_list is
// captured as a plain string, read the way every other [galaxy] key is
// (quotes and a trailing '#' kept, a ';' comment after whitespace removed,
// last occurrence wins).
func TestParseAnsibleConfigServerList(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "server_list captured verbatim",
			input: "[galaxy]\nserver_list = prod, staging",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{ServerList: "prod, staging"}},
		},
		{
			name:  "duplicate key last wins",
			input: "[galaxy]\nserver_list = a\nserver_list = b\n",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{ServerList: "b"}},
		},
	})
}

// TestParseAnsibleConfigSignatureKeysAreNotRead pins a decision rather than a
// behavior: the signature policy is configured from flags and environment
// variables only, so a [galaxy] section carrying all four of ansible's
// signature keys contributes their NAMES and not one of their values.
//
// The reason is that this program cannot establish who authored a discovered
// ansible.cfg, and a setting that can relax a verification check must not come
// from a file whose author is unknown. cliflags.SignatureFlags holds that
// argument, including why each proxy for the authorship question leaks.
//
// Binding absence is what makes this stronger than a deny-list, and recording
// names sharpens rather than weakens it: the want value below is the whole
// ansibleConfig, compared structurally, so every value-carrying field of it has
// to stay zero. It needs no list to keep current, and it fails the moment
// anyone teaches the parser to store one of these VALUES without answering the
// authorship question first.
//
// The second row is what makes the names a fact about the file rather than
// about the parser's own table: keys ansible does not define are not recorded,
// so the first row's four cannot be "every unrecognized [galaxy] key".
//
// KILLING MUTATION, run and reverted: recordSignatureKey's membership test
// (`!slices.Contains(signatureKeyNames[:], key)`) deleted, so every
// unrecognized [galaxy] key is recorded. The second row fails:
//
//	ansiblecfg_test.go:35: parseAnsibleConfig() = {GalaxyServers:map[]
//	Defaults:{CollectionsPath:} Galaxy:{CacheDir: Server: ServerList:
//	SignatureKeys:[gpg_keyrings verify_signatures]}}, want {GalaxyServers:map[]
//	Defaults:{CollectionsPath:} Galaxy:{CacheDir: Server: ServerList:
//	SignatureKeys:[]}}
func TestParseAnsibleConfigSignatureKeysAreNotRead(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name: "every signature key is recorded by name and by name alone",
			input: "[galaxy]\n" +
				"gpg_keyring = /repo/keys.gpg\n" +
				"required_valid_signature_count = 0\n" +
				"ignore_signature_status_codes = BADSIG\n" +
				"disable_gpg_verify = yes\n",
			want: ansibleConfig{Galaxy: ansibleGalaxyConfig{SignatureKeys: []string{
				"gpg_keyring",
				"required_valid_signature_count",
				"ignore_signature_status_codes",
				"disable_gpg_verify",
			}}},
		},
		{
			name:  "a [galaxy] key ansible does not define is not recorded",
			input: "[galaxy]\ngpg_keyrings = /repo/keys.gpg\nverify_signatures = yes\n",
			want:  ansibleConfig{},
		},
		{
			name: "a repeated key is recorded once",
			input: "[galaxy]\n" +
				"gpg_keyring = /repo/one.gpg\n" +
				"gpg_keyring = /repo/two.gpg\n",
			want: ansibleConfig{Galaxy: ansibleGalaxyConfig{SignatureKeys: []string{"gpg_keyring"}}},
		},
	})
}

// TestParseAnsibleConfigGalaxyServerSections checks that every
// [galaxy_server.<id>] section is captured in full into GalaxyServers,
// keyed by the id exactly as written after the dot, with the outer map
// staying nil when no such section is present (the overwhelmingly common
// case must not pay for an allocation it never uses).
func TestParseAnsibleConfigGalaxyServerSections(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "no galaxy_server section: outer map stays nil",
			input: "[galaxy]\nserver = https://x\n",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "single section, single key",
			input: "[galaxy_server.prod]\nurl = https://prod.example\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": "https://prod.example"},
			}},
		},
		{
			name: "single section, multiple keys",
			input: "[galaxy_server.prod]\n" +
				"url = https://prod.example\n" +
				"token = abc123\n" +
				"validate_certs = false\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": "https://prod.example", "token": "abc123", "validate_certs": "false"},
			}},
		},
		{
			name: "multiple sections coexist",
			input: "[galaxy_server.prod]\n" +
				"url = https://prod.example\n" +
				"[galaxy_server.staging]\n" +
				"url = https://staging.example\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod":    {"url": "https://prod.example"},
				"staging": {"url": "https://staging.example"},
			}},
		},
		{
			name: "duplicate key within a section: last wins",
			input: "[galaxy_server.prod]\n" +
				"url = https://one.example\n" +
				"url = https://two.example\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": "https://two.example"},
			}},
		},
		{
			name:  "id containing a dot is captured verbatim",
			input: "[galaxy_server.my.hub]\nurl = https://x\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"my.hub": {"url": "https://x"},
			}},
		},
		{
			// "galaxy_server" without a trailing dot is not a per-server
			// section at all - the fixed prefix requires the dot.
			name:  "bare galaxy_server section without a dot is ignored",
			input: "[galaxy_server]\nurl = https://x\n",
			want:  ansibleConfig{},
		},
	})
}

// TestParseAnsibleConfigGalaxyServerSectionsFidelity checks that a
// [galaxy_server.<id>] section is read the way every other section this
// parser tracks is: values as ansible reads them (quotes and a trailing '#'
// kept, a ';' comment after whitespace removed), section names matched
// case-sensitively, keys lowercased.
func TestParseAnsibleConfigGalaxyServerSectionsFidelity(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name: "quoted value and trailing hash preserved verbatim",
			input: "[galaxy_server.prod]\n" +
				`url = "https://prod.example"` + "\n" +
				"token = abc123 # comment\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": `"https://prod.example"`, "token": "abc123 # comment"},
			}},
		},
		{
			// Section names (including the galaxy_server.<id> prefix match)
			// are case-sensitive, same as every other section.
			name:  "section name case is not normalized",
			input: "[Galaxy_Server.prod]\nurl = https://x\n",
			want:  ansibleConfig{},
		},
		{
			// Keys within a galaxy_server section are lowercased, same as
			// every other section.
			name:  "keys within galaxy_server section are lowercased",
			input: "[galaxy_server.prod]\nURL = https://x\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": "https://x"},
			}},
		},
	})
}

// TestParseAnsibleConfigInformationSeparatorTrim pins the trimming half of
// isINISpace, which TestParseAnsibleConfigInlineSemicolonComment's separator
// row leaves open: configparser strips the whitespace str.isspace names from a
// line, a key and a value, U+001C through U+001F included. Each row puts the
// separator where exactly one trim can remove it - around a header only the
// line trim reaches, before the delimiter only the key trim, after it only the
// value trim - so each trim is pinned by a row of its own. Every want is what
// CPython 3.14's ConfigParser(inline_comment_prefixes=(';',)) returned.
//
// KILLING MUTATION, run and reverted, in parseAnsibleConfig (ansiblecfg.go) -
// trim the line with strings.TrimSpace. Only the header row fails, the key and
// value rows pass:
//
//	ansiblecfg_test.go:35: parseAnsibleConfig() = {GalaxyServers:map[]
//	Defaults:{CollectionsPath: RolesPath:} Galaxy:{CacheDir: Server:
//	ServerList: ServerTimeout: SignatureKeys:[]}}, want {GalaxyServers:map[]
//	Defaults:{CollectionsPath: RolesPath:} Galaxy:{CacheDir: Server:https://x
//	ServerList: ServerTimeout: SignatureKeys:[]}}
//
// KILLING MUTATION, run and reverted, in splitKeyValue (ansiblecfg.go) - trim
// the key with strings.TrimSpace, and separately the value. Each fails only
// its own row, with the same shape of output as above.
func TestParseAnsibleConfigInformationSeparatorTrim(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "an information separator is trimmed around a section header",
			input: "\x1c[galaxy]\x1c\nserver = https://x",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "an information separator is trimmed from a key",
			input: "[galaxy]\nserver\x1c = https://x",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "an information separator is trimmed from the start of a value",
			input: "[galaxy]\nserver =\x1chttps://x",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
	})
}

// TestParseAnsibleConfigRefusedHeaderClosesSection pins what happens under a
// line that opens like a section header but is not one: a header whose ';'
// comment cut off its ']', and one never closed. configparser refuses such a
// file outright, so there is no ansible reading to match; what matters is that
// the keys below the line are not filed under the section above it. Otherwise
// the dev url below becomes prod's url, and prod's token, which the same-file
// pairing rule lets through, is sent to the dev host.
//
// The last row is the positive control: a line starting with '[' that is a
// key line to configparser too keeps its section, so the reset is not simply
// "any line starting with '['".
//
// KILLING MUTATION, run and reverted, in parseAnsibleConfig (ansiblecfg.go) -
// delete the section = "" reset. Both refused-header rows fail:
//
//	ansiblecfg_test.go:35: parseAnsibleConfig() = {GalaxyServers:map[prod:map[token:t
//	url:https://dev.example]] Defaults:{CollectionsPath: RolesPath:} Galaxy:{CacheDir:
//	Server: ServerList: ServerTimeout: SignatureKeys:[]}}, want
//	{GalaxyServers:map[prod:map[token:t url:https://prod.example]] Defaults:{CollectionsPath:
//	RolesPath:} Galaxy:{CacheDir: Server: ServerList: ServerTimeout: SignatureKeys:[]}}
func TestParseAnsibleConfigRefusedHeaderClosesSection(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name: "a header cut short by a comment closes the section before it",
			input: "[galaxy_server.prod]\nurl = https://prod.example\ntoken = t\n" +
				"[galaxy_server.dev ;scratch hub]\nurl = https://dev.example\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": "https://prod.example", "token": "t"},
			}},
		},
		{
			name:  "a header never closed closes the section before it",
			input: "[galaxy_server.prod]\ntoken = t\n[galaxy_server.dev\nurl = https://dev.example\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"token": "t"},
			}},
		},
		{
			name:  "a key line starting with a bracket keeps its section",
			input: "[galaxy_server.prod]\n[note = x\nurl = https://prod.example\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"[note": "x", "url": "https://prod.example"},
			}},
		},
	})
}

// TestLoadAnsibleConfig checks that loadAnsibleConfig opens and parses a
// real file from disk, and surfaces a wrapped os.ErrNotExist for a missing
// path (the existing swallow-and-continue behavior in
// loadAnsibleConfigFromCLI depends on this).
func TestLoadAnsibleConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ansible.cfg")
	content := "[defaults]\ncollections_path = ./collections\n\n[galaxy]\nserver = https://x\ncache_dir = /c\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v, want nil", err)
	}

	got, gotPath, err := loadAnsibleConfig(path)
	if err != nil {
		t.Fatalf("loadAnsibleConfig() error = %v, want nil", err)
	}
	if gotPath != path {
		t.Errorf("loadAnsibleConfig() path = %q, want %q", gotPath, path)
	}
	want := ansibleConfig{
		Defaults: ansibleDefaultsConfig{CollectionsPath: "./collections"},
		Galaxy:   ansibleGalaxyConfig{Server: "https://x", CacheDir: "/c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loadAnsibleConfig() = %+v, want %+v", got, want)
	}

	_, _, err = loadAnsibleConfig(filepath.Join(dir, "missing.cfg"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("loadAnsibleConfig() error = %v, want os.ErrNotExist", err)
	}
}
