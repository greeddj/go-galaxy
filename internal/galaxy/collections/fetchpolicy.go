package collections

import (
	"context"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// fetchJSONWithCachePolicy fetches JSON under the cache policy, always bound to
// runtime.MetadataDeadline() rather than a caller-supplied budget, so no call
// site can pass a wrong or missing one.
func fetchJSONWithCachePolicy(
	ctx context.Context,
	runtime *infra.Infra,
	url string,
	st *store.Store,
	out any,
	policy cacheManager.Policy,
) error {
	return cacheManager.FetchJSONWithCachePolicy(ctx, runtime.HTTP, url, st, out, policy, runtime.MetadataDeadline())
}
