// Package projectfile decodes galaxy.toml, go-galaxy's own project file, into
// the shapes the requirements and config packages judge. It is the module's
// only importer of github.com/BurntSushi/toml, so a decode error is rendered here.
package projectfile

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Document is a decoded galaxy.toml: the [project] table and the
// [tool.go-galaxy] settings as written, ${VAR} references included. Any other
// top-level table is refused rather than ignored.
type Document struct {
	Project  Project
	Settings Settings
}

// Project is the [project] table. Name, Version and Description are "" when
// absent and refused when present as a non-string; Collections and Roles are
// nil when absent and otherwise carry the value for requirements to judge.
type Project struct {
	Collections any
	Roles       any
	Name        string
	Version     string
	Description string
}

// Settings is the [tool.go-galaxy] table. Decode leaves every string as
// written; LoadSettings expands ${VAR} and resolves the three paths against
// the file's directory, so a [project] reader never needs the environment.
type Settings struct {
	// Path is the file LoadSettings read, for messages that name a source;
	// "" from Decode, which sees bytes alone.
	Path               string
	LockFile           string
	CacheDir           string
	MetricsFile        string
	Servers            []ServerSetting
	S3                 S3Settings
	Workers            int
	DownloadWorkers    int
	HasWorkers         bool
	HasDownloadWorkers bool
}

// S3Settings is the [tool.go-galaxy.s3] table. The two keys are plain strings
// only while they pass through here; config wraps them in a Secret, and
// nothing keeps a Settings value once config has read it.
type S3Settings struct {
	Bucket            string
	Region            string
	Prefix            string
	Endpoint          string
	AccessKey         string
	SecretKey         string
	SessionToken      string
	PathStyleDisabled bool
}

// ServerSetting is one [[tool.go-galaxy.servers]] entry. ValidateCerts is nil
// when the key is absent; TokenExpanded reports that Token held a ${VAR}
// reference, which makes the secret the operator's rather than the file's.
type ServerSetting struct {
	ValidateCerts *bool
	ID            string
	URL           string
	Token         string
	TokenExpanded bool
}

// envRefPattern is the one expansion form, as mimir's config reader spells
// it: a bare $VAR is a literal, so a token holding one is never rewritten.
var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// IsTOMLPath reports whether path names a TOML file, judged by extension
// alone and without regard to case, so ".TOML" counts and "galaxy.txt" holding
// TOML does not.
func IsTOMLPath(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".toml")
}

// Decode parses data as galaxy.toml. Syntax errors wrap
// helpers.ErrInvalidRequirementsTOML and never echo the input; schema errors
// wrap helpers.ErrUnsupportedRequirementsFormat and name a key, never a value.
func Decode(data []byte) (Document, error) {
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return Document{}, renderDecodeError(err)
	}
	for _, key := range sortedKeys(raw) {
		if key != "project" && key != "tool" {
			return Document{}, fmt.Errorf("%w: unknown table %q in galaxy.toml",
				helpers.ErrUnsupportedRequirementsFormat, helpers.TruncateForMessage(key))
		}
	}
	projectRaw, hasProject := raw["project"]
	if !hasProject {
		return Document{}, fmt.Errorf("%w: galaxy.toml has no [project] table", helpers.ErrUnsupportedRequirementsFormat)
	}
	table, isTable := projectRaw.(map[string]any)
	if !isTable {
		return Document{}, fmt.Errorf("%w: [project] is not a table", helpers.ErrUnsupportedRequirementsFormat)
	}
	project, err := decodeProject(table)
	if err != nil {
		return Document{}, err
	}
	settings, err := decodeTool(raw["tool"])
	if err != nil {
		return Document{}, err
	}
	return Document{Project: project, Settings: settings}, nil
}

