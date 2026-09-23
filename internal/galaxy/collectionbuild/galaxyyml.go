package collectionbuild

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// metadataMaxBytes is helpers.BuildMetadataMaxBytes, the cap every
	// builder applies to a metadata file before decoding it.
	metadataMaxBytes = helpers.BuildMetadataMaxBytes
	// buildIgnoreMaxLen caps one build_ignore pattern. The matcher is linear
	// in pattern length times name length, so the bound on the pattern is
	// what keeps a crafted galaxy.yml from turning the walk quadratic.
	buildIgnoreMaxLen = 256
	// versionRemedy is what every version refusal tells the operator to do.
	versionRemedy = "set version: to MAJOR.MINOR.PATCH"
	// galaxyYMLName is the only spelling discovery recognizes: ansible's
	// dir classification looks for this name alone, and lists galaxy.yaml
	// only among the files a build leaves out.
	galaxyYMLName = "galaxy.yml"

	keyNamespace     = "namespace"
	keyName          = "name"
	keyVersion       = "version"
	keyReadme        = "readme"
	keyAuthors       = "authors"
	keyDescription   = "description"
	keyLicense       = "license"
	keyLicenseFile   = "license_file"
	keyTags          = "tags"
	keyDependencies  = "dependencies"
	keyRepository    = "repository"
	keyDocumentation = "documentation"
	keyHomepage      = "homepage"
	keyIssues        = "issues"
	keyBuildIgnore   = "build_ignore"
	keyManifest      = "manifest"
)

// stringKeys are the galaxy.yml keys whose value is one string; listKeys the
// ones whose value is a list of strings. Together with dependencies and
// manifest they are the schema ansible ships as collections_galaxy_meta.yml.
func stringKeys() []string {
	return []string{keyNamespace, keyName, keyVersion, keyReadme, keyDescription, keyLicenseFile,
		keyRepository, keyDocumentation, keyHomepage, keyIssues}
}

func listKeys() []string {
	return []string{keyAuthors, keyLicense, keyTags, keyBuildIgnore}
}

// mandatoryKeys are the keys ansible marks required. version is checked
// apart from the others so its refusal can name the remedy.
func mandatoryKeys() []string {
	return []string{keyNamespace, keyName, keyReadme, keyAuthors}
}

// ParseGalaxyYML parses galaxy.yml bytes into metadata, warning on unknown
// keys. A null string key reads as "" and a lone string as a one-item list;
// any other mismatch is refused, since ansible would turn version 1.10 to 1.1.
func ParseGalaxyYML(data []byte) (GalaxyYML, []string, error) {
	if len(data) > metadataMaxBytes {
		return GalaxyYML{}, nil, fmt.Errorf("%w: %s is %d bytes, the limit is %d",
			helpers.ErrGalaxyYMLInvalid, galaxyYMLName, len(data), metadataMaxBytes)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return GalaxyYML{}, nil, fmt.Errorf("%w: %s does not parse: %w", helpers.ErrGalaxyYMLInvalid, galaxyYMLName, err)
	}
	if doc == nil {
		return GalaxyYML{}, nil, fmt.Errorf("%w: %s is not a mapping", helpers.ErrGalaxyYMLInvalid, galaxyYMLName)
	}
	if _, ok := doc[keyManifest]; ok {
		return GalaxyYML{}, nil, fmt.Errorf("%w: the manifest: key is not supported; use %s",
			helpers.ErrGalaxyYMLInvalid, keyBuildIgnore)
	}
	warnings := unknownKeyWarnings(doc)

	meta, err := metaFromDoc(doc)
	if err != nil {
		return GalaxyYML{}, nil, err
	}
	if err := validateMeta(&meta); err != nil {
		return GalaxyYML{}, nil, err
	}
	return meta, warnings, nil
}

