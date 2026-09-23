package collections

import (
	"context"
	"sync"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
)

// prewarmRootMetadata warms, cfg.Workers roots at a time and through the
// solve's own provider methods, the metadata the sequential solve will ask
// for. It is best effort: errors are only logged, and the solve reports them.
func prewarmRootMetadata(ctx context.Context, deps collectionDeps, roots []collection) {
	if !prewarmEnabled(deps, roots) {
		return
	}

	// Local, never hoisted into resolveCollectionsInternal: the solve must run
	// on the caller's untouched ctx, so a prewarm failure never cancels it.
	warmCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sources := rootSourceMap(roots)
	// A zero Workers would make sem unbuffered and deadlock the first send.
	sem := make(chan struct{}, max(deps.cfg.Workers, 1))
	var wg sync.WaitGroup
	for _, root := range roots {
		// An earlier root's error canceled warmCtx: dispatch no further root.
		// Goroutines already started see the same cancellation.
		if warmCtx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := prewarmOne(warmCtx, deps, sources, root); err != nil {
				deps.runtime.Output.Debugf("Prewarm %s.%s: %v", root.Namespace, root.Name, err)
				cancel()
			}
		})
	}
	// Mandatory: the caller runs solveCollections right after this returns,
	// and no prewarm goroutine may still be live when the solve starts.
	wg.Wait()
}

// prewarmPolicyUsable reports whether a prewarm request under policy is
// worth issuing: only when it both writes and the solve reads back what it
// wrote; otherwise the run pays for every request twice.
func prewarmPolicyUsable(policy cacheManager.Policy) bool {
	return policy.Read && policy.Write
}

// prewarmEnabled reports whether prewarmRootMetadata should run: a store,
// two or more roots, and a usable exact or non-exact policy (--refresh leaves
// only the exact one usable). Without a store every warm request is repeated.
func prewarmEnabled(deps collectionDeps, roots []collection) bool {
	if deps.cfg == nil || deps.st == nil || len(roots) < 2 {
		return false
	}
	return prewarmPolicyUsable(cacheManager.PolicyForConstraint(deps.cfg, false)) ||
		prewarmPolicyUsable(cacheManager.PolicyForConstraint(deps.cfg, true))
}

// prewarmOne warms root via Highest, or via Dependencies for an exact pin.
// It skips git and url roots, an exact pin under NoDeps (the solve asks
// nothing for it), a malformed constraint and an unusable policy.
func prewarmOne(ctx context.Context, deps collectionDeps, sources map[string]string, root collection) error {
	// A git or url root is answered by discovery; a server would only 404.
	if root.isGit() || root.isURL() {
		return nil
	}
	constraint := root.Constraint
	if constraint == "" {
		constraint = root.Version
	}
	version, exact, err := exactVersionFromConstraints([]string{constraint})
	if err != nil {
		// The solve hits the same parse and reports it, exactly once.
		return nil
	}
	if exact && deps.cfg.NoDeps {
		return nil
	}
	if !prewarmPolicyUsable(cacheManager.PolicyForConstraint(deps.cfg, exact)) {
		return nil
	}

	fqdn := root.Namespace + "." + root.Name
	// A fresh provider per call keeps the unsynchronized bindings map
	// goroutine-local while sharing deps' mutex-guarded memos and Store.
	mp := newMetadataProviderWithDeps(deps, sources)
	if !exact {
		_, _, err := mp.Highest(ctx, fqdn)
		return err
	}

	v, err := solver.NewVersion(version)
	if err != nil {
		// Unreachable: exactVersionFromConstraints already parsed version.
		// Skipped rather than reported, like every other prewarm failure.
		return nil
	}
	_, err = mp.Dependencies(ctx, fqdn, v)
	return err
}
