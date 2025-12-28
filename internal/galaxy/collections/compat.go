package collections

import (
	"context"
	"net/http"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// installedEntry aliases the store entry type for compatibility.
type installedEntry = store.InstalledEntry

// requirementSpec aliases the store requirement spec for compatibility.
type requirementSpec = store.RequirementSpec

// setResolvedAll stores resolved collection versions in the snapshot.
func setResolvedAll(st *store.Store, resolved map[string]collection) {
	if st == nil {
		return
	}
	entries := make(map[string]store.ResolvedEntry, len(resolved))
	for fqdn, col := range resolved {
		entries[fqdn] = store.ResolvedEntry{Version: col.Version, Source: col.Source}
	}
	st.SetResolvedAll(entries)
}

// fetchJSONWithCachePolicy fetches JSON using cache policy and context.
func fetchJSONWithCachePolicy(
	ctx context.Context,
	client *http.Client,
	url string,
	st *store.Store,
	out any,
	policy cacheManager.Policy,
) error {
	return cacheManager.FetchJSONWithCachePolicy(ctx, client, url, st, out, policy)
}

// cachePolicyForConstraint builds a cache policy from config options.
func cachePolicyForConstraint(cfg *config.Config, exact bool) cacheManager.Policy {
	return cacheManager.PolicyForConstraint(cfg, exact)
}
