// Package projectfile decodes galaxy.toml, go-galaxy's own project file, into
// the shapes the requirements package judges. It is the module's only importer
// of github.com/BurntSushi/toml, so a decode error is rendered in one place.
package projectfile

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Document is a decoded galaxy.toml. Only the [project] table exists; any
// other top-level table is refused rather than ignored.
type Document struct {
	Project Project
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
		if key != "project" {
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
	return Document{Project: project}, nil
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
// outside the closed set is unknown, since a later commit may give it meaning.
func applyProjectKey(project *Project, key string, value any) error {
	var err error
	switch key {
	case "name":
		project.Name, err = stringField(key, value)
	case "version":
		project.Version, err = stringField(key, value)
	case "description":
		project.Description, err = stringField(key, value)
	case "collections":
		project.Collections = listValue(value)
	case "roles":
		project.Roles = listValue(value)
	default:
		err = fmt.Errorf("%w: unknown key %q in [project]",
			helpers.ErrUnsupportedRequirementsFormat, helpers.TruncateForMessage(key))
	}
	return err
}

// stringField returns value as a string or refuses it by key alone: the value
// is never printed, since a TOML float renders in a shape that misleads.
func stringField(key string, value any) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%w: [project] %s is not a string", helpers.ErrUnsupportedRequirementsFormat, key)
	}
	return s, nil
}

// listValue makes the [[project.x]] spelling, which decodes as a slice of
// tables, indistinguishable from an inline array; any other value passes
// through untouched so requirements refuses it with its own sentinel.
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
