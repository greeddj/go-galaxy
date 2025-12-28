package store

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
)

// SnapshotMeta holds metadata about the cached snapshot.
type SnapshotMeta struct {
	SchemaVersion    int       `json:"schema_version"`
	LastSnapshot     time.Time `json:"last_snapshot"`
	RequirementsHash string    `json:"requirements_hash"`
	Server           string    `json:"server"`
}

// APICacheEntry stores a cached API response and validation data.
type APICacheEntry struct {
	URL          string        `json:"url"`
	ETag         string        `json:"etag"`
	LastModified string        `json:"last_modified"`
	FetchedAt    time.Time     `json:"fetched_at"`
	TTL          time.Duration `json:"ttl"`
	Body         []byte        `json:"body"`
}

// InstalledEntry records an installed collection entry.
type InstalledEntry struct {
	InstallPath    string    `json:"install_path"`
	Source         string    `json:"source"`
	ArtifactSHA256 string    `json:"artifact_sha256"`
	InstalledAt    time.Time `json:"installed_at"`
	Deps           []string  `json:"deps"`
}

// Store holds cached state for collections and metadata.
type Store struct {
	mu           sync.RWMutex                 `json:"-"`
	Meta         SnapshotMeta                 `json:"meta"`
	APICache     map[string]APICacheEntry     `json:"api_cache"`
	DepsCache    map[string]map[string]string `json:"deps_cache"`
	Installed    map[string]InstalledEntry    `json:"installed"`
	Graph        map[string][]string          `json:"graph"`
	Requirements map[string]RequirementSpec   `json:"requirements"`
	Roots        map[string][]string          `json:"roots"`
	Resolved     map[string]ResolvedEntry     `json:"resolved"`
	Versions     map[string][]string          `json:"versions_cache"`
}

// New creates an initialized Store with empty maps.
func New() *Store {
	return &Store{
		Meta: SnapshotMeta{
			SchemaVersion: helpers.StoreSnapshotSchemaVersion,
		},
		APICache:     make(map[string]APICacheEntry),
		DepsCache:    make(map[string]map[string]string),
		Installed:    make(map[string]InstalledEntry),
		Graph:        make(map[string][]string),
		Requirements: make(map[string]RequirementSpec),
		Roots:        make(map[string][]string),
		Resolved:     make(map[string]ResolvedEntry),
		Versions:     make(map[string][]string),
	}
}

// ResolvedEntry stores a resolved collection version and source.
type ResolvedEntry struct {
	Version string `json:"version"`
	Source  string `json:"source"`
}

// RequirementSpec captures a requirement constraint and metadata.
type RequirementSpec struct {
	Constraint string   `json:"constraint"`
	Source     string   `json:"source"`
	Type       string   `json:"type,omitempty"`
	Signatures []string `json:"signatures,omitempty"`
}

// SetInstalled records an installed collection entry.
func (m *Store) SetInstalled(key string, entry InstalledEntry) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Installed[key] = entry
}

// DeleteInstalled removes an installed entry by key.
func (m *Store) DeleteInstalled(key string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Installed, key)
}

// GetInstalled returns an installed entry by key.
func (m *Store) GetInstalled(key string) (InstalledEntry, bool) {
	if m == nil {
		return InstalledEntry{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.Installed[key]
	return entry, ok
}

// GetDepsCache returns cached dependency constraints for a key.
func (m *Store) GetDepsCache(key string) (map[string]string, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.DepsCache[key]
	if !ok {
		return nil, false
	}
	clone := make(map[string]string, len(entry))
	maps.Copy(clone, entry)
	return clone, true
}

// SetDepsCache stores dependency constraints for a key.
func (m *Store) SetDepsCache(key string, deps map[string]string) {
	if m == nil {
		return
	}
	clone := make(map[string]string, len(deps))
	maps.Copy(clone, deps)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DepsCache[key] = clone
}

// DeleteDepsCache removes cached dependency data for a key.
func (m *Store) DeleteDepsCache(key string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.DepsCache, key)
}

// GetAPICache returns a cached API entry by key.
func (m *Store) GetAPICache(key string) (APICacheEntry, bool) {
	if m == nil {
		return APICacheEntry{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.APICache[key]
	return entry, ok
}

// SetAPICache stores a cached API entry.
func (m *Store) SetAPICache(key string, entry APICacheEntry) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.APICache[key] = entry
}

// ClearCaches clears API, dependency, and versions caches.
func (m *Store) ClearCaches() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.APICache = make(map[string]APICacheEntry)
	m.DepsCache = make(map[string]map[string]string)
	m.Versions = make(map[string][]string)
}

// GetVersionsCache returns cached versions for a key.
func (m *Store) GetVersionsCache(key string) ([]string, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.Versions[key]
	if !ok {
		return nil, false
	}
	clone := make([]string, len(entry))
	copy(clone, entry)
	return clone, true
}

// SetVersionsCache stores cached versions for a key.
func (m *Store) SetVersionsCache(key string, versions []string) {
	if m == nil {
		return
	}
	clone := make([]string, len(versions))
	copy(clone, versions)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Versions[key] = clone
}

// SetResolvedAll replaces the resolved entries map.
func (m *Store) SetResolvedAll(resolved map[string]ResolvedEntry) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Resolved = resolved
}

