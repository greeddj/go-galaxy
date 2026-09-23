package collections

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
	"go.yaml.in/yaml/v3"
)

// errEmptyRolesPath names the misconfiguration directly, as
// errEmptyDownloadPath does for the collections tree.
var errEmptyRolesPath = errors.New("--roles-path (or [defaults] roles_path) is empty")

// galaxyInstallInfoRel is the file ansible-galaxy writes beside an installed
// role's meta, relative to the role directory, and reads back for
// `ansible-galaxy role list`. This tool writes it for the same readers.
const galaxyInstallInfoRel = "meta/.galaxy_install_info"

// galaxyInstallInfoTimeLayout is C's %c in the C locale, the format
// ansible-galaxy writes install_date in.
const galaxyInstallInfoTimeLayout = "Mon Jan _2 15:04:05 2006"

// openRolesRoot opens the os.Root every role write funnels through, at
// rolesPath itself, so a swapped directory is refused by the kernel; with
// create false (a dry run) an absent rolesPath yields a nil root.
func openRolesRoot(rolesPath string, create bool) (*os.Root, error) {
	if strings.TrimSpace(rolesPath) == "" {
		return nil, errEmptyRolesPath
	}
	if !create {
		root, err := os.OpenRoot(rolesPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil //nolint:nilnil // absent roles path on a dry run is "nothing to describe yet", not a failure.
			}
			return nil, err
		}
		return root, nil
	}
	if err := os.MkdirAll(rolesPath, helpers.DirMod); err != nil {
		return nil, err
	}
	return os.OpenRoot(rolesPath)
}

// newRoleTarget builds a role's installTarget under root, the single
// chokepoint that validates the install name before it is joined; a nil
// root or an unsafe name fails closed as ok=false.
func newRoleTarget(root *os.Root, cfg *config.Config, r resolvedRole) (installTarget, bool) {
	if root == nil || !helpers.IsRoleInstallName(r.Name) {
		return installTarget{}, false
	}
	return installTarget{root: root, rel: r.Name, path: absoluteOrAsIs(filepath.Join(cfg.RolesPath, r.Name)), marker: r.Name}, true
}

// absoluteOrAsIs renders p absolute, or as given when it cannot. Cleanup
// finds a role's record by its absolute install path, so the record must
// not depend on how --roles-path was spelled.
func absoluteOrAsIs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// installRoles installs every resolved role on the Workers-bounded pool,
// flat, since each role is its own directory, recording each failure as a
// role's so the run's headline counts it apart from the collections'.
func installRoles(ctx context.Context, deps installDeps, res roleResolution, failures *failureRecorder) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(deps.cfg.Workers, 1))
	defer wg.Wait()
	for _, name := range res.order {
		if ctx.Err() != nil {
			break
		}
		role := res.roles[name]
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := installRole(ctx, deps, role); err != nil {
				deps.runtime.Output.ErrorVersionf(role.Version, fmt.Sprintf("error: %s", err),
					"Failed: role %s", role.Name)
				failures.recordRole(err)
			} else {
				deps.runtime.Output.OkVersionf(role.Version, "Installed: role %s", role.Name)
			}
		})
	}
}

// installRole acquires, extracts and records one role. The directory policy
// runs before any fetch: a directory neither this tool nor ansible-galaxy
// installed is refused, since it could be a role somebody wrote by hand.
func installRole(ctx context.Context, deps installDeps, r resolvedRole) error {
	runtime := deps.runtime
	start := time.Now()
	defer func() { runtime.Output.DebugSincef(start, "Role %s", r.key()) }()

	target, ok := newRoleTarget(deps.rolesRoot, deps.cfg, r)
	if !ok {
		return fmt.Errorf("%w: role name %q", helpers.ErrUnsafeCollectionIdentifier, r.Name)
	}
	if canSkipRoleInstall(target, r, deps.st, runtime.Output) {
		runtime.Output.Printf("Skipping install, already installed: role %s", r.key())
		return nil
	}
	owner, err := checkRoleDirectoryOwned(target)
	if err != nil {
		return err
	}
	if owner == roleDirectoryAnsible {
		runtime.Output.Warnf("Role %s: replacing a role ansible-galaxy installed at %s", r.Name, target.path)
	}
	artifact, err := fetchRoleArtifact(ctx, deps, r)
	if err != nil {
		return err
	}
	defer cleanupIfNeeded(artifact.Cleanup)
	sha, computed, err := resolveArtifactSHA(artifact.Path, nil, artifact.Meta, artifact.SHA, "")
	if err != nil {
		return err
	}
	extractStart := time.Now()
	installInfo := func(target installTarget) error { return writeGalaxyInstallInfo(target, r.Version, runtime.Now()) }
	if err := extractTree(ctx, "role "+r.Name, artifact.Path, target, runtime, deps.extractStore, sha, computed, installInfo); err != nil {
		return fmt.Errorf("failed to extract role %s: %w", r.Name, err)
	}
	runtime.Output.DebugSincef(extractStart, "%s", "Extract role "+r.Name)
	recordRoleInstall(deps.st, r, target.path, sha, runtime.Now())
	return nil
}

