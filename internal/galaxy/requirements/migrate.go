package requirements

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/projectfile"
	"go.yaml.in/yaml/v3"
)

// Migration is a requirements.yml rendered as galaxy.toml: the bytes, one
// notice per thing the file cannot carry, and the entry counts.
type Migration struct {
	TOML        []byte
	Notices     []string
	Collections int
	Roles       int
}

// MigrateYAML renders requirements.yml data as a galaxy.toml holding only a
// [project] table and proves it: the bytes must parse back through ParseTOML
// to what Parse read, else ErrMigrateRoundTrip and no bytes.
func MigrateYAML(data []byte, projectName string) (Migration, error) {
	raw, err := decodeYAML(data)
	if err != nil {
		return Migration{}, err
	}
	file, err := parseRaw(raw, "")
	if err != nil {
		return Migration{}, err
	}
	if err := checkMigratable(file); err != nil {
		return Migration{}, err
	}
	var notices []string
	if !utf8.ValidString(projectName) {
		projectName = ""
		notices = append(notices, "the directory name is not UTF-8 text; galaxy.toml gets no project name")
	}
	draft := projectfile.Draft{Name: projectName}
	draft.HasCollections, draft.HasRoles = listsPresent(raw)
	for _, c := range file.Collections {
		draft.Collections = append(draft.Collections, collectionEntry(c))
	}
	for _, r := range file.Roles {
		draft.Roles = append(draft.Roles, roleEntry(r))
	}
	out := projectfile.Encode(draft)
	if err := verifyMigration(file, projectName, out); err != nil {
		return Migration{}, err
	}
	notices = append(notices, keyNotices(raw)...)
	notices = append(notices, gitVersionNotices(raw, file.Collections)...)
	notices = append(notices, documentNotices(data)...)
	return Migration{TOML: out, Notices: notices, Collections: len(file.Collections), Roles: len(file.Roles)}, nil
}

// checkMigratable refuses a Galaxy constraint ParseTOML would refuse at
// load, naming the entry; Parse hands over valid UTF-8 alone, and a defect
// there would still meet the round trip as a replaced rune.
func checkMigratable(f File) error {
	for i, c := range f.Collections {
		if c.Type != "" {
			continue
		}
		if err := checkConstraint(c.Namespace+"."+c.Name, c.Version); err != nil {
			return fmt.Errorf("collections[%d]: %w", i, err)
		}
	}
	return nil
}

// listsPresent reports which lists the file names, a null or empty one
// included, so the output names the same lists and always decodes.
func listsPresent(raw any) (bool, bool) {
	m, ok := raw.(map[string]any)
	if !ok {
		return true, false
	}
	_, hasCollections := m["collections"]
	_, hasRoles := m["roles"]
	return hasCollections, hasRoles
}

// collectionEntry renders one parsed collection, leaving every default out.
func collectionEntry(c CollectionRequirement) projectfile.Entry {
	switch {
	case c.IsGit():
		return gitCollectionEntry(c)
	case c.IsURL():
		if c.Version == "" {
			return projectfile.Entry{Text: c.Source}
		}
		return projectfile.Entry{Fields: []projectfile.Field{{Key: "name", Value: c.Source}, {Key: "version", Value: c.Version}}}
	}
	name := c.Namespace + "." + c.Name
	if c.Source == "" && len(c.Signatures) == 0 {
		if c.Version == "*" {
			return projectfile.Entry{Text: name}
		}
		return projectfile.Entry{Text: name + " " + c.Version}
	}
	fields := []projectfile.Field{{Key: "name", Value: name}}
	if c.Version != "*" {
		fields = append(fields, projectfile.Field{Key: "version", Value: c.Version})
	}
	if c.Source != "" {
		fields = append(fields, projectfile.Field{Key: "source", Value: c.Source})
	}
	if len(c.Signatures) > 0 {
		fields = append(fields, projectfile.Field{Key: "signatures", List: c.Signatures, IsList: true})
	}
	return projectfile.Entry{Fields: fields}
}

