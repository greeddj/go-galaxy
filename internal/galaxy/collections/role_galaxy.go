package collections

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/galaxyv1"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// galaxyRolePinKey is the store key of a Galaxy role's pin, keyed by the
// requirement line (name and version asked for); it records the repository
// and tag so a rerun skips the v1 round trips, the git pin below it the commit.
func galaxyRolePinKey(galaxyName, requested string) string {
	return "galaxy\n" + galaxyName + "\n" + requested
}

// resolveGalaxyRole maps a Galaxy role to a repository and tag (the lockfile's
// entry under lock, else the pin or the v1 API), then takes the git path. The
// Galaxy pin carries its commit, so it is recorded after; never from the entry.
func resolveGalaxyRole(ctx context.Context, deps collectionDeps, req requirements.RoleRequirement) (rolePin, error) {
	policy := cacheManager.PolicyForConstraint(deps.cfg, req.Version != "")
	key := galaxyRolePinKey(req.Src, req.Version)
	locked, isLocked := deps.lockPrefs.role(req)
	var unreachable error
	if isLocked {
		pin, kept, err := resolveLockedGalaxyRole(ctx, deps, req, locked)
		if unreachable = unreachableRepository(err); unreachable == nil && (kept || err != nil) {
			return pin, err
		}
	}
	res, server, err := galaxyRoleResolution(ctx, deps, req, key, policy, avoidedRepository(locked, unreachable))
	if err != nil {
		return rolePin{}, err
	}
	if err := checkMovedGalaxyRole(deps, req.Name, locked, res, unreachable); err != nil {
		return rolePin{}, err
	}
	pin, err := resolveGitRoleRequest(ctx, deps, galaxyGitRoleRequest(deps, req.Name, res), res.GalaxySHA)
	if err != nil {
		return rolePin{}, err
	}
	pin.galaxyName = req.Src
	pin.server = server
	pin.version = res.Version
	if policy.Write {
		recordGalaxyRolePin(deps.st, key, store.RolePinEntry{
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

// galaxyRoleResolution maps a Galaxy role to its repository and tag from the
// pin under key, else, outside --offline, from the servers' v1 API, naming the
// server that answered; avoid, a repository that failed, skips both caches.
func galaxyRoleResolution(
	ctx context.Context, deps collectionDeps, req requirements.RoleRequirement, key string, policy cacheManager.Policy,
	avoid string,
) (galaxyv1.Resolution, string, error) {
	res, server, ok, err := replayGalaxyPin(deps, key, policy)
	if err != nil || (ok && (avoid == "" || res.RepoURL.String() != avoid)) {
		return res, server, err
	}
	if deps.cfg != nil && deps.cfg.Offline {
		return galaxyv1.Resolution{}, "", fmt.Errorf("%w: Galaxy role %s@%s is not recorded in the cache",
			helpers.ErrOfflineMode, req.Src, displayRoleVersion(req.Version))
	}
	if avoid != "" {
		policy.Read = false
	}
	return lookupGalaxyRole(ctx, deps, req, policy)
}

// galaxyGitRoleRequest is the git request a Galaxy role's resolution names,
// with the credential bound to that repository rather than to the server.
func galaxyGitRoleRequest(deps collectionDeps, name string, res galaxyv1.Resolution) gitRoleRequest {
	greq := gitRoleRequest{
		name:    name,
		pinKey:  gitsource.PinKey(res.RepoURL.String(), res.Ref.Name, ""),
		display: helpers.URLForMessage(res.RepoURL.String()),
		url:     res.RepoURL,
		ref:     res.Ref,
	}
	greq.cred, _ = gitsource.MatchCredential(res.RepoURL, gitCredentialsOf(deps))
	return greq
}

// resolveLockedGalaxyRole keeps the lockfile's repository, tag and commit for a
// Galaxy role without asking the v1 API, and records no Galaxy pin from them, as
// no v1 answer named that repository. kept=false: entry not usable, commit gone.
func resolveLockedGalaxyRole(
	ctx context.Context, deps collectionDeps, req requirements.RoleRequirement, locked lockfile.RoleEntry,
) (rolePin, bool, error) {
	res, ok := usableGalaxyRoleEntry(deps, req.Name, locked)
	if !ok {
		return rolePin{}, false, nil
	}
	pin, kept, err := resolveLockedGitRole(ctx, deps, galaxyGitRoleRequest(deps, req.Name, res), locked.Commit, res.Version)
	if !kept || err != nil {
		return rolePin{}, kept, err
	}
	pin.galaxyName = req.Src
	pin.server = locked.Source
	pin.version = res.Version
	return pin, true, nil
}

// unreachableRepository returns err when it is a fetch failing on the
// repository itself, gone, refused or empty, rather than on the commit, the
// run's own state or its context, and nil otherwise.
func unreachableRepository(err error) error {
	if errors.Is(err, helpers.ErrGitTransportFailed) || errors.Is(err, helpers.ErrGitAuthFailed) ||
		errors.Is(err, helpers.ErrGitRefNotFound) {
		return err
	}
	return nil
}

// avoidedRepository is the locked repository once unreachable failed it, the
// one galaxyRoleResolution must not take from a pin or a cached answer.
func avoidedRepository(locked lockfile.RoleEntry, unreachable error) string {
	if unreachable == nil {
		return ""
	}
	return locked.Repository
}

// checkMovedGalaxyRole judges what the v1 API names once the locked repository
// failed: the same one leaves nothing else to fetch from, so its failure ends
// the run; another means the role moved, which warns once and is taken.
func checkMovedGalaxyRole(deps collectionDeps, name string, locked lockfile.RoleEntry, res galaxyv1.Resolution, unreachable error) error {
	if unreachable == nil {
		return nil
	}
	if res.RepoURL.String() == locked.Repository {
		return unreachable
	}
	deps.lockPrefs.warnRolef(deps.runtime.Output, name, "Locked role %s: repository %s can no longer be fetched; resolving the role anew",
		name, helpers.URLForMessage(locked.Repository))
	return nil
}

// usableGalaxyRoleEntry judges a locked Galaxy role as galaxyPinResolution
// judges a recorded pin, and holds its server to one this run asks; either
// failure warns once that the entry is not used.
func usableGalaxyRoleEntry(deps collectionDeps, name string, e lockfile.RoleEntry) (galaxyv1.Resolution, bool) {
	if !lockedRoleServerAsked(deps.cfg, e.Source) {
		deps.lockPrefs.warnRolef(deps.runtime.Output, name, "Locked role %s: server %s is not one this run uses; resolving the role anew",
			name, helpers.URLForMessage(e.Source))
		return galaxyv1.Resolution{}, false
	}
	res, err := galaxyPinResolution(store.RolePinEntry{Repository: e.Repository, Ref: e.Ref, Version: e.Version})
	if err != nil {
		problem := "repository " + helpers.URLForMessage(e.Repository) + " is not a GitHub repository"
		if errors.Is(err, helpers.ErrInvalidRoleVersion) {
			problem = "ref " + helpers.ValueForMessage(e.Ref) + " is not a refs/tags/ or refs/heads/ ref"
		}
		deps.lockPrefs.warnRolef(deps.runtime.Output, name, "Locked role %s: %s; resolving the role anew", name, problem)
		return galaxyv1.Resolution{}, false
	}
	return res, true
}

// recordGalaxyRolePin records entry under key unless the pin there already
// says the same: a rewrite would only restamp FetchedAt and mark the store
// dirty, so no run replaying a Galaxy role could skip its snapshot save.
func recordGalaxyRolePin(st *store.Store, key string, entry store.RolePinEntry) {
	if recorded, ok := st.GetRolePin(key); ok && sameRolePin(recorded, entry) {
		return
	}
	st.SetRolePin(key, entry)
}

// sameRolePin reports whether two pins agree in every field but FetchedAt,
// a nil and an empty Deps alike.
func sameRolePin(a, b store.RolePinEntry) bool {
	if !slices.Equal(a.Deps, b.Deps) {
		return false
	}
	a.FetchedAt, a.Deps = time.Time{}, nil
	b.FetchedAt, b.Deps = time.Time{}, nil
	return reflect.DeepEqual(a, b)
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
