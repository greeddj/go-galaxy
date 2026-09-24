package projectfile

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// projectTable is the smallest [project] table Decode admits.
const projectTable = "\n[project]\ncollections = []\n"

// tomlPlaintext is the value every secret-bearing key in these fixtures
// holds; no message may echo it, whatever fault sits beside it.
const tomlPlaintext = "s3cr3t-toml-token"

// fileOwnPlaintext is a token written literally into a file, the shape that
// leaves TokenExpanded false.
const fileOwnPlaintext = "file-own-plaintext"

// fullSettingsFixture spells every [tool.go-galaxy] key once: the s3 table
// with its boolean, and two [[servers]] entries, one carrying a token and a
// disabled certificate check. Decode keeps the ${HOME} as written.
const fullSettingsFixture = `[tool.go-galaxy]
lock_file = "locks/galaxy.lock"
cache_dir = "${HOME}/.cache/go-galaxy"
metrics_file = "metrics.json"
workers = 4
download_workers = 8

[tool.go-galaxy.s3]
bucket = "galaxy-cache"
region = "eu-central-1"
prefix = "ci/"
endpoint = "https://minio.example:9000"
access_key = "AKIAEXAMPLE"
secret_key = "s3cr3t-toml-token"
session_token = "session-plaintext"
path_style_disabled = true

[[tool.go-galaxy.servers]]
id = "hub"
url = "https://hub.example/api/"
token = "s3cr3t-toml-token"
validate_certs = false

[[tool.go-galaxy.servers]]
id = "public"
url = "https://galaxy.ansible.com/api/"
`

// inlineServersFixture is the inline array spelling of the same two servers.
const inlineServersFixture = `[tool.go-galaxy]
servers = [
  { id = "hub", url = "https://hub.example/api/", token = "s3cr3t-toml-token", validate_certs = false },
  { id = "public", url = "https://galaxy.ansible.com/api/" },
]
`

// fullSettings is what fullSettingsFixture decodes to: Path "" and
// TokenExpanded false, since Decode sees bytes alone and expands nothing.
func fullSettings() Settings {
	return Settings{
		LockFile:    "locks/galaxy.lock",
		CacheDir:    "${HOME}/.cache/go-galaxy",
		MetricsFile: "metrics.json",
		Servers:     fullServers(),
		S3: S3Settings{
			Bucket: "galaxy-cache", Region: "eu-central-1", Prefix: "ci/", Endpoint: "https://minio.example:9000",
			AccessKey: "AKIAEXAMPLE", SecretKey: tomlPlaintext, SessionToken: "session-plaintext", PathStyleDisabled: true,
		},
		Workers:            4,
		DownloadWorkers:    8,
		HasWorkers:         true,
		HasDownloadWorkers: true,
	}
}

// fullServers is the two entries both fixtures carry: validate_certs = false
// is a pointer to false, an absent key is nil.
func fullServers() []ServerSetting {
	validateCerts := false
	return []ServerSetting{
		{ID: "hub", URL: "https://hub.example/api/", Token: tomlPlaintext, ValidateCerts: &validateCerts},
		{ID: "public", URL: "https://galaxy.ansible.com/api/"},
	}
}

// document appends projectTable to a [tool.go-galaxy] source, after it so
// that a top-level key such as tool = 1 stays top-level; the settings shape
// is then the whole difference between rows.
func document(toolSrc string) string {
	return toolSrc + projectTable
}

// TestDecodeAcceptsSettings pins the exact Settings the documented
// [tool.go-galaxy] shapes decode to, the inline servers spelling included, and
// that an absent or empty table is the zero value.
func TestDecodeAcceptsSettings(t *testing.T) {
	t.Parallel()

	validateCerts := true
	for _, tt := range []struct {
		name string
		src  string
		want Settings
	}{
		{name: "every key in the table spelling", src: fullSettingsFixture, want: fullSettings()},
		{name: "the inline servers spelling", src: inlineServersFixture, want: Settings{Servers: fullServers()}},
		{name: "no [tool] table", src: "", want: Settings{}},
		{name: "an empty [tool] table", src: "[tool]\n", want: Settings{}},
		{name: "an empty [tool.go-galaxy] table", src: "[tool.go-galaxy]\n", want: Settings{}},
		{name: "an empty servers array", src: "[tool.go-galaxy]\nservers = []\n", want: Settings{Servers: []ServerSetting{}}},
		{
			name: "validate_certs = true",
			src:  "[[tool.go-galaxy.servers]]\nid = \"hub\"\nurl = \"https://hub.example/api/\"\nvalidate_certs = true\n",
			want: Settings{Servers: []ServerSetting{{ID: "hub", URL: "https://hub.example/api/", ValidateCerts: &validateCerts}}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			doc, err := Decode([]byte(document(tt.src)))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !reflect.DeepEqual(doc.Settings, tt.want) {
				t.Fatalf("Settings = %+v, want %+v", doc.Settings, tt.want)
			}
		})
	}
}