// gitCollectionEntry renders a git entry as the documented pointer string, or
// as a table when it names its collection, which the string cannot carry.
func gitCollectionEntry(c CollectionRequirement) projectfile.Entry {
	src := c.Source
	if c.Subdir != "" {
		src += "#" + c.Subdir
	}
	ref := c.Ref
	if isHeadRef(ref) {
		ref = ""
	}
	if c.Namespace == "" && c.Name == "" {
		text := src
		if !gitsource.IsPointer(src) {
			text = "git+" + src
		}
		if ref != "" {
			text += "," + ref
		}
		return projectfile.Entry{Text: text}
	}
	fields := []projectfile.Field{
		{Key: "name", Value: c.Namespace + "." + c.Name}, {Key: "type", Value: TypeGit}, {Key: "source", Value: src},
	}
	if ref != "" {
		fields = append(fields, projectfile.Field{Key: "version", Value: ref})
	}
	return projectfile.Entry{Fields: fields}
}

// roleEntry renders one parsed role as src[,version[,name]], or as a table
// when a part holds a comma or a renamed role has no version. The name is
// left out when the entry derives the same one without it.
func roleEntry(r RoleRequirement) projectfile.Entry {
	src, version, name := roleParts(r)
	if !strings.Contains(src+version+name, ",") && (name == "" || version != "") {
		parts := []string{src}
		if version != "" {
			parts = append(parts, version)
		}
		if name != "" {
			parts = append(parts, name)
		}
		return projectfile.Entry{Text: strings.Join(parts, ",")}
	}
	var fields []projectfile.Field
	if name != "" {
		fields = append(fields, projectfile.Field{Key: "name", Value: name})
	}
	fields = append(fields, projectfile.Field{Key: "src", Value: src})
	if version != "" {
		fields = append(fields, projectfile.Field{Key: "version", Value: version})
	}
	return projectfile.Entry{Fields: fields}
}

// roleParts is what a role entry writes: a git role's src with git+ unless it
// is a pointer already, no HEAD ref, and "" for a name the entry derives.
func roleParts(r RoleRequirement) (string, string, string) {
	src, version, name := r.Src, r.Version, r.Name
	if r.IsGit() && !gitsource.IsPointer(src) {
		src = "git+" + src
	}
	if r.IsGit() && isHeadRef(version) {
		version = ""
	}
	if derivedRoleName(src, version) == name {
		name = ""
	}
	return src, version, name
}

// isHeadRef reports a ref naming the remote's HEAD, which an entry need not spell.
func isHeadRef(ref string) bool {
	parsed, err := gitsource.ParseRef(ref)
	return err == nil && parsed.Kind == gitsource.RefHEAD
}

// derivedRoleName is the install name an entry of src and version gets with
// no name key, or "" when such an entry does not parse, which keeps the name.
func derivedRoleName(src, version string) string {
	entry := map[string]any{"src": src}
	if version != "" {
		entry["version"] = version
	}
	req, _, err := parseRoleItem(entry)
	if err != nil {
		return ""
	}
	return req.Name
}

// verifyMigration parses out back and compares it with want, folding only
// what the two formats cannot tell apart; a difference names the entry and
// no value, and the parse error is rendered so its own class does not apply.
func verifyMigration(want File, name string, out []byte) error {
	got, err := ParseTOML(out, "")
	if err != nil {
		return fmt.Errorf("%w: %v", helpers.ErrMigrateRoundTrip, err) //nolint:errorlint // the cause must not classify the exit.
	}
	if where := firstDifference(migrationView(want), migrationView(got)); where != "" {
		return fmt.Errorf("%w: %s differs", helpers.ErrMigrateRoundTrip, where)
	}
	doc, err := projectfile.Decode(out)
	if err != nil || doc.Project.Name != name {
		return fmt.Errorf("%w: [project] name differs", helpers.ErrMigrateRoundTrip)
	}
	return nil
}

// migrationView is f as the comparison sees it: no warnings, and an empty
// list equal to an absent one, for both lists and every signatures list.
func migrationView(f File) File {
	view := File{}
	for _, c := range f.Collections {
		if len(c.Signatures) == 0 {
			c.Signatures = nil
		}
		view.Collections = append(view.Collections, c)
	}
	if len(f.Roles) > 0 {
		view.Roles = append([]RoleRequirement(nil), f.Roles...)
	}
	return view
}

