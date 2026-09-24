// Package requirements parses requirements.yml and galaxy.toml into collection
// and role entries. It is the boundary where every name, URL and signature
// source is validated, and it refuses any shape this tool cannot install.
package requirements

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/projectfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/signature"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
	"go.yaml.in/yaml/v3"
)

// Collections is a list of collection requirements.
type Collections = []CollectionRequirement

// CollectionRequirement is one collection entry. For Galaxy, Version is a
// constraint and Source a server id or URL; for git, Source is the repository
// URL, Ref the ref (HEAD by default), Subdir the fragment, Version empty.
type CollectionRequirement struct {
	Namespace  string
	Name       string
	Version    string
	Source     string
	Type       string
	Ref        string
	Subdir     string
	Signatures []string
}

// Type values a requirements entry may carry. An empty type is Galaxy.
const (
	TypeGalaxy = "galaxy"
	TypeGit    = "git"
	TypeURL    = "url"
)

// IsGit reports whether the requirement names a git source.
func (r CollectionRequirement) IsGit() bool { return r.Type == TypeGit }

// IsURL reports whether the requirement names a url source: Source is then
// the canonical tarball URL, Version the asserted exact version or "", and the
// artifact's MANIFEST.json supplies the identity.
func (r CollectionRequirement) IsURL() bool { return r.Type == TypeURL }

// File is everything a requirements file declares: its collections, its
// roles, and the warnings parsing raised (a key on a role entry ansible
// would drop without a word), for the caller to print.
type File struct {
	Collections Collections
	Roles       []RoleRequirement
	Warnings    []string
}

// Load reads and parses a requirements file. The format is picked by the
// path's extension alone: a .toml file is galaxy.toml, anything else YAML.
func Load(path, defaultSource string) (File, error) {
	data, err := Read(path)
	if err != nil {
		return File{}, err
	}
	if projectfile.IsTOMLPath(path) {
		return ParseTOML(data, defaultSource)
	}
	return Parse(data, defaultSource)
}

// Read returns a requirements file's bytes unparsed. Absence stays a bare
// fs.ErrNotExist; any other failure wraps helpers.ErrRequirementsUnreadable.
func Read(path string) ([]byte, error) {
	//nolint:gosec // path is user-provided requirements file.
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %w", helpers.ErrRequirementsUnreadable, err)
	}
	return data, err
}

// Parse parses requirements data.
func Parse(data []byte, defaultSource string) (File, error) {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return File{}, fmt.Errorf("%w: %w", helpers.ErrInvalidRequirementsYAML, err)
	}
	return parseRaw(raw, defaultSource)
}

// parseRaw parses a decoded requirements payload. A mapping needs at least
// one of the two keys; a bare list is a collections list.
func parseRaw(raw any, defaultSource string) (File, error) {
	switch v := raw.(type) {
	case map[string]any:
		collectionsRaw, hasCollections := v["collections"]
		rolesRaw, hasRoles := v["roles"]
		if !hasCollections && !hasRoles {
			return File{}, helpers.ErrUnsupportedRequirementsFormat
		}
		var f File
		var err error
		if f.Collections, err = parseCollectionList(collectionsRaw, defaultSource); err != nil {
			return File{}, err
		}
		if f.Roles, f.Warnings, err = parseRoleList(rolesRaw); err != nil {
			// The collections ride beside the refusal so cleanup's
			// reachability walk can still use them.
			return File{Collections: f.Collections}, &RolesError{Err: err}
		}
		return f, nil
	case []any:
		cols, err := parseCollectionList(v, defaultSource)
		if err != nil {
			return File{}, err
		}
		return File{Collections: cols}, nil
	default:
		return File{}, helpers.ErrUnsupportedRequirementsFormat
	}
}

// parseCollectionList parses a list of collection items. A nil value (ansible
// takes a bare "collections:" as an empty list) yields no entries; any other
// non-list value is refused.
func parseCollectionList(raw any, defaultSource string) (Collections, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, helpers.ErrInvalidCollectionsList
	}
	items := make(Collections, 0, len(list))
	for _, item := range list {
		req, err := parseCollectionItem(item, defaultSource)
		if err != nil {
			return nil, err
		}
		items = append(items, req)
	}
	return items, nil
}

