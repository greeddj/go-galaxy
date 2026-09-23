package requirements

import (
	"fmt"
	"path"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// RoleRequirement is one validated roles: entry, normalized as ansible's
// role_yaml_parse does. Name is the install directory under roles_path; per
// Type, Src is owner.role or a canonical URL and Version a tag, ref or label.
type RoleRequirement struct {
	Name    string
	Src     string
	Version string
	Type    string
}

// IsGit reports whether the role comes from a git repository.
func (r RoleRequirement) IsGit() bool { return r.Type == TypeGit }

// IsURL reports whether the role comes from a tarball URL. Src is then the
// canonical URL and Version the label the role installs under ("" for
// none).
func (r RoleRequirement) IsURL() bool { return r.Type == TypeURL }

// roleSpecMaxCommas is how many commas ansible's string form admits:
// src[,version[,name]].
const roleSpecMaxCommas = 2

// roleStringSpec is the three positions of ansible's string spelling.
const (
	roleSpecSrc = iota
	roleSpecVersion
	roleSpecName
)

// parseRoleList parses the roles: value; nil is an empty list. Two entries
// installing into one directory are refused here, where both spellings are
// still in hand.
func parseRoleList(raw any) ([]RoleRequirement, []string, error) {
	if raw == nil {
		return nil, nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, nil, helpers.ErrInvalidRolesList
	}
	roles := make([]RoleRequirement, 0, len(list))
	var warnings []string
	// Keyed by the folded name: two names differing only by case would land
	// on one directory on a case-insensitive filesystem, the way the git
	// tree reader refuses two such entries of one tree.
	seen := make(map[string]int, len(list))
	for i, item := range list {
		req, itemWarnings, err := parseRoleItem(item)
		if err != nil {
			return nil, nil, fmt.Errorf("roles[%d]: %w", i, err)
		}
		folded := strings.ToLower(req.Name)
		if first, dup := seen[folded]; dup {
			return nil, nil, fmt.Errorf("%w: roles[%d] and roles[%d] both install into %s",
				helpers.ErrDuplicateRoleRequirement, first, i, req.Name)
		}
		seen[folded] = i
		roles = append(roles, req)
		warnings = append(warnings, itemWarnings...)
	}
	return roles, warnings, nil
}

// roleSpec is a roles: entry with its keys read and nothing judged yet: the
// common ground of the string and the mapping spellings.
type roleSpec struct {
	name    string
	src     string
	scm     string
	version string
}

// parseRoleItem dispatches on the entry's YAML shape and judges the result.
func parseRoleItem(item any) (RoleRequirement, []string, error) {
	switch v := item.(type) {
	case string:
		spec, err := parseRoleString(v)
		if err != nil {
			return RoleRequirement{}, nil, err
		}
		req, err := finishRole(spec)
		return req, nil, err
	case map[string]any:
		spec, warnings, err := parseRoleMap(v)
		if err != nil {
			return RoleRequirement{}, nil, err
		}
		req, err := finishRole(spec)
		return req, warnings, err
	default:
		return RoleRequirement{}, nil, fmt.Errorf("%w: a role entry is a name or a mapping, not a %T",
			helpers.ErrInvalidRoleEntry, item)
	}
}

// parseRoleString reads ansible's string spelling, src[,version[,name]],
// with a "scm+" prefix on src naming the scm.
func parseRoleString(value string) (roleSpec, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return roleSpec{}, fmt.Errorf("%w: an empty role name", helpers.ErrInvalidRoleEntry)
	}
	if strings.Count(value, ",") > roleSpecMaxCommas {
		return roleSpec{}, fmt.Errorf("%w: %q has more than %d commas; the format is src[,version[,name]]",
			helpers.ErrInvalidRoleEntry, helpers.TruncateForMessage(value), roleSpecMaxCommas)
	}
	parts := strings.Split(value, ",")
	spec := roleSpec{src: strings.TrimSpace(parts[roleSpecSrc])}
	if len(parts) > roleSpecVersion {
		spec.version = strings.TrimSpace(parts[roleSpecVersion])
	}
	if len(parts) > roleSpecName {
		spec.name = strings.TrimSpace(parts[roleSpecName])
	}
	spec.scm, spec.src = splitScmPrefix(spec.src)
	return spec, nil
}