// LoadSettings reads path's [tool.go-galaxy] table with every ${VAR} expanded
// and each relative path resolved against the file's directory. An absent
// file is empty settings; every other failure keeps the requirements sentinels.
func LoadSettings(path string) (Settings, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the operator's own project file.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Settings{}, nil
		}
		return Settings{}, fmt.Errorf("%w: %w", helpers.ErrRequirementsUnreadable, err)
	}
	doc, err := Decode(data)
	if err != nil {
		return Settings{}, err
	}
	settings, err := expandSettings(doc.Settings)
	if err != nil {
		return Settings{}, err
	}
	if err := checkServerIDs(settings.Servers); err != nil {
		return Settings{}, err
	}
	dir := filepath.Dir(path)
	settings.Path = path
	settings.LockFile = resolvePath(dir, settings.LockFile)
	settings.CacheDir = resolvePath(dir, settings.CacheDir)
	settings.MetricsFile = resolvePath(dir, settings.MetricsFile)
	return settings, nil
}

// resolvePath joins a relative p under dir; "" stays "" so an unset key
// never becomes the project directory, and an absolute path is kept.
func resolvePath(dir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

// renderDecodeError maps a toml decode failure onto ErrInvalidRequirementsTOML.
// A ParseError is a value type; only its line and last key are rendered, since
// its Message echoes string bodies and bare tokens from the input.
func renderDecodeError(err error) error {
	var pe toml.ParseError
	if !errors.As(err, &pe) {
		return fmt.Errorf("%w: cannot decode", helpers.ErrInvalidRequirementsTOML)
	}
	if pe.LastKey == "" {
		return fmt.Errorf("%w: line %d", helpers.ErrInvalidRequirementsTOML, pe.Position.Line)
	}
	return fmt.Errorf("%w: line %d (last key %q)",
		helpers.ErrInvalidRequirementsTOML, pe.Position.Line, helpers.TruncateForMessage(pe.LastKey))
}

// decodeProject judges the [project] table key by key in sorted order, so the
// first refusal of a table with several faults is deterministic.
func decodeProject(table map[string]any) (Project, error) {
	var project Project
	for _, key := range sortedKeys(table) {
		if err := applyProjectKey(&project, key, table[key]); err != nil {
			return Project{}, err
		}
	}
	if project.Collections == nil && project.Roles == nil {
		return Project{}, fmt.Errorf("%w: [project] has neither collections nor roles", helpers.ErrUnsupportedRequirementsFormat)
	}
	return project, nil
}

// applyProjectKey stores one [project] key on project or refuses it: a key
// outside the closed set is unknown, since a later release may give it meaning.
func applyProjectKey(project *Project, key string, value any) error {
	var err error
	switch key {
	case "name":
		project.Name, err = stringField("[project]", key, value)
	case "version":
		project.Version, err = stringField("[project]", key, value)
	case "description":
		project.Description, err = stringField("[project]", key, value)
	case "collections":
		project.Collections = listValue(value)
	case "roles":
		project.Roles = listValue(value)
	default:
		err = unknownKey("[project]", key)
	}
	return err
}

// decodeTool judges the [tool] table: absent or empty is no settings, else a
// table holding [tool.go-galaxy] alone, since a galaxy.toml is nobody's file
// but this tool's and a foreign tool table would be silently half-read otherwise.
func decodeTool(value any) (Settings, error) {
	if value == nil {
		return Settings{}, nil
	}
	table, ok := value.(map[string]any)
	if !ok {
		return Settings{}, fmt.Errorf("%w: [tool] is not a table", helpers.ErrUnsupportedRequirementsFormat)
	}
	for _, key := range sortedKeys(table) {
		if key != "go-galaxy" {
			return Settings{}, fmt.Errorf("%w: unknown table \"tool.%s\" in galaxy.toml",
				helpers.ErrUnsupportedRequirementsFormat, helpers.TruncateForMessage(key))
		}
	}
	raw, present := table["go-galaxy"]
	if !present {
		return Settings{}, nil
	}
	settings, ok := raw.(map[string]any)
	if !ok {
		return Settings{}, fmt.Errorf("%w: [tool.go-galaxy] is not a table", helpers.ErrUnsupportedRequirementsFormat)
	}
	return decodeSettings(settings)
}

// decodeSettings judges [tool.go-galaxy] key by key in sorted order. A string
// key holding a ${VAR} is kept as written here; a number or a boolean is
// typed by TOML itself, so "4" in quotes is refused rather than read.
func decodeSettings(table map[string]any) (Settings, error) {
	var settings Settings
	for _, key := range sortedKeys(table) {
		if err := applySettingsKey(&settings, key, table[key]); err != nil {
			return Settings{}, err
		}
	}
	return settings, nil
}

func applySettingsKey(settings *Settings, key string, value any) error {
	const table = "[tool.go-galaxy]"
	var err error
	switch key {
	case "lock_file":
		settings.LockFile, err = stringField(table, key, value)
	case "cache_dir":
		settings.CacheDir, err = stringField(table, key, value)
	case "metrics_file":
		settings.MetricsFile, err = stringField(table, key, value)
	case "workers":
		settings.Workers, err = intField(table, key, value)
		settings.HasWorkers = err == nil
	case "download_workers":
		settings.DownloadWorkers, err = intField(table, key, value)
		settings.HasDownloadWorkers = err == nil
	case "s3":
		settings.S3, err = decodeS3(value)
	case "servers":
		settings.Servers, err = decodeServers(value)
	default:
		err = unknownKey(table, key)
	}
	return err
}

// decodeS3 judges the [tool.go-galaxy.s3] table against its closed key set:
// seven strings and one boolean.
func decodeS3(value any) (S3Settings, error) {
	const table = "[tool.go-galaxy.s3]"
	raw, ok := value.(map[string]any)
	if !ok {
		return S3Settings{}, fmt.Errorf("%w: %s is not a table", helpers.ErrUnsupportedRequirementsFormat, table)
	}
	var s3 S3Settings
	strings := map[string]*string{
		"bucket": &s3.Bucket, "region": &s3.Region, "prefix": &s3.Prefix, "endpoint": &s3.Endpoint,
		"access_key": &s3.AccessKey, "secret_key": &s3.SecretKey, "session_token": &s3.SessionToken,
	}
	for _, key := range sortedKeys(raw) {
		var err error
		if key == "path_style_disabled" {
			s3.PathStyleDisabled, err = boolField(table, key, raw[key])
		} else if target, known := strings[key]; known {
			*target, err = stringField(table, key, raw[key])
		} else {
			err = unknownKey(table, key)
		}
		if err != nil {
			return S3Settings{}, err
		}
	}
	return s3, nil
}

// decodeServers judges the servers array, [[tool.go-galaxy.servers]] or the
// inline spelling, entry by entry; id and url are required on each, since an
// entry is the section and no ansible.cfg is consulted beside it.
func decodeServers(value any) ([]ServerSetting, error) {
	const table = "[[tool.go-galaxy.servers]]"
	items, ok := listValue(value).([]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s is not an array of tables", helpers.ErrUnsupportedRequirementsFormat, table)
	}
	servers := make([]ServerSetting, 0, len(items))
	for i, item := range items {
		raw, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: %s entry %d is not a table", helpers.ErrUnsupportedRequirementsFormat, table, i+1)
		}
		server, err := decodeServer(raw)
		if err != nil {
			return nil, fmt.Errorf("%s entry %d: %w", table, i+1, err)
		}
		servers = append(servers, server)
	}
	if err := checkServerIDs(servers); err != nil {
		return nil, err
	}
	return servers, nil
}

