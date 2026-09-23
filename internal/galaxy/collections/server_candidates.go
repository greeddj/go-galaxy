package collections

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// serverCandidate is one server a root-metadata fetch may try: base is the
// normalized URL, id the server_list id or "". id only labels errors; token
// and TLS policy follow base's origin through internal/galaxy/fetch.
type serverCandidate struct {
	base string
	id   string
}

// label names the server for an operator-facing error: its server_list id
// when it has one, else its base URL, so an aborted run always names it.
func (s serverCandidate) label() string {
	if s.id != "" {
		return s.id
	}
	return s.base
}

// serverCandidates returns the servers col's root-metadata fetch may try: the
// one server a source: pins it to, else every configured server in list
// order for the first-match-wins walk. A git or url collection yields none.
func serverCandidates(deps collectionDeps, col collection) []serverCandidate {
	if deps.cfg == nil {
		return nil
	}
	// A git or url locator is not a server and must never be probed with the
	// Galaxy API root suffixes as an unmatched source: would be.
	if col.isGit() || col.isURL() {
		return nil
	}
	if col.Source != "" {
		candidate, matched := pinnedServerCandidate(deps.cfg, col.Source)
		if !matched {
			warnUnmatchedSource(deps, col, candidate.base)
		}
		return []serverCandidate{candidate}
	}
	return unpinnedServerCandidates(deps.cfg)
}

// warnUnmatchedSource warns, once per distinct base per phase, that a source:
// matches no configured server; the request is still made, and carries no
// token or TLS policy since fetch dispatches both by origin.
func warnUnmatchedSource(deps collectionDeps, col collection, base string) {
	if base == "" || deps.runtime == nil || !deps.unmatchedSources.first(base) {
		return
	}
	deps.runtime.Output.Warnf(
		"%s declares source %q, which matches no configured Galaxy server; requesting it anyway",
		col.key(), base,
	)
}

// pinnedServerCandidate resolves a source: by exact server_list id, else by
// origin while keeping its own path as base. matched is separate from id since
// a --server-only server has no id and must not read as unmatched.
func pinnedServerCandidate(cfg *config.Config, source string) (serverCandidate, bool) {
	for _, srv := range cfg.Servers {
		if srv.ID != "" && srv.ID == source {
			return serverCandidate{base: srv.URL, id: srv.ID}, true
		}
	}

	normalized := normalizeServerBase(source)
	if origin, ok := parsedOrigin(normalized); ok {
		for _, srv := range cfg.Servers {
			if srvOrigin, ok := parsedOrigin(normalizeServerBase(srv.URL)); ok && srvOrigin == origin {
				return serverCandidate{base: normalized, id: srv.ID}, true
			}
		}
	}
	return serverCandidate{base: normalized}, false
}

// unmatchedSourceMemo remembers which unmatched sources one collectionDeps has
// warned about; nil-tolerant, as a hand-built collectionDeps carries none.
type unmatchedSourceMemo struct {
	seen map[string]bool
	mu   sync.Mutex
}

// newUnmatchedSourceMemo returns an empty memo, presized to the one unmatched
// source a misconfigured run almost always has.
func newUnmatchedSourceMemo() *unmatchedSourceMemo {
	return &unmatchedSourceMemo{seen: make(map[string]bool, 1)}
}

// first reports whether base has not been warned about yet, recording it if
// so. A nil receiver reports true every time: without a memo there is nothing
// to deduplicate against, and warning repeatedly is better than not at all.
func (m *unmatchedSourceMemo) first(base string) bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen[base] {
		return false
	}
	m.seen[base] = true
	return true
}