// roleRecordMatches reports whether the store's record names this target
// and locator and the marker and install info are present: the Stat-only
// half of the skip check, which the dry-run probe uses too.
func roleRecordMatches(target installTarget, r resolvedRole, st *store.Store) (store.InstalledRoleEntry, bool) {
	if st == nil {
		return store.InstalledRoleEntry{}, false
	}
	entry, ok := st.GetInstalledRole(r.Name)
	if !ok || !roleEntryMatches(entry, r, target.path) {
		return store.InstalledRoleEntry{}, false
	}
	markerRelPath, ok := markerRel(target, entry.ArtifactSHA256)
	if !ok {
		return store.InstalledRoleEntry{}, false
	}
	if _, err := target.root.Stat(markerRelPath); err != nil {
		return store.InstalledRoleEntry{}, false
	}
	if _, err := target.root.Stat(path.Join(target.rel, galaxyInstallInfoRel)); err != nil {
		return store.InstalledRoleEntry{}, false
	}
	return entry, true
}

// roleEntryMatches is installEntryMatches for a role. The version is
// compared too: .galaxy_install_info shows it, so a ref respelled onto the
// same commit must still rewrite it.
func roleEntryMatches(entry store.InstalledRoleEntry, r resolvedRole, installPath string) bool {
	return entry.InstallPath != "" && entry.InstallPath == installPath && entry.ArtifactSHA256 != "" &&
		entry.Source == r.Source && entry.Version == r.Version
}

// canSkipRoleInstall is roleRecordMatches plus verifyExtractMarker's tally
// walk: the gate that decides whether real work is skipped, like
// canSkipInstall for a collection.
func canSkipRoleInstall(target installTarget, r resolvedRole, st *store.Store, out output.Printer) bool {
	entry, ok := roleRecordMatches(target, r, st)
	if !ok {
		return false
	}
	return verifyExtractMarker(out, target, entry.ArtifactSHA256)
}

// roleDirectoryOwner is who wrote the directory a role would install into.
type roleDirectoryOwner uint8

const (
	// roleDirectoryAbsent: nothing is there yet.
	roleDirectoryAbsent roleDirectoryOwner = iota
	// roleDirectoryOurs: this tool's extract marker is there.
	roleDirectoryOurs
	// roleDirectoryAnsible: ansible-galaxy's .galaxy_install_info is there
	// and no marker of ours.
	roleDirectoryAnsible
)

// checkRoleDirectoryOwned allows replacing a directory that is absent or
// carries this tool's marker or ansible-galaxy's install info, and refuses
// anything else; listing through target.root refuses a symlinked directory.
func checkRoleDirectoryOwned(target installTarget) (roleDirectoryOwner, error) {
	entries, err := fs.ReadDir(target.root.FS(), target.rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return roleDirectoryAbsent, nil
		}
		return roleDirectoryAbsent, classifyRolesRootError(target.root, target.rel, err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), helpers.ExtractMarkerPrefix) {
			return roleDirectoryOurs, nil
		}
	}
	if _, err := target.root.Stat(path.Join(target.rel, galaxyInstallInfoRel)); err == nil {
		return roleDirectoryAnsible, nil
	}
	return roleDirectoryAbsent, fmt.Errorf("%w: %s appears to already exist and is not a Galaxy-installed role; "+
		"remove it to let this tool manage it", helpers.ErrRoleDirectoryForeign, target.path)
}