// checkServerIDs refuses an exact duplicate id, where --server=<id> would
// otherwise take the first entry's place with the last entry's keys. It runs
// on the ids as written and again once expanded; case-folded pairs are config's.
func checkServerIDs(servers []ServerSetting) error {
	seen := make(map[string]bool, len(servers))
	for i, server := range servers {
		if seen[server.ID] {
			return fmt.Errorf("%w: [[tool.go-galaxy.servers]] entry %d repeats id %q",
				helpers.ErrUnsupportedRequirementsFormat, i+1, helpers.TruncateForMessage(server.ID))
		}
		seen[server.ID] = true
	}
	return nil
}

func decodeServer(raw map[string]any) (ServerSetting, error) {
	const table = "[[tool.go-galaxy.servers]]"
	var server ServerSetting
	for _, key := range sortedKeys(raw) {
		var err error
		switch key {
		case "id":
			server.ID, err = stringField(table, key, raw[key])
		case "url":
			server.URL, err = stringField(table, key, raw[key])
		case "token":
			server.Token, err = stringField(table, key, raw[key])
		case "validate_certs":
			var validate bool
			validate, err = boolField(table, key, raw[key])
			server.ValidateCerts = &validate
		default:
			err = unknownKey(table, key)
		}
		if err != nil {
			return ServerSetting{}, err
		}
	}
	if server.ID == "" || server.URL == "" {
		return ServerSetting{}, fmt.Errorf("%w: %s needs both id and url", helpers.ErrUnsupportedRequirementsFormat, table)
	}
	return server, nil
}