// schemaRow is one [tool.go-galaxy] shape Decode refuses: the fragment its
// message must carry and, for a fault inside one server entry, the prefix
// naming that entry.
type schemaRow struct {
	name       string
	src        string
	wantMsg    string
	wantPrefix string
}

// toolTableRows lists the [tool] and [tool.go-galaxy] refusals: a foreign
// tool, a scalar where a table belongs, an unknown key and a mistyped value.
func toolTableRows() []schemaRow {
	return []schemaRow{
		{name: "[tool.other]", src: "[tool.other]\nx = 1\n", wantMsg: "unknown table \"tool.other\" in galaxy.toml"},
		{name: "[tool] scalar", src: "tool = 1\n", wantMsg: "[tool] is not a table"},
		{name: "[tool.go-galaxy] scalar", src: "[tool]\ngo-galaxy = \"x\"\n", wantMsg: "[tool.go-galaxy] is not a table"},
		{name: "unknown key in [tool.go-galaxy]", src: "[tool.go-galaxy]\nworker = 4\n", wantMsg: "unknown key \"worker\" in [tool.go-galaxy]"},
		{name: "lock_file integer", src: "[tool.go-galaxy]\nlock_file = 1\n", wantMsg: "[tool.go-galaxy] lock_file is not a string"},
		{name: "workers quoted", src: "[tool.go-galaxy]\nworkers = \"4\"\n", wantMsg: "[tool.go-galaxy] workers is not an integer"},
		{name: "workers float", src: "[tool.go-galaxy]\nworkers = 4.0\n", wantMsg: "[tool.go-galaxy] workers is not an integer"},
		{name: "workers boolean", src: "[tool.go-galaxy]\nworkers = true\n", wantMsg: "[tool.go-galaxy] workers is not an integer"},
		{
			name:    "download_workers quoted",
			src:     "[tool.go-galaxy]\ndownload_workers = \"8\"\n",
			wantMsg: "[tool.go-galaxy] download_workers is not an integer",
		},
		{name: "s3 = 1", src: "[tool.go-galaxy]\ns3 = 1\n", wantMsg: "[tool.go-galaxy.s3] is not a table"},
		{name: "servers scalar", src: "[tool.go-galaxy]\nservers = \"hub\"\n", wantMsg: "[[tool.go-galaxy.servers]] is not an array of tables"},
	}
}

// s3Rows lists the [tool.go-galaxy.s3] refusals: an unknown key, a mistyped
// string and a quoted boolean, each beside a secret the message must not echo.
func s3Rows() []schemaRow {
	return []schemaRow{
		{
			name:    "unknown key in [tool.go-galaxy.s3]",
			src:     "[tool.go-galaxy.s3]\nbukcet = \"x\"\nsecret_key = \"s3cr3t-toml-token\"\n",
			wantMsg: "unknown key \"bukcet\" in [tool.go-galaxy.s3]",
		},
		{
			name:    "bucket integer",
			src:     "[tool.go-galaxy.s3]\nbucket = 1\nsecret_key = \"s3cr3t-toml-token\"\n",
			wantMsg: "[tool.go-galaxy.s3] bucket is not a string",
		},
		{
			name:    "path_style_disabled quoted",
			src:     "[tool.go-galaxy.s3]\npath_style_disabled = \"true\"\nsecret_key = \"s3cr3t-toml-token\"\n",
			wantMsg: "[tool.go-galaxy.s3] path_style_disabled is not a boolean",
		},
	}
}

