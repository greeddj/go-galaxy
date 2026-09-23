package rolebuild

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/safeout"
)

const (
	// metadataMaxBytes is helpers.BuildMetadataMaxBytes, the cap every
	// builder applies to a metadata file before decoding it.
	metadataMaxBytes = helpers.BuildMetadataMaxBytes

	// metaDirName and the file names under it are the spellings ansible's
	// GalaxyRole.META_MAIN and META_REQUIREMENTS search, in its order: the
	// .yml spelling first, the .yaml one second.
	metaDirName          = "meta"
	mainYMLName          = "main.yml"
	mainYAMLName         = "main.yaml"
	requirementsYMLName  = "requirements.yml"
	requirementsYAMLName = "requirements.yaml"

	// metaMainPath and metaRequirementsPath name the files in a message
	// from the exported parsers, which see bytes and not a tree.
	metaMainPath         = metaDirName + "/" + mainYMLName
	metaRequirementsPath = metaDirName + "/" + requirementsYMLName

	keyDependencies = "dependencies"
	keyGalaxyInfo   = "galaxy_info"
	keyRoleName     = "role_name"
	keyRole         = "role"
	keyName         = "name"
	keySrc          = "src"
	keyScm          = "scm"
	keyVersion      = "version"

	// tagNull and tagStr are the resolved YAML tags the parser tells a null
	// and a string scalar by.
	tagNull = "!!null"
	tagStr  = "!!str"
)

// ParseMetaMain parses meta/main.yml bytes into dependencies and role name.
// An empty document is no dependencies and a warning, as in ansible-galaxy;
// a shape role_yaml_parse would not accept is helpers.ErrRoleMetaInvalid.
func ParseMetaMain(data []byte) (Meta, error) {
	return parseMetaMain(data, metaMainPath)
}

// ParseMetaRequirements parses meta/requirements.yml bytes: a list of specs or
// an empty document. A top-level mapping is refused, as ansible refuses it:
// the roles:/collections: form belongs to a CLI requirements file.
func ParseMetaRequirements(data []byte) ([]gitsource.RoleDependency, []string, error) {
	return parseMetaRequirements(data, metaRequirementsPath)
}

func parseMetaMain(data []byte, file string) (Meta, error) {
	root, err := decodeDocument(data, file)
	if err != nil {
		return Meta{}, err
	}
	if isNull(root) {
		return Meta{Warnings: []string{"meta file " + file + " is empty; skipping dependencies"}}, nil
	}
	if root.Kind != yaml.MappingNode {
		return Meta{}, fmt.Errorf("%w: %s is not a mapping, got a YAML %s", helpers.ErrRoleMetaInvalid, file, describe(root))
	}
	var doc map[string]yaml.Node
	if err := root.Decode(&doc); err != nil {
		return Meta{}, fmt.Errorf("%w: %s does not parse: %w", helpers.ErrRoleMetaInvalid, file, err)
	}
	deps, warnings, err := parseSpecList(lookup(doc, keyDependencies), file+" "+keyDependencies)
	if err != nil {
		return Meta{}, err
	}
	roleName, nameWarnings := roleNameFrom(lookup(doc, keyGalaxyInfo), file)
	return Meta{Dependencies: deps, RoleName: roleName, Warnings: append(warnings, nameWarnings...)}, nil
}

func parseMetaRequirements(data []byte, file string) ([]gitsource.RoleDependency, []string, error) {
	root, err := decodeDocument(data, file)
	if err != nil {
		return nil, nil, err
	}
	return parseSpecList(root, file)
}

// decodeDocument applies the size cap to the bytes given and parses them into
// the root node, a null node for an empty document.
func decodeDocument(data []byte, file string) (*yaml.Node, error) {
	if len(data) > metadataMaxBytes {
		return nil, fmt.Errorf("%w: %s is %d bytes, the limit is %d", helpers.ErrRoleMetaInvalid, file, len(data), metadataMaxBytes)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%w: %s does not parse: %w", helpers.ErrRoleMetaInvalid, file, err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nullNode(), nil
	}
	return root.Content[0], nil
}

// nullNode is the node an empty document reads as, so every caller judges
// emptiness through isNull alone.
func nullNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagNull}
}

// isNull reports whether n is absent or a YAML null, the two readings of a
// key that was not written.
func isNull(n *yaml.Node) bool {
	return n == nil || n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.Tag == tagNull)
}

// lookup returns the node under key, nil when the mapping has no such key.
func lookup(m map[string]yaml.Node, key string) *yaml.Node {
	n, ok := m[key]
	if !ok {
		return nil
	}
	return &n
}

// describe names a node's type for a message, the way "got a YAML map"
// reads: the resolved tag without its "!!" prefix.
func describe(n *yaml.Node) string {
	if n.Tag == "" {
		return "node"
	}
	return strings.TrimPrefix(n.Tag, "!!")
}

// parseSpecList reads a list of role specs. An absent or null list is empty;
// anything but a sequence is a refusal naming where it sits.
func parseSpecList(list *yaml.Node, where string) ([]gitsource.RoleDependency, []string, error) {
	if isNull(list) {
		return nil, nil, nil
	}
	if list.Kind != yaml.SequenceNode {
		return nil, nil, fmt.Errorf("%w: %s must be a list, got a YAML %s", helpers.ErrRoleMetaInvalid, where, describe(list))
	}
	deps := make([]gitsource.RoleDependency, 0, len(list.Content))
	var warnings []string
	for i, item := range list.Content {
		dep, warns, err := parseSpec(item, where, i)
		if err != nil {
			return nil, nil, err
		}
		deps = append(deps, dep)
		warnings = append(warnings, warns...)
	}
	return deps, warnings, nil
}

