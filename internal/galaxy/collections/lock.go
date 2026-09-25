package collections

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// buildLockfile assembles a lockfile from a resolved set and its graph. A
// Galaxy entry's sha256 and download URL come from version metadata the
// resolver already cached, so each costs a metadata fetch, never a tarball.
func buildLockfile(
	ctx context.Context,
	deps collectionDeps,
	resolved map[string]collection,
	graph map[string][]string,
	roles roleResolution,
) (*lockfile.File, error) {
	cfg := deps.cfg
	roleEntries, err := roleLockfileEntries(cfg, roles)
	if err != nil {
		return nil, err
	}
	entries := make([]lockfile.Entry, 0, len(resolved))
	for fqdn, col := range resolved {
		// col.Version may come from a reused, lenient snapshot entry; a pin
		// like "*" would make --frozen install the server's highest. Checked
		// before any metadata fetch, so a poisoned entry buys no request.
		if !helpers.IsExactVersion(col.Version) {
			return nil, fmt.Errorf("lockfile: %s: %w: %q", fqdn, helpers.ErrInvalidCollectionVersion, col.Version)
		}
		if entry, handled, err := sourceLockfileEntry(fqdn, col, graph); handled {
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
			continue
		}
		entry, err := galaxyLockfileEntry(ctx, deps, fqdn, col, graph)
		if err != nil {
			return nil, fmt.Errorf("lockfile: %s: %w", fqdn, err)
		}
		entries = append(entries, entry)
	}
	return &lockfile.File{
		SchemaVersion: lockfile.SchemaVersionFor(entries, roleEntries),
		Server:        cfg.Server,
		Collections:   entries,
		Roles:         roleEntries,
	}, nil
}

// galaxyLockfileEntry renders a Galaxy collection's pin from its version
// metadata: the sha256 and download URL a frozen install then trusts, each
// refused here if it could not be, before anything is written.
func galaxyLockfileEntry(
	ctx context.Context, deps collectionDeps, fqdn string, col collection, graph map[string][]string,
) (lockfile.Entry, error) {
	meta, err := loadCollectionMetadata(ctx, deps, col)
	if err != nil {
		return lockfile.Entry{}, err
	}
	// An empty sha is kept: verifyPinnedSHA reads it as no pin, for
	// digest-less servers.
	sha := strings.TrimSpace(meta.Artifact.Sha256)
	if sha != "" && !helpers.IsSHA256Hex(sha) {
		return lockfile.Entry{}, fmt.Errorf("%w: %q", helpers.ErrMalformedArtifactSHA256, sha)
	}
	downloadURL, err := lockableDownloadURL(meta.DownloadURL)
	if err != nil {
		return lockfile.Entry{}, err
	}
	if err := checkServerArtifactURL(col, lockedServerBase(deps.cfg, col.Source), downloadURL); err != nil {
		return lockfile.Entry{}, err
	}
	return lockfile.Entry{
		Name:        fqdn,
		Version:     col.Version,
		Source:      col.Source,
		DownloadURL: downloadURL,
		SHA256:      sha,
		Deps:        lockfileDepsFromGraph(graph, col.key()),
	}, nil
}

// lockableDownloadURL returns a server's download URL in the canonical form
// lockfile.Load re-parses, refusing what install would refuse and a query: a
// lockfile is committed, and a presigned query is a capability that expires.
func lockableDownloadURL(raw string) (string, error) {
	if raw == "" {
		return "", helpers.ErrMissingDownloadURL
	}
	if err := checkDownloadURL(raw); err != nil {
		return "", err
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Unreached: checkDownloadURL has just parsed the same value.
		return "", fmt.Errorf("%w: %q", helpers.ErrUnsupportedDownloadURLScheme, helpers.URLForMessage(raw))
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("%w: %q", helpers.ErrDownloadURLQuery, helpers.URLForMessage(raw))
	}
	// A fragment never reaches the server, so dropping it changes no request.
	u.Fragment, u.RawFragment = "", ""
	return u.String(), nil
}

// lockedServerBase is the server a Galaxy entry's source names, resolved the
// way its metadata requests resolve it; an empty source is the run's server.
func lockedServerBase(cfg *config.Config, source string) string {
	if source == "" {
		source = cfg.Server
	}
	candidate, _ := pinnedServerCandidate(cfg, source)
	return candidate.base
}

