package collections

import (
	"os"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

type collectionDeps struct {
	cfg     *config.Config
	runtime *infra.Infra
	st      *store.Store

	// apiRoots memoizes, per server base, the API root that already answered,
	// so a non-v3-first server is not re-probed per collection. Scoped to one
	// phase: resolve, install and prefetch each get their own memo.
	apiRoots *apiRootMemo

	// unmatchedSources memoizes the source: values already warned about this
	// phase, so one misconfigured host produces one line rather than one per
	// collection pinned to it. Scoped exactly like apiRoots above.
	unmatchedSources *unmatchedSourceMemo

	// gitStore is where discovery commits its builds and gitMemo what it found;
	// both are run-wide and nil for outdated. It is not named artifacts, since
	// installDeps and prefetchDeps embed this struct and would shadow it.
	gitStore cacheManager.ArtifactStore
	gitMemo  *gitDiscoveryMemo
	// roleMemo is gitMemo's counterpart for roles, filled by resolve and read
	// by install for a --no-cache build; role artifacts share gitStore.
	roleMemo *roleDiscoveryMemo
	// urlMemo is gitMemo's counterpart for url sources, read by the solver and
	// install; url artifacts share gitStore, since both source kinds commit
	// during discovery rather than at install time.
	urlMemo *urlDiscoveryMemo
}

// withSources returns d with the run's discovery-side artifact store and the
// three discovery memos attached.
func (d collectionDeps) withSources(
	gitStore cacheManager.ArtifactStore, memo *gitDiscoveryMemo, roles *roleDiscoveryMemo, urls *urlDiscoveryMemo,
) collectionDeps {
	d.gitStore = gitStore
	d.gitMemo = memo
	d.roleMemo = roles
	d.urlMemo = urls
	return d
}

type installDeps struct {
	collectionDeps

	artifacts    cacheManager.ArtifactStore
	extractStore *extracted.Store
	// root is the single os.Root every install-side write funnels through, so a
	// symlinked path component cannot redirect a write outside DownloadPath. It
	// is nil for warm, and newInstallTarget's nil-root guard then fails closed.
	root *os.Root
	// rolesRoot is root's counterpart for the roles tree, opened at RolesPath
	// only when the plan holds a role; newRoleTarget fails closed on nil.
	rolesRoot *os.Root
	// verify is this run's signature verification state, nil when nothing is
	// verified; test it through verifyContext.enabled(). One instance is shared
	// by every install and warm worker and read concurrently.
	verify *verifyContext
	// presence holds the prefetcher's scan-time cache-presence hints, keyed by
	// artifactKey, so isCacheHit skips a repeat probe; a nil map means no hint.
	presence map[string]bool
}

// prefetchDeps carries no verifyContext: the artifact cache it fills is
// policy-free and records no verdict, so verification runs only in the worker
// about to write a collection, in prepareWithRecovery's action closure.
type prefetchDeps struct {
	collectionDeps

	artifacts cacheManager.ArtifactStore
	// root lets shouldSchedulePrefetch check installRecordMatches through the
	// same rooted target as the install. warm passes nil, read as "not
	// installed", so it decides on the cache probe alone.
	root *os.Root
}

func newCollectionDeps(cfg *config.Config, runtime *infra.Infra, st *store.Store) collectionDeps {
	return collectionDeps{
		cfg:              cfg,
		runtime:          runtime,
		st:               st,
		apiRoots:         newAPIRootMemo(),
		unmatchedSources: newUnmatchedSourceMemo(),
	}
}

func newInstallDeps(
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	artifacts cacheManager.ArtifactStore,
	extractStore *extracted.Store,
	root *os.Root,
	presence map[string]bool,
	verify *verifyContext,
) installDeps {
	return installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
		extractStore:   extractStore,
		root:           root,
		presence:       presence,
		verify:         verify,
	}
}

func newPrefetchDeps(
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	artifacts cacheManager.ArtifactStore,
	root *os.Root,
) prefetchDeps {
	return prefetchDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
		root:           root,
	}
}