// unknownKeyWarnings renders the one warning ansible prints for keys outside
// the schema, sorted so the text is stable.
func unknownKeyWarnings(doc map[string]any) []string {
	known := make(map[string]struct{})
	for _, k := range stringKeys() {
		known[k] = struct{}{}
	}
	for _, k := range listKeys() {
		known[k] = struct{}{}
	}
	known[keyDependencies] = struct{}{}
	known[keyManifest] = struct{}{}
	var unknown []string
	for k := range doc {
		if _, ok := known[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return []string{"Found unknown keys in " + galaxyYMLName + ": " + strings.Join(unknown, ", ")}
}

// metaFromDoc reads the typed keys out of the parsed mapping, refusing a
// wrong type and a missing mandatory key.
func metaFromDoc(doc map[string]any) (GalaxyYML, error) {
	if err := checkMandatory(doc); err != nil {
		return GalaxyYML{}, err
	}
	version, err := versionFromDoc(doc)
	if err != nil {
		return GalaxyYML{}, err
	}

	strs := make(map[string]string, len(stringKeys()))
	for _, k := range stringKeys() {
		if k == keyVersion {
			continue
		}
		v, err := stringValue(doc, k)
		if err != nil {
			return GalaxyYML{}, err
		}
		strs[k] = v
	}
	lists := make(map[string][]string, len(listKeys()))
	for _, k := range listKeys() {
		v, err := listValue(doc, k)
		if err != nil {
			return GalaxyYML{}, err
		}
		lists[k] = v
	}
	deps, err := dependenciesValue(doc)
	if err != nil {
		return GalaxyYML{}, err
	}
	return GalaxyYML{
		Dependencies:  deps,
		Namespace:     strs[keyNamespace],
		Name:          strs[keyName],
		Version:       version,
		Readme:        strs[keyReadme],
		Description:   strs[keyDescription],
		LicenseFile:   strs[keyLicenseFile],
		Repository:    strs[keyRepository],
		Documentation: strs[keyDocumentation],
		Homepage:      strs[keyHomepage],
		Issues:        strs[keyIssues],
		Authors:       lists[keyAuthors],
		License:       lists[keyLicense],
		Tags:          lists[keyTags],
		BuildIgnore:   lists[keyBuildIgnore],
	}, nil
}

// checkMandatory refuses a document missing a required key other than
// version, naming every one missing at once as ansible does.
func checkMandatory(doc map[string]any) error {
	var missing []string
	for _, k := range mandatoryKeys() {
		if _, ok := doc[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s is missing the following mandatory keys: %s",
		helpers.ErrGalaxyYMLInvalid, galaxyYMLName, strings.Join(missing, ", "))
}

// versionFromDoc reads version, whose every defect is reported under the
// exact-version sentinel with the remedy, because ansible's own answer to a
// missing or empty one is to install "*" and this tool has no such version.
func versionFromDoc(doc map[string]any) (string, error) {
	raw, ok := doc[keyVersion]
	if !ok || raw == nil {
		return "", fmt.Errorf("%w: version is missing; %s", helpers.ErrGitCollectionVersionNotExact, versionRemedy)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%w: version is a YAML %T, not a string; quote it and %s",
			helpers.ErrGitCollectionVersionNotExact, raw, versionRemedy)
	}
	if s == "" {
		return "", fmt.Errorf("%w: version is empty; %s", helpers.ErrGitCollectionVersionNotExact, versionRemedy)
	}
	return s, nil
}

func stringValue(doc map[string]any, key string) (string, error) {
	raw, ok := doc[key]
	if !ok || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%w: %s must be a string, got a YAML %T", helpers.ErrGalaxyYMLInvalid, key, raw)
	}
	return s, nil
}

// listValue reads a list key. A single string is wrapped, as ansible does;
// the result is never nil so it marshals as [] rather than null.
func listValue(doc map[string]any, key string) ([]string, error) {
	raw, ok := doc[key]
	if !ok || raw == nil {
		return []string{}, nil
	}
	switch v := raw.(type) {
	case string:
		return []string{v}, nil
	case []any:
		out := make([]string, 0, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%w: %s[%d] must be a string, got a YAML %T", helpers.ErrGalaxyYMLInvalid, key, i, item)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: %s must be a list of strings, got a YAML %T", helpers.ErrGalaxyYMLInvalid, key, raw)
	}
}

// dependenciesValue reads dependencies as a map of collection name to
// constraint. The result is never nil so it marshals as {}.
func dependenciesValue(doc map[string]any) (map[string]string, error) {
	raw, ok := doc[keyDependencies]
	if !ok || raw == nil {
		return map[string]string{}, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s must be a mapping, got a YAML %T", helpers.ErrGalaxyYMLInvalid, keyDependencies, raw)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%w: the constraint of dependency %q must be a string, got a YAML %T",
				helpers.ErrGalaxyYMLInvalid, helpers.TruncateForMessage(k), v)
		}
		out[k] = s
	}
	return out, nil
}