// serverRows lists the [[tool.go-galaxy.servers]] refusals: a required key
// missing or empty, a mistyped value, an entry that is no table, a repeated
// id, and a fault in the second entry named as such.
func serverRows() []schemaRow {
	const entry1 = "[[tool.go-galaxy.servers]] entry 1: "
	const hub = "[[tool.go-galaxy.servers]]\nid = \"hub\"\nurl = \"https://hub.example/api/\"\ntoken = \"s3cr3t-toml-token\"\n"
	const needsBoth = "[[tool.go-galaxy.servers]] needs both id and url"
	return []schemaRow{
		{
			name:    "missing id",
			src:     "[[tool.go-galaxy.servers]]\nurl = \"https://hub.example/api/\"\ntoken = \"s3cr3t-toml-token\"\n",
			wantMsg: needsBoth, wantPrefix: entry1,
		},
		{name: "missing url", src: "[[tool.go-galaxy.servers]]\nid = \"hub\"\n", wantMsg: needsBoth, wantPrefix: entry1},
		{
			name:    "empty id",
			src:     "[[tool.go-galaxy.servers]]\nid = \"\"\nurl = \"https://hub.example/api/\"\n",
			wantMsg: needsBoth, wantPrefix: entry1,
		},
		{
			name:    "url integer",
			src:     "[[tool.go-galaxy.servers]]\nid = \"hub\"\nurl = 1\n",
			wantMsg: "[[tool.go-galaxy.servers]] url is not a string", wantPrefix: entry1,
		},
		{
			name:    "validate_certs quoted",
			src:     hub + "validate_certs = \"false\"\n",
			wantMsg: "[[tool.go-galaxy.servers]] validate_certs is not a boolean", wantPrefix: entry1,
		},
		{name: "a non-table entry", src: "[tool.go-galaxy]\nservers = [1]\n", wantMsg: "[[tool.go-galaxy.servers]] entry 1 is not a table"},
		{name: "a duplicate id", src: hub + hub, wantMsg: "[[tool.go-galaxy.servers]] entry 2 repeats id \"hub\""},
		{
			name:    "a fault in the second entry",
			src:     hub + "[[tool.go-galaxy.servers]]\nid = \"mirror\"\n",
			wantMsg: needsBoth, wantPrefix: "[[tool.go-galaxy.servers]] entry 2: ",
		},
	}
}

// ansibleCfgKeyRows lists the [galaxy_server.*] keys ansible.cfg admits and a
// server entry does not: each is unknown here, not silently ignored.
func ansibleCfgKeyRows() []schemaRow {
	rows := make([]schemaRow, 0, 5)
	for _, key := range []string{"username", "password", "auth_url", "client_id", "api_version"} {
		rows = append(rows, schemaRow{
			name:       "ansible.cfg key " + key,
			src:        "[[tool.go-galaxy.servers]]\nid = \"hub\"\nurl = \"https://hub.example/api/\"\n" + key + " = \"s3cr3t-toml-token\"\n",
			wantMsg:    "unknown key \"" + key + "\" in [[tool.go-galaxy.servers]]",
			wantPrefix: "[[tool.go-galaxy.servers]] entry 1: ",
		})
	}
	return rows
}

// TestDecodeRefusesSettingsSchema pins the sentinel and message of every
// [tool.go-galaxy] shape the schema refuses: a message names a key and an
// entry ordinal, never a value, so the secret beside a fault stays out of it.
func TestDecodeRefusesSettingsSchema(t *testing.T) {
	t.Parallel()

	for _, tt := range slices.Concat(toolTableRows(), s3Rows(), serverRows(), ansibleCfgKeyRows()) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Decode([]byte(document(tt.src)))
			if !errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) {
				t.Fatalf("Decode = %v, want %v", err, helpers.ErrUnsupportedRequirementsFormat)
			}
			msg := err.Error()
			if !strings.Contains(msg, tt.wantMsg) {
				t.Fatalf("Decode = %q, want it to contain %q", msg, tt.wantMsg)
			}
			if !strings.HasPrefix(msg, tt.wantPrefix) {
				t.Fatalf("Decode = %q, want the prefix %q", msg, tt.wantPrefix)
			}
			if strings.Contains(msg, tomlPlaintext) {
				t.Fatalf("Decode = %q echoes a value from the file", msg)
			}
		})
	}
}

