package collections

import (
	"fmt"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// prepareRoots normalizes and validates root requirements, refusing
// duplicates by locator and by name. A git or url root must carry that
// kind's locator as Source, since downstream dispatches on the prefix alone.
func prepareRoots(roots []collection) ([]collection, error) {
	prepared := make([]collection, 0, len(roots))
	seen := make(map[string]collection)
	addRoot := func(key string, col collection) error {
		if existing, ok := seen[key]; ok {
			return fmt.Errorf("%w for %s (type %s vs %s)", helpers.ErrDuplicateCollectionRequirement, key, existing.Type, col.Type)
		}
		seen[key] = col
		return nil
	}

	for _, root := range roots {
		if err := normalizeRootType(&root); err != nil {
			return nil, err
		}
		if hasLocatorSource(root) {
			if err := addRoot(root.Source, root); err != nil {
				return nil, err
			}
			if root.Namespace == "" && root.Name == "" {
				prepared = append(prepared, root)
				continue
			}
		}
		if err := splitRootName(&root); err != nil {
			return nil, err
		}
		if err := addRoot(fmt.Sprintf("%s.%s", root.Namespace, root.Name), root); err != nil {
			return nil, err
		}
		prepared = append(prepared, root)
	}

	return prepared, nil
}

// hasLocatorSource reports whether root's Source is a git or url locator: a
// root whose identity discovery supplies rather than the file.
func hasLocatorSource(root collection) bool {
	return root.isGit() || root.isURL()
}

// splitRootName fills a root's namespace and name from a dotted Name when
// either half is missing; a Name that is not a valid fqdn is refused.
func splitRootName(root *collection) error {
	if root.Namespace != "" && root.Name != "" {
		return nil
	}
	namespace, name, ok := helpers.SplitFQDN(root.Name)
	if !ok {
		return fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionName, root.Name)
	}
	root.Namespace = namespace
	root.Name = name
	return nil
}

// normalizeRootType canonicalizes root's type (empty means galaxy), refuses
// a type other than galaxy, git or url, and enforces that a root is typed
// git or url exactly when its Source is that kind's locator.
func normalizeRootType(root *collection) error {
	root.Type = normalizeType(root.Type)
	if root.Type == "" {
		root.Type = typeGalaxy
	}
	if !isSupportedType(root.Type) {
		return fmt.Errorf("%w: %q (only galaxy, git and url are supported)", helpers.ErrUnsupportedCollectionType, root.Type)
	}
	if (root.Type == typeGit) != root.isGit() || (root.Type == typeURL) != root.isURL() {
		return fmt.Errorf("%w: type %q does not match source %q", helpers.ErrInvalidCollectionEntry,
			root.Type, helpers.URLForMessage(root.Source))
	}
	return nil
}

// normalizeType normalizes a collection type string.
func normalizeType(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// isGalaxyType reports whether the type is a Galaxy type (the empty type
// is Galaxy).
func isGalaxyType(value string) bool {
	normalized := normalizeType(value)
	return normalized == "" || normalized == typeGalaxy
}

// isSupportedType reports whether the type is one this tool resolves:
// Galaxy, git or url.
func isSupportedType(value string) bool {
	normalized := normalizeType(value)
	return isGalaxyType(value) || normalized == typeGit || normalized == typeURL
}
