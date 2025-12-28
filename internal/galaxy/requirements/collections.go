package requirements

import (
	"fmt"
	"os"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"gopkg.in/yaml.v3"
)

// Collections is a list of collection requirements.
type Collections = []CollectionRequirement

// CollectionRequirement describes a single collection requirement entry.
type CollectionRequirement struct {
	Namespace  string
	Name       string
	Version    string
	Source     string
	Type       string
	Signatures []string
}

// LoadCollections reads and parses requirements from a file.
func LoadCollections(path, defaultSource string) (Collections, bool, error) {
	//nolint:gosec // path is user-provided requirements file.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	return ParseCollections(data, defaultSource)
}

// ParseCollections parses requirements data and returns collections and roles flag.
func ParseCollections(data []byte, defaultSource string) (Collections, bool, error) {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, false, err
	}
	return parseCollectionsRaw(raw, defaultSource)
}

// parseCollectionsRaw parses a decoded requirements payload.
func parseCollectionsRaw(raw any, defaultSource string) (Collections, bool, error) {
	switch v := raw.(type) {
	case map[string]any:
		rolesFound := false
		if _, ok := v["roles"]; ok {
			rolesFound = true
		}
		if collectionsRaw, ok := v["collections"]; ok {
			cols, err := parseCollectionList(collectionsRaw, defaultSource)
			return cols, rolesFound, err
		}
		if rolesFound {
			return nil, rolesFound, nil
		}
		return nil, rolesFound, helpers.ErrUnsupportedRequirementsFormat
	case []any:
		cols, err := parseCollectionList(v, defaultSource)
		return cols, false, err
	default:
		return nil, false, helpers.ErrUnsupportedRequirementsFormat
	}
}

// parseCollectionList parses a list of collection items.
func parseCollectionList(raw any, defaultSource string) (Collections, error) {
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

// parseCollectionItem parses a single collection entry.
func parseCollectionItem(item any, defaultSource string) (CollectionRequirement, error) {
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
	if looksLikeSourceName(name) {
		return CollectionRequirement{}, fmt.Errorf("%w %q (only Galaxy API sources are supported)", helpers.ErrUnsupportedCollectionSource, name)
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
	req := parseCollectionMapFields(value)
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
	if req.Name == "" {
		return fmt.Errorf("%w: %v", helpers.ErrInvalidCollectionEntry, raw)
	}
	if req.Type == "git" || req.Type == "url" {
		return fmt.Errorf("%w %q (only galaxy is supported)", helpers.ErrUnsupportedCollectionType, req.Type)
	}
	if req.Type != "" && req.Type != "galaxy" {
		return fmt.Errorf("%w %q (only galaxy is supported)", helpers.ErrUnsupportedCollectionType, req.Type)
	}
	if req.Type == "" && looksLikeSourceName(req.Name) {
		return fmt.Errorf("%w %q (only Galaxy API sources are supported)", helpers.ErrUnsupportedCollectionSource, req.Name)
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