// TestProjectReferencesStayLiteral pins that ${VAR} is a [tool.go-galaxy]
// grammar alone: Decode keeps one under [project] as written and LoadSettings
// neither expands nor reports it. Not parallel: it clears the variable.
func TestProjectReferencesStayLiteral(t *testing.T) {
	const src = "[project]\ncollections = [\"acme.app == ${ACME_VERSION}\"]\n"
	unsetEnv(t, "ACME_VERSION")

	doc, err := Decode([]byte(src))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	assertStrings(t, itemsOf(t, doc.Project.Collections), []string{"acme.app == ${ACME_VERSION}"})

	path := writeProjectFile(t, filepath.Join(t.TempDir(), "galaxy.toml"), src)
	settings, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	assertLoaded(t, settings, Settings{Path: path})
}

// TestLoadSettingsExpandsReferences pins the ${VAR} rule over a value: one
// pass, no escape, a bare $VAR untouched and an exported-empty variable as
// "". Not parallel: every row sets variables.
func TestLoadSettingsExpandsReferences(t *testing.T) {
	for _, tt := range []struct {
		env  map[string]string
		name string
		src  string
		want string
	}{
		{
			name: "${A}${B} concatenates",
			env:  map[string]string{"GALAXY_TEST_A": "ci-", "GALAXY_TEST_B": "main"},
			src:  "prefix = \"${GALAXY_TEST_A}${GALAXY_TEST_B}\"", want: "ci-main",
		},
		{
			name: "a bare $A is a literal, set or not",
			env:  map[string]string{"GALAXY_TEST_A": "ci-"},
			src:  "prefix = \"$GALAXY_TEST_A/$GALAXY_TEST_UNSET\"", want: "$GALAXY_TEST_A/$GALAXY_TEST_UNSET",
		},
		{
			name: "an exported-empty variable expands to empty",
			env:  map[string]string{"GALAXY_TEST_A": ""},
			src:  "prefix = \"<${GALAXY_TEST_A}>\"", want: "<>",
		},
		{
			name: "a value read from a variable is not expanded again",
			env:  map[string]string{"GALAXY_TEST_A": "${GALAXY_TEST_B}", "GALAXY_TEST_B": "main"},
			src:  "prefix = \"${GALAXY_TEST_A}\"", want: "${GALAXY_TEST_B}",
		},
		{
			name: "a doubled dollar does not escape",
			env:  map[string]string{"GALAXY_TEST_A": "ci-"},
			src:  "prefix = '$${GALAXY_TEST_A}'", want: "$ci-",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			unsetEnv(t, "GALAXY_TEST_UNSET")
			for name, value := range tt.env {
				t.Setenv(name, value)
			}
			settings := mustLoadSettings(t, "[tool.go-galaxy.s3]\n"+tt.src+"\n")
			if settings.S3.Prefix != tt.want {
				t.Fatalf("Prefix = %q, want %q", settings.S3.Prefix, tt.want)
			}
		})
	}
}

// TestLoadSettingsExpansionBoundaries pins where expansion stops: a key is
// never a reference, and a path is resolved only after its value has been
// expanded. Not parallel: both cases set a variable.
func TestLoadSettingsExpansionBoundaries(t *testing.T) {
	t.Run("a reference in a key is not expanded", func(t *testing.T) {
		t.Setenv("GALAXY_TEST_A", "workers")
		path := writeProjectFile(t, filepath.Join(t.TempDir(), "galaxy.toml"), document("[tool.go-galaxy]\n\"${GALAXY_TEST_A}\" = 4\n"))
		_, err := LoadSettings(path)
		if !errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) {
			t.Fatalf("LoadSettings = %v, want %v", err, helpers.ErrUnsupportedRequirementsFormat)
		}
		if want := `unknown key "${GALAXY_TEST_A}" in [tool.go-galaxy]`; !strings.Contains(err.Error(), want) {
			t.Fatalf("LoadSettings = %q, want it to contain %q", err.Error(), want)
		}
	})

	t.Run("a path is resolved after expansion", func(t *testing.T) {
		t.Setenv("GALAXY_TEST_A", "locks")
		settings := mustLoadSettings(t, "[tool.go-galaxy]\nlock_file = \"${GALAXY_TEST_A}/galaxy.lock\"\n")
		if want := filepath.Join(filepath.Dir(settings.Path), "locks", "galaxy.lock"); settings.LockFile != want {
			t.Fatalf("LockFile = %q, want %q", settings.LockFile, want)
		}
	})
}