// roleMapKeys are the keys ansible's role_yaml_parse keeps; anything else
// is dropped there and warned about here.
func roleMapKeys() map[string]struct{} {
	return map[string]struct{}{"name": {}, "role": {}, "src": {}, "scm": {}, "version": {}}
}

// roleForbiddenKeys are collection keys meaningless on a role entry. Each is
// refused by name rather than dropped, since dropping it would silently
// change what the entry means.
func roleForbiddenKeys() []string { return []string{"source", "signatures", "type"} }

// parseRoleMap reads the mapping spelling (src:, scm:, version:, name:, role:).
// include: and the collection keys are refused; any other key is a warning,
// where ansible drops it without a word.
func parseRoleMap(value map[string]any) (roleSpec, []string, error) {
	if _, ok := value["include"]; ok {
		return roleSpec{}, nil, fmt.Errorf("%w: list the included roles inline", helpers.ErrUnsupportedRoleInclude)
	}
	for _, key := range roleForbiddenKeys() {
		if _, ok := value[key]; ok {
			return roleSpec{}, nil, fmt.Errorf("%w: a role entry takes no %s key", helpers.ErrInvalidRoleEntry, key)
		}
	}
	var warnings []string
	for key := range value {
		if _, ok := roleMapKeys()[key]; !ok {
			warnings = append(warnings, "ignoring unknown key "+helpers.TruncateForMessage(key)+" on a role entry")
		}
	}
	spec, err := roleMapSpec(value)
	if err != nil {
		return roleSpec{}, nil, err
	}
	return spec, warnings, nil
}

// roleMapSpec reads the spec keys of a mapping entry and fills the
// defaults ansible fills: role: is name: and, absent a src:, the src too;
// a name: alone is the src as well; an "scm+" prefix on src names the scm.
func roleMapSpec(value map[string]any) (roleSpec, error) {
	spec := roleSpec{
		name:    stringField(value, "name"),
		src:     stringField(value, "src"),
		scm:     strings.ToLower(stringField(value, "scm")),
		version: stringField(value, "version"),
	}
	if role := stringField(value, "role"); role != "" {
		if strings.Contains(role, ",") {
			return roleSpec{}, fmt.Errorf("%w: an old-style role: %q carries a comma",
				helpers.ErrInvalidRoleEntry, helpers.TruncateForMessage(role))
		}
		spec.name = role
		if spec.src == "" {
			spec.src = role
		}
	}
	if spec.src == "" {
		if spec.name == "" {
			return roleSpec{}, fmt.Errorf("%w: a role entry needs a src or a name", helpers.ErrInvalidRoleEntry)
		}
		spec.src = spec.name
	}
	if scm, src := splitScmPrefix(spec.src); scm != "" {
		spec.scm, spec.src = scm, src
	}
	return spec, nil
}

// stringField reads one scalar key of a mapping as a trimmed string; a
// non-string scalar (a bare 1.0 version) is rendered, a missing key is "".
func stringField(value map[string]any, key string) string {
	raw, ok := value[key]
	if !ok || raw == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(raw))
}

// splitScmPrefix cuts ansible's "scm+url" spelling: the text before the
// first "+" is the scm when the remainder reads as a URL. A "+" inside a
// plain Galaxy name is not one, and the alphabet refuses it later.
func splitScmPrefix(src string) (string, string) {
	before, after, ok := strings.Cut(src, "+")
	if !ok || before == "" || !strings.Contains(after, ":") {
		return "", src
	}
	return strings.ToLower(before), after
}