// validateMeta applies the identity and shape rules both metadata sources
// share: namespace and name alphabet, exact version, dependency keys, and
// the bounds on build_ignore patterns.
func validateMeta(meta *GalaxyYML) error {
	if !helpers.IsCollectionNamePart(meta.Namespace) {
		return fmt.Errorf("%w: namespace %q", helpers.ErrInvalidCollectionName, helpers.TruncateForMessage(meta.Namespace))
	}
	if !helpers.IsCollectionNamePart(meta.Name) {
		return fmt.Errorf("%w: name %q", helpers.ErrInvalidCollectionName, helpers.TruncateForMessage(meta.Name))
	}
	if !helpers.IsExactVersion(meta.Version) {
		return fmt.Errorf("%w: version %q; %s",
			helpers.ErrGitCollectionVersionNotExact, helpers.TruncateForMessage(meta.Version), versionRemedy)
	}
	for dep := range meta.Dependencies {
		if !helpers.IsCollectionName(dep) {
			return fmt.Errorf("%w: %q", helpers.ErrInvalidDependencyKey, helpers.TruncateForMessage(dep))
		}
	}
	for i, pattern := range meta.BuildIgnore {
		switch {
		case pattern == "":
			return fmt.Errorf("%w: %s[%d] is empty", helpers.ErrGalaxyYMLInvalid, keyBuildIgnore, i)
		case len(pattern) > buildIgnoreMaxLen:
			return fmt.Errorf("%w: %s[%d] is %d bytes, the limit is %d",
				helpers.ErrGalaxyYMLInvalid, keyBuildIgnore, i, len(pattern), buildIgnoreMaxLen)
		case strings.ContainsRune(pattern, 0):
			return fmt.Errorf("%w: %s[%d] contains a NUL", helpers.ErrGalaxyYMLInvalid, keyBuildIgnore, i)
		}
	}
	return nil
}

// manifestInfo is the collection_info block of a MANIFEST.json, read with
// the same identity rules as a galaxy.yml. license_file is a pointer because
// the writer emits null for an empty one.
type manifestInfo struct {
	Dependencies  map[string]string `json:"dependencies"`
	LicenseFile   *string           `json:"license_file"`
	Namespace     string            `json:"namespace"`
	Name          string            `json:"name"`
	Version       string            `json:"version"`
	Readme        string            `json:"readme"`
	Description   string            `json:"description"`
	Repository    string            `json:"repository"`
	Documentation string            `json:"documentation"`
	Homepage      string            `json:"homepage"`
	Issues        string            `json:"issues"`
	Authors       []string          `json:"authors"`
	License       []string          `json:"license"`
	Tags          []string          `json:"tags"`
}

type manifestEnvelope struct {
	CollectionInfo *manifestInfo `json:"collection_info"`
}

