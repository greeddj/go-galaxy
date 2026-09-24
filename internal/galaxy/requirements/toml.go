package requirements

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/projectfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// ParseTOML parses galaxy.toml data. Each entry is reshaped into what the
// YAML path judges (a dependency string into a name/version mapping), so
// both files produce one File through parseRaw and share every rule.
func ParseTOML(data []byte, defaultSource string) (File, error) {
	doc, err := projectfile.Decode(data)
	if err != nil {
		return File{}, err
	}
	raw := map[string]any{}
	if doc.Project.Collections != nil {
		if raw["collections"], err = shimCollections(doc.Project.Collections); err != nil {
			return File{}, err
		}
	}
	if doc.Project.Roles == nil {
		return parseRaw(raw, defaultSource)
	}
	roles, err := shimRoles(doc.Project.Roles)
	if err != nil {
		return collectionsBesideRolesError(raw, defaultSource, err)
	}
	raw["roles"] = roles
	return parseRaw(raw, defaultSource)
}

// collectionsBesideRolesError returns a roles refusal as parseRaw would: as
// a RolesError beside the collections, which cleanup can still act on. A
// collections refusal outranks it, since parseRaw judges collections first.
func collectionsBesideRolesError(raw map[string]any, defaultSource string, rolesErr error) (File, error) {
	if _, ok := raw["collections"]; !ok {
		return File{}, &RolesError{Err: rolesErr}
	}
	f, err := parseRaw(raw, defaultSource)
	if err != nil {
		return File{}, err
	}
	return File{Collections: f.Collections}, &RolesError{Err: rolesErr}
}

// shimCollections reshapes a collections array for parseRaw: a string is
// split into name and constraint, an inline table is held to the closed key
// set. Any other value passes through so parseRaw refuses it by its own rule.
func shimCollections(value any) (any, error) {
	list, ok := value.([]any)
	if !ok {
		return value, nil
	}
	items := make([]any, 0, len(list))
	for _, item := range list {
		shimmed, err := shimCollectionItem(item)
		if err != nil {
			return nil, err
		}
		items = append(items, shimmed)
	}
	return items, nil
}

func shimCollectionItem(item any) (any, error) {
	switch v := item.(type) {
	case string:
		return splitCollectionSpec(v)
	case map[string]any:
		if err := checkCollectionTable(v); err != nil {
			return nil, err
		}
		return v, nil
	case []any:
		// Refused by type alone: the shared %v refusal would print every
		// string inside the array, a credential-bearing URL included.
		return nil, fmt.Errorf("%w: a collections entry is a %T, not a string or a table",
			helpers.ErrUnsupportedCollectionFormat, item)
	default:
		return item, nil
	}
}

// collectionSpecOperators are the bytes a constraint may start with directly
// after the name; a space or a tab is the other admitted separator.
const collectionSpecOperators = "=<>!~^*"

// splitCollectionSpec cuts "name constraint" into the mapping the YAML path
// judges. A git pointer, a URL or a path-shaped value passes whole, since its
// ref, subdir and query are not a constraint; so does a value with no name.
func splitCollectionSpec(spec string) (any, error) {
	trimmed := strings.TrimSpace(spec)
	if passesWhole(trimmed) {
		return spec, nil
	}
	name := trimmed[:nameRunLength(trimmed)]
	rest := trimmed[len(name):]
	if name == "" || rest == "" {
		return spec, nil
	}
	if rest[0] != ' ' && rest[0] != '\t' && !strings.ContainsRune(collectionSpecOperators, rune(rest[0])) {
		return nil, fmt.Errorf("%w: %q: put a space or a version operator between the name and its constraint",
			helpers.ErrInvalidCollectionName, helpers.TruncateForMessage(spec))
	}
	constraint := strings.TrimSpace(rest)
	if err := checkConstraint(name, constraint); err != nil {
		return nil, err
	}
	return map[string]any{"name": name, "version": constraint}, nil
}

// passesWhole reports the strings the splitter never touches: an empty one,
// a git pointer, an http(s) URL and a path-shaped value, each judged whole
// by parseCollectionStringItem in that same order.
func passesWhole(trimmed string) bool {
	return trimmed == "" || gitsource.IsPointer(trimmed) || urlsource.IsHTTPURL(trimmed) || looksLikeSourceName(trimmed)
}

// nameRunLength is the length of the longest prefix of [A-Za-z0-9_.]. Upper
// case is inside the run so "Acme.App >= 1" is refused by the name alphabet
// later rather than mis-split here.
func nameRunLength(value string) int {
	for i := range len(value) {
		if !isNameByte(value[i]) {
			return i
		}
	}
	return len(value)
}

func isNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '.'
}

// checkConstraint refuses a Galaxy version constraint the solver could not
// parse. "" and "*" are any version; semver's own message is left out, since
// it echoes the input in its own shape.
func checkConstraint(name, constraint string) error {
	normalized := helpers.NormalizeConstraint(constraint)
	if normalized == "" {
		return nil
	}
	if _, err := semver.NewConstraint(normalized); err != nil {
		return fmt.Errorf("%w: %q for %s", helpers.ErrInvalidCollectionConstraint,
			helpers.TruncateForMessage(constraint), helpers.TruncateForMessage(name))
	}
	return nil
}

// collectionTableKeys is the closed key set of a collection inline table.
func collectionTableKeys() map[string]struct{} {
	return map[string]struct{}{
		"namespace": {}, "name": {}, "version": {}, "source": {}, "type": {}, "signatures": {},
	}
}

// checkCollectionTable holds an inline table to the closed key set and its
// scalar keys to strings, in sorted key order so the first refusal is
// deterministic; a Galaxy entry's version is judged as the string form is.
func checkCollectionTable(table map[string]any) error {
	for _, key := range sortedKeys(table) {
		if _, ok := collectionTableKeys()[key]; !ok {
			return fmt.Errorf("%w: unknown key %q on a collection entry",
				helpers.ErrInvalidCollectionEntry, helpers.TruncateForMessage(key))
		}
		if err := checkStringKey(helpers.ErrInvalidCollectionEntry, key, table[key], key != "signatures"); err != nil {
			return err
		}
	}
	if !isGalaxyTable(table) {
		return nil
	}
	if version, ok := table["version"].(string); ok {
		name, _ := table["name"].(string)
		return checkConstraint(strings.TrimSpace(name), strings.TrimSpace(version))
	}
	return nil
}

// isGalaxyTable reports whether the table names a Galaxy entry: no type or
// type galaxy, a name that is no pointer, URL or path, and a source naming no
// repository or artifact, so parseRaw's own refusal of those shapes wins.
func isGalaxyTable(table map[string]any) bool {
	typ, _ := table["type"].(string)
	if typ = strings.ToLower(strings.TrimSpace(typ)); typ != "" && typ != TypeGalaxy {
		return false
	}
	name, _ := table["name"].(string)
	source, _ := table["source"].(string)
	return !looksLikeSourceName(name) && !gitsource.IsPointer(source) &&
		!gitsource.IsLocator(source) && !urlsource.IsLocator(source)
}

// checkStringKey refuses a scalar key holding a non-string, naming the key
// and the Go type and never the value: a TOML float 1.0 would otherwise
// render as "1" and read as a 1.x range.
func checkStringKey(sentinel error, key string, value any, scalar bool) error {
	if !scalar {
		return nil
	}
	if _, ok := value.(string); !ok {
		return fmt.Errorf("%w: %s is a %T, not a string", sentinel, key, value)
	}
	return nil
}

// shimRoles holds each inline table of a roles array to the closed role key
// set. A string is ansible's src[,version[,name]] form and is never split;
// any other value passes through so parseRaw refuses it by its own rule.
func shimRoles(value any) (any, error) {
	list, ok := value.([]any)
	if !ok {
		return value, nil
	}
	for i, item := range list {
		table, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if err := checkRoleTable(table); err != nil {
			return nil, fmt.Errorf("roles[%d]: %w", i, err)
		}
	}
	return list, nil
}

// roleTableKeys is the closed key set of a role inline table. include stays
// in it so parseRoleMap refuses it as an include rather than an unknown key.
func roleTableKeys() map[string]struct{} {
	return map[string]struct{}{
		"name": {}, "role": {}, "src": {}, "scm": {}, "version": {}, "include": {},
	}
}

// checkRoleTable refuses an unknown key, where the YAML path only warns,
// and a non-string scalar; galaxy.toml is this tool's own file and is strict.
func checkRoleTable(table map[string]any) error {
	for _, key := range sortedKeys(table) {
		if _, ok := roleTableKeys()[key]; !ok {
			return fmt.Errorf("%w: unknown key %q on a role entry",
				helpers.ErrInvalidRoleEntry, helpers.TruncateForMessage(key))
		}
		if err := checkStringKey(helpers.ErrInvalidRoleEntry, key, table[key], key != "include"); err != nil {
			return err
		}
	}
	return nil
}

// sortedKeys returns the table's keys in sorted order.
func sortedKeys(table map[string]any) []string {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