// ResolvedSnapshot returns a copy of resolved entries.
func (m *Store) ResolvedSnapshot() map[string]ResolvedEntry {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	clone := make(map[string]ResolvedEntry, len(m.Resolved))
	maps.Copy(clone, m.Resolved)
	return clone
}

// SetGraph records dependencies for a collection key.
func (m *Store) SetGraph(key string, deps []string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Graph[key] = deps
}

// DeleteGraph removes dependency data for a key.
func (m *Store) DeleteGraph(key string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Graph, key)
}

// SetGraphSnapshot replaces the dependency graph.
func (m *Store) SetGraphSnapshot(graph map[string][]string) {
	if m == nil {
		return
	}
	clone := make(map[string][]string, len(graph))
	for key, deps := range graph {
		out := make([]string, len(deps))
		copy(out, deps)
		clone[key] = out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Graph = clone
}

// GraphSnapshot returns a copy of the dependency graph.
func (m *Store) GraphSnapshot() map[string][]string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	clone := make(map[string][]string, len(m.Graph))
	for key, deps := range m.Graph {
		out := make([]string, len(deps))
		copy(out, deps)
		clone[key] = out
	}
	return clone
}

// SetRequirements stores a snapshot of requirement specs.
func (m *Store) SetRequirements(spec map[string]RequirementSpec) {
	if m == nil {
		return
	}
	clone := make(map[string]RequirementSpec, len(spec))
	maps.Copy(clone, spec)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Requirements = clone
}

// RequirementsSnapshot returns a copy of requirement specs.
func (m *Store) RequirementsSnapshot() map[string]RequirementSpec {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	clone := make(map[string]RequirementSpec, len(m.Requirements))
	maps.Copy(clone, m.Requirements)
	return clone
}

// SetRoots stores root collection keys under a label.
func (m *Store) SetRoots(key string, roots []string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Roots[key] = roots
}