// unpinnedServerCandidates returns every cfg.Servers entry in list order,
// deduplicated by normalized base, or cfg.Server alone when the list is empty.
func unpinnedServerCandidates(cfg *config.Config) []serverCandidate {
	if len(cfg.Servers) == 0 {
		return []serverCandidate{{base: normalizeServerBase(cfg.Server)}}
	}

	seen := make(map[string]bool, len(cfg.Servers))
	out := make([]serverCandidate, 0, len(cfg.Servers))
	for _, srv := range cfg.Servers {
		base := normalizeServerBase(srv.URL)
		if base == "" || seen[base] {
			continue
		}
		seen[base] = true
		out = append(out, serverCandidate{base: base, id: srv.ID})
	}
	return out
}

// normalizeServerBase trims whitespace, strips one balanced pair of
// surrounding double quotes and removes trailing slashes; an unbalanced quote
// is kept.
func normalizeServerBase(value string) string {
	trimmed := strings.TrimSpace(value)
	if inner, ok := strings.CutPrefix(trimmed, "\""); ok {
		if inner, ok := strings.CutSuffix(inner, "\""); ok {
			trimmed = inner
		}
	}
	return strings.TrimRight(trimmed, "/")
}

// parsedOrigin returns value's helpers.Origin, or ("", false) when it is not
// an absolute URL with a scheme and host, which then matches nothing.
func parsedOrigin(value string) (string, bool) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	return helpers.Origin(u), true
}

// rootMetaCandidate is one root-metadata URL with the base and API root it
// came from, which tryServerRootMetadata memoizes once its fetch succeeds.
type rootMetaCandidate struct {
	url     string
	base    string
	apiRoot string
}

// rootMetadataURLCandidates builds root metadata URLs for one server base:
// only the memoized winning API root's two trailing-slash variants when known,
// else every apiRootCandidates root in priority order.
func rootMetadataURLCandidates(base string, col collection, memo *apiRootMemo) []rootMetaCandidate {
	seen := make(map[string]bool)
	var out []rootMetaCandidate

	add := func(u, apiRoot string) {
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, rootMetaCandidate{url: u, base: base, apiRoot: apiRoot})
	}

	addWithVariants := func(apiRoot string) {
		u := fmt.Sprintf("%s/collections/%s/%s/", apiRoot, col.Namespace, col.Name)
		add(u, apiRoot)
		add(strings.TrimRight(u, "/"), apiRoot)
	}

	if winningRoot, ok := memo.winner(base); ok {
		addWithVariants(winningRoot)
		return out
	}
	for _, apiRoot := range apiRootCandidates(base) {
		addWithVariants(apiRoot)
	}

	return out
}

// joinCandidateURLs renders a candidate list's URLs as a comma-joined string
// for debug logging, without allocating an intermediate []string.
func joinCandidateURLs(candidates []rootMetaCandidate) string {
	var b strings.Builder
	for i, cand := range candidates {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(cand.url)
	}
	return b.String()
}

// apiRootCandidates derives API roots from base in priority order: /api/v3,
// /v3 (Galaxy NG, Automation Hub), /api/v2, /v2, /api. The plain concatenation
// is deliberate: it keeps a query-bearing base from ever naming an API root.
func apiRootCandidates(base string) []string {
	trimmed := normalizeServerBase(base)
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

	// A base already ending in an API root suffix is used as-is, so the
	// suffix is never doubled (".../api/v3/api/v3").
	switch {
	case strings.HasSuffix(trimmed, "/api/v3"):
		add(trimmed)
	case strings.HasSuffix(trimmed, "/api/v2"):
		add(trimmed)
	case strings.HasSuffix(trimmed, "/v3"):
		add(trimmed)
	case strings.HasSuffix(trimmed, "/v2"):
		add(trimmed)
	case strings.HasSuffix(trimmed, "/api"):
		add(trimmed + "/v3")
		add(trimmed + "/v2")
		add(trimmed)
	default:
		add(trimmed + "/api/v3")
		add(trimmed + "/v3")
		add(trimmed + "/api/v2")
		add(trimmed + "/v2")
		add(trimmed + "/api")
	}

	return out
}