// ParseManifestInfo reads a url artifact's identity and dependencies from its
// MANIFEST.json. Namespace and name use the wider IsURLCollectionNamePart,
// since real release artifacts carry mixed-case namespaces ansible installs.
func ParseManifestInfo(data []byte) (GalaxyYML, error) {
	meta, err := decodeManifestInfo(data)
	if err != nil {
		return GalaxyYML{}, err
	}
	if !helpers.IsURLCollectionNamePart(meta.Namespace) {
		return GalaxyYML{}, fmt.Errorf("%w: namespace %q", helpers.ErrInvalidCollectionName, helpers.TruncateForMessage(meta.Namespace))
	}
	if !helpers.IsURLCollectionNamePart(meta.Name) {
		return GalaxyYML{}, fmt.Errorf("%w: name %q", helpers.ErrInvalidCollectionName, helpers.TruncateForMessage(meta.Name))
	}
	if !helpers.IsExactVersion(meta.Version) {
		return GalaxyYML{}, fmt.Errorf("%w: MANIFEST.json declares %q",
			helpers.ErrInvalidCollectionVersion, helpers.TruncateForMessage(meta.Version))
	}
	for dep := range meta.Dependencies {
		if !helpers.IsCollectionName(dep) {
			return GalaxyYML{}, fmt.Errorf("%w: %q", helpers.ErrInvalidDependencyKey, helpers.TruncateForMessage(dep))
		}
	}
	return meta, nil
}

// parseManifestInfo reads a built tree's identity from its MANIFEST.json under
// validateMeta. Only collection_info is read: the tree is rebuilt from scratch,
// as ansible's install_src does.
func parseManifestInfo(data []byte) (GalaxyYML, error) {
	meta, err := decodeManifestInfo(data)
	if err != nil {
		return GalaxyYML{}, err
	}
	if err := validateMeta(&meta); err != nil {
		return GalaxyYML{}, err
	}
	return meta, nil
}

// decodeManifestInfo is the decode half shared by both manifest readers:
// everything except the identity alphabet the two callers disagree on.
func decodeManifestInfo(data []byte) (GalaxyYML, error) {
	if len(data) > metadataMaxBytes {
		return GalaxyYML{}, fmt.Errorf("%w: %s is %d bytes, the limit is %d",
			helpers.ErrGalaxyYMLInvalid, helpers.ManifestFileName, len(data), metadataMaxBytes)
	}
	var env manifestEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return GalaxyYML{}, fmt.Errorf("%w: %s does not parse: %w", helpers.ErrGalaxyYMLInvalid, helpers.ManifestFileName, err)
	}
	if env.CollectionInfo == nil {
		return GalaxyYML{}, fmt.Errorf("%w: %s has no collection_info", helpers.ErrGalaxyYMLInvalid, helpers.ManifestFileName)
	}
	info := env.CollectionInfo
	if info.Version == "" {
		return GalaxyYML{}, fmt.Errorf("%w: %s collection_info has no version; %s",
			helpers.ErrGitCollectionVersionNotExact, helpers.ManifestFileName, versionRemedy)
	}
	meta := GalaxyYML{
		Dependencies:  info.Dependencies,
		Namespace:     info.Namespace,
		Name:          info.Name,
		Version:       info.Version,
		Readme:        info.Readme,
		Description:   info.Description,
		Repository:    info.Repository,
		Documentation: info.Documentation,
		Homepage:      info.Homepage,
		Issues:        info.Issues,
		Authors:       info.Authors,
		License:       info.License,
		Tags:          info.Tags,
		BuildIgnore:   []string{},
	}
	if info.LicenseFile != nil {
		meta.LicenseFile = *info.LicenseFile
	}
	if meta.Dependencies == nil {
		meta.Dependencies = map[string]string{}
	}
	for _, list := range []*[]string{&meta.Authors, &meta.License, &meta.Tags} {
		if *list == nil {
			*list = []string{}
		}
	}
	return meta, nil
}