// TestLoadSettingsReportsUnsetReferences pins that every unset name across
// the table is gathered into one ErrProjectFileEnvUnset, sorted and named
// once, and that no set variable's value reaches the message.
func TestLoadSettingsReportsUnsetReferences(t *testing.T) {
	unsetEnv(t, "A_VAR")
	unsetEnv(t, "Z_VAR")
	t.Setenv("GALAXY_TEST_SET", tomlPlaintext)
	const src = `[tool.go-galaxy]
metrics_file = "${Z_VAR}/metrics.json"

[tool.go-galaxy.s3]
access_key = "${GALAXY_TEST_SET}"
secret_key = "${Z_VAR}"

[[tool.go-galaxy.servers]]
id = "hub"
url = "https://hub.example/api/"
token = "${A_VAR}"
`
	settings, err := LoadSettings(writeProjectFile(t, filepath.Join(t.TempDir(), "galaxy.toml"), document(src)))
	if !errors.Is(err, helpers.ErrProjectFileEnvUnset) {
		t.Fatalf("LoadSettings = %v, want %v", err, helpers.ErrProjectFileEnvUnset)
	}
	const want = "project file references unset environment variables: A_VAR, Z_VAR"
	if err.Error() != want {
		t.Fatalf("LoadSettings = %q, want %q", err.Error(), want)
	}
	if strings.Contains(err.Error(), tomlPlaintext) {
		t.Fatalf("LoadSettings = %q echoes a set variable's value", err.Error())
	}
	if !reflect.DeepEqual(settings, Settings{}) {
		t.Fatalf("Settings = %+v beside an error, want the zero value", settings)
	}
}

// TestLoadSettingsMarksExpandedTokens pins TokenExpanded on the one server
// whose token held a reference: a literal token beside it, and a reference in
// that entry's url, leave it false.
func TestLoadSettingsMarksExpandedTokens(t *testing.T) {
	t.Setenv("GALAXY_TEST_TOKEN", tomlPlaintext)
	t.Setenv("GALAXY_TEST_HOST", "mirror.example")
	const src = `[[tool.go-galaxy.servers]]
id = "hub"
url = "https://hub.example/api/"
token = "${GALAXY_TEST_TOKEN}"

[[tool.go-galaxy.servers]]
id = "mirror"
url = "https://${GALAXY_TEST_HOST}/api/"
token = "file-own-plaintext"
`
	settings := mustLoadSettings(t, src)
	want := []ServerSetting{
		{ID: "hub", URL: "https://hub.example/api/", Token: tomlPlaintext, TokenExpanded: true},
		{ID: "mirror", URL: "https://mirror.example/api/", Token: fileOwnPlaintext},
	}
	if !reflect.DeepEqual(settings.Servers, want) {
		t.Fatalf("Servers = %+v, want %+v", settings.Servers, want)
	}
}

// TestLoadSettingsRefusesExpandedDuplicateID pins the second duplicate-id
// check: ids distinct as written pass Decode, and one that expands onto the
// other is refused by LoadSettings. Not parallel: it sets a variable.
func TestLoadSettingsRefusesExpandedDuplicateID(t *testing.T) {
	t.Setenv("GALAXY_TEST_ID", "hub")
	const src = `[[tool.go-galaxy.servers]]
id = "hub"
url = "https://hub.example/api/"

[[tool.go-galaxy.servers]]
id = "${GALAXY_TEST_ID}"
url = "https://mirror.example/api/"
`
	if _, err := Decode([]byte(document(src))); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	_, err := LoadSettings(writeProjectFile(t, filepath.Join(t.TempDir(), "galaxy.toml"), document(src)))
	if !errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) {
		t.Fatalf("LoadSettings = %v, want %v", err, helpers.ErrUnsupportedRequirementsFormat)
	}
	if want := `[[tool.go-galaxy.servers]] entry 2 repeats id "hub"`; !strings.Contains(err.Error(), want) {
		t.Fatalf("LoadSettings = %q, want it to contain %q", err.Error(), want)
	}
}