// expandSettings replaces every ${VAR} in the string values of settings from
// the environment, keys untouched; what a variable holds is the environment's
// concern. Unset names are reported once, sorted, as ErrProjectFileEnvUnset.
func expandSettings(settings Settings) (Settings, error) {
	missing := map[string]struct{}{}
	expand := func(value string) string {
		return envRefPattern.ReplaceAllStringFunc(value, func(match string) string {
			name := match[2 : len(match)-1]
			if v, ok := os.LookupEnv(name); ok {
				return v
			}
			missing[name] = struct{}{}
			return ""
		})
	}
	settings.LockFile = expand(settings.LockFile)
	settings.CacheDir = expand(settings.CacheDir)
	settings.MetricsFile = expand(settings.MetricsFile)
	settings.S3 = expandS3(settings.S3, expand)
	for i := range settings.Servers {
		server := &settings.Servers[i]
		server.TokenExpanded = envRefPattern.MatchString(server.Token)
		server.ID, server.URL, server.Token = expand(server.ID), expand(server.URL), expand(server.Token)
	}
	if len(missing) > 0 {
		return Settings{}, fmt.Errorf("%w: %s", helpers.ErrProjectFileEnvUnset, strings.Join(slices.Sorted(maps.Keys(missing)), ", "))
	}
	return settings, nil
}

func expandS3(s3 S3Settings, expand func(string) string) S3Settings {
	s3.Bucket, s3.Region, s3.Prefix, s3.Endpoint = expand(s3.Bucket), expand(s3.Region), expand(s3.Prefix), expand(s3.Endpoint)
	s3.AccessKey, s3.SecretKey, s3.SessionToken = expand(s3.AccessKey), expand(s3.SecretKey), expand(s3.SessionToken)
	return s3
}

// stringField returns value as a string or refuses it by table and key alone:
// the value is never printed, since a TOML float renders in a shape that
// misleads and a string may be a secret.
func stringField(table, key string, value any) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%w: %s %s is not a string", helpers.ErrUnsupportedRequirementsFormat, table, key)
	}
	return s, nil
}

// intField returns a TOML integer as an int; a float, a string or a boolean
// is refused by key, so workers = "4" is a schema error rather than 4.
func intField(table, key string, value any) (int, error) {
	n, ok := value.(int64)
	if !ok {
		return 0, fmt.Errorf("%w: %s %s is not an integer", helpers.ErrUnsupportedRequirementsFormat, table, key)
	}
	return int(n), nil
}

// boolField returns a TOML boolean; "true" in quotes is refused, since the
// ansible.cfg spellings (yes, on, 1) belong to that file's grammar, not this one.
func boolField(table, key string, value any) (bool, error) {
	b, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%w: %s %s is not a boolean", helpers.ErrUnsupportedRequirementsFormat, table, key)
	}
	return b, nil
}

func unknownKey(table, key string) error {
	return fmt.Errorf("%w: unknown key %q in %s", helpers.ErrUnsupportedRequirementsFormat, helpers.TruncateForMessage(key), table)
}

// listValue makes the [[x]] spelling, which decodes as a slice of tables,
// indistinguishable from an inline array; any other value passes through
// untouched so the caller refuses it with its own sentinel.
func listValue(value any) any {
	tables, ok := value.([]map[string]any)
	if !ok {
		return value
	}
	items := make([]any, 0, len(tables))
	for _, table := range tables {
		items = append(items, table)
	}
	return items
}

// sortedKeys returns m's keys in sorted order.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
