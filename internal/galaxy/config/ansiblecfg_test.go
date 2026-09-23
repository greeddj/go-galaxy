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
// ansible's configparser reads them: quotes are kept and a trailing '#' is
// part of the value.
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

// TestParseAnsibleConfigInlineSemicolonComment pins configparser's ';' inline
// comment on key lines and headers alike, each want taken from CPython 3.14;
// the glued rows are the control against cutting at every ';'.
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

// TestParseAnsibleConfigSections checks that keys before any header, unknown
// sections and unknown keys are ignored, a duplicate key keeps its last
// value, and section names are matched case-sensitively.
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
// SECTCRE: text after the last ']' is ignored, the name runs to that last ']'
// and is not trimmed. Each want is what CPython 3.14 returned.
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

// TestParseAnsibleConfigServerList checks that [galaxy] server_list is kept
// as a plain string, read like every other [galaxy] key.
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

// TestParseAnsibleConfigSignatureKeysAreNotRead pins that ansible's signature
// keys contribute their names and no value, compared over the whole
// ansibleConfig, and that a [galaxy] key ansible does not define is not named.
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

// TestParseAnsibleConfigGalaxyServerSections checks that each
// [galaxy_server.<id>] section is kept whole, keyed by the id as written, and
// that GalaxyServers stays nil when no such section exists.
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
// [galaxy_server.<id>] section follows the same value, section-case and
// key-case rules as every other section.
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

// TestParseAnsibleConfigInformationSeparatorTrim pins that U+001C is trimmed,
// as configparser trims it, from a header line, a key and a value, one row per
// trim. Each want is what CPython 3.14 returned.
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

// TestParseAnsibleConfigRefusedHeaderClosesSection pins that a broken header
// closes the section above it, so a dev url never pairs with prod's token; the
// last row is the control that a key line starting with '[' keeps its section.
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

// TestLoadAnsibleConfig checks that loadAnsibleConfig parses a file from disk
// and wraps os.ErrNotExist for a missing path, which loadAnsibleConfigFromCLI
// relies on to tell a missing file from a broken one.
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