// finishRole judges a spec: its scm, its src classification, its version
// and the install name it ends up with.
func finishRole(spec roleSpec) (RoleRequirement, error) {
	if spec.scm != "" && spec.scm != TypeGit {
		return RoleRequirement{}, fmt.Errorf("%w %q (only git is supported)", helpers.ErrUnsupportedRoleScm, spec.scm)
	}
	if spec.version == "*" {
		spec.version = ""
	}
	req, err := classifyRole(spec)
	if err != nil {
		return RoleRequirement{}, err
	}
	if !helpers.IsRoleInstallName(req.Name) {
		return RoleRequirement{}, fmt.Errorf("%w: %q", helpers.ErrInvalidRoleInstallName, helpers.TruncateForMessage(req.Name))
	}
	return req, nil
}

// classifyRole decides what src: names, in ansible's order: git (scm git, a
// git pointer, or the github.com case), then an http(s) .tar.gz url, then a
// refused URL, path or tarball, else a Galaxy role name.
func classifyRole(spec roleSpec) (RoleRequirement, error) {
	switch {
	case spec.scm == TypeGit || gitsource.IsPointer(spec.src) || isGitHubHTTPSource(spec.src):
		return gitRole(spec)
	case isURLRoleSource(spec.src):
		return urlRole(spec)
	case looksLikeSourceName(spec.src) || strings.HasSuffix(strings.ToLower(spec.src), ".tar.gz"):
		return RoleRequirement{}, fmt.Errorf("%w %q (only Galaxy roles, git and url sources are supported)",
			helpers.ErrUnsupportedRoleSource, helpers.URLForMessage(spec.src))
	default:
		return galaxyRole(spec)
	}
}

// isURLRoleSource reports whether src is an http(s) URL ending .tar.gz. Any
// other URL is refused, where ansible would try to download it: it is far
// more often a mistyped repository than an artifact.
func isURLRoleSource(src string) bool {
	return urlsource.IsHTTPURL(src) && strings.HasSuffix(strings.ToLower(src), ".tar.gz")
}

// urlRole judges a url spec through urlsource's grammar (no credential, no
// fragment). Version is only an install label, since the sha256 pins the
// bytes; the name defaults to the URL's basename minus .tar.gz, as in ansible.
func urlRole(spec roleSpec) (RoleRequirement, error) {
	u, err := urlsource.ParseURL(spec.src)
	if err != nil {
		return RoleRequirement{}, err
	}
	if spec.version != "" && !helpers.IsRoleVersion(spec.version) {
		return RoleRequirement{}, fmt.Errorf("%w: %q", helpers.ErrInvalidRoleVersion, helpers.TruncateForMessage(spec.version))
	}
	name := spec.name
	if name == "" {
		name = strings.TrimSuffix(path.Base(u.Path), ".tar.gz")
	}
	return RoleRequirement{Name: name, Src: u.String(), Version: spec.version, Type: TypeURL}, nil
}

// isGitHubHTTPSource is ansible's special case: an http(s) github.com URL
// with no .tar.gz suffix is a git repository. The parsed host must be exactly
// github.com, where ansible matches the substring anywhere.
func isGitHubHTTPSource(src string) bool {
	lower := strings.ToLower(src)
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		return false
	}
	if strings.HasSuffix(lower, ".tar.gz") {
		return false
	}
	u, err := gitsource.ParseURL(src)
	return err == nil && u.Host == "github.com"
}

// gitRole refuses a #subdir and judges the URL (no credential) and the version
// (a ref) through gitsource's grammar. The name defaults to the URL's last path
// element minus .git, as ansible's repo_url_to_role_name derives it.
func gitRole(spec roleSpec) (RoleRequirement, error) {
	raw := strings.TrimPrefix(strings.TrimPrefix(spec.src, "git+"), "GIT+")
	if strings.Contains(raw, "#") {
		return RoleRequirement{}, fmt.Errorf("%w: a role source has no #subdir; the repository root is the role",
			helpers.ErrUnsupportedRoleSource)
	}
	u, err := gitsource.ParseURL(raw)
	if err != nil {
		return RoleRequirement{}, err
	}
	ref, err := gitsource.ParseRef(spec.version)
	if err != nil {
		return RoleRequirement{}, fmt.Errorf("%s: %w", helpers.URLForMessage(u.String()), err)
	}
	name := spec.name
	if name == "" {
		name = strings.TrimSuffix(path.Base(u.Path), ".git")
	}
	return RoleRequirement{Name: name, Src: u.String(), Version: ref.Name, Type: TypeGit}, nil
}