// parseCollectionItem parses one entry and applies the collection name
// alphabet. It sits where both item shapes converge, since a mapping's
// explicit namespace: and name: never pass through helpers.SplitFQDN.
func parseCollectionItem(item any, defaultSource string) (CollectionRequirement, error) {
	req, err := parseCollectionItemByShape(item, defaultSource)
	if err != nil {
		return CollectionRequirement{}, err
	}
	// An unnamed git entry and every url entry get their identity at
	// discovery, from galaxy.yml or MANIFEST.json, where it is judged.
	if (req.IsGit() || req.IsURL()) && req.Namespace == "" && req.Name == "" {
		return req, nil
	}
	if !helpers.IsCollectionNamePart(req.Namespace) || !helpers.IsCollectionNamePart(req.Name) {
		return CollectionRequirement{}, fmt.Errorf("%w: %q.%q must each match ^[a-z][a-z0-9_]*$",
			helpers.ErrInvalidCollectionName, req.Namespace, req.Name)
	}
	return req, nil
}

// parseCollectionItemByShape dispatches on the entry's YAML shape; it is kept
// apart so the alphabet check in parseCollectionItem covers both branches.
func parseCollectionItemByShape(item any, defaultSource string) (CollectionRequirement, error) {
	switch v := item.(type) {
	case string:
		return parseCollectionStringItem(v, defaultSource)
	case map[string]any:
		return parseCollectionMapItem(v, defaultSource)
	default:
		return CollectionRequirement{}, fmt.Errorf("%w: %v", helpers.ErrUnsupportedCollectionFormat, item)
	}
}

func parseCollectionStringItem(value string, defaultSource string) (CollectionRequirement, error) {
	name := strings.TrimSpace(value)
	if name == "" {
		return CollectionRequirement{}, helpers.ErrEmptyCollectionName
	}
	if gitsource.IsPointer(name) {
		return parseGitRequirement(name, "", "", "")
	}
	if urlsource.IsHTTPURL(name) {
		// ansible infers a url source from any http(s) URL in the name
		// position; whether it really serves a collection tarball is the
		// download's own question.
		return parseURLRequirement(name, "")
	}
	if looksLikeSourceName(name) {
		return CollectionRequirement{}, fmt.Errorf("%w %q (only Galaxy API, git and url sources are supported)",
			helpers.ErrUnsupportedCollectionSource, helpers.URLForMessage(name))
	}
	namespace, collection, ok := helpers.SplitFQDN(name)
	if !ok {
		return CollectionRequirement{}, fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionName, name)
	}
	return CollectionRequirement{
		Namespace: namespace,
		Name:      collection,
		Version:   "*",
		Source:    defaultSource,
	}, nil
}

func parseCollectionMapItem(value map[string]any, defaultSource string) (CollectionRequirement, error) {
	for _, key := range []string{"src", "scm"} {
		if _, ok := value[key]; ok {
			return CollectionRequirement{}, fmt.Errorf("%w: %s is a role key; a roles list goes under the roles key",
				helpers.ErrInvalidCollectionEntry, key)
		}
	}
	req := parseCollectionMapFields(value)
	if req.Type == TypeGit || (req.Type == "" && gitsource.IsPointer(req.Name)) {
		return parseGitMapItem(req, value)
	}
	if req.Type == TypeURL || (req.Type == "" && urlsource.IsHTTPURL(req.Name)) {
		return parseURLMapItem(req, value)
	}
	if err := checkNamespaceNameConflict(req); err != nil {
		return CollectionRequirement{}, err
	}
	req = normalizeCollectionName(req)
	return finalizeCollectionRequirement(req, defaultSource, value)
}

func parseCollectionMapFields(value map[string]any) CollectionRequirement {
	req := CollectionRequirement{}
	if raw, ok := value["namespace"].(string); ok {
		req.Namespace = strings.TrimSpace(raw)
	}
	if raw, ok := value["name"]; ok {
		req.Name = strings.TrimSpace(fmt.Sprint(raw))
	}
	if raw, ok := value["source"].(string); ok {
		req.Source = strings.TrimSpace(raw)
	}
	if raw, ok := value["type"].(string); ok {
		req.Type = strings.ToLower(strings.TrimSpace(raw))
	}
	if raw, ok := value["signatures"]; ok {
		req.Signatures = parseStringList(raw)
	}
	if raw, ok := value["version"]; ok {
		req.Version = strings.TrimSpace(fmt.Sprint(raw))
	}
	return req
}

