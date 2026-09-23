package config

import (
	"fmt"
	"slices"
	"strings"
)

// This file holds the id-list grammar and unknown-variable scan that the
// GO_GALAXY_GIT_* and GO_GALAXY_URL_* credential surfaces share, so their rules
// cannot drift; each surface's keys and binding kinds stay in its own file.

// credentialIDList splits a comma-separated id list under validateServerIDs'
// rules, since each id becomes a variable prefix. A blank list is empty, while
// a blank element (a stray comma) is refused; refusals wrap sentinel.
func credentialIDList(raw, listVar, envPrefix string, sentinel error) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	ids := make([]string, 0, len(parts))
	seen := make(map[string]string, len(parts))
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id == "" {
			return nil, fmt.Errorf("%w: %s carries an empty id", sentinel, listVar)
		}
		if !serverIDPattern.MatchString(id) {
			return nil, fmt.Errorf("%w: %s lists id %q, which is not spellable as an environment variable (allowed: A-Z a-z 0-9 _ -)",
				sentinel, listVar, id)
		}
		lower := strings.ToLower(id)
		if prior, ok := seen[lower]; ok {
			return nil, fmt.Errorf("%w: %s lists %q and %q, which read the same %s%s_* variables",
				sentinel, listVar, prior, id, envPrefix, strings.ToUpper(id))
		}
		seen[lower] = id
		ids = append(ids, id)
	}
	return ids, nil
}

// unknownCredentialVariableWarnings warns, sorted, about each variable under a
// declared id's prefix that is none of keys. Every id's known names are
// collected first, since id "a"'s prefix also covers id "a_b"'s variables.
func unknownCredentialVariableWarnings(ids, environ, keys []string, varFor func(id, key string) string) []string {
	known := make(map[string]bool, len(ids)*len(keys))
	prefixes := make([]string, 0, len(ids))
	for _, id := range ids {
		prefixes = append(prefixes, varFor(id, ""))
		for _, key := range keys {
			known[varFor(id, key)] = true
		}
	}
	var warnings []string
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if known[name] {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				warnings = append(warnings, fmt.Sprintf("unsupported variable %s ignored", name))
				break
			}
		}
	}
	slices.Sort(warnings)
	return warnings
}