// parseSpec reads one spec: a string, carried verbatim as Src, or a mapping.
// Any other shape is one role_yaml_parse would crash on, and is refused.
func parseSpec(item *yaml.Node, where string, i int) (gitsource.RoleDependency, []string, error) {
	switch {
	case item.Kind == yaml.ScalarNode && item.Tag == tagStr:
		return gitsource.RoleDependency{Src: item.Value}, nil, nil
	case item.Kind == yaml.MappingNode:
		var m map[string]yaml.Node
		if err := item.Decode(&m); err != nil {
			return gitsource.RoleDependency{}, nil, fmt.Errorf("%w: %s[%d] does not parse: %w", helpers.ErrRoleMetaInvalid, where, i, err)
		}
		return specFromMapping(m, where, i)
	default:
		return gitsource.RoleDependency{}, nil, fmt.Errorf("%w: %s[%d] must be a string or a mapping, got a YAML %s",
			helpers.ErrRoleMetaInvalid, where, i, describe(item))
	}
}

// specFromMapping normalizes a mapping spec the way role_yaml_parse does:
// the old style under role:, or the new style under name:, src:, scm: and
// version:. Every field is carried as written.
func specFromMapping(m map[string]yaml.Node, where string, i int) (gitsource.RoleDependency, []string, error) {
	fields := make(map[string]string, len(m))
	for _, key := range []string{keyRole, keyName, keySrc, keyScm, keyVersion} {
		v, ok, err := scalarField(m, key, where, i)
		if err != nil {
			return gitsource.RoleDependency{}, nil, err
		}
		if ok {
			fields[key] = v
		}
	}
	if role, ok := fields[keyRole]; ok {
		return oldStyleSpec(fields, role, where, i)
	}
	return newStyleSpec(fields, where, i)
}

// oldStyleSpec reads a spec under role:, which is the install name and, unless
// src: is given, the source. A comma in it is refused and a name: beside it is
// overridden with a warning, both as role_yaml_parse does.
func oldStyleSpec(fields map[string]string, role, where string, i int) (gitsource.RoleDependency, []string, error) {
	if strings.Contains(role, ",") {
		return gitsource.RoleDependency{}, nil, fmt.Errorf("%w: %s[%d] is an invalid old style role requirement: %q",
			helpers.ErrRoleMetaInvalid, where, i, display(role))
	}
	var warnings []string
	if name, ok := fields[keyName]; ok && name != role {
		warnings = append(warnings, fmt.Sprintf("%s[%d]: name %q is ignored in favor of role %q, as ansible reads it",
			where, i, display(name), display(role)))
	}
	dep := gitsource.RoleDependency{Src: role, Scm: fields[keyScm], Version: fields[keyVersion], Name: role}
	if src, ok := fields[keySrc]; ok {
		dep.Src = src
	}
	return dep, warnings, nil
}

// newStyleSpec reads a spec under src: and name:, which stand for
// themselves; a missing src: is the name (GalaxyRole reads src or name), and
// a spec with neither is a refusal.
func newStyleSpec(fields map[string]string, where string, i int) (gitsource.RoleDependency, []string, error) {
	name, hasName := fields[keyName]
	src, hasSrc := fields[keySrc]
	if !hasName && !hasSrc {
		return gitsource.RoleDependency{}, nil, fmt.Errorf("%w: %s[%d] names no role, name or src", helpers.ErrRoleMetaInvalid, where, i)
	}
	if !hasSrc {
		src = name
	}
	return gitsource.RoleDependency{Src: src, Scm: fields[keyScm], Version: fields[keyVersion], Name: name}, nil, nil
}

// scalarField reads one spec key as the text written. A key that is absent
// or null was not written; a list or a mapping under it is a refusal, since
// role_yaml_parse would hand it on as a path component.
func scalarField(m map[string]yaml.Node, key, where string, i int) (string, bool, error) {
	n := lookup(m, key)
	if isNull(n) {
		return "", false, nil
	}
	if n.Kind != yaml.ScalarNode {
		return "", false, fmt.Errorf("%w: %s[%d].%s must be a scalar, got a YAML %s", helpers.ErrRoleMetaInvalid, where, i, key, describe(n))
	}
	return n.Value, true, nil
}

// roleNameFrom reads galaxy_info.role_name. galaxy_info is information
// ansible never reads at install time, so a shape it cannot be read from is
// a warning and not a refusal.
func roleNameFrom(info *yaml.Node, file string) (string, []string) {
	if isNull(info) {
		return "", nil
	}
	if info.Kind != yaml.MappingNode {
		return "", []string{fmt.Sprintf("%s: %s is a YAML %s, not a mapping; %s is ignored", file, keyGalaxyInfo, describe(info), keyRoleName)}
	}
	var m map[string]yaml.Node
	if err := info.Decode(&m); err != nil {
		return "", []string{fmt.Sprintf("%s: %s does not parse: %v; %s is ignored", file, keyGalaxyInfo, err, keyRoleName)}
	}
	n := lookup(m, keyRoleName)
	if isNull(n) {
		return "", nil
	}
	if n.Kind != yaml.ScalarNode {
		return "", []string{fmt.Sprintf("%s: %s.%s is a YAML %s, not a string; it is ignored", file, keyGalaxyInfo, keyRoleName, describe(n))}
	}
	return n.Value, nil
}

// display renders a value from the meta file for a message: cleaned of
// control runes and bounded, since it is repository content.
func display(s string) string {
	return helpers.TruncateForMessage(string(safeout.Clean(s)))
}