// checkNamespaceNameConflict refuses an explicit namespace beside a dotted
// name (namespace: foo, name: bar.baz) under exactly the conditions in which
// normalizeCollectionName would silently replace name with its last segment.
func checkNamespaceNameConflict(req CollectionRequirement) error {
	if req.Namespace == "" || req.Name == "" || !strings.Contains(req.Name, ".") ||
		req.Type != "" || looksLikeSourceName(req.Name) {
		return nil
	}
	if _, _, ok := helpers.SplitFQDN(req.Name); !ok {
		return nil
	}
	return fmt.Errorf("%w: namespace %q with dotted name %q", helpers.ErrConflictingNamespaceName, req.Namespace, req.Name)
}

func normalizeCollectionName(req CollectionRequirement) CollectionRequirement {
	if req.Name == "" || !strings.Contains(req.Name, ".") || req.Type != "" || looksLikeSourceName(req.Name) {
		return req
	}
	namespace, collection, ok := helpers.SplitFQDN(req.Name)
	if !ok {
		return req
	}
	if req.Namespace == "" {
		req.Namespace = namespace
	}
	req.Name = collection
	return req
}

func finalizeCollectionRequirement(req CollectionRequirement, defaultSource string, raw any) (CollectionRequirement, error) {
	if err := validateRequirement(req, raw); err != nil {
		return CollectionRequirement{}, err
	}
	req = applyRequirementDefaults(req, defaultSource)
	req, err := normalizeRequirementNamespace(req)
	if err != nil {
		return CollectionRequirement{}, err
	}
	return req, nil
}

func validateRequirement(req CollectionRequirement, raw any) error {
	// Checked before the req.Name branch below, which echoes raw in its
	// error, so a userinfo-bearing source: is refused before it can print.
	if err := checkSourceUserinfo(req); err != nil {
		return err
	}
	// Second, and ahead of the raw-echoing branch below for the same reason
	// checkSourceUserinfo is first: a signatures: entry can carry a credential
	// of its own, and this check refuses one without printing it.
	if err := checkSignatureSources(req, raw); err != nil {
		return err
	}
	if req.Name == "" {
		return fmt.Errorf("%w: %v", helpers.ErrInvalidCollectionEntry, raw)
	}
	if req.Type != "" && req.Type != TypeGalaxy {
		return fmt.Errorf("%w %q (only galaxy, git and url are supported)", helpers.ErrUnsupportedCollectionType, req.Type)
	}
	if req.Type == "" && looksLikeSourceName(req.Name) {
		return fmt.Errorf("%w %q (only Galaxy API, git and url sources are supported)",
			helpers.ErrUnsupportedCollectionSource, helpers.URLForMessage(req.Name))
	}
	return checkGalaxySourceShape(req)
}

// checkGalaxySourceShape refuses a Galaxy entry whose source: is a git pointer
// or a git or url locator: it would otherwise be dispatched later by prefix
// alone with its URL never judged. type: git and type: url spell those.
func checkGalaxySourceShape(req CollectionRequirement) error {
	if gitsource.IsPointer(req.Source) || gitsource.IsLocator(req.Source) {
		return fmt.Errorf("%w: source %q names a git repository; spell the entry with type: git",
			helpers.ErrUnsupportedCollectionSource, helpers.URLForMessage(req.Source))
	}
	if urlsource.IsLocator(req.Source) {
		return fmt.Errorf("%w: source %q names a url artifact; spell the entry with type: url",
			helpers.ErrUnsupportedCollectionSource, helpers.URLForMessage(req.Source))
	}
	return nil
}