// MetaSnapshot returns the current snapshot metadata.
func (m *Store) MetaSnapshot() SnapshotMeta {
	if m == nil {
		return SnapshotMeta{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Meta
}

// SetMetaRequirements stores the requirements hash and server.
func (m *Store) SetMetaRequirements(hash, server string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Meta.RequirementsHash = hash
	m.Meta.Server = server
}

// snapshotData is a serialized view of Store contents.
type snapshotData struct {
	Meta         SnapshotMeta
	APICache     map[string]APICacheEntry
	DepsCache    map[string]map[string]string
	Installed    map[string]InstalledEntry
	Graph        map[string][]string
	Requirements map[string]RequirementSpec
	Roots        map[string][]string
	Resolved     map[string]ResolvedEntry
	Versions     map[string][]string
}

// snapshotData builds a snapshot payload from the store.
func (m *Store) snapshotData() snapshotData {
	m.mu.RLock()
	defer m.mu.RUnlock()

	data := snapshotData{
		Meta:         m.Meta,
		APICache:     make(map[string]APICacheEntry, len(m.APICache)),
		DepsCache:    make(map[string]map[string]string, len(m.DepsCache)),
		Installed:    make(map[string]InstalledEntry, len(m.Installed)),
		Graph:        make(map[string][]string, len(m.Graph)),
		Requirements: make(map[string]RequirementSpec, len(m.Requirements)),
		Roots:        make(map[string][]string, len(m.Roots)),
		Resolved:     make(map[string]ResolvedEntry, len(m.Resolved)),
		Versions:     make(map[string][]string, len(m.Versions)),
	}

	maps.Copy(data.APICache, m.APICache)
	for key, deps := range m.DepsCache {
		clone := make(map[string]string, len(deps))
		maps.Copy(clone, deps)
		data.DepsCache[key] = clone
	}
	maps.Copy(data.Installed, m.Installed)
	for key, deps := range m.Graph {
		clone := make([]string, len(deps))
		copy(clone, deps)
		data.Graph[key] = clone
	}
	maps.Copy(data.Requirements, m.Requirements)
	for key, roots := range m.Roots {
		clone := make([]string, len(roots))
		copy(clone, roots)
		data.Roots[key] = clone
	}
	maps.Copy(data.Resolved, m.Resolved)
	for key, versions := range m.Versions {
		clone := make([]string, len(versions))
		copy(clone, versions)
		data.Versions[key] = clone
	}

	return data
}

// Load reads cached state from Bolt databases.
func Load(dbs *DBs) (*Store, error) {
	store := New()
	if dbs == nil {
		return store, nil
	}

	if err := loadMeta(dbs, store); err != nil {
		return nil, err
	}
	if err := validateSnapshotSchema(store.Meta.SchemaVersion); err != nil {
		return nil, err
	}
	if err := runLoadSteps(dbs, store); err != nil {
		return nil, err
	}
	return store, nil
}

// Save writes cached state to Bolt databases.
func Save(dbs *DBs, store *Store) error {
	if dbs == nil {
		return helpers.ErrDbNil
	}
	if store == nil {
		return helpers.ErrStoreNil
	}

	data := store.snapshotData()
	if data.Meta.SchemaVersion == 0 {
		data.Meta.SchemaVersion = helpers.StoreSnapshotSchemaVersion
	}
	data.Meta.SchemaVersion = helpers.StoreSnapshotSchemaVersion
	data.Meta.LastSnapshot = time.Now().UTC()

	if err := saveMeta(dbs, data.Meta); err != nil {
		return err
	}
	return runSaveSteps(dbs, data)
}

func validateSnapshotSchema(version int) error {
	if version > helpers.StoreSnapshotSchemaVersion {
		return fmt.Errorf("%w: %d", helpers.ErrUnsupportedSchemaVersion, version)
	}
	return nil
}

func runLoadSteps(dbs *DBs, store *Store) error {
	steps := []func() error{
		func() error { return loadAPICache(dbs, store) },
		func() error { return loadInstalled(dbs, store) },
		func() error { return loadDepsCache(dbs, store) },
		func() error { return loadGraph(dbs, store) },
		func() error { return loadRequirements(dbs, store) },
		func() error { return loadRoots(dbs, store) },
		func() error { return loadResolved(dbs, store) },
		func() error { return loadVersions(dbs, store) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func runSaveSteps(dbs *DBs, data snapshotData) error {
	steps := []func() error{
		func() error { return saveAPICache(dbs, data) },
		func() error { return saveDepsCache(dbs, data) },
		func() error { return saveInstalled(dbs, data) },
		func() error { return saveGraph(dbs, data) },
		func() error { return saveRequirements(dbs, data) },
		func() error { return saveRoots(dbs, data) },
		func() error { return saveResolved(dbs, data) },
		func() error { return saveVersions(dbs, data) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func loadMeta(dbs *DBs, store *Store) error {
	if dbs.meta == nil {
		return nil
	}
	return dbs.meta.View(func(tx *bolt.Tx) error {
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
		}
		if v := metaBucket.Get([]byte(helpers.StoreMetaLastSnapshot)); v != nil {
			t, err := time.Parse(time.RFC3339Nano, string(v))
			if err != nil {
				return fmt.Errorf("invalid snapshot time: %w", err)
			}
			store.Meta.LastSnapshot = t
		}
		if v := metaBucket.Get([]byte(helpers.StoreMetaRequirementsHash)); v != nil {
			store.Meta.RequirementsHash = string(v)
		}
		if v := metaBucket.Get([]byte(helpers.StoreMetaServer)); v != nil {
			store.Meta.Server = string(v)
		}
		return nil
	})
}

func loadAPICache(dbs *DBs, store *Store) error {
	return loadBucket(dbs.apiCache, helpers.StoreBucketAPICache, func(k, v []byte) error {
		var entry APICacheEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			return err
		}
		store.APICache[string(k)] = entry
		return nil
	})
}

func loadInstalled(dbs *DBs, store *Store) error {
	return loadBucket(dbs.installed, helpers.StoreBucketInstalled, func(k, v []byte) error {
		var entry InstalledEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			return err
		}
		store.Installed[string(k)] = entry
		return nil
	})
}

func loadDepsCache(dbs *DBs, store *Store) error {
	return loadBucket(dbs.depsCache, helpers.StoreBucketDepsCache, func(k, v []byte) error {
		var entry map[string]string
		if err := json.Unmarshal(v, &entry); err != nil {
			return err
		}
		store.DepsCache[string(k)] = entry
		return nil
	})
}

func loadGraph(dbs *DBs, store *Store) error {
	return loadBucket(dbs.graph, helpers.StoreBucketGraph, func(k, v []byte) error {
		var deps []string
		if err := json.Unmarshal(v, &deps); err != nil {
			return err
		}
		store.Graph[string(k)] = deps
		return nil
	})
}

func loadRequirements(dbs *DBs, store *Store) error {
	return loadBucket(dbs.requirements, helpers.StoreBucketRequirements, func(k, v []byte) error {
		var spec RequirementSpec
		if err := json.Unmarshal(v, &spec); err != nil {
			return err
		}
		store.Requirements[string(k)] = spec
		return nil
	})
}

func loadRoots(dbs *DBs, store *Store) error {
	return loadBucket(dbs.roots, helpers.StoreBucketRoots, func(k, v []byte) error {
		var roots []string
		if err := json.Unmarshal(v, &roots); err != nil {
			return err
		}
		store.Roots[string(k)] = roots
		return nil
	})
}

func loadResolved(dbs *DBs, store *Store) error {
	return loadBucket(dbs.resolved, helpers.StoreBucketResolved, func(k, v []byte) error {
		var entry ResolvedEntry
		if err := json.Unmarshal(v, &entry); err == nil && entry.Version != "" {
			store.Resolved[string(k)] = entry
			return nil
		}
		store.Resolved[string(k)] = ResolvedEntry{Version: string(v)}
		return nil
	})
}

func loadVersions(dbs *DBs, store *Store) error {
	return loadBucket(dbs.versions, helpers.StoreBucketVersions, func(k, v []byte) error {
		var entry []string
		if err := json.Unmarshal(v, &entry); err != nil {
			return err
		}
		store.Versions[string(k)] = entry
		return nil
	})
}

func saveMeta(dbs *DBs, meta SnapshotMeta) error {
	if dbs.meta == nil {
		return nil
	}
	return dbs.meta.Update(func(tx *bolt.Tx) error {
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
	})
}

func saveAPICache(dbs *DBs, data snapshotData) error {
	return saveBucket(dbs.apiCache, helpers.StoreBucketAPICache, data.APICache, func(entry APICacheEntry) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveDepsCache(dbs *DBs, data snapshotData) error {
	return saveBucket(dbs.depsCache, helpers.StoreBucketDepsCache, data.DepsCache, func(entry map[string]string) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveInstalled(dbs *DBs, data snapshotData) error {
	return saveBucket(dbs.installed, helpers.StoreBucketInstalled, data.Installed, func(entry InstalledEntry) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveGraph(dbs *DBs, data snapshotData) error {
	return saveBucket(dbs.graph, helpers.StoreBucketGraph, data.Graph, func(entry []string) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveRequirements(dbs *DBs, data snapshotData) error {
	return saveBucket(dbs.requirements, helpers.StoreBucketRequirements, data.Requirements, func(entry RequirementSpec) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveRoots(dbs *DBs, data snapshotData) error {
	return saveBucket(dbs.roots, helpers.StoreBucketRoots, data.Roots, func(entry []string) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveResolved(dbs *DBs, data snapshotData) error {
	return saveBucket(dbs.resolved, helpers.StoreBucketResolved, data.Resolved, func(entry ResolvedEntry) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveVersions(dbs *DBs, data snapshotData) error {
	return saveBucket(dbs.versions, helpers.StoreBucketVersions, data.Versions, func(entry []string) ([]byte, error) {
		return json.Marshal(&entry)
	})
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
func loadBucket(db *bolt.DB, name string, fn func(k, v []byte) error) error {
	if db == nil {
		return nil
	}
	return db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(name))
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(fn)
	})
}

// saveBucket writes data to a bucket using the encode callback.
func saveBucket[T any](db *bolt.DB, name string, data map[string]T, encode func(T) ([]byte, error)) error {
	if db == nil {
		return nil
	}
	return db.Update(func(tx *bolt.Tx) error {
		bucket, err := ensureEmptyBucket(tx, name)
		if err != nil {
			return err
		}
		for key, entry := range data {
			encoded, err := encode(entry)
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte(key), encoded); err != nil {
				return err
			}
		}
		return nil
	})
}
