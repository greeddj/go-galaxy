package collections

import (
	"net/url"
	"strings"
)

// resolveURL resolves ref relative to base with API path heuristics.
func resolveURL(base, ref string) string {
	base = strings.TrimSpace(base)
	ref = strings.TrimSpace(ref)
	if base == "" {
		return ref
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return joinURL(base, ref)
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return joinURL(base, ref)
	}
	if refURL.IsAbs() {
		return refURL.String()
	}
	if refURL.Path != "" && strings.HasPrefix(ref, "/") {
		basePath := strings.TrimRight(baseURL.Path, "/")
		refPath := refURL.Path
		if basePath != "" && needsBasePathMerge(basePath, refPath) {
			merged := *baseURL
			merged.Path = strings.TrimRight(basePath, "/") + "/" + strings.TrimLeft(refPath, "/")
			merged.RawPath = ""
			merged.RawQuery = refURL.RawQuery
			merged.Fragment = refURL.Fragment
			return merged.String()
		}
	}
	return baseURL.ResolveReference(refURL).String()
}

// joinURL joins base and ref with a single slash.
func joinURL(base, ref string) string {
	base = strings.TrimRight(base, "/")
	ref = strings.TrimLeft(ref, "/")
	if base == "" {
		return ref
	}
	if ref == "" {
		return base
	}
	return base + "/" + ref
}

// needsBasePathMerge reports whether to merge base and ref paths.
func needsBasePathMerge(basePath, refPath string) bool {
	if basePath == "" || refPath == "" {
		return false
	}
	if strings.HasPrefix(refPath, basePath+"/") || refPath == basePath {
		return false
	}
	if strings.HasPrefix(refPath, "/api/") || strings.HasPrefix(refPath, "/api/v") {
		return true
	}
	return false
}