// fetchRoleArtifact produces the role's artifact: a --no-cache build left by
// discovery, else a cache hit, else - unless --offline - a fetch by the
// pinned commit.
func fetchRoleArtifact(ctx context.Context, deps installDeps, r resolvedRole) (artifactData, error) {
	if prebuilt, ok := deps.roleMemo.takePrebuilt(r.Name); ok {
		return artifactData{Path: prebuilt.Path, Cleanup: prebuilt.Cleanup, SHA: prebuilt.SHA}, nil
	}
	key := roleArtifactKey(r)
	if !deps.cfg.NoCache && deps.artifacts != nil {
		if ok, err := deps.artifacts.Has(ctx, key); err == nil && ok {
			return fetchCachedArtifact(ctx, deps, key)
		}
	}
	if deps.cfg.Offline {
		return artifactData{}, fmt.Errorf("%w: role artifact %s not in cache", helpers.ErrOfflineMode, r.key())
	}
	result, err := roleFetchToCache(ctx, deps, r, !deps.cfg.NoCache)
	if err != nil {
		return artifactData{}, err
	}
	return artifactData{Path: result.Path, Cleanup: result.Cleanup, SHA: result.SHA}, nil
}

// roleArtifactKey is the one composition of a role's artifact cache key,
// shared by discovery's commit, the install's fetch and cleanup's delete.
func roleArtifactKey(r resolvedRole) string {
	return helpers.ArtifactKey(r.Source, helpers.RoleArtifactFilename(r.Name, r.Version))
}

// fetchCachedArtifact serves a cached artifact under the artifact deadline,
// exactly as fetchArtifact's cache-hit arm does for a collection.
func fetchCachedArtifact(ctx context.Context, deps installDeps, key string) (artifactData, error) {
	budget := deps.runtime.ArtifactDeadline()
	fetchCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cached, err := deps.artifacts.Fetch(fetchCtx, key)
	if err != nil {
		return artifactData{}, artifactDeadlineError(ctx, fetchCtx, budget, err)
	}
	deps.runtime.Metrics.AddCacheHit()
	return artifactData{Path: cached.Path, Cleanup: cached.Cleanup, Meta: cached.Meta, SHA: cached.SHA}, nil
}

// roleFetchToCache rebuilds a pinned role's artifact on a cache miss at
// install time; a repository serving another commit than the pin fails as
// helpers.ErrRoleArtifactIdentityMismatch.
func roleFetchToCache(ctx context.Context, deps installDeps, r resolvedRole, useCache bool) (downloadResult, error) {
	if urlsource.IsLocator(r.Source) {
		return urlRoleFetchToCache(ctx, deps, r, useCache)
	}
	return gitRoleFetchToCache(ctx, deps, r, useCache)
}

// gitRoleFetchToCache is roleFetchToCache's git and Galaxy arm: the refetch
// by pinned commit.
func gitRoleFetchToCache(ctx context.Context, deps installDeps, r resolvedRole, useCache bool) (downloadResult, error) {
	runtime := deps.runtime
	if runtime == nil || runtime.Git == nil {
		return downloadResult{}, fmt.Errorf("%w: no git client is wired into this run", helpers.ErrConfigIsNil)
	}
	req, display, err := roleRefetchRequest(deps, r)
	if err != nil {
		return downloadResult{}, err
	}
	start := time.Now()
	gitCtx, cancel := context.WithTimeout(ctx, runtime.GitDeadline())
	defer cancel()
	result, err := runtime.Git.AcquireRole(gitCtx, req)
	if err != nil {
		return downloadResult{}, artifactDeadlineError(ctx, gitCtx, runtime.GitDeadline(), err)
	}
	runtime.Output.DebugSincef(start, "Fetch %s@%s for role %s (%d bytes)", display, req.Commit, r.Name, result.BytesFetched)
	runtime.Metrics.AddBytesDownloaded(result.BytesFetched)
	for _, warning := range result.Warnings {
		runtime.Output.Warnf("%s: %s", display, warning)
	}
	if result.Commit != req.Commit {
		cleanupIfNeeded(result.Cleanup)
		return downloadResult{}, fmt.Errorf("%w: %s served commit %s for role %s, the pin names %s",
			helpers.ErrRoleArtifactIdentityMismatch, display, result.Commit, r.Name, req.Commit)
	}
	if !useCache || deps.artifacts == nil {
		return downloadResult{Path: result.ArtifactPath, SHA: result.ArtifactSHA, Cleanup: result.Cleanup}, nil
	}
	stored, err := commitDownload(ctx, deps.artifacts, roleArtifactKey(r), result.ArtifactPath, result.ArtifactSHA, result.Cleanup)
	if err != nil {
		return downloadResult{}, err
	}
	runtime.Metrics.AddCacheMiss()
	return stored, nil
}

