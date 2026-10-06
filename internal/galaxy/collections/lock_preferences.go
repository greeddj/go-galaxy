package collections

import (
	"errors"
	"maps"
	"slices"
	"sync"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
)

// lockPreferences is what a lock run keeps from the galaxy.lock it read: the
// pins its resolve prefers, looked up per entry kind, and what the run found
// out about them. A nil *lockPreferences prefers nothing; every method is nil-safe.
type lockPreferences struct {
	// galaxy holds the Galaxy entries by fqdn, urls the url entries by source
	// and roles the role entries by install name. They and git are read-only
	// after newLockPreferences, so concurrent readers take no lock.
	galaxy map[string]lockfile.Entry
	urls   map[string]lockfile.Entry
	roles  map[string]lockfile.RoleEntry
	// unpublished holds each fqdn whose locked version a server answered 404
	// for, and warned the entries already warned about; mu guards both.
	unpublished map[string]struct{}
	warned      map[lockEntryRef]struct{}
	// kept holds the git roots the last expansion kept at their locked commit
	// and released the commits a failed resolve ruled out, both by gitRootKey;
	// mu guards both.
	kept     map[string]keptGitRoot
	released map[string]string
	// git holds the git entries, which matchGitRoots hands to the git roots.
	git []lockfile.Entry
	mu  sync.Mutex
}

// lockEntryKind tells galaxy.lock's entry lists apart, since a collection
// entry and a role entry may carry one name.
type lockEntryKind uint8

const (
	// lockedCollection is an entry of galaxy.lock's collections list.
	lockedCollection lockEntryKind = iota
	// lockedRole is an entry of its roles list, named by install name.
	lockedRole
)

// lockEntryRef names one galaxy.lock entry in the warn-once set.
type lockEntryRef struct {
	name string
	kind lockEntryKind
}

// newLockPreferences indexes lf's pins, or returns nil when there is no file
// to keep pins from or --refresh sets them aside; --offline skips --refresh.
func newLockPreferences(cfg *config.Config, lf *lockfile.File) *lockPreferences {
	if lf == nil || (cfg.Refresh && !cfg.Offline) {
		return nil
	}
	prefs := &lockPreferences{
		galaxy:      make(map[string]lockfile.Entry, len(lf.Collections)),
		urls:        make(map[string]lockfile.Entry),
		roles:       make(map[string]lockfile.RoleEntry, len(lf.Roles)),
		unpublished: make(map[string]struct{}),
		warned:      make(map[lockEntryRef]struct{}),
		released:    make(map[string]string),
	}
	for _, e := range lf.Collections {
		switch {
		case e.IsGalaxy():
			prefs.galaxy[e.Name] = e
		case e.IsGit():
			prefs.git = append(prefs.git, e)
		case e.IsURL():
			prefs.urls[e.Source] = e
		}
	}
	for _, e := range lf.Roles {
		prefs.roles[e.Name] = e
	}
	return prefs
}

// lockedGitRoot is the commit galaxy.lock pins one git root at and the names
// of the entries that root owns, in name order; the zero value pins nothing.
// A commit the requirements ruled out moves to released, which pins nothing.
type lockedGitRoot struct {
	commit   string
	released string
	names    []string
}

