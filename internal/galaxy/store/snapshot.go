// Package store owns the persisted cache state (Store, guarded by its own
// RWMutex) and its local plumbing: the Bolt snapshot, project registry,
// instance lock and cache sweeps. Both backends persist through snapshotData.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
)

// SnapshotMeta holds metadata about the cached snapshot. LastSnapshot says
// only that a snapshot exists; ContentRecorded says a save carried on-disk
// content records, and is what a destructive pass consults.
type SnapshotMeta struct {
	LastSnapshot     time.Time `json:"last_snapshot"`
	ContentRecorded  time.Time `json:"content_recorded"`
	RequirementsHash string    `json:"requirements_hash"`
	Server           string    `json:"server"`
	SchemaVersion    int       `json:"schema_version"`
}

// APICacheEntry stores a cached API response and validation data.
type APICacheEntry struct {
	FetchedAt    time.Time     `json:"fetched_at"`
	URL          string        `json:"url"`
	ETag         string        `json:"etag"`
	LastModified string        `json:"last_modified"`
	Body         []byte        `json:"body"`
	TTL          time.Duration `json:"ttl"`
}

// VersionsEntry stores a cached versions list with the time it was written,
// so age-based eviction can prune it from the persisted snapshot.
type VersionsEntry struct {
	FetchedAt time.Time `json:"fetched_at"`
	List      []string  `json:"list"`
}

// DepsCacheEntry stores cached dependency constraints with the time they were
// written, so age-based eviction can prune them from the persisted snapshot.
type DepsCacheEntry struct {
	FetchedAt time.Time         `json:"fetched_at"`
	Deps      map[string]string `json:"deps"`
}

// InstalledEntry records an installed collection entry.
type InstalledEntry struct {
	InstallPath    string    `json:"install_path"`
	Source         string    `json:"source"`
	ArtifactSHA256 string    `json:"artifact_sha256"`
	InstalledAt    time.Time `json:"installed_at"`
	Deps           []string  `json:"deps"`
}

// WarmedEntry records that warm materialized an artifact into the extracted
// store, which is cleanup's only sign that a warm-only machine still wants it.
// Its key is not server-scoped, so a cross-server collision keeps the last sha.
type WarmedEntry struct {
	WarmedAt       time.Time `json:"warmed_at"`
	ArtifactSHA256 string    `json:"artifact_sha256"`
}

// GitPinCollection is one collection a pinned git commit carried: identity,
// source subdir and raw galaxy.yml dependencies, enough for the solver to
// answer for it without touching the remote again.
type GitPinCollection struct {
	Dependencies map[string]string `json:"dependencies,omitempty"`
	Namespace    string            `json:"namespace"`
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Subdir       string            `json:"subdir,omitempty"`
}

// GitPinEntry records the commit a (url, ref, subdir) git requirement resolved
// to and the collections at it, keyed by gitsource.PinKey. It has no retention
// window: an unmoved ref is the same answer later; --refresh re-resolves it.
type GitPinEntry struct {
	FetchedAt   time.Time          `json:"fetched_at"`
	Commit      string             `json:"commit"`
	Collections []GitPinCollection `json:"collections"`
}

// InstalledRoleEntry records one installed role, keyed by install name (its
// directory under the roles path). Source is the locator its artifact was
// built from, the one string artifact key, record and lockfile all key on.
type InstalledRoleEntry struct {
	InstalledAt    time.Time `json:"installed_at"`
	InstallPath    string    `json:"install_path"`
	Source         string    `json:"source"`
	ArtifactSHA256 string    `json:"artifact_sha256"`
	Version        string    `json:"version"`
	GalaxyName     string    `json:"galaxy_name,omitempty"`
	Deps           []string  `json:"deps,omitempty"`
}

// RolePinDep is one dependency a pinned role's meta declared, as written.
type RolePinDep struct {
	Src     string `json:"src,omitempty"`
	Scm     string `json:"scm,omitempty"`
	Version string `json:"version,omitempty"`
	Name    string `json:"name,omitempty"`
}

// RolePinEntry records what one role requirement line resolved to: a git or
// Galaxy pin names Repository and Commit, a url pin URL and SHA256. Like
// GitPinEntry it never expires by the clock.
type RolePinEntry struct {
	FetchedAt      time.Time `json:"fetched_at"`
	Repository     string    `json:"repository"`
	Commit         string    `json:"commit"`
	Version        string    `json:"version"`
	GalaxySHA      string    `json:"galaxy_sha,omitempty"`
	GalaxyRoleName string    `json:"galaxy_role_name,omitempty"`
	// Server is the Galaxy server whose v1 API answered for a Galaxy role's
	// pin, "" for a git role's: the provenance the lockfile records and the
	// server outdated asks again.
	Server string `json:"server,omitempty"`
	// Ref is the qualified ref a Galaxy role's pin chose (refs/tags/<tag> or
	// refs/heads/<branch>), "" for a git role's pin, whose ref is its key.
	Ref string `json:"ref,omitempty"`
	// URL is the tarball a url role's pin was fetched from and SHA256 the digest
	// of the bytes it served: a url pin's identity, "" on git and Galaxy pins.
	URL    string       `json:"url,omitempty"`
	SHA256 string       `json:"sha256,omitempty"`
	Deps   []RolePinDep `json:"deps,omitempty"`
}