// checkServerArtifactURL holds a locked download URL to its server's own
// artifact - that server's origin, a path ending in the artifact's file name -
// since the bytes land in the cache slot every later install of it reads.
func checkServerArtifactURL(col collection, server, downloadURL string) error {
	want, ok := parsedOrigin(server)
	if !ok {
		return fmt.Errorf("%w: its server %q is not an absolute URL", helpers.ErrDownloadURLNotServerArtifact, helpers.URLForMessage(server))
	}
	u, err := url.Parse(downloadURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: %q is not an absolute URL", helpers.ErrDownloadURLNotServerArtifact, helpers.URLForMessage(downloadURL))
	}
	if got := helpers.Origin(u); got != want {
		return fmt.Errorf("%w: its origin %s is not its server's %s", helpers.ErrDownloadURLNotServerArtifact, got, want)
	}
	filename := helpers.ArtifactFilename(col.Namespace, col.Name, col.Version)
	if !strings.HasSuffix(u.Path, "/"+filename) {
		return fmt.Errorf("%w: %q does not end in /%s", helpers.ErrDownloadURLNotServerArtifact, helpers.URLForMessage(downloadURL), filename)
	}
	return nil
}

// sourceLockfileEntry renders the pin of a collection whose Source is a
// locator - a git or url entry - and reports handled=false for a Galaxy
// collection, whose entry buildLockfile itself renders from server metadata.
func sourceLockfileEntry(fqdn string, col collection, graph map[string][]string) (lockfile.Entry, bool, error) {
	switch {
	case col.isGit():
		entry, err := gitLockfileEntry(fqdn, col, graph)
		return entry, true, err
	case col.isURL():
		entry, err := urlLockfileEntry(fqdn, col, graph)
		return entry, true, err
	default:
		return lockfile.Entry{}, false, nil
	}
}

// gitLockfileEntry renders a git collection's pin: repository URL, the ref as
// asked, commit and subdir, and no sha256 (see lockfile.Entry). The locator is
// taken apart so the reviewed file names the repository, not an internal key.
func gitLockfileEntry(fqdn string, col collection, graph map[string][]string) (lockfile.Entry, error) {
	loc, err := col.gitLocator()
	if err != nil {
		return lockfile.Entry{}, fmt.Errorf("lockfile: %s: %w", fqdn, err)
	}
	if !loc.Pinned() {
		return lockfile.Entry{}, fmt.Errorf("lockfile: %s: %w: git source is not pinned to a commit", fqdn, helpers.ErrInvalidGitLocator)
	}
	ref := col.Ref
	if ref == "" {
		// A resolved collection that lost its ref on the way here (a snapshot
		// written before refs were recorded) still pins correctly by commit.
		ref = loc.Commit
	}
	return lockfile.Entry{
		Name:    fqdn,
		Type:    lockfile.TypeGit,
		Version: col.Version,
		Source:  loc.URL,
		Ref:     ref,
		Commit:  loc.Commit,
		Subdir:  loc.Subdir,
		Deps:    lockfileDepsFromGraph(graph, col.key()),
	}, nil
}

// urlLockfileEntry renders a url collection's pin: the tarball URL and the
// origin bytes' sha256, required where a git entry's is refused (see
// lockfile.Entry). Everything it records was settled at discovery.
func urlLockfileEntry(fqdn string, col collection, graph map[string][]string) (lockfile.Entry, error) {
	loc, err := urlSourceOf(col)
	if err != nil {
		return lockfile.Entry{}, fmt.Errorf("lockfile: %s: %w", fqdn, err)
	}
	return lockfile.Entry{
		Name:    fqdn,
		Type:    lockfile.TypeURL,
		Version: col.Version,
		Source:  loc.URL,
		SHA256:  loc.SHA256,
		Deps:    lockfileDepsFromGraph(graph, col.key()),
	}, nil
}

func lockfileDepsFromGraph(graph map[string][]string, key string) []string {
	deps := graph[key]
	if len(deps) == 0 {
		return nil
	}
	out := make([]string, 0, len(deps))
	for _, dep := range deps {
		fqdn, _, err := splitCollectionKey(dep)
		if err != nil {
			continue
		}
		out = append(out, fqdn)
	}
	return out
}