// roleRefetchRequest builds the acquisition for r's pinned locator and the
// repository's display name for messages, the role counterpart of
// gitRefetchRequest.
func roleRefetchRequest(deps installDeps, r resolvedRole) (gitsource.RoleRequest, string, error) {
	loc, err := gitsource.ParseLocator(r.Source)
	if err != nil {
		return gitsource.RoleRequest{}, "", err
	}
	if !loc.Pinned() {
		return gitsource.RoleRequest{}, "", fmt.Errorf("%w: role %s is not pinned to a commit", helpers.ErrInvalidGitLocator, r.Name)
	}
	u, err := gitsource.ParseURL(loc.URL)
	if err != nil {
		return gitsource.RoleRequest{}, "", err
	}
	ref, err := gitsource.ParseRef(r.Ref)
	if err != nil {
		return gitsource.RoleRequest{}, "", err
	}
	cred, _ := gitsource.MatchCredential(u, deps.runtime.GitCredentials)
	return gitsource.RoleRequest{
		URL: u, Ref: ref, Commit: loc.Commit, Auth: cred, TempFile: gitTempFile(deps.collectionDeps),
	}, helpers.URLForMessage(loc.URL), nil
}

// galaxyInstallInfo is the document ansible-galaxy writes into
// meta/.galaxy_install_info; ansible reads it with safe_load, so yaml v3's
// quoting of a numeric-looking version is not part of the contract.
type galaxyInstallInfo struct {
	InstallDate string `yaml:"install_date"`
	Version     string `yaml:"version"`
}

// writeGalaxyInstallInfo writes ansible-galaxy's install record through
// target.root before the extract marker, so the tally counts it; the date is
// the wall clock, as ansible writes it, outside the deterministic artifact.
func writeGalaxyInstallInfo(target installTarget, version string, now time.Time) error {
	data, err := yaml.Marshal(galaxyInstallInfo{
		InstallDate: now.UTC().Format(galaxyInstallInfoTimeLayout),
		Version:     version,
	})
	if err != nil {
		return err
	}
	rel := path.Join(target.rel, galaxyInstallInfoRel)
	if err := target.root.MkdirAll(path.Dir(rel), helpers.DirMod); err != nil {
		return classifyRolesRootError(target.root, path.Dir(rel), err)
	}
	// Removed before it is written, never truncated in place: a file here
	// may be a hard link into the shared extracted store, and writing
	// through it would rewrite the store's bytes under their sha.
	if err := target.root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return classifyRolesRootError(target.root, rel, err)
	}
	if err := target.root.WriteFile(rel, data, helpers.FileMod); err != nil {
		return classifyRolesRootError(target.root, rel, err)
	}
	return nil
}

// classifyRolesRootError is classifyCollectionsRootError for the roles
// tree: the same escape classification, with the remedy naming the roles
// path setting rather than the collections one.
func classifyRolesRootError(root *os.Root, rel string, err error) error {
	classified := classifyCollectionsRootError(root, rel, err)
	if errors.Is(classified, helpers.ErrCollectionsPathEscape) {
		return fmt.Errorf("%w: %q: point --roles-path/roles_path at the real directory instead of a symlink: %w",
			helpers.ErrCollectionsPathEscape, rel, err)
	}
	return classified
}

// recordRoleInstall records the installed role in the snapshot by install
// name.
func recordRoleInstall(st *store.Store, r resolvedRole, installPath, artifactSHA string, now time.Time) {
	if st == nil {
		return
	}
	st.SetInstalledRole(r.Name, store.InstalledRoleEntry{
		InstallPath:    installPath,
		Source:         r.Source,
		ArtifactSHA256: artifactSHA,
		Version:        r.Version,
		GalaxyName:     r.GalaxyName,
		InstalledAt:    now.UTC(),
		Deps:           r.Deps,
	})
}