// parseURLMapItem parses a mapping naming a url source (type: url, or an
// http(s) URL in name:). signatures:, source: and namespace: are refused, and
// version: must be exact, as it is asserted against the MANIFEST.json.
func parseURLMapItem(req CollectionRequirement, raw map[string]any) (CollectionRequirement, error) {
	if value, ok := raw["signatures"]; ok && value != nil {
		return CollectionRequirement{}, fmt.Errorf("%w: a signatures key is not supported on a url requirement",
			helpers.ErrInvalidCollectionEntry)
	}
	if req.Source != "" {
		return CollectionRequirement{}, fmt.Errorf("%w: a url entry carries its URL in the name key; source names a Galaxy server",
			helpers.ErrInvalidCollectionEntry)
	}
	if req.Namespace != "" {
		return CollectionRequirement{}, fmt.Errorf("%w: a url entry has no namespace key; the artifact's MANIFEST.json names its collection",
			helpers.ErrInvalidCollectionEntry)
	}
	if !urlsource.IsHTTPURL(req.Name) {
		return CollectionRequirement{}, fmt.Errorf("%w: a url entry needs an http(s) URL in its name key",
			helpers.ErrInvalidCollectionEntry)
	}
	if req.Version != "" && req.Version != "*" && !helpers.IsExactVersion(req.Version) {
		return CollectionRequirement{}, fmt.Errorf("%w: url version %q is not an exact version",
			helpers.ErrInvalidCollectionVersion, req.Version)
	}
	version := req.Version
	if version == "*" {
		version = ""
	}
	return parseURLRequirement(req.Name, version)
}

// parseURLRequirement judges a tarball URL through urlsource's grammar, which
// refuses a credential before any error message can render the URL.
func parseURLRequirement(rawURL, version string) (CollectionRequirement, error) {
	u, err := urlsource.ParseURL(rawURL)
	if err != nil {
		return CollectionRequirement{}, err
	}
	return CollectionRequirement{
		Source:  u.String(),
		Type:    TypeURL,
		Version: version,
	}, nil
}

// parseGitMapItem parses a mapping naming a git source (type: git, or a git
// pointer in name:). With the URL in source:, name: may pick one collection;
// signatures: is refused, since nobody signed an artifact built here.
func parseGitMapItem(req CollectionRequirement, raw map[string]any) (CollectionRequirement, error) {
	if value, ok := raw["signatures"]; ok && value != nil {
		return CollectionRequirement{}, fmt.Errorf("%w: a signatures key is not supported on a git requirement",
			helpers.ErrInvalidCollectionEntry)
	}
	if req.Source == "" {
		if req.Namespace != "" {
			return CollectionRequirement{}, fmt.Errorf("%w: a git entry names its collection through the name key beside a source key",
				helpers.ErrInvalidCollectionEntry)
		}
		if req.Name == "" {
			return CollectionRequirement{}, fmt.Errorf("%w: a git entry needs a repository URL in its name or source key",
				helpers.ErrInvalidCollectionEntry)
		}
		return parseGitRequirement(req.Name, req.Version, "", "")
	}
	namespace, name, err := gitCollectionName(req)
	if err != nil {
		return CollectionRequirement{}, err
	}
	return parseGitRequirement(req.Source, req.Version, namespace, name)
}

// gitCollectionName reads the optional collection name of a git entry whose
// source: holds the URL: none, the namespace:/name: pair, or name: as an FQDN.
func gitCollectionName(req CollectionRequirement) (string, string, error) {
	switch {
	case req.Name == "" && req.Namespace == "":
		return "", "", nil
	case gitsource.IsPointer(req.Name):
		return "", "", fmt.Errorf("%w: name and source both name a git repository",
			helpers.ErrInvalidCollectionEntry)
	case req.Namespace != "":
		return req.Namespace, req.Name, nil
	}
	namespace, name, ok := helpers.SplitFQDN(req.Name)
	if !ok {
		return "", "", fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionName, req.Name)
	}
	return namespace, name, nil
}

// parseGitRequirement splits a git pointer in ansible's parse_scm order
// (gitsource.SplitSCM) and judges URL, ref and subdir through gitsource's
// grammar, which refuses a credential before any message renders the URL.
func parseGitRequirement(pointer, version, namespace, name string) (CollectionRequirement, error) {
	rawURL, rawRef, rawSubdir := gitsource.SplitSCM(pointer, version)
	u, err := gitsource.ParseURL(rawURL)
	if err != nil {
		return CollectionRequirement{}, err
	}
	ref, err := gitsource.ParseRef(rawRef)
	if err != nil {
		return CollectionRequirement{}, fmt.Errorf("%s: %w", helpers.URLForMessage(u.String()), err)
	}
	subdir, err := gitsource.ParseSubdir(rawSubdir)
	if err != nil {
		return CollectionRequirement{}, fmt.Errorf("%s: %w", helpers.URLForMessage(u.String()), err)
	}
	return CollectionRequirement{
		Namespace: namespace,
		Name:      name,
		Source:    u.String(),
		Type:      TypeGit,
		Ref:       ref.Name,
		Subdir:    subdir,
	}, nil
}