// gitRoots returns, aligned with roots, what galaxy.lock pins each git root at:
// the commit every entry it owns by matchGitRoots shares, the rule --frozen
// judges it by. A root asked at a commit is never looked up: that ref is its pin.
func (p *lockPreferences) gitRoots(roots []collection) []lockedGitRoot {
	locked := make([]lockedGitRoot, len(roots))
	if p == nil || len(p.git) == 0 {
		return locked
	}
	matches, err := matchGitRoots(roots, p.git)
	if err != nil {
		// The root whose locator does not parse fails its own expansion.
		return locked
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, root := range roots {
		if !root.isGit() || gitsource.IsCommitHash(root.Ref) {
			continue
		}
		locked[i] = lockedFromOwned(matches[i].Owned)
		if key, ok := gitRootKey(root); ok && locked[i].commit != "" && p.released[key] != "" {
			locked[i] = lockedGitRoot{released: p.released[key], names: locked[i].names}
		}
	}
	return locked
}

// gitRootKey names a git root by its repository and subdir, which no two
// requirements share (prepareRoots refuses that).
func gitRootKey(root collection) (string, bool) {
	loc, err := root.gitLocator()
	if err != nil {
		return "", false
	}
	return loc.URL + "\n" + loc.Subdir, true
}

// keptGitRoot is one git root an expansion kept at its locked commit: the pin,
// how a warning names the root, and the collections the commit holds for it.
type keptGitRoot struct {
	display string
	ref     string
	fqdns   []string
	locked  lockedGitRoot
}

// noteKeptGitRoots records which git roots this expansion kept at the commit
// galaxy.lock pins, in place of what an earlier expansion recorded, so a
// failed resolve can release the ones holding a collection it names.
func (p *lockPreferences) noteKeptGitRoots(roots []collection, locked []lockedGitRoot, results [][]collection) {
	if p == nil {
		return
	}
	kept := make(map[string]keptGitRoot)
	for i, root := range roots {
		if locked[i].commit == "" || !expandedAt(results[i], locked[i].commit) {
			continue
		}
		key, keyOK := gitRootKey(root)
		loc, locErr := root.gitLocator()
		ref, refErr := gitsource.ParseRef(root.Ref)
		if !keyOK || locErr != nil || refErr != nil {
			continue
		}
		fqdns := make([]string, 0, len(results[i]))
		for _, col := range results[i] {
			fqdns = append(fqdns, col.fqdn())
		}
		kept[key] = keptGitRoot{display: helpers.URLForMessage(loc.URL), ref: ref.Name, fqdns: fqdns, locked: locked[i]}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.kept = kept
}

// expandedAt reports whether every collection of one root's expansion, which
// holds at least one, comes from commit.
func expandedAt(expanded []collection, commit string) bool {
	for _, col := range expanded {
		loc, err := col.gitLocator()
		if err != nil || loc.Commit != commit {
			return false
		}
	}
	return len(expanded) > 0
}

// releaseRuledOutGitRoots releases each kept git root holding a collection err
// names, warning once for each, and reports whether it released any: the
// caller then resolves again, those roots as if unlocked.
func (p *lockPreferences) releaseRuledOutGitRoots(deps collectionDeps, err error) bool {
	if p == nil {
		return false
	}
	named := ruledOutFQDNs(err)
	if len(named) == 0 {
		return false
	}
	p.mu.Lock()
	var released []keptGitRoot
	for _, key := range slices.Sorted(maps.Keys(p.kept)) {
		root := p.kept[key]
		if slices.ContainsFunc(root.fqdns, func(fqdn string) bool { return slices.Contains(named, fqdn) }) {
			p.released[key] = root.locked.commit
			delete(p.kept, key)
			released = append(released, root)
		}
	}
	p.mu.Unlock()
	for _, root := range released {
		p.warnRuledOut(deps.runtime.Output, root)
	}
	return len(released) > 0
}

// ruledOutFQDNs returns the collections a failed resolve names: the one two
// roots expanded into, else every package of a solver conflict's proof.
func ruledOutFQDNs(err error) []string {
	if dup, ok := errors.AsType[*duplicateRootError](err); ok {
		return []string{dup.fqdn}
	}
	if conflict, ok := errors.AsType[*solver.ConflictError](err); ok {
		return conflict.Packages()
	}
	return nil
}

// warnRuledOut warns once that the requirements rule out root's locked commit,
// naming the one collection the root owns, else its source and ref, keyed as
// warnLockedCommitGone keys it.
func (p *lockPreferences) warnRuledOut(out output.Printer, root keptGitRoot) {
	first := root.locked.names[0]
	if len(root.locked.names) == 1 {
		p.warnCollectionf(out, first, "Locked %s: the requirements rule out commit %s; resolving the collection anew",
			first, root.locked.commit)
		return
	}
	p.warnCollectionf(out, first, "Locked git source %s@%s: the requirements rule out commit %s; resolving its collections anew",
		root.display, root.ref, root.locked.commit)
}

// lockedFromOwned is the commit the entries a git root owns share. They carry
// its ref by construction; entries at two commits, which a hand edit or two
// overlapping roots of one ref leave, pin nothing.
func lockedFromOwned(owned []lockfile.Entry) lockedGitRoot {
	if len(owned) == 0 {
		return lockedGitRoot{}
	}
	names := make([]string, 0, len(owned))
	for _, e := range owned {
		if e.Commit != owned[0].Commit {
			return lockedGitRoot{}
		}
		names = append(names, e.Name)
	}
	return lockedGitRoot{commit: owned[0].Commit, names: names}
}

// urlEntry returns the url entry galaxy.lock locks from rawURL when it pins
// what the root asks for, by the rule verifyURLRootAgainstLockfile applies:
// a version the root asserts ("" for none) must be the locked one.
func (p *lockPreferences) urlEntry(rawURL, requested string) (lockfile.Entry, bool) {
	if p == nil {
		return lockfile.Entry{}, false
	}
	entry, ok := p.urls[rawURL]
	if !ok || (requested != "" && requested != entry.Version) {
		return lockfile.Entry{}, false
	}
	return entry, true
}

// warnCollectionf is warnOncef for the collection entry named name.
func (p *lockPreferences) warnCollectionf(out output.Printer, name, format string, args ...any) {
	p.warnOncef(out, lockEntryRef{name: name, kind: lockedCollection}, format, args...)
}

// role returns the entry galaxy.lock holds under req's install name when it
// pins what req asks for, by the rule --frozen accepts a root by; a git role
// asked at a commit is never looked up, since that ref is its own pin.
func (p *lockPreferences) role(req requirements.RoleRequirement) (lockfile.RoleEntry, bool) {
	if p == nil || verifyRoleRootAgainstLockfile(req, p.roles) != nil {
		return lockfile.RoleEntry{}, false
	}
	if req.IsGit() && gitsource.IsCommitHash(req.Version) {
		return lockfile.RoleEntry{}, false
	}
	return p.roles[req.Name], true
}

// warnRolef is warnOncef for the role entry installed as name.
func (p *lockPreferences) warnRolef(out output.Printer, name, format string, args ...any) {
	p.warnOncef(out, lockEntryRef{name: name, kind: lockedRole}, format, args...)
}

// galaxyVersion returns the version galaxy.lock pins fqdn's Galaxy entry to;
// a git or url entry of that name pins no Galaxy version.
func (p *lockPreferences) galaxyVersion(fqdn string) (string, bool) {
	if p == nil {
		return "", false
	}
	entry, ok := p.galaxy[fqdn]
	return entry.Version, ok
}

// recordUnpublished notes that fqdn's locked version is no longer published.
// It prints nothing: the solver may ask about a package its result leaves
// out, so warnUnpublished decides what to report once the result is known.
func (p *lockPreferences) recordUnpublished(fqdn string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unpublished[fqdn] = struct{}{}
}

// isUnpublished reports whether recordUnpublished noted fqdn this run.
func (p *lockPreferences) isUnpublished(fqdn string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.unpublished[fqdn]
	return ok
}

// agreesWith reports whether resolved holds each Galaxy collection galaxy.lock
// pins at its locked version, or at another its root's constraint asks for. A
// replay that moved a pin loses to the file, as from install --refresh.
func (p *lockPreferences) agreesWith(roots []collection, resolved map[string]collection) bool {
	if p == nil {
		return true
	}
	for fqdn, col := range resolved {
		if col.isGit() || col.isURL() {
			continue
		}
		if locked, ok := p.galaxyVersion(fqdn); ok && locked != col.Version && !rootExcludes(roots, fqdn, locked) {
			return false
		}
	}
	return true
}

// rootExcludes reports whether the Galaxy root asking for fqdn excludes
// version by its constraint, else its version, as --frozen judges a root's
// entry; a pin it excludes is moved on purpose, so a replay may move it.
func rootExcludes(roots []collection, fqdn, version string) bool {
	for _, root := range roots {
		if root.isGit() || root.isURL() || root.fqdn() != fqdn {
			continue
		}
		constraint := root.Constraint
		if constraint == "" {
			constraint = root.Version
		}
		ok, err := constraintSatisfied(version, constraint)
		return err == nil && !ok
	}
	return false
}

// keptVersionGone reports whether err is buildLockfile finding that fqdn's
// server no longer serves the version galaxy.lock pins and resolved kept, as
// a replay agreeing with the file does once that version's metadata is gone.
func (p *lockPreferences) keptVersionGone(resolved map[string]collection, err error) bool {
	entryErr, ok := errors.AsType[*galaxyEntryError](err)
	if p == nil || !ok || !errors.Is(err, helpers.ErrNoSemverCandidates) || !isNotFoundStatus(err) {
		return false
	}
	locked, ok := p.galaxyVersion(entryErr.fqdn)
	return ok && locked == resolved[entryErr.fqdn].Version
}

// warnUnpublished warns once per collection in resolved whose locked version
// was recorded unpublished and which resolved to another version. Only Confirm
// records, for a version the constraints allow, so an excluded pin draws none.
func (p *lockPreferences) warnUnpublished(out output.Printer, resolved map[string]collection) {
	if p == nil {
		return
	}
	for _, fqdn := range slices.Sorted(maps.Keys(resolved)) {
		col := resolved[fqdn]
		locked, ok := p.galaxyVersion(fqdn)
		if !ok || locked == col.Version || !p.isUnpublished(fqdn) {
			continue
		}
		p.warnCollectionf(out, fqdn, "Locked %s %s is no longer published; resolved %s instead",
			fqdn, helpers.ValueForMessage(locked), helpers.ValueForMessage(col.Version))
	}
}

// warnOncef prints a warning about ref unless one was already printed this
// run, since warnings may come from concurrent expansions and repeated solves.
// A nil receiver keeps no set, so it prints every call.
func (p *lockPreferences) warnOncef(out output.Printer, ref lockEntryRef, format string, args ...any) {
	if p == nil {
		out.Warnf(format, args...)
		return
	}
	p.mu.Lock()
	_, seen := p.warned[ref]
	p.warned[ref] = struct{}{}
	p.mu.Unlock()
	if !seen {
		out.Warnf(format, args...)
	}
}