// firstDifference names the first entry that differs, or "" when none does.
func firstDifference(want, got File) string {
	if len(want.Collections) != len(got.Collections) {
		return "the collections count"
	}
	for i := range want.Collections {
		if !reflect.DeepEqual(want.Collections[i], got.Collections[i]) {
			return fmt.Sprintf("collections[%d]", i)
		}
	}
	if len(want.Roles) != len(got.Roles) {
		return "the roles count"
	}
	for i := range want.Roles {
		if want.Roles[i] != got.Roles[i] {
			return fmt.Sprintf("roles[%d]", i)
		}
	}
	return ""
}

// keyNotices names every key the decoded tree holds that galaxy.toml has no
// place for: a top-level key other than the two lists, and an entry key
// outside the entry's closed set, in sorted order within each mapping.
func keyNotices(raw any) []string {
	var notices []string
	collections := raw
	var roles any
	if m, ok := raw.(map[string]any); ok {
		for _, key := range sortedKeys(m) {
			if key != "collections" && key != "roles" {
				notices = append(notices, fmt.Sprintf("top-level key %q is not carried into galaxy.toml", helpers.TruncateForMessage(key)))
			}
		}
		collections, roles = m["collections"], m["roles"]
	}
	notices = append(notices, entryKeyNotices("collections", collections, collectionTableKeys())...)
	return append(notices, entryKeyNotices("roles", roles, roleMapKeys())...)
}

// entryKeyNotices names the keys of each mapping entry of list outside known.
func entryKeyNotices(list string, value any, known map[string]struct{}) []string {
	items, _ := value.([]any)
	var notices []string
	for i, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range sortedKeys(entry) {
			if _, ok := known[key]; !ok {
				notices = append(notices, fmt.Sprintf("%s[%d]: key %q is not carried into galaxy.toml",
					list, i, helpers.TruncateForMessage(key)))
			}
		}
	}
	return notices
}

// gitVersionNotices names each git entry whose version key loses to the ref
// after a comma in its URL, which is how gitsource.SplitSCM reads the pair.
func gitVersionNotices(raw any, collections Collections) []string {
	items, _ := raw.([]any)
	if m, ok := raw.(map[string]any); ok {
		items, _ = m["collections"].([]any)
	}
	var notices []string
	for i, item := range items {
		entry, ok := item.(map[string]any)
		if !ok || i >= len(collections) || !collections[i].IsGit() {
			continue
		}
		version, _ := entry["version"].(string)
		pointer, _ := entry["source"].(string)
		if strings.TrimSpace(pointer) == "" {
			pointer, _ = entry["name"].(string)
		}
		if strings.TrimSpace(version) != "" && strings.Contains(pointer, ",") {
			notices = append(notices, fmt.Sprintf(
				"collections[%d]: key \"version\" is ignored beside the ref after the comma; it is not carried", i))
		}
	}
	return notices
}

// documentNotices reports comments anywhere and any YAML document past the
// first that holds more than a null or does not parse; go-galaxy reads the
// first document alone, so neither changes what the file installs.
func documentNotices(data []byte) []string {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var comments, later bool
	for i := 0; ; i++ {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			later = later || i > 0
			break
		}
		comments = comments || hasComment(&doc)
		later = later || (i > 0 && !isNullDocument(&doc))
	}
	var notices []string
	if comments {
		notices = append(notices, "comments are not carried into galaxy.toml")
	}
	if later {
		notices = append(notices, "YAML documents after the first are not carried into galaxy.toml; go-galaxy reads only the first")
	}
	return notices
}

// hasComment reports whether n or any node below it carries a comment.
func hasComment(n *yaml.Node) bool {
	if n.HeadComment != "" || n.LineComment != "" || n.FootComment != "" {
		return true
	}
	return slices.ContainsFunc(n.Content, hasComment)
}

// isNullDocument reports a document that holds nothing but a null, as a
// trailing "---" leaves.
func isNullDocument(doc *yaml.Node) bool {
	if len(doc.Content) == 0 {
		return true
	}
	n := doc.Content[0]
	return len(doc.Content) == 1 && n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}