// galaxyRole judges a Galaxy spec: the name through the role alphabet, the
// version through the persisted-shape rule, and the install name defaulting
// to the Galaxy name itself, which is the directory ansible installs it to.
func galaxyRole(spec roleSpec) (RoleRequirement, error) {
	if !helpers.IsRoleName(spec.src) {
		return RoleRequirement{}, fmt.Errorf("%w: %q is not owner.role with each half matching ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$",
			helpers.ErrInvalidRoleName, helpers.TruncateForMessage(spec.src))
	}
	if spec.version != "" && !helpers.IsRoleVersion(spec.version) {
		return RoleRequirement{}, fmt.Errorf("%w: %q", helpers.ErrInvalidRoleVersion, helpers.TruncateForMessage(spec.version))
	}
	name := spec.name
	if name == "" {
		name = spec.src
	}
	return RoleRequirement{Name: name, Src: spec.src, Version: spec.version, Type: TypeGalaxy}, nil
}

// DependencySkip says why a dependency a role's meta declares is not one
// this tool installs: ansible skips the same two shapes, so the caller
// reports the skip and moves on rather than failing the role.
type DependencySkip uint8

const (
	// DependencyInstalled is the zero value: the dependency is a role to
	// install.
	DependencyInstalled DependencySkip = iota
	// DependencyLocal is a bare name with no dot and no scm - a role the
	// playbook's own roles_path is expected to hold, which ansible-galaxy
	// never looks up.
	DependencyLocal
	// DependencyCollection is a three-part name, namespace.collection.role -
	// a role inside a collection, which the collection install supplies.
	DependencyCollection
)

// ParseRoleDependency judges a dependency from a role's meta with the same
// grammar as a roles: entry. A local role or a collection's role, which
// ansible skips too, is reported as a DependencySkip rather than an error.
func ParseRoleDependency(dep gitsource.RoleDependency) (RoleRequirement, DependencySkip, error) {
	spec := roleSpec{name: strings.TrimSpace(dep.Name), src: strings.TrimSpace(dep.Src),
		scm: strings.ToLower(strings.TrimSpace(dep.Scm)), version: strings.TrimSpace(dep.Version)}
	if strings.Contains(spec.src, ",") {
		parsed, err := parseRoleString(spec.src)
		if err != nil {
			return RoleRequirement{}, DependencyInstalled, err
		}
		if spec.name == "" {
			spec.name = parsed.name
		}
		if spec.version == "" {
			spec.version = parsed.version
		}
		spec.src = parsed.src
		if parsed.scm != "" {
			spec.scm = parsed.scm
		}
	}
	if spec.src == "" {
		spec.src = spec.name
	}
	if scm, src := splitScmPrefix(spec.src); scm != "" {
		spec.scm, spec.src = scm, src
	}
	if skip := dependencySkip(spec); skip != DependencyInstalled {
		return RoleRequirement{}, skip, nil
	}
	req, err := finishRole(spec)
	return req, DependencyInstalled, err
}

// dependencySkip classifies the two dependency shapes ansible leaves alone,
// before source classification: a no-dot name is local, a name with two or
// more dots is a collection's role.
func dependencySkip(spec roleSpec) DependencySkip {
	if spec.scm != "" || gitsource.IsPointer(spec.src) || looksLikeSourceName(spec.src) {
		return DependencyInstalled
	}
	switch strings.Count(spec.src, ".") {
	case 0:
		return DependencyLocal
	case 1:
		return DependencyInstalled
	default:
		return DependencyCollection
	}
}
