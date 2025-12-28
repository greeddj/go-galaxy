package collections

import (
	"fmt"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// serverBaseCandidates returns candidate base URLs for metadata lookup.
func serverBaseCandidates(cfg *config.Config, col collection) []string {
	seen := make(map[string]bool)
	var out []string

	add := func(value string) {
		trimmed := strings.TrimSpace(strings.Trim(value, "\""))
		trimmed = strings.TrimRight(trimmed, "/")
		if trimmed == "" || seen[trimmed] {
			return
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}

	add(col.Source)
	if cfg != nil {
		add(cfg.Server)
	}

	return out
}

// rootMetadataURLCandidates builds candidate root metadata URLs.
func rootMetadataURLCandidates(cfg *config.Config, col collection) []string {
	seen := make(map[string]bool)
	var out []string

	add := func(url string) {
		if url == "" || seen[url] {
			return
		}
		seen[url] = true
		out = append(out, url)
	}

	addWithVariants := func(url string) {
		add(url)
		add(strings.TrimRight(url, "/"))
	}

	for _, base := range serverBaseCandidates(cfg, col) {
		for _, apiRoot := range apiRootCandidates(base) {
			addWithVariants(fmt.Sprintf("%s/collections/%s/%s/", apiRoot, col.Namespace, col.Name))
		}
	}

	return out
}

// apiRootCandidates derives API root candidates from a base URL.
func apiRootCandidates(base string) []string {
	trimmed := strings.TrimSpace(strings.Trim(base, "\""))
	trimmed = strings.TrimRight(trimmed, "/")
	if trimmed == "" {
		return nil
	}

	var out []string
	add := func(value string) {
		value = strings.TrimRight(value, "/")
		if slices.Contains(out, value) {
			return
		}
		out = append(out, value)
	}

	lower := trimmed
	switch {
	case strings.HasSuffix(lower, "/api/v3"):
		add(trimmed)
	case strings.HasSuffix(lower, "/api/v2"):
		add(trimmed)
	case strings.HasSuffix(lower, "/api"):
		add(trimmed + "/v3")
		add(trimmed + "/v2")
		add(trimmed)
	default:
		add(trimmed + "/api/v3")
		add(trimmed + "/api/v2")
		add(trimmed + "/api")
	}

	return out
}