// URLPinEntry records what a url collection requirement resolved to: the
// sha256 of the served bytes and its MANIFEST.json identity and dependencies.
// Keyed by urlsource.PinKey; like GitPinEntry it never expires by the clock.
type URLPinEntry struct {
	FetchedAt    time.Time         `json:"fetched_at"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
	SHA256       string            `json:"sha256"`
	Namespace    string            `json:"namespace"`
	Name         string            `json:"name"`
	Version      string            `json:"version"`
}

// Store holds cached state for collections, roles and metadata.
type Store struct {
	APICache       map[string]APICacheEntry      `json:"api_cache"`
	DepsCache      map[string]DepsCacheEntry     `json:"deps_cache"`
	Installed      map[string]InstalledEntry     `json:"installed"`
	Graph          map[string][]string           `json:"graph"`
	Requirements   map[string]RequirementSpec    `json:"requirements"`
	Resolved       map[string]ResolvedEntry      `json:"resolved"`
	Versions       map[string]VersionsEntry      `json:"versions_cache"`
	Warmed         map[string]WarmedEntry        `json:"warmed"`
	GitPins        map[string]GitPinEntry        `json:"git_pins"`
	InstalledRoles map[string]InstalledRoleEntry `json:"installed_roles"`
	RolePins       map[string]RolePinEntry       `json:"role_pins"`
	URLPins        map[string]URLPinEntry        `json:"url_pins"`
	Meta           SnapshotMeta                  `json:"meta"`
	mu             sync.RWMutex                  `json:"-"`
	// dirty records whether this process has written something into the
	// store since it was loaded (or since New built a fresh one); see Dirty
	// for the full contract.
	dirty bool `json:"-"`
}

// New creates an initialized Store with empty maps. UnmarshalJSON is the
// method that restores this same all-maps-non-nil invariant after a decode,
// since a decode can nil a map in a way this constructor never does.
func New() *Store {
	return &Store{
		Meta: SnapshotMeta{
			SchemaVersion: helpers.StoreSnapshotSchemaVersion,
		},
		APICache:       make(map[string]APICacheEntry),
		DepsCache:      make(map[string]DepsCacheEntry),
		Installed:      make(map[string]InstalledEntry),
		Graph:          make(map[string][]string),
		Requirements:   make(map[string]RequirementSpec),
		Resolved:       make(map[string]ResolvedEntry),
		Versions:       make(map[string]VersionsEntry),
		Warmed:         make(map[string]WarmedEntry),
		GitPins:        make(map[string]GitPinEntry),
		InstalledRoles: make(map[string]InstalledRoleEntry),
		RolePins:       make(map[string]RolePinEntry),
		URLPins:        make(map[string]URLPinEntry),
	}
}

// UnmarshalJSON decodes a Store and re-allocates any map an explicit JSON null
// nilled: a write into a nil map panics a worker goroutine, killing the run
// before it releases the S3 lock. Guarding here covers every decode path.
func (s *Store) UnmarshalJSON(data []byte) error {
	// storeJSON strips the json.Unmarshaler method set, so the decode below
	// cannot recurse back into this method. The conversion is on the pointer,
	// so the RWMutex is never copied.
	type storeJSON Store

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := json.Unmarshal(data, (*storeJSON)(s)); err != nil {
		return err
	}
	s.ensureMaps()
	return nil
}

// ResolvedEntry stores a resolved collection version and source. Ref is set
// only for a git source, so a lockfile built from a replayed resolution
// records the same ref a fresh one would.
type ResolvedEntry struct {
	Version string `json:"version"`
	Source  string `json:"source"`
	Ref     string `json:"ref,omitempty"`
}

// RequirementSpec captures a requirement constraint and metadata.
type RequirementSpec struct {
	Constraint string   `json:"constraint"`
	Source     string   `json:"source"`
	Type       string   `json:"type,omitempty"`
	Signatures []string `json:"signatures,omitempty"`
}

// SetInstalled records an installed collection entry. The entry's Deps
// slice is cloned before storing, so a later caller mutation of its
// backing array cannot corrupt the stored snapshot state.
func (s *Store) SetInstalled(key string, entry InstalledEntry) {
	if s == nil {
		return
	}
	entry.Deps = slices.Clone(entry.Deps)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Installed[key] = entry
	s.dirty = true
}

// DeleteInstalled removes an installed entry by key.
func (s *Store) DeleteInstalled(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Installed, key)
	s.dirty = true
}

// GetInstalled returns an installed entry by key. Deps is cloned before
// returning, so a caller mutation of the returned slice cannot corrupt the
// stored snapshot state.
func (s *Store) GetInstalled(key string) (InstalledEntry, bool) {
	if s == nil {
		return InstalledEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.Installed[key]
	entry.Deps = slices.Clone(entry.Deps)
	return entry, ok
}

// InstalledArtifactSHAByKey maps each installed collection key to its non-empty
// ArtifactSHA256: the collections' half of the extracted keep set, which unlike
// a workspace scan covers projects whose workspace is currently absent.
func (s *Store) InstalledArtifactSHAByKey() map[string]string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.Installed))
	for key, entry := range s.Installed {
		if entry.ArtifactSHA256 == "" {
			continue
		}
		out[key] = entry.ArtifactSHA256
	}
	return out
}

// SetWarmed records that key's artifact (artifactSHA) is materialized in the
// extracted store, stamped now. An empty key or sha is ignored, so it never
// persists an entry that protects nothing.
func (s *Store) SetWarmed(key, artifactSHA string) {
	if s == nil || key == "" || artifactSHA == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Warmed[key] = WarmedEntry{WarmedAt: time.Now().UTC(), ArtifactSHA256: artifactSHA}
	s.dirty = true
}

// GetGitPin returns the pin recorded under key, deep-copied so a caller can
// neither observe nor cause a later mutation.
func (s *Store) GetGitPin(key string) (GitPinEntry, bool) {
	if s == nil {
		return GitPinEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.GitPins[key]
	if !ok {
		return GitPinEntry{}, false
	}
	return cloneGitPin(entry), true
}

// SetGitPin records a deep copy of entry under key, stamping FetchedAt now. An
// empty key or commit is ignored: such a pin would only replay into a failure.
func (s *Store) SetGitPin(key string, entry GitPinEntry) {
	if s == nil || key == "" || entry.Commit == "" {
		return
	}
	clone := cloneGitPin(entry)
	clone.FetchedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.GitPins[key] = clone
	s.dirty = true
}

// cloneGitPin copies a pin and every reference-type field inside it.
func cloneGitPin(entry GitPinEntry) GitPinEntry {
	clone := entry
	clone.Collections = make([]GitPinCollection, len(entry.Collections))
	for i, c := range entry.Collections {
		cc := c
		if c.Dependencies != nil {
			cc.Dependencies = make(map[string]string, len(c.Dependencies))
			maps.Copy(cc.Dependencies, c.Dependencies)
		}
		clone.Collections[i] = cc
	}
	return clone
}

// SetInstalledRole records an installed role under its install name. The
// entry's Deps slice is cloned before storing, so a later caller mutation of
// its backing array cannot corrupt the stored snapshot state.
func (s *Store) SetInstalledRole(name string, entry InstalledRoleEntry) {
	if s == nil {
		return
	}
	entry.Deps = slices.Clone(entry.Deps)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.InstalledRoles[name] = entry
	s.dirty = true
}

// GetInstalledRole returns the installed role recorded under name. Deps is
// cloned before returning, so a caller mutation of the returned slice cannot
// corrupt the stored snapshot state.
func (s *Store) GetInstalledRole(name string) (InstalledRoleEntry, bool) {
	if s == nil {
		return InstalledRoleEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.InstalledRoles[name]
	entry.Deps = slices.Clone(entry.Deps)
	return entry, ok
}

// DeleteInstalledRole removes the installed role recorded under name.
func (s *Store) DeleteInstalledRole(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.InstalledRoles, name)
	s.dirty = true
}

// InstalledRolesSnapshot returns a deep copy of the installed roles, Deps
// included, so the caller cannot corrupt the store. Cleanup walks it to decide
// which role directories are still owned.
func (s *Store) InstalledRolesSnapshot() map[string]InstalledRoleEntry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	clone := make(map[string]InstalledRoleEntry, len(s.InstalledRoles))
	copyInstalledRoles(clone, s.InstalledRoles)
	return clone
}

// InstalledRoleArtifactSHAs maps each installed role name to its non-empty
// ArtifactSHA256: the roles' half of the extracted keep set, which like
// InstalledArtifactSHAByKey outlives an absent roles path.
func (s *Store) InstalledRoleArtifactSHAs() map[string]string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.InstalledRoles))
	for name, entry := range s.InstalledRoles {
		if entry.ArtifactSHA256 == "" {
			continue
		}
		out[name] = entry.ArtifactSHA256
	}
	return out
}

// GetRolePin returns the pin recorded under key, deep-copied so a caller can
// neither observe nor cause a later mutation.
func (s *Store) GetRolePin(key string) (RolePinEntry, bool) {
	if s == nil {
		return RolePinEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.RolePins[key]
	if !ok {
		return RolePinEntry{}, false
	}
	return cloneRolePin(entry), true
}

// SetRolePin records a deep copy of entry under key, stamping FetchedAt now if
// zero. An empty key, or an entry with neither Commit (git, Galaxy) nor SHA256
// (url), is ignored: such a pin would only replay into a failure.
func (s *Store) SetRolePin(key string, entry RolePinEntry) {
	if s == nil || key == "" || (entry.Commit == "" && entry.SHA256 == "") {
		return
	}
	clone := cloneRolePin(entry)
	if clone.FetchedAt.IsZero() {
		clone.FetchedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.RolePins[key] = clone
	s.dirty = true
}

// DeleteRolePin removes the pin recorded under key.
func (s *Store) DeleteRolePin(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.RolePins, key)
	s.dirty = true
}

// cloneRolePin copies a pin and the one reference-type field inside it.
func cloneRolePin(entry RolePinEntry) RolePinEntry {
	clone := entry
	clone.Deps = slices.Clone(entry.Deps)
	return clone
}

// GetURLPin returns the pin recorded under key, deep-copied so a caller can
// neither observe nor cause a later mutation.
func (s *Store) GetURLPin(key string) (URLPinEntry, bool) {
	if s == nil {
		return URLPinEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.URLPins[key]
	if !ok {
		return URLPinEntry{}, false
	}
	return cloneURLPin(entry), true
}

// SetURLPin records a deep copy of entry under key, stamping FetchedAt now. An
// empty key or sha256 is ignored: such a pin would only replay into a failure.
func (s *Store) SetURLPin(key string, entry URLPinEntry) {
	if s == nil || key == "" || entry.SHA256 == "" {
		return
	}
	clone := cloneURLPin(entry)
	clone.FetchedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.URLPins[key] = clone
	s.dirty = true
}

// cloneURLPin copies a pin and the one reference-type field inside it.
func cloneURLPin(entry URLPinEntry) URLPinEntry {
	clone := entry
	if entry.Dependencies != nil {
		clone.Dependencies = make(map[string]string, len(entry.Dependencies))
		maps.Copy(clone.Dependencies, entry.Dependencies)
	}
	return clone
}

// WarmedArtifactSHAByKey maps each warmed key to its non-empty artifact sha,
// omitting entries outside WarmedEntryMaxAge. Filtering here, as snapshotData
// does on save, keeps cleanup's keep set in step with the snapshot it writes.
func (s *Store) WarmedArtifactSHAByKey() map[string]string {
	if s == nil {
		return nil
	}
	window := newRetentionWindow(time.Now().UTC(), helpers.WarmedEntryMaxAge)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.Warmed))
	for key, entry := range s.Warmed {
		if entry.ArtifactSHA256 == "" || window.isStale(entry.WarmedAt) {
			continue
		}
		out[key] = entry.ArtifactSHA256
	}
	return out
}

// GetDepsCache returns cached dependency constraints for a key. This is a
// pure read under RLock: it does not bump the entry's FetchedAt, so a hit
// here never requires upgrading to the write lock.
func (s *Store) GetDepsCache(key string) (map[string]string, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.DepsCache[key]
	if !ok {
		return nil, false
	}
	clone := make(map[string]string, len(entry.Deps))
	maps.Copy(clone, entry.Deps)
	return clone, true
}

// SetDepsCache stores dependency constraints for key, stamping FetchedAt with
// the write time. GetDepsCache never bumps it, so a referenced entry still
// ages out CacheEntryMaxAge after its write: a cheap refetch, no write lock.
func (s *Store) SetDepsCache(key string, deps map[string]string) {
	if s == nil {
		return
	}
	clone := make(map[string]string, len(deps))
	maps.Copy(clone, deps)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.DepsCache[key] = DepsCacheEntry{FetchedAt: time.Now().UTC(), Deps: clone}
	s.dirty = true
}

// DeleteDepsCache removes cached dependency data for a key.
func (s *Store) DeleteDepsCache(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.DepsCache, key)
	s.dirty = true
}

// GetAPICache returns a cached API entry by key. Body shares its backing array
// with the store and is not cloned on this hot path: callers must not mutate it.
func (s *Store) GetAPICache(key string) (APICacheEntry, bool) {
	if s == nil {
		return APICacheEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.APICache[key]
	return entry, ok
}

// SetAPICache stores a cached API entry. The entry's Body is cloned before
// storing, so a later caller mutation (or reuse) of its backing buffer
// cannot corrupt the stored snapshot state.
func (s *Store) SetAPICache(key string, entry APICacheEntry) {
	if s == nil {
		return
	}
	entry.Body = slices.Clone(entry.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.APICache[key] = entry
	s.dirty = true
}

// ClearCaches clears every bucket that is an answer from a remote (API, deps,
// versions, git, role and url pins). Installed, InstalledRoles and Warmed are
// records of content on disk and survive.
func (s *Store) ClearCaches() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.APICache = make(map[string]APICacheEntry)
	s.DepsCache = make(map[string]DepsCacheEntry)
	s.Versions = make(map[string]VersionsEntry)
	s.GitPins = make(map[string]GitPinEntry)
	s.RolePins = make(map[string]RolePinEntry)
	s.URLPins = make(map[string]URLPinEntry)
	s.dirty = true
}

// GetVersionsCache returns cached versions for a key. This is a pure read
// under RLock: it does not bump the entry's FetchedAt, so a hit here never
// requires upgrading to the write lock.
func (s *Store) GetVersionsCache(key string) ([]string, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.Versions[key]
	if !ok {
		return nil, false
	}
	clone := make([]string, len(entry.List))
	copy(clone, entry.List)
	return clone, true
}

// SetVersionsCache stores versions for key, stamping FetchedAt with the write
// time. GetVersionsCache never bumps it, so a referenced entry still ages out
// CacheEntryMaxAge after its write: a cheap refetch, no write lock.
func (s *Store) SetVersionsCache(key string, versions []string) {
	if s == nil {
		return
	}
	clone := make([]string, len(versions))
	copy(clone, versions)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Versions[key] = VersionsEntry{FetchedAt: time.Now().UTC(), List: clone}
	s.dirty = true
}

// SetResolvedAll replaces the resolved entries map.
func (s *Store) SetResolvedAll(resolved map[string]ResolvedEntry) {
	if s == nil {
		return
	}
	clone := make(map[string]ResolvedEntry, len(resolved))
	maps.Copy(clone, resolved)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Resolved = clone
	s.dirty = true
}

// ResolvedSnapshot returns a copy of resolved entries.
func (s *Store) ResolvedSnapshot() map[string]ResolvedEntry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	clone := make(map[string]ResolvedEntry, len(s.Resolved))
	maps.Copy(clone, s.Resolved)
	return clone
}

// SetGraph records dependencies for a collection key. deps is cloned before
// storing, so a later caller mutation of its backing array cannot corrupt
// the stored snapshot state.
func (s *Store) SetGraph(key string, deps []string) {
	if s == nil {
		return
	}
	clone := slices.Clone(deps)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Graph[key] = clone
	s.dirty = true
}

// DeleteGraph removes dependency data for a key.
func (s *Store) DeleteGraph(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Graph, key)
	s.dirty = true
}

// SetGraphSnapshot replaces the dependency graph.
func (s *Store) SetGraphSnapshot(graph map[string][]string) {
	if s == nil {
		return
	}
	clone := make(map[string][]string, len(graph))
	for key, deps := range graph {
		out := make([]string, len(deps))
		copy(out, deps)
		clone[key] = out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Graph = clone
	s.dirty = true
}

// GraphSnapshot returns a copy of the dependency graph.
func (s *Store) GraphSnapshot() map[string][]string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	clone := make(map[string][]string, len(s.Graph))
	for key, deps := range s.Graph {
		out := make([]string, len(deps))
		copy(out, deps)
		clone[key] = out
	}
	return clone
}

// SetRequirements replaces the requirement specs with a copy whose Signatures
// slices are cloned too. It never writes into an existing array in place,
// which signaturesWithoutQuery's aliasing relies on.
func (s *Store) SetRequirements(spec map[string]RequirementSpec) {
	if s == nil {
		return
	}
	clone := make(map[string]RequirementSpec, len(spec))
	for key, value := range spec {
		value.Signatures = slices.Clone(value.Signatures)
		clone[key] = value
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requirements = clone
	s.dirty = true
}

// RequirementsSnapshot returns a deep copy of the requirement specs, each
// Signatures slice included, so the caller cannot corrupt the store.
func (s *Store) RequirementsSnapshot() map[string]RequirementSpec {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	clone := make(map[string]RequirementSpec, len(s.Requirements))
	for key, value := range s.Requirements {
		value.Signatures = slices.Clone(value.Signatures)
		clone[key] = value
	}
	return clone
}

// MetaSnapshot returns the current snapshot metadata.
func (s *Store) MetaSnapshot() SnapshotMeta {
	if s == nil {
		return SnapshotMeta{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Meta
}

// WasPersisted reports whether this store was loaded from a persisted snapshot
// (LastSnapshot is non-zero). A never-persisted store is ignorance, not
// evidence: its empty maps must not be read as "nothing installed".
func (s *Store) WasPersisted() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.Meta.LastSnapshot.IsZero()
}

// HasRecordedContent reports whether any save ever carried installed, warmed
// or installed-role records. A destructive pass consults it, not WasPersisted,
// because `lock` saves a snapshot whose empty content maps prove nothing.
func (s *Store) HasRecordedContent() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.Meta.ContentRecorded.IsZero()
}

// SetMetaRequirements stores the requirements hash and server.
func (s *Store) SetMetaRequirements(hash, server string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Meta.RequirementsHash = hash
	s.Meta.Server = server
	s.dirty = true
}

// Dirty reports whether this process has called a mutator since load or New.
// Every mutator sets it unconditionally and nothing clears it: a false positive
// costs one save, a false negative loses state. Loads never set it.
func (s *Store) Dirty() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirty
}

// stampSaveMeta stamps SchemaVersion and LastSnapshot for both backends' saves,
// and ContentRecorded only when hasContent. hasContent comes from the live store
// before age eviction, so a record expiring on this save still counts.
func stampSaveMeta(data *snapshotData, hasContent bool) {
	data.Meta.SchemaVersion = helpers.StoreSnapshotSchemaVersion
	data.Meta.LastSnapshot = time.Now().UTC()
	if hasContent {
		data.Meta.ContentRecorded = data.Meta.LastSnapshot
	}
}

// snapshotData is a serialized view of Store contents.
type snapshotData struct {
	APICache       map[string]APICacheEntry
	DepsCache      map[string]DepsCacheEntry
	Installed      map[string]InstalledEntry
	Graph          map[string][]string
	Requirements   map[string]RequirementSpec
	Resolved       map[string]ResolvedEntry
	Versions       map[string]VersionsEntry
	Warmed         map[string]WarmedEntry
	GitPins        map[string]GitPinEntry
	InstalledRoles map[string]InstalledRoleEntry
	RolePins       map[string]RolePinEntry
	URLPins        map[string]URLPinEntry
	Meta           SnapshotMeta
}

// MarshalSnapshot returns the schema-stamped JSON snapshot for a remote
// backend, built from snapshotData's deep copy so a concurrent writer cannot
// tear the payload. It stamps meta exactly as Save does.
func (s *Store) MarshalSnapshot() ([]byte, error) {
	data := s.snapshotData()
	stampSaveMeta(&data, s.hasContentEntries())

	// snapshotData has no json tags of its own; assign its fields onto a
	// throwaway Store so the encoding reuses Store's existing json tags and
	// the wire shape stays byte-identical to marshaling a *Store directly.
	snapshot := &Store{
		APICache:       data.APICache,
		DepsCache:      data.DepsCache,
		Installed:      data.Installed,
		Graph:          data.Graph,
		Requirements:   data.Requirements,
		Resolved:       data.Resolved,
		Versions:       data.Versions,
		Warmed:         data.Warmed,
		GitPins:        data.GitPins,
		InstalledRoles: data.InstalledRoles,
		RolePins:       data.RolePins,
		URLPins:        data.URLPins,
		Meta:           data.Meta,
	}
	return json.Marshal(snapshot)
}

// hasContentEntries reports whether the live store holds an installed
// collection, warmed entry or installed role; see stampSaveMeta for why a save
// reads it from the store rather than from its evicted payload.
func (s *Store) hasContentEntries() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Installed) > 0 || len(s.Warmed) > 0 || len(s.InstalledRoles) > 0
}

// ensureMaps re-allocates every map a decode may have nilled. The caller must
// hold the write lock. Every map New() initializes is listed here; a new map
// on Store must be added to both.
func (s *Store) ensureMaps() {
	s.APICache = ensureMap(s.APICache)
	s.DepsCache = ensureMap(s.DepsCache)
	s.Installed = ensureMap(s.Installed)
	s.Graph = ensureMap(s.Graph)
	s.Requirements = ensureMap(s.Requirements)
	s.Resolved = ensureMap(s.Resolved)
	s.Versions = ensureMap(s.Versions)
	s.Warmed = ensureMap(s.Warmed)
	s.GitPins = ensureMap(s.GitPins)
	s.InstalledRoles = ensureMap(s.InstalledRoles)
	s.RolePins = ensureMap(s.RolePins)
	s.URLPins = ensureMap(s.URLPins)
}

// ensureMap returns m when it is non-nil and a fresh empty map otherwise.
func ensureMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return make(map[K]V)
	}
	return m
}

// retentionWindow is the closed interval [oldest, newest] of write stamps a
// pass accepts. Both bounds come from one clock sample, so every entry in one
// save is judged against the same instant.
type retentionWindow struct {
	oldest time.Time
	newest time.Time
}

// newRetentionWindow builds the window [now-maxAge, now] for a single
// wall-clock sample now, shared by every entry classified against the
// returned window.
func newRetentionWindow(now time.Time, maxAge time.Duration) retentionWindow {
	return retentionWindow{oldest: now.Add(-maxAge), newest: now}
}

// isStale reports whether stampedAt falls outside the inclusive window. A
// future stamp is stale, neither clamped (a fresh lease) nor fatal, and there
// is deliberately no clock-skew tolerance: a skewed entry is just refetched.
func (w retentionWindow) isStale(stampedAt time.Time) bool {
	return stampedAt.Before(w.oldest) || stampedAt.After(w.newest)
}

// snapshotData deep-copies the store for Save and MarshalSnapshot, dropping
// API, deps and versions entries outside CacheEntryMaxAge and warmed ones
// outside WarmedEntryMaxAge, and cutting signature queries; live maps are kept.
func (s *Store) snapshotData() snapshotData {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now().UTC()
	window := newRetentionWindow(now, helpers.CacheEntryMaxAge)
	warmedWindow := newRetentionWindow(now, helpers.WarmedEntryMaxAge)

	data := snapshotData{
		Meta:           s.Meta,
		APICache:       make(map[string]APICacheEntry, len(s.APICache)),
		DepsCache:      make(map[string]DepsCacheEntry, len(s.DepsCache)),
		Installed:      make(map[string]InstalledEntry, len(s.Installed)),
		Graph:          make(map[string][]string, len(s.Graph)),
		Requirements:   make(map[string]RequirementSpec, len(s.Requirements)),
		Resolved:       make(map[string]ResolvedEntry, len(s.Resolved)),
		Versions:       make(map[string]VersionsEntry, len(s.Versions)),
		Warmed:         make(map[string]WarmedEntry, len(s.Warmed)),
		GitPins:        make(map[string]GitPinEntry, len(s.GitPins)),
		InstalledRoles: make(map[string]InstalledRoleEntry, len(s.InstalledRoles)),
		RolePins:       make(map[string]RolePinEntry, len(s.RolePins)),
		URLPins:        make(map[string]URLPinEntry, len(s.URLPins)),
	}

	for key, entry := range s.APICache {
		if window.isStale(entry.FetchedAt) {
			continue
		}
		data.APICache[key] = entry
	}
	for key, entry := range s.DepsCache {
		if window.isStale(entry.FetchedAt) {
			continue
		}
		clone := make(map[string]string, len(entry.Deps))
		maps.Copy(clone, entry.Deps)
		data.DepsCache[key] = DepsCacheEntry{FetchedAt: entry.FetchedAt, Deps: clone}
	}
	// Installed is copied whole with no retention window on purpose: an install
	// that finds its collection present never re-records it, so a window would
	// age out a live project's entries and its extracted trees with them.
	maps.Copy(data.Installed, s.Installed)
	for key, deps := range s.Graph {
		clone := make([]string, len(deps))
		copy(clone, deps)
		data.Graph[key] = clone
	}
	copyRequirementsCutQuery(data.Requirements, s.Requirements)
	maps.Copy(data.Resolved, s.Resolved)
	for key, entry := range s.Versions {
		if window.isStale(entry.FetchedAt) {
			continue
		}
		clone := make([]string, len(entry.List))
		copy(clone, entry.List)
		data.Versions[key] = VersionsEntry{FetchedAt: entry.FetchedAt, List: clone}
	}
	copyFreshWarmed(data.Warmed, s.Warmed, warmedWindow)
	// Git, role and url pins are copied whole: see GitPinEntry for why a pin
	// carries no retention window. Installed roles are copied whole for the
	// reasons Installed is, just above.
	copyGitPins(data.GitPins, s.GitPins)
	copyInstalledRoles(data.InstalledRoles, s.InstalledRoles)
	copyRolePins(data.RolePins, s.RolePins)
	copyURLPins(data.URLPins, s.URLPins)

	return data
}

// copyGitPins copies every entry from src into dst through cloneGitPin; the
// caller holds the store's read lock.
func copyGitPins(dst, src map[string]GitPinEntry) {
	for key, entry := range src {
		dst[key] = cloneGitPin(entry)
	}
}

// copyURLPins copies every entry from src into dst through cloneURLPin; the
// caller holds the store's read lock.
func copyURLPins(dst, src map[string]URLPinEntry) {
	for key, entry := range src {
		dst[key] = cloneURLPin(entry)
	}
}

// copyInstalledRoles copies every entry from src into dst, cloning each Deps
// slice; InstalledRolesSnapshot shares it so the two copies cannot drift. The
// caller holds the store's read lock.
func copyInstalledRoles(dst, src map[string]InstalledRoleEntry) {
	for name, entry := range src {
		entry.Deps = slices.Clone(entry.Deps)
		dst[name] = entry
	}
}

// copyRolePins copies every entry from src into dst through cloneRolePin; the
// caller holds the store's read lock.
func copyRolePins(dst, src map[string]RolePinEntry) {
	for key, entry := range src {
		dst[key] = cloneRolePin(entry)
	}
}

// copyRequirementsCutQuery copies src into dst with every Signatures query cut.
// It backstops normalizeSignatures: install --frozen never rebuilds the spec, so
// an uncut entry an older binary persisted would otherwise survive every save.
func copyRequirementsCutQuery(dst, src map[string]RequirementSpec) {
	for key, entry := range src {
		entry.Signatures = signaturesWithoutQuery(entry.Signatures)
		dst[key] = entry
	}
}

// signaturesWithoutQuery returns sources with every query cut, allocating only
// when some entry changes. The no-change path returns sources itself, safe
// because SetRequirements never writes a Signatures array in place.
func signaturesWithoutQuery(sources []string) []string {
	cut := -1
	for i, source := range sources {
		if helpers.WithoutQuery(source) != source {
			cut = i
			break
		}
	}
	if cut < 0 {
		return sources
	}

	out := make([]string, len(sources))
	copy(out, sources[:cut])
	for i := cut; i < len(sources); i++ {
		out[i] = helpers.WithoutQuery(sources[i])
	}
	return out
}

// copyFreshWarmed copies every entry from src into dst that falls inside
// window; the caller holds the store's read lock.
func copyFreshWarmed(dst, src map[string]WarmedEntry, window retentionWindow) {
	for key, entry := range src {
		if window.isStale(entry.WarmedAt) {
			continue
		}
		dst[key] = entry
	}
}

// Load reads cached state from the consolidated Bolt database. An older schema
// version yields a fresh empty Store (drop and rebuild); a newer one is an
// error, since this binary cannot safely interpret it.
func Load(dbs *DBs) (*Store, error) {
	store := New()
	if dbs == nil || dbs.db == nil {
		return store, nil
	}

	if err := dbs.db.View(func(tx *bolt.Tx) error {
		return loadMeta(tx, store)
	}); err != nil {
		return nil, err
	}

	if err := ValidateSchema(store.Meta.SchemaVersion); err != nil {
		if errors.Is(err, helpers.ErrOutdatedSchemaVersion) {
			return New(), nil
		}
		return nil, err
	}

	if err := dbs.db.View(func(tx *bolt.Tx) error {
		return runLoadSteps(tx, store)
	}); err != nil {
		return nil, err
	}
	return store, nil
}

// Save writes cached state to the consolidated Bolt database: meta and every
// data bucket in one Bolt transaction, so a mid-save failure leaves the last
// committed snapshot intact.
func Save(dbs *DBs, store *Store) error {
	if dbs == nil || dbs.db == nil {
		return helpers.ErrDbNil
	}
	if store == nil {
		return helpers.ErrStoreNil
	}

	data := store.snapshotData()
	stampSaveMeta(&data, store.hasContentEntries())

	return dbs.db.Update(func(tx *bolt.Tx) error {
		if err := saveMeta(tx, data.Meta); err != nil {
			return err
		}
		return runSaveSteps(tx, data)
	})
}

// ValidateSchema accepts only helpers.StoreSnapshotSchemaVersion: a newer
// version is unsupported by this build, an older one predates a breaking change
// and must be dropped and rebuilt rather than partially trusted.
func ValidateSchema(version int) error {
	switch {
	case version == helpers.StoreSnapshotSchemaVersion:
		return nil
	case version > helpers.StoreSnapshotSchemaVersion:
		return fmt.Errorf("%w: %d", helpers.ErrUnsupportedSchemaVersion, version)
	default:
		return fmt.Errorf("%w: %d", helpers.ErrOutdatedSchemaVersion, version)
	}
}

// runSteps runs each step in order and returns the first error, leaving the
// remaining steps unrun. Both bucket runners below share it, so the rule that
// a failing bucket aborts the rest is stated once rather than per runner.
func runSteps(steps []func() error) error {
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// jsonBucketIO binds one data bucket to its load and save directions at once,
// so a bucket cannot be listed for one direction and forgotten in the other.
type jsonBucketIO struct {
	load func(*bolt.Tx) error
	save func(*bolt.Tx) error
}

// bindJSONBucket pairs the named bucket with the store map it loads into and
// the snapshot map it saves from.
func bindJSONBucket[T any](name string, dst, src map[string]T) jsonBucketIO {
	return jsonBucketIO{
		load: func(tx *bolt.Tx) error { return loadJSONBucket(tx, name, dst) },
		save: func(tx *bolt.Tx) error { return saveJSONBucket(tx, name, src) },
	}
}

// jsonBuckets lists the data buckets in the one fixed order both runners use.
// A new bucket is appended, never inserted: save-side fault tests depend on
// the order (TestSaveRollsBackWholeTransactionOnMidSaveFailure).
func jsonBuckets(store *Store, data snapshotData) []jsonBucketIO {
	return []jsonBucketIO{
		bindJSONBucket(helpers.StoreBucketAPICache, store.APICache, data.APICache),
		bindJSONBucket(helpers.StoreBucketDepsCache, store.DepsCache, data.DepsCache),
		bindJSONBucket(helpers.StoreBucketInstalled, store.Installed, data.Installed),
		bindJSONBucket(helpers.StoreBucketGraph, store.Graph, data.Graph),
		bindJSONBucket(helpers.StoreBucketRequirements, store.Requirements, data.Requirements),
		bindJSONBucket(helpers.StoreBucketResolved, store.Resolved, data.Resolved),
		bindJSONBucket(helpers.StoreBucketVersions, store.Versions, data.Versions),
		bindJSONBucket(helpers.StoreBucketWarmed, store.Warmed, data.Warmed),
		bindJSONBucket(helpers.StoreBucketGitPins, store.GitPins, data.GitPins),
		bindJSONBucket(helpers.StoreBucketInstalledRoles, store.InstalledRoles, data.InstalledRoles),
		bindJSONBucket(helpers.StoreBucketRolePins, store.RolePins, data.RolePins),
		bindJSONBucket(helpers.StoreBucketURLPins, store.URLPins, data.URLPins),
	}
}

// runLoadSteps reads the twelve data buckets in the given transaction.
func runLoadSteps(tx *bolt.Tx, store *Store) error {
	buckets := jsonBuckets(store, snapshotData{})
	steps := make([]func() error, 0, len(buckets))
	for _, b := range buckets {
		steps = append(steps, func() error { return b.load(tx) })
	}
	return runSteps(steps)
}

// runSaveSteps writes the twelve data buckets in the given transaction, in the
// fixed order jsonBuckets states.
func runSaveSteps(tx *bolt.Tx, data snapshotData) error {
	buckets := jsonBuckets(&Store{}, data)
	steps := make([]func() error, 0, len(buckets))
	for _, b := range buckets {
		steps = append(steps, func() error { return b.save(tx) })
	}
	return runSteps(steps)
}

// loadMeta reads the meta bucket into store.Meta. A missing bucket (new file)
// keeps New()'s current version; a bucket without the schema key reads as 0,
// so a populated but unstamped database is dropped rather than trusted.
func loadMeta(tx *bolt.Tx, store *Store) error {
	metaBucket := tx.Bucket([]byte(helpers.StoreBucketMeta))
	if metaBucket == nil {
		return nil
	}
	if v := metaBucket.Get([]byte(helpers.StoreMetaSchemaVersion)); v != nil {
		version, err := strconv.Atoi(string(v))
		if err != nil {
			return fmt.Errorf("invalid schema version: %w", err)
		}
		store.Meta.SchemaVersion = version
	} else {
		store.Meta.SchemaVersion = 0
	}
	if v := metaBucket.Get([]byte(helpers.StoreMetaLastSnapshot)); v != nil {
		t, err := time.Parse(time.RFC3339Nano, string(v))
		if err != nil {
			return fmt.Errorf("invalid snapshot time: %w", err)
		}
		store.Meta.LastSnapshot = t
	}
	// An absent key reads as zero, "no content recorded": the conservative
	// answer, and what an older binary's snapshot yields, so a shared older
	// binary can only make a destructive pass do less.
	if v := metaBucket.Get([]byte(helpers.StoreMetaContentRecorded)); v != nil {
		t, err := time.Parse(time.RFC3339Nano, string(v))
		if err != nil {
			return fmt.Errorf("invalid content-recorded time: %w", err)
		}
		store.Meta.ContentRecorded = t
	}
	if v := metaBucket.Get([]byte(helpers.StoreMetaRequirementsHash)); v != nil {
		store.Meta.RequirementsHash = string(v)
	}
	if v := metaBucket.Get([]byte(helpers.StoreMetaServer)); v != nil {
		store.Meta.Server = string(v)
	}
	return nil
}

func saveMeta(tx *bolt.Tx, meta SnapshotMeta) error {
	metaBucket, err := ensureEmptyBucket(tx, helpers.StoreBucketMeta)
	if err != nil {
		return err
	}
	if err := metaBucket.Put([]byte(helpers.StoreMetaSchemaVersion), []byte(strconv.Itoa(meta.SchemaVersion))); err != nil {
		return err
	}
	if err := metaBucket.Put([]byte(helpers.StoreMetaLastSnapshot), []byte(meta.LastSnapshot.Format(time.RFC3339Nano))); err != nil {
		return err
	}
	// Written only when set, so an unrecorded cache carries no key at all: the
	// same shape an older binary leaves, and loadMeta's one absent-key path.
	if !meta.ContentRecorded.IsZero() {
		if err := metaBucket.Put([]byte(helpers.StoreMetaContentRecorded), []byte(meta.ContentRecorded.Format(time.RFC3339Nano))); err != nil {
			return err
		}
	}
	if meta.RequirementsHash != "" {
		if err := metaBucket.Put([]byte(helpers.StoreMetaRequirementsHash), []byte(meta.RequirementsHash)); err != nil {
			return err
		}
	}
	if meta.Server != "" {
		if err := metaBucket.Put([]byte(helpers.StoreMetaServer), []byte(meta.Server)); err != nil {
			return err
		}
	}
	return nil
}

// ensureEmptyBucket recreates a bucket to ensure it is empty.
func ensureEmptyBucket(tx *bolt.Tx, name string) (*bolt.Bucket, error) {
	bucket := tx.Bucket([]byte(name))
	if bucket != nil {
		if err := tx.DeleteBucket([]byte(name)); err != nil {
			return nil, err
		}
	}
	return tx.CreateBucket([]byte(name))
}

// loadBucket iterates over a bucket and calls fn for each entry.
func loadBucket(tx *bolt.Tx, name string, fn func(k, v []byte) error) error {
	bucket := tx.Bucket([]byte(name))
	if bucket == nil {
		return nil
	}
	return bucket.ForEach(fn)
}

// loadJSONBucket decodes every value of the named bucket into dst. A value that
// does not decode is corruption, reported with bucket and key, never a zero T;
// filling dst directly, with no *Store, is what keeps Dirty false after a load.
func loadJSONBucket[T any](tx *bolt.Tx, name string, dst map[string]T) error {
	return loadBucket(tx, name, func(k, v []byte) error {
		var entry T
		if err := json.Unmarshal(v, &entry); err != nil {
			return fmt.Errorf("invalid %s entry %q: %w", name, string(k), err)
		}
		dst[string(k)] = entry
		return nil
	})
}

// saveJSONBucket replaces the named bucket's contents with data as JSON within
// the caller's transaction; loadJSONBucket treats anything else as corruption.
func saveJSONBucket[T any](tx *bolt.Tx, name string, data map[string]T) error {
	bucket, err := ensureEmptyBucket(tx, name)
	if err != nil {
		return err
	}
	for key, entry := range data {
		encoded, err := json.Marshal(&entry)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(key), encoded); err != nil {
			return err
		}
	}
	return nil
}
