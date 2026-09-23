package collections

import (
	"context"
	"errors"
	"fmt"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/galaxyv1"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// galaxyRolePinKey is the store key of a Galaxy role's pin, keyed by the
// requirement line (name and version asked for); it records the repository
// and tag so a rerun skips the v1 round trips, the git pin below it the commit.
func galaxyRolePinKey(galaxyName, requested string) string {
	return "galaxy\n" + galaxyName + "\n" + requested
}

// resolveGalaxyRole maps a Galaxy role to a repository and tag (pin or v1
// API) and then takes the git path as an scm role does; the Galaxy pin is
// recorded only after the git path answered, since it carries the commit.
func resolveGalaxyRole(ctx context.Context, deps collectionDeps, req requirements.RoleRequirement) (rolePin, error) {
	policy := cacheManager.PolicyForConstraint(deps.cfg, req.Version != "")
	key := galaxyRolePinKey(req.Src, req.Version)
	res, server, ok, err := replayGalaxyPin(deps, key, policy)
	if err != nil {
		return rolePin{}, err
	}
	if !ok {
		if deps.cfg != nil && deps.cfg.Offline {
			return rolePin{}, fmt.Errorf("%w: Galaxy role %s@%s is not recorded in the cache",
				helpers.ErrOfflineMode, req.Src, displayRoleVersion(req.Version))
		}
		res, server, err = lookupGalaxyRole(ctx, deps, req, policy)
		if err != nil {
			return rolePin{}, err
		}
	}
	greq := gitRoleRequest{
		name:    req.Name,
		pinKey:  gitsource.PinKey(res.RepoURL.String(), res.Ref.Name, ""),
		display: helpers.URLForMessage(res.RepoURL.String()),
		url:     res.RepoURL,
		ref:     res.Ref,
	}
	greq.cred, _ = gitsource.MatchCredential(res.RepoURL, gitCredentialsOf(deps))
	pin, err := resolveGitRoleRequest(ctx, deps, greq, res.GalaxySHA)
	if err != nil {
		return rolePin{}, err
	}
	pin.galaxyName = req.Src
	pin.server = server
	pin.version = res.Version
	if policy.Write {
		deps.st.SetRolePin(key, store.RolePinEntry{
			Repository: res.RepoURL.String(),
			Commit:     pin.commit,
			Version:    res.Version,
			Ref:        res.Ref.Name,
			GalaxySHA:  res.GalaxySHA,
			Server:     server,
		})
	}
	return pin, nil
}

// replayGalaxyPin rebuilds the v1 answer from the Galaxy pin unless the run
// refreshes (--offline outranks --refresh); the pin is cache state, so it is
// judged by the same rules a live server answer is.
func replayGalaxyPin(deps collectionDeps, key string, policy cacheManager.Policy) (galaxyv1.Resolution, string, bool, error) {
	refreshing := deps.cfg != nil && deps.cfg.Refresh && !deps.cfg.Offline
	if !policy.Read || refreshing {
		return galaxyv1.Resolution{}, "", false, nil
	}
	pin, ok := deps.st.GetRolePin(key)
	if !ok {
		return galaxyv1.Resolution{}, "", false, nil
	}
	res, err := galaxyPinResolution(pin)
	if err != nil {
		return galaxyv1.Resolution{}, "", false, err
	}
	return res, pin.Server, true, nil
}

// galaxyPinResolution re-validates a Galaxy pin's fields into a resolution.
func galaxyPinResolution(pin store.RolePinEntry) (galaxyv1.Resolution, error) {
	repo, err := gitsource.ParseURL(pin.Repository)
	if err != nil {
		return galaxyv1.Resolution{}, fmt.Errorf("%w: recorded Galaxy role pin names repository %s",
			helpers.ErrInvalidGitLocator, helpers.URLForMessage(pin.Repository))
	}
	if err := galaxyv1.ValidateRepository(repo); err != nil {
		return galaxyv1.Resolution{}, fmt.Errorf("recorded Galaxy role pin: %w", err)
	}
	ref, err := gitsource.ParseRef(pin.Ref)
	if err != nil || ref.Kind != gitsource.RefQualified || !helpers.IsRoleVersion(pin.Version) {
		return galaxyv1.Resolution{}, fmt.Errorf("%w: recorded Galaxy role pin names ref %q for version %q",
			helpers.ErrInvalidRoleVersion, helpers.TruncateForMessage(pin.Ref), helpers.TruncateForMessage(pin.Version))
	}
	return galaxyv1.Resolution{RepoURL: repo, Ref: ref, Version: pin.Version, GalaxySHA: pin.GalaxySHA}, nil
}

// lookupGalaxyRole asks each configured server's v1 API in order, passing
// over one without v1 or without the role and aborting on any other failure;
// it returns the answering server's base for the lockfile's provenance.
func lookupGalaxyRole(
	ctx context.Context, deps collectionDeps, req requirements.RoleRequirement, policy cacheManager.Policy,
) (galaxyv1.Resolution, string, error) {
	owner, name, ok := helpers.SplitRoleName(req.Src)
	if !ok {
		return galaxyv1.Resolution{}, "", fmt.Errorf("%w: %q", helpers.ErrInvalidRoleName, helpers.TruncateForMessage(req.Src))
	}
	fetch := func(ctx context.Context, u string, out any, policy cacheManager.Policy) error {
		return fetchJSONWithCachePolicy(ctx, deps.runtime, u, deps.st, out, policy)
	}
	var (
		withoutV1 []string
		anyV1     bool
	)
	for _, srv := range unpinnedServerCandidates(deps.cfg) {
		deps.runtime.Output.Debugf("Role %s: v1 lookup on server %s", req.Src, srv.label())
		res, found, warnings, err := galaxyv1.Resolve(ctx, fetch, srv.base, owner, name, req.Version, policy)
		for _, w := range warnings {
			deps.runtime.Output.Warnf("%s: %s", req.Src, w)
		}
		switch {
		case errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable):
			withoutV1 = append(withoutV1, srv.label())
			continue
		case err != nil:
			return galaxyv1.Resolution{}, "", fmt.Errorf("server %s: %w", srv.label(), err)
		case found:
			return res, srv.base, nil
		}
		anyV1 = true
	}
	if !anyV1 {
		return galaxyv1.Resolution{}, "", fmt.Errorf("%w: none of %v serves it; roles need galaxy.ansible.com or a standalone Galaxy NG",
			helpers.ErrGalaxyRoleAPIUnavailable, withoutV1)
	}
	return galaxyv1.Resolution{}, "", fmt.Errorf("%w: %s", helpers.ErrRoleNotFound, req.Src)
}