// checkSignatureSources judges signatures:, the one repository-authored field
// no other boundary does: its shape, the declared-count cap, and each source
// through signature.ValidateRequirementSource, the rule the fetch shares.
func checkSignatureSources(req CollectionRequirement, raw any) error {
	if err := checkSignatureSourceShape(raw); err != nil {
		return err
	}
	if len(req.Signatures) > helpers.MaxSignaturesPerCollection {
		return fmt.Errorf("%w: %d declared, at most %d are gathered",
			helpers.ErrTooManySignatureSources, len(req.Signatures), helpers.MaxSignaturesPerCollection)
	}
	for _, source := range req.Signatures {
		if err := signature.ValidateRequirementSource(source); err != nil {
			return err
		}
	}

	return nil
}

// checkSignatureSourceShape refuses a signatures: value that is neither a
// string nor a list of strings, naming its Go type and never its content. It
// reads raw because parseStringList's fmt.Sprint would disguise a bad value.
func checkSignatureSourceShape(raw any) error {
	item, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	value, ok := item["signatures"]
	if !ok || value == nil {
		return nil
	}
	switch v := value.(type) {
	case string:
		return nil
	case []any:
		for _, entry := range v {
			if _, ok := entry.(string); !ok {
				return fmt.Errorf("%w: signatures: carries a %T where a source string was expected",
					helpers.ErrUnsupportedSignatureSource, entry)
			}
		}

		return nil
	default:
		return fmt.Errorf("%w: signatures: is a %T, not a list of source strings",
			helpers.ErrUnsupportedSignatureSource, value)
	}
}

// checkSourceUserinfo refuses a URL-shaped source: embedding userinfo: it is
// repository content, and url.URL.String() would render the password into
// request URLs, logs, the snapshot, the lockfile and GALAXY.yml.
func checkSourceUserinfo(req CollectionRequirement) error {
	if req.Source == "" {
		return nil
	}
	parsed, err := url.Parse(req.Source)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: collection %q", helpers.ErrGalaxyServerURLUserinfo, req.Name)
	}
	return nil
}

func applyRequirementDefaults(req CollectionRequirement, defaultSource string) CollectionRequirement {
	if req.Version == "" {
		req.Version = "*"
	}
	if req.Source == "" && (req.Type == "galaxy" || (req.Type == "" && !looksLikeSourceName(req.Name))) {
		req.Source = defaultSource
	}
	return req
}

func normalizeRequirementNamespace(req CollectionRequirement) (CollectionRequirement, error) {
	if req.Namespace != "" || req.Type != "" || looksLikeSourceName(req.Name) {
		return req, nil
	}
	namespace, collection, ok := helpers.SplitFQDN(req.Name)
	if !ok {
		return CollectionRequirement{}, fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionName, req.Name)
	}
	req.Namespace = namespace
	req.Name = collection
	return req, nil
}

// parseStringList converts an arbitrary value to a string slice.
func parseStringList(value any) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		item := strings.TrimSpace(v)
		if item == "" {
			return nil
		}
		return []string{item}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			str := strings.TrimSpace(fmt.Sprint(item))
			if str == "" {
				continue
			}
			out = append(out, str)
		}
		return out
	default:
		str := strings.TrimSpace(fmt.Sprint(v))
		if str == "" {
			return nil
		}
		return []string{str}
	}
}

// looksLikeSourceName reports whether the value looks like a URL or path.
func looksLikeSourceName(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	switch {
	case strings.Contains(lower, "://"):
		return true
	case strings.HasPrefix(lower, "git+"):
		return true
	case strings.HasPrefix(lower, "git@"):
		return true
	case strings.HasPrefix(lower, "./"),
		strings.HasPrefix(lower, "../"),
		strings.HasPrefix(lower, "/"),
		strings.HasPrefix(lower, "~"):
		return true
	}
	return false
}

// RolesError reports that collections: parsed and roles: did not. Load and
// Parse return it with File.Collections filled, for a caller (cleanup) that
// can act on the collections alone; every other caller treats it as fatal.
type RolesError struct {
	Err error
}

func (e *RolesError) Error() string { return e.Err.Error() }

// Unwrap exposes the role sentinel the refusal classifies by.
func (e *RolesError) Unwrap() error { return e.Err }