// resolveFromLockfile builds resolved/graph maps from a lockfile with no HTTP
// call, validating every requested root against its entry; lockedURLs carries
// each download_url over, and a verifying run passes false.
func resolveFromLockfile(
	cfg *config.Config,
	lf *lockfile.File,
	roots []collection,
	lockedURLs bool,
) (map[string]collection, map[string][]string, error) {
	byFQDN, err := indexLockfile(lf, cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := verifyRootsAgainstLockfile(roots, byFQDN); err != nil {
		return nil, nil, err
	}
	if err := checkLockedDownloadURLs(cfg, byFQDN); err != nil {
		return nil, nil, err
	}
	return materializeLockfile(byFQDN, lockedURLs)
}

// checkLockedDownloadURLs holds every Galaxy entry's download_url to its
// server's own artifact, which lockfile.Load cannot judge: an entry's server
// may be a server_list id, and resolving one takes this run's configuration.
func checkLockedDownloadURLs(cfg *config.Config, byFQDN map[string]lockfile.Entry) error {
	for _, fqdn := range slices.Sorted(maps.Keys(byFQDN)) {
		e := byFQDN[fqdn]
		if !e.IsGalaxy() {
			continue
		}
		ns, name, _ := helpers.SplitFQDN(fqdn)
		col := collection{Namespace: ns, Name: name, Version: e.Version}
		if err := checkServerArtifactURL(col, lockedServerBase(cfg, e.Source), e.DownloadURL); err != nil {
			return fmt.Errorf("%w: %s: %w", helpers.ErrLockfileInvalid, fqdn, err)
		}
	}
	return nil
}

func indexLockfile(lf *lockfile.File, cfg *config.Config) (map[string]lockfile.Entry, error) {
	if lf == nil {
		return nil, fmt.Errorf("%w: nil lockfile", helpers.ErrLockfileInvalid)
	}
	out := make(map[string]lockfile.Entry, len(lf.Collections))
	for _, e := range lf.Collections {
		if _, _, ok := helpers.SplitFQDN(e.Name); !ok {
			return nil, fmt.Errorf("%w: invalid name %q", helpers.ErrLockfileInvalid, e.Name)
		}
		entry := e
		if entry.Source == "" && !entry.IsGit() && !entry.IsURL() {
			entry.Source = cfg.Server
		}
		out[e.Name] = entry
	}
	return out, nil
}

func verifyRootsAgainstLockfile(roots []collection, byFQDN map[string]lockfile.Entry) error {
	for _, root := range roots {
		if root.isGit() {
			if err := verifyGitRootAgainstLockfile(root, byFQDN); err != nil {
				return err
			}
			continue
		}
		if root.isURL() {
			if err := verifyURLRootAgainstLockfile(root, byFQDN); err != nil {
				return err
			}
			continue
		}
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		entry, ok := byFQDN[fqdn]
		if !ok {
			return fmt.Errorf("%w: root %s missing", helpers.ErrLockfileMismatch, fqdn)
		}
		constraint := root.Constraint
		if constraint == "" {
			constraint = root.Version
		}
		ok, err := constraintSatisfied(entry.Version, constraint)
		if err != nil {
			return fmt.Errorf("%w: root %s: %s", helpers.ErrLockfileMismatch, fqdn, err.Error())
		}
		if !ok {
			return fmt.Errorf("%w: root %s constraint %q not satisfied by lockfile %s",
				helpers.ErrLockfileMismatch, fqdn, constraint, entry.Version)
		}
	}
	return nil
}

func materializeLockfile(byFQDN map[string]lockfile.Entry, lockedURLs bool) (map[string]collection, map[string][]string, error) {
	resolved := make(map[string]collection, len(byFQDN))
	graph := make(map[string][]string, len(byFQDN))
	for fqdn, e := range byFQDN {
		ns, name, ok := helpers.SplitFQDN(fqdn)
		if !ok {
			return nil, nil, fmt.Errorf("%w: %s", helpers.ErrLockfileInvalid, fqdn)
		}
		col := collection{Namespace: ns, Name: name, Version: e.Version, Source: e.Source, SHA256: e.SHA256}
		switch {
		case e.IsGit():
			// The pin is the commit: the locator rebuilt here is what keys the
			// artifact cache and the installed record, and SHA256 stays empty
			// so verifyPinnedSHA has nothing to compare (see lockfile.Entry).
			col.Source = gitsource.Locator{URL: e.Source, Subdir: e.Subdir, Commit: e.Commit}.String()
			col.Type = typeGit
			col.Ref = e.Ref
		case e.IsURL():
			// The pin is the sha256, carried in both the locator (the artifact
			// key) and col.SHA256 (what verifyPinnedSHA compares bytes to).
			col.Source = urlsource.Locator{URL: e.Source, SHA256: e.SHA256}.String()
			col.Type = typeURL
		case lockedURLs:
			col.DownloadURL = e.DownloadURL
		}
		resolved[fqdn] = col
		graph[col.key()] = lockfileDepsToKeys(e.Deps, byFQDN)
	}
	return resolved, graph, nil
}

// verifyGitRootAgainstLockfile requires a git entry from the root's repository
// under its subdir, every such entry locked from the root's ref (a changed ref
// is a mismatch even at the same commit), and a named root's own fqdn.
func verifyGitRootAgainstLockfile(root collection, byFQDN map[string]lockfile.Entry) error {
	loc, err := root.gitLocator()
	if err != nil {
		return err
	}
	display := helpers.URLForMessage(loc.URL)
	matched := 0
	for fqdn, entry := range byFQDN {
		if !lockedFromGitRoot(entry, loc) {
			continue
		}
		if entry.Ref != root.Ref {
			return fmt.Errorf("%w: git root %s locked from ref %q, requirements ask for %q",
				helpers.ErrLockfileMismatch, display, entry.Ref, root.Ref)
		}
		matched++
		if root.Namespace != "" && fqdn == root.Namespace+"."+root.Name {
			return nil
		}
	}
	return gitRootUnmatched(root, display, matched)
}

// lockedFromGitRoot reports whether entry is a git entry locked from loc's
// repository under loc's subdir (itself or an immediate child).
func lockedFromGitRoot(entry lockfile.Entry, loc gitsource.Locator) bool {
	return entry.IsGit() && entry.Source == loc.URL && subdirWithin(entry.Subdir, loc.Subdir)
}

// gitRootUnmatched decides a git root whose own fqdn no entry carried: no entry
// from the repository is a mismatch, so is a named root, and an unnamed root is
// satisfied by any matched entry.
func gitRootUnmatched(root collection, display string, matched int) error {
	switch {
	case matched == 0:
		return fmt.Errorf("%w: git root %s has no lockfile entry", helpers.ErrLockfileMismatch, display)
	case root.Namespace != "":
		return fmt.Errorf("%w: git root %s has no lockfile entry for %s.%s",
			helpers.ErrLockfileMismatch, display, root.Namespace, root.Name)
	default:
		return nil
	}
}

// verifyURLRootAgainstLockfile requires the entry locked from the root's URL,
// and a version: the root asserts must equal the locked version, since
// --frozen means what was asked for has not changed.
func verifyURLRootAgainstLockfile(root collection, byFQDN map[string]lockfile.Entry) error {
	loc, err := root.urlLocator()
	if err != nil {
		return err
	}
	display := helpers.URLForMessage(loc.URL)
	for _, entry := range byFQDN {
		if !entry.IsURL() || entry.Source != loc.URL {
			continue
		}
		if root.Constraint != "" && entry.Version != root.Constraint {
			return fmt.Errorf("%w: url root %s locked as version %q, requirements ask for %q",
				helpers.ErrLockfileMismatch, display, entry.Version, root.Constraint)
		}
		return nil
	}
	return fmt.Errorf("%w: url root %s has no lockfile entry", helpers.ErrLockfileMismatch, display)
}

// subdirWithin reports whether a locked entry's subdir is the root's own
// subdir or an immediate child of it.
func subdirWithin(entrySubdir, rootSubdir string) bool {
	if entrySubdir == rootSubdir {
		return true
	}
	parent := path.Dir(entrySubdir)
	if parent == "." {
		parent = ""
	}
	return parent == rootSubdir
}

func lockfileDepsToKeys(deps []string, byFQDN map[string]lockfile.Entry) []string {
	if len(deps) == 0 {
		return nil
	}
	out := make([]string, 0, len(deps))
	for _, dep := range deps {
		entry, ok := byFQDN[dep]
		if !ok {
			continue
		}
		out = append(out, fmt.Sprintf("%s@%s", dep, entry.Version))
	}
	return out
}

// lockDryRunBaseline loads the lockfile at path as a dry run's "before" side,
// or nil when it is absent or unloadable (warned, never fatal, since a real
// lock only overwrites it). lockFrozen, whose verdict is that file, fails closed.
func lockDryRunBaseline(runtime *infra.Infra, path string) *lockfile.File {
	lf, err := lockfile.Load(path)
	if err == nil {
		return lf
	}
	if !lockfile.IsNotExist(err) {
		runtime.Output.Warnf("Existing lockfile %s cannot be read (%v); reporting every collection as added", path, err)
	}
	return nil
}

// dryRunDiffPrefix and frozenDiffPrefix lead reportLockfileDiff's summary line
// for lockDryRun's preview and lockFrozen's drift gate.
const (
	dryRunDiffPrefix = "Dry run"
	frozenDiffPrefix = "Frozen"
)

// reportLockfileDiff prints the server change, one Okf line per changed
// collection and a summary whose verdict is diff.Empty(), since a server-only
// change leaves every count zero. The unchanged count needs unique names in lf.
func reportLockfileDiff(runtime *infra.Infra, lf *lockfile.File, path, prefix string, diff lockfile.Diff) {
	if diff.Server != nil {
		runtime.Output.Okf("Would change: %s", renderFieldChange(*diff.Server))
	}
	for _, e := range diff.Added {
		runtime.Output.Okf("Would add: %s@%s", e.Name, e.Version)
	}
	for _, c := range diff.Updated {
		runtime.Output.Okf("Would update: %s (%s)", c.To.Name, renderFieldChanges(c.Fields()))
	}
	for _, e := range diff.Removed {
		runtime.Output.Okf("Would remove: %s@%s", e.Name, e.Version)
	}
	reportRoleDiff(runtime, lf, prefix, diff)
	unchanged := len(lf.Collections) - len(diff.Added) - len(diff.Updated)
	verdict := "lockfile would change"
	if diff.Empty() {
		verdict = "lockfile is up to date"
	}
	runtime.Output.PersistentPrintf(
		"%s: %s; %d would be added, %d would be updated, %d would be removed, %d unchanged (%s)",
		prefix, verdict, len(diff.Added), len(diff.Updated), len(diff.Removed), unchanged, path,
	)
}

// reportRoleDiff renders the roles half of a diff only when the file or the
// diff has a role, so a collections-only report carries no roles lines; lf's
// roles come from a name-keyed map, so the unchanged count counts each once.
func reportRoleDiff(runtime *infra.Infra, lf *lockfile.File, prefix string, diff lockfile.Diff) {
	if len(lf.Roles) == 0 && !diff.HasRoles() {
		return
	}
	for _, e := range diff.RolesAdded {
		runtime.Output.Okf("Would add role: %s@%s", e.Name, e.Version)
	}
	for _, c := range diff.RolesUpdated {
		runtime.Output.Okf("Would update role: %s (%s)", c.To.Name, renderFieldChanges(c.Fields()))
	}
	for _, e := range diff.RolesRemoved {
		runtime.Output.Okf("Would remove role: %s@%s", e.Name, e.Version)
	}
	unchanged := len(lf.Roles) - len(diff.RolesAdded) - len(diff.RolesUpdated)
	runtime.Output.PersistentPrintf(
		"%s: roles: %d would be added, %d would be updated, %d would be removed, %d unchanged",
		prefix, len(diff.RolesAdded), len(diff.RolesUpdated), len(diff.RolesRemoved), unchanged,
	)
}

// renderFieldChange renders one FieldChange as "version 0.9.0 -> 1.0.0".
func renderFieldChange(f lockfile.FieldChange) string {
	return fmt.Sprintf("%s %s -> %s", f.Field, quoteEmpty(f.From), quoteEmpty(f.To))
}

// renderFieldChanges joins an entry's changed fields with "; ". Values are
// never abbreviated: a truncated sha256 can make two different digests look
// identical, the very difference this report exists to surface.
func renderFieldChanges(fields []lockfile.FieldChange) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, renderFieldChange(f))
	}
	return strings.Join(parts, "; ")
}

// quoteEmpty renders an empty field value as "(none)" so a line reporting a
// value gained or lost does not read as though it changed into nothing
// visible at all.
func quoteEmpty(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}