// TestLoadSettingsResolvesPaths pins that a relative lock_file, cache_dir or
// metrics_file is joined under the file's directory, an absolute one is kept,
// an absent one stays empty, and Path is the argument as given.
func TestLoadSettingsResolvesPaths(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		want func(dir string) Settings
		name string
		src  string
	}{
		{
			name: "relative paths are joined under the file's directory",
			src:  "[tool.go-galaxy]\nlock_file = \"locks/galaxy.lock\"\ncache_dir = \"cache\"\nmetrics_file = \"./metrics.json\"\n",
			want: func(dir string) Settings {
				return Settings{
					LockFile:    filepath.Join(dir, "locks/galaxy.lock"),
					CacheDir:    filepath.Join(dir, "cache"),
					MetricsFile: filepath.Join(dir, "metrics.json"),
				}
			},
		},
		{
			name: "an absolute path is kept",
			src:  "[tool.go-galaxy]\nlock_file = \"/var/lib/go-galaxy/galaxy.lock\"\n",
			want: func(string) Settings { return Settings{LockFile: "/var/lib/go-galaxy/galaxy.lock"} },
		},
		{
			name: "an absent path stays empty",
			src:  "[tool.go-galaxy]\nworkers = 2\n",
			want: func(string) Settings { return Settings{Workers: 2, HasWorkers: true} },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := filepath.Join(t.TempDir(), "sub", "dir")
			path := writeProjectFile(t, filepath.Join(dir, "galaxy.toml"), document(tt.src))
			settings, err := LoadSettings(path)
			if err != nil {
				t.Fatalf("LoadSettings: %v", err)
			}
			want := tt.want(dir)
			want.Path = path
			assertLoaded(t, settings, want)
		})
	}
}

// TestLoadSettingsFileOutcomes pins the three outcomes before any schema: an
// absent file is zero settings and no error, a directory is unreadable, and
// bytes that are not TOML carry the requirements sentinel.
func TestLoadSettingsFileOutcomes(t *testing.T) {
	t.Parallel()

	t.Run("an absent file contributes nothing", func(t *testing.T) {
		t.Parallel()

		settings, err := LoadSettings(filepath.Join(t.TempDir(), "galaxy.toml"))
		if err != nil {
			t.Fatalf("LoadSettings = %v, want nil", err)
		}
		if !reflect.DeepEqual(settings, Settings{}) {
			t.Fatalf("Settings = %+v, want the zero value", settings)
		}
	})

	t.Run("a directory is unreadable", func(t *testing.T) {
		t.Parallel()

		if _, err := LoadSettings(t.TempDir()); !errors.Is(err, helpers.ErrRequirementsUnreadable) {
			t.Fatalf("LoadSettings = %v, want %v", err, helpers.ErrRequirementsUnreadable)
		}
	})

	t.Run("a file that is not TOML", func(t *testing.T) {
		t.Parallel()

		path := writeProjectFile(t, filepath.Join(t.TempDir(), "galaxy.toml"), "[project\ncollections = []\n")
		if _, err := LoadSettings(path); !errors.Is(err, helpers.ErrInvalidRequirementsTOML) {
			t.Fatalf("LoadSettings = %v, want %v", err, helpers.ErrInvalidRequirementsTOML)
		}
	})
}

// assertLoaded compares a LoadSettings result to want exactly: a file naming
// no server keeps the nil Servers slice Decode gave it, since expansion
// mutates the decoded slice in place and never builds a new one.
func assertLoaded(t *testing.T, got, want Settings) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Settings = %+v, want %+v", got, want)
	}
}

// mustLoadSettings writes document(toolSrc) into a fresh directory and loads
// it, failing the test on any error.
func mustLoadSettings(t *testing.T, toolSrc string) Settings {
	t.Helper()

	path := writeProjectFile(t, filepath.Join(t.TempDir(), "galaxy.toml"), document(toolSrc))
	settings, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	return settings
}

// writeProjectFile writes src at path, creating the directories above it,
// and returns path.
func writeProjectFile(t *testing.T, path, src string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("os.MkdirAll(%q) error = %v, want nil", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v, want nil", path, err)
	}
	return path
}

// unsetEnv removes name from this test's environment; t.Setenv is called
// first purely for the restore it registers, which is also what keeps the
// test off t.Parallel.
func unsetEnv(t *testing.T, name string) {
	t.Helper()

	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("os.Unsetenv(%q) error = %v, want nil", name, err)
	}
}
