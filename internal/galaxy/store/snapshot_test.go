package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// errTestResolvedBucketMissing is a static test-only error used when the
// resolved bucket is unexpectedly absent while corrupting a test fixture.
var errTestResolvedBucketMissing = errors.New("resolved bucket missing")

// errTestAPICacheBucketMissing is a static test-only error used when the
// api_cache bucket is unexpectedly absent while corrupting a test fixture.
var errTestAPICacheBucketMissing = errors.New("api_cache bucket missing")

// testDepsConstraint is the shared dependency constraint value seeded across
// several deps-cache test fixtures.
const testDepsConstraint = ">=1.0.0"

// testArtifactSHA is the shared artifact sha value seeded across several
// installed-entry test fixtures.
const testArtifactSHA = "abc"

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	st := buildTestStore(fixed)
	mustSave(t, dbs, st)
	loaded := mustLoad(t, dbs)
	assertMeta(t, loaded)
	assertAPICache(t, loaded)
	assertDepsCache(t, loaded)
	assertInstalled(t, loaded)
	assertGraph(t, loaded)
	assertRequirements(t, loaded)
	assertResolved(t, loaded)
	assertVersions(t, loaded)
	assertWarmed(t, loaded)
	assertGitPin(t, loaded)
	assertInstalledRole(t, loaded)
	assertRolePin(t, loaded)
	assertURLPin(t, loaded)
}

func openTestDBs(t *testing.T) *DBs {
	t.Helper()
	dir := t.TempDir()
	dbs, err := OpenDBs(dir, helpers.BoltOpenTimeout)
	if err != nil {
		t.Fatalf("OpenDBs error: %v", err)
	}
	t.Cleanup(func() {
		_ = dbs.Close()
	})
	return dbs
}

func buildTestStore(fixed time.Time) *Store {
	return populateTestStore(New(), fixed)
}

// populateTestStore writes a known fixture into every one of Store's twelve
// map buckets through their normal mutators, so a store obtained another way
// (e.g. decoded from JSON) can take the same fixture and the assert* helpers.
func populateTestStore(st *Store, fixed time.Time) *Store {
	st.SetMetaRequirements("req-hash", "https://example.com")
	st.SetAPICache("api", APICacheEntry{
		// FetchedAt is the wall clock, not fixed: APICache is pruned by
		// CacheEntryMaxAge at persist time and fixed predates that window.
		FetchedAt: time.Now().UTC(),
		URL:       "https://example.com/api",
		ETag:      "etag",
		TTL:       time.Minute,
		Body:      []byte(`{"ok":true}`),
	})
	st.SetDepsCache("deps", map[string]string{"a.b": testDepsConstraint})
	st.SetInstalled("a.b@1.0.0", InstalledEntry{
		InstallPath:    "/tmp/a/b",
		Source:         "https://example.com",
		ArtifactSHA256: testArtifactSHA,
		InstalledAt:    fixed,
		Deps:           []string{"c.d@1.2.3"},
	})
	st.SetGraph("a.b@1.0.0", []string{"c.d@1.2.3"})
	st.SetRequirements(map[string]RequirementSpec{
		"a.b": {Constraint: "1.0.0", Source: "https://example.com", Type: "galaxy"},
	})
	st.SetResolvedAll(map[string]ResolvedEntry{
		"a.b": {Version: "1.0.0", Source: "https://example.com"},
	})
	st.SetVersionsCache("versions", []string{"1.0.0", "2.0.0"})
	st.SetWarmed("a.b@1.0.0", "warmed-sha")
	st.SetGitPin(testGitPinKey, GitPinEntry{
		Commit: testGitPinCommit,
		Collections: []GitPinCollection{{
			Namespace:    "acme",
			Name:         "app",
			Version:      "1.2.3",
			Subdir:       "collections/app",
			Dependencies: map[string]string{"a.b": testDepsConstraint},
		}},
	})
	st.SetInstalledRole(testRoleName, InstalledRoleEntry{
		InstallPath:    "/tmp/roles/" + testRoleName,
		Source:         testRoleSource,
		ArtifactSHA256: testRoleArtifactSHA,
		Version:        "v1.2.3",
		GalaxyName:     "acme.nginx",
		InstalledAt:    fixed,
		Deps:           []string{"common"},
	})
	st.SetRolePin(testRolePinKey, RolePinEntry{
		Repository:     testRoleRepository,
		Commit:         testGitPinCommit,
		Version:        "v1.2.3",
		GalaxySHA:      testGitPinCommit,
		GalaxyRoleName: "nginx",
		Deps:           []RolePinDep{{Src: "acme.common", Version: "v2.0.0", Name: "common"}},
	})
	st.SetURLPin(testURLPinKey, URLPinEntry{
		SHA256:       testURLPinSHA,
		Namespace:    "acme",
		Name:         "app",
		Version:      "1.2.3",
		Dependencies: map[string]string{"a.b": testDepsConstraint},
	})
	return st
}

func assertURLPin(t *testing.T, loaded *Store) {
	t.Helper()
	pin, ok := loaded.GetURLPin(testURLPinKey)
	if !ok || pin.SHA256 != testURLPinSHA || pin.Namespace != "acme" || pin.Name != "app" ||
		pin.Version != "1.2.3" || pin.Dependencies["a.b"] != testDepsConstraint {
		t.Fatalf("unexpected url pin: %#v (ok=%t)", pin, ok)
	}
}

const (
	testGitPinKey    = "https://github.com/acme/app.git\nmain\n"
	testGitPinCommit = "0123456789abcdef0123456789abcdef01234567"

	testRoleName        = "nginx"
	testRoleRepository  = "https://github.com/acme/ansible-role-nginx.git"
	testRoleSource      = "git+" + testRoleRepository + "#@" + testGitPinCommit
	testRoleArtifactSHA = "role-sha"
	testRolePinKey      = "acme.nginx,v1.2.3"

	testURLPinKey = "https://example.com/dl/acme-app-1.2.3.tar.gz"
	testURLPinSHA = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
)

func assertInstalledRole(t *testing.T, loaded *Store) {
	t.Helper()
	role, ok := loaded.GetInstalledRole(testRoleName)
	if !ok || role.ArtifactSHA256 != testRoleArtifactSHA || role.Source != testRoleSource ||
		role.Version != "v1.2.3" || role.GalaxyName != "acme.nginx" || role.InstallPath != "/tmp/roles/"+testRoleName {
		t.Fatalf("unexpected installed role: %#v (ok=%t)", role, ok)
	}
	if len(role.Deps) != 1 || role.Deps[0] != "common" {
		t.Fatalf("unexpected installed role deps: %#v", role.Deps)
	}
	if role.InstalledAt.IsZero() {
		t.Fatalf("installed role InstalledAt was lost")
	}
}

func assertRolePin(t *testing.T, loaded *Store) {
	t.Helper()
	pin, ok := loaded.GetRolePin(testRolePinKey)
	if !ok || pin.Repository != testRoleRepository || pin.Commit != testGitPinCommit || pin.Version != "v1.2.3" ||
		pin.GalaxySHA != testGitPinCommit || pin.GalaxyRoleName != "nginx" {
		t.Fatalf("unexpected role pin: %#v (ok=%t)", pin, ok)
	}
	if len(pin.Deps) != 1 || pin.Deps[0] != (RolePinDep{Src: "acme.common", Version: "v2.0.0", Name: "common"}) {
		t.Fatalf("unexpected role pin deps: %#v", pin.Deps)
	}
	if pin.FetchedAt.IsZero() {
		t.Fatalf("role pin FetchedAt was not stamped")
	}
}

func assertGitPin(t *testing.T, loaded *Store) {
	t.Helper()
	pin, ok := loaded.GetGitPin(testGitPinKey)
	if !ok || pin.Commit != testGitPinCommit || len(pin.Collections) != 1 {
		t.Fatalf("unexpected git pin: %#v (ok=%t)", pin, ok)
	}
	c := pin.Collections[0]
	if c.Namespace != "acme" || c.Name != "app" || c.Version != "1.2.3" || c.Subdir != "collections/app" ||
		c.Dependencies["a.b"] != testDepsConstraint {
		t.Fatalf("unexpected git pin collection: %#v", c)
	}
	if pin.FetchedAt.IsZero() {
		t.Fatalf("git pin FetchedAt was not stamped")
	}
}

func mustSave(t *testing.T, dbs *DBs, st *Store) {
	t.Helper()
	if err := Save(dbs, st); err != nil {
		t.Fatalf("Save error: %v", err)
	}
}

func mustLoad(t *testing.T, dbs *DBs) *Store {
	t.Helper()
	loaded, err := Load(dbs)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	return loaded
}

func assertMeta(t *testing.T, loaded *Store) {
	t.Helper()
	if loaded.Meta.SchemaVersion != helpers.StoreSnapshotSchemaVersion {
		t.Fatalf("unexpected schema version: %d", loaded.Meta.SchemaVersion)
	}
	if loaded.Meta.RequirementsHash != "req-hash" {
		t.Fatalf("unexpected requirements hash: %q", loaded.Meta.RequirementsHash)
	}
	if loaded.Meta.Server != "https://example.com" {
		t.Fatalf("unexpected server: %q", loaded.Meta.Server)
	}
	if loaded.Meta.LastSnapshot.IsZero() {
		t.Fatalf("expected LastSnapshot to be set")
	}
}

func assertAPICache(t *testing.T, loaded *Store) {
	t.Helper()
	entry, ok := loaded.GetAPICache("api")
	if !ok {
		t.Fatalf("expected API cache entry")
	}
	if entry.URL != "https://example.com/api" {
		t.Fatalf("unexpected api cache url: %q", entry.URL)
	}
	if string(entry.Body) != `{"ok":true}` {
		t.Fatalf("unexpected api cache body: %s", string(entry.Body))
	}
}

func assertDepsCache(t *testing.T, loaded *Store) {
	t.Helper()
	deps, ok := loaded.GetDepsCache("deps")
	if !ok || deps["a.b"] != testDepsConstraint {
		t.Fatalf("unexpected deps cache: %#v", deps)
	}
}

func assertInstalled(t *testing.T, loaded *Store) {
	t.Helper()
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry: %#v", installed)
	}
}

func assertGraph(t *testing.T, loaded *Store) {
	t.Helper()
	graph := loaded.GraphSnapshot()
	if len(graph["a.b@1.0.0"]) != 1 || graph["a.b@1.0.0"][0] != "c.d@1.2.3" {
		t.Fatalf("unexpected graph: %#v", graph)
	}
}

func assertRequirements(t *testing.T, loaded *Store) {
	t.Helper()
	reqs := loaded.RequirementsSnapshot()
	if reqs["a.b"].Constraint != "1.0.0" {
		t.Fatalf("unexpected requirements: %#v", reqs)
	}
}

func assertResolved(t *testing.T, loaded *Store) {
	t.Helper()
	resolved := loaded.ResolvedSnapshot()
	if resolved["a.b"].Version != "1.0.0" {
		t.Fatalf("unexpected resolved: %#v", resolved)
	}
}

func assertVersions(t *testing.T, loaded *Store) {
	t.Helper()
	versions, ok := loaded.GetVersionsCache("versions")
	if !ok || len(versions) != 2 {
		t.Fatalf("unexpected versions cache: %#v", versions)
	}
}

func assertWarmed(t *testing.T, loaded *Store) {
	t.Helper()
	warmed := loaded.WarmedArtifactSHAByKey()
	if got := warmed["a.b@1.0.0"]; got != "warmed-sha" {
		t.Fatalf("unexpected warmed entry: %#v", warmed)
	}
}

// TestSaveRollsBackWholeTransactionOnMidSaveFailure pins that Save writes
// every bucket in one Bolt transaction: a failure in a later bucket rolls back
// the whole attempt and leaves the previous snapshot intact, never a mix.
func TestSaveRollsBackWholeTransactionOnMidSaveFailure(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)

	st := New()
	// FetchedAt must lie within the CacheEntryMaxAge window, or persist-time
	// pruning drops the entry whatever the rollback does.
	st.SetAPICache("api", APICacheEntry{URL: "v1", FetchedAt: time.Now().UTC()})
	st.SetInstalled("normal.key", InstalledEntry{Source: "v1"})
	mustSave(t, dbs, st)

	// Mutate api_cache (written before installed) and inject a key that
	// exceeds bolt.MaxKeySize into installed (written after api_cache),
	// forcing a mid-transaction failure in a non-first bucket.
	st.SetAPICache("api", APICacheEntry{URL: "v2", FetchedAt: time.Now().UTC()})
	giantKey := strings.Repeat("k", 40000)
	st.SetInstalled(giantKey, InstalledEntry{Source: "v2"})

	err := Save(dbs, st)
	if err == nil {
		t.Fatalf("expected Save to fail on oversized key")
	}
	if !errors.Is(err, bolterrors.ErrKeyTooLarge) {
		t.Fatalf("expected ErrKeyTooLarge, got %v", err)
	}

	loaded := mustLoad(t, dbs)
	entry, ok := loaded.GetAPICache("api")
	if !ok || entry.URL != "v1" {
		t.Fatalf("expected api cache to remain at v1 after rollback, got %#v (ok=%v)", entry, ok)
	}
	if _, ok := loaded.GetInstalled(giantKey); ok {
		t.Fatalf("expected giant key to be absent after rolled-back save")
	}
	if _, ok := loaded.GetInstalled("normal.key"); !ok {
		t.Fatalf("expected normal.key from the first save to survive")
	}
}

// TestLoadRejectsNewerSchemaAndDropsOlderSchema pins Load's schema policy: a
// newer schema version is ErrUnsupportedSchemaVersion, an older one is dropped
// for a fresh empty Store with a nil error.
func TestLoadRejectsNewerSchemaAndDropsOlderSchema(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	st := buildTestStore(fixed)
	mustSave(t, dbs, st)

	newerVersion := helpers.StoreSnapshotSchemaVersion + 1
	stampSchemaVersion(t, dbs, newerVersion)
	if _, err := Load(dbs); !errors.Is(err, helpers.ErrUnsupportedSchemaVersion) {
		t.Fatalf("expected ErrUnsupportedSchemaVersion, got %v", err)
	}

	olderVersion := helpers.StoreSnapshotSchemaVersion - 1
	stampSchemaVersion(t, dbs, olderVersion)
	loaded, err := Load(dbs)
	if err != nil {
		t.Fatalf("expected nil error for outdated schema, got %v", err)
	}
	fresh := New()
	if loaded.Meta.SchemaVersion != fresh.Meta.SchemaVersion {
		t.Fatalf("expected fresh schema version %d, got %d", fresh.Meta.SchemaVersion, loaded.Meta.SchemaVersion)
	}
	if len(loaded.APICache) != 0 || len(loaded.Installed) != 0 {
		t.Fatalf("expected an empty store after dropping an outdated schema, got %#v", loaded)
	}
}

// stampSchemaVersion directly overwrites the meta bucket's schema version
// key, bypassing Save, to simulate a snapshot written by a different
// binary version.
func stampSchemaVersion(t *testing.T, dbs *DBs, version int) {
	t.Helper()
	err := dbs.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(helpers.StoreBucketMeta))
		if err != nil {
			return err
		}
		return bucket.Put([]byte(helpers.StoreMetaSchemaVersion), []byte(strconv.Itoa(version)))
	})
	if err != nil {
		t.Fatalf("failed to stamp schema version: %v", err)
	}
}

// TestWasPersistedFalseForFreshStore pins that a store from New(), never saved
// or loaded, reports no persisted snapshot.
func TestWasPersistedFalseForFreshStore(t *testing.T) {
	t.Parallel()
	if New().WasPersisted() {
		t.Fatal("expected a fresh store to report WasPersisted() == false")
	}
}

// TestWasPersistedFalseForNilStore pins the conservative nil-receiver answer:
// a nil *Store is no evidence that a snapshot was persisted.
func TestWasPersistedFalseForNilStore(t *testing.T) {
	t.Parallel()
	var st *Store
	if st.WasPersisted() {
		t.Fatal("expected a nil store to report WasPersisted() == false")
	}
}

// TestWasPersistedTrueAfterLocalSaveLoadRoundTrip proves a store that went
// through a real local Save -> Load round trip reports a persisted snapshot,
// since Save unconditionally stamps Meta.LastSnapshot before writing.
func TestWasPersistedTrueAfterLocalSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	st := buildTestStore(fixed)
	mustSave(t, dbs, st)

	loaded := mustLoad(t, dbs)
	if !loaded.WasPersisted() {
		t.Fatal("expected a store loaded after a real Save to report WasPersisted() == true")
	}
}

// TestWasPersistedTrueAfterMarshalSnapshotUnmarshal pins the S3 wire path: a
// store decoded from MarshalSnapshot's JSON, as the S3 backend's LoadStore
// produces it, reports a persisted snapshot.
func TestWasPersistedTrueAfterMarshalSnapshotUnmarshal(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	st := buildTestStore(fixed)

	data, err := st.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot error: %v", err)
	}

	decoded := New()
	if err := json.Unmarshal(data, decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	if !decoded.WasPersisted() {
		t.Fatal("expected a store decoded from MarshalSnapshot's output to report WasPersisted() == true")
	}
}

// TestWasPersistedFalseAfterOutdatedSchemaLoad pins the post-schema-bump case:
// a snapshot with a real last_snapshot but an older schema is dropped by Load,
// so the fresh store it returns reports WasPersisted false.
func TestWasPersistedFalseAfterOutdatedSchemaLoad(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	st := buildTestStore(fixed)
	mustSave(t, dbs, st)

	olderVersion := helpers.StoreSnapshotSchemaVersion - 1
	stampSchemaVersion(t, dbs, olderVersion)

	loaded := mustLoad(t, dbs)
	if loaded.WasPersisted() {
		t.Fatal("expected a store dropped for an outdated schema to report WasPersisted() == false")
	}
}

// TestLoadRejectsCorruptResolvedEntry pins that Load reports a corrupt
// resolved value as an error naming its key, never coercing it into a garbage
// version string.
func TestLoadRejectsCorruptResolvedEntry(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	st := New()
	st.SetResolvedAll(map[string]ResolvedEntry{
		"a.b": {Version: "1.0.0"},
	})
	mustSave(t, dbs, st)

	err := dbs.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(helpers.StoreBucketResolved))
		if bucket == nil {
			return errTestResolvedBucketMissing
		}
		return bucket.Put([]byte("a.b"), []byte("{not-json"))
	})
	if err != nil {
		t.Fatalf("failed to corrupt resolved entry: %v", err)
	}

	_, err = Load(dbs)
	if err == nil {
		t.Fatalf("expected Load to reject a corrupt resolved entry")
	}
	if !strings.Contains(err.Error(), "a.b") {
		t.Fatalf("expected error to mention the corrupt key, got %v", err)
	}
}

// TestLoadNamesBucketAndKeyOfCorruptEntry pins that Load's error for a corrupt
// entry names both its bucket and its key; using api_cache rather than resolved
// shows the wrapper is shared across buckets.
func TestLoadNamesBucketAndKeyOfCorruptEntry(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	st := New()
	// FetchedAt is sampled fresh: SetAPICache does not stamp it, and a zero
	// stamp is pruned at save time, leaving the corruption nothing to land on.
	st.SetAPICache("a.b", APICacheEntry{
		URL:       "https://example.com/api",
		FetchedAt: time.Now().UTC(),
		Body:      []byte(`{"ok":true}`),
	})
	mustSave(t, dbs, st)

	// Positive control on this same fixture, before it is corrupted: the entry
	// has to be shown loading cleanly, otherwise the refusal below cannot be
	// told apart from a fixture whose value never reaches the decode at all.
	if _, ok := mustLoad(t, dbs).GetAPICache("a.b"); !ok {
		t.Fatal("expected the intact fixture to load its api_cache entry")
	}

	err := dbs.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(helpers.StoreBucketAPICache))
		if bucket == nil {
			return errTestAPICacheBucketMissing
		}
		return bucket.Put([]byte("a.b"), []byte("{not-json"))
	})
	if err != nil {
		t.Fatalf("failed to corrupt api_cache entry: %v", err)
	}

	_, err = Load(dbs)
	// This guard only makes the err.Error() calls below safe.
	if err == nil {
		t.Fatal("expected Load to reject a corrupt api_cache entry")
	}
	// Only this test asserts the bucket name;
	// TestLoadRejectsCorruptResolvedEntry asserts the key alone.
	if !strings.Contains(err.Error(), helpers.StoreBucketAPICache) {
		t.Fatalf("expected error to name the bucket, got %v", err)
	}
	if !strings.Contains(err.Error(), "a.b") {
		t.Fatalf("expected error to name the corrupt key, got %v", err)
	}
}

// TestSnapshotV4RoundTripLocal pins that the schema-4 wire shape, APICache,
// DepsCache and Versions each with a FetchedAt stamp, round-trips through
// Save/Load under the current helpers.StoreSnapshotSchemaVersion.
func TestSnapshotV4RoundTripLocal(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	// recent, not a fixed date: persist-time CacheEntryMaxAge pruning drops an
	// APICache stamp outside the retention window.
	recent := time.Now().UTC().Add(-time.Hour)

	st := New()
	st.SetAPICache("api", APICacheEntry{URL: "https://example.com/api", FetchedAt: recent, Body: []byte("body")})
	st.SetDepsCache("deps", map[string]string{"a.b": testDepsConstraint})
	st.SetVersionsCache("versions", []string{"1.0.0", "2.0.0"})
	mustSave(t, dbs, st)

	loaded := mustLoad(t, dbs)
	if loaded.Meta.SchemaVersion != helpers.StoreSnapshotSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", helpers.StoreSnapshotSchemaVersion, loaded.Meta.SchemaVersion)
	}
	assertV4APICacheEntry(t, loaded, recent)
	assertV4DepsCacheEntry(t, loaded)
	assertV4VersionsEntry(t, loaded)
}

// assertV4APICacheEntry confirms the "api" key survived a round trip with
// its FetchedAt and Body intact.
func assertV4APICacheEntry(t *testing.T, loaded *Store, wantFetchedAt time.Time) {
	t.Helper()
	apiEntry, ok := loaded.GetAPICache("api")
	if !ok || !apiEntry.FetchedAt.Equal(wantFetchedAt) || string(apiEntry.Body) != "body" {
		t.Fatalf("unexpected api cache entry after round trip: %#v (ok=%v)", apiEntry, ok)
	}
}

// assertV4DepsCacheEntry confirms the "deps" key survived a round trip as a
// DepsCacheEntry with a non-zero FetchedAt stamp.
func assertV4DepsCacheEntry(t *testing.T, loaded *Store) {
	t.Helper()
	depsEntry, ok := loaded.DepsCache["deps"]
	if !ok || depsEntry.Deps["a.b"] != testDepsConstraint || depsEntry.FetchedAt.IsZero() {
		t.Fatalf("unexpected deps cache entry after round trip: %#v (ok=%v)", depsEntry, ok)
	}
}

// assertV4VersionsEntry confirms the "versions" key survived a round trip as
// a VersionsEntry with a non-zero FetchedAt stamp.
func assertV4VersionsEntry(t *testing.T, loaded *Store) {
	t.Helper()
	versionsEntry, ok := loaded.Versions["versions"]
	if !ok || len(versionsEntry.List) != 2 || versionsEntry.FetchedAt.IsZero() {
		t.Fatalf("unexpected versions cache entry after round trip: %#v (ok=%v)", versionsEntry, ok)
	}
}

// TestSnapshotV4RoundTripJSON pins that MarshalSnapshot round-trips through
// json.Unmarshal and encodes versions_cache and deps_cache values as objects
// with fetched_at, not the bare arrays and maps of the pre-v4 shape.
func TestSnapshotV4RoundTripJSON(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetAPICache("api", APICacheEntry{URL: "https://example.com/api", Body: []byte("body")})
	st.SetDepsCache("deps", map[string]string{"a.b": testDepsConstraint})
	st.SetVersionsCache("versions", []string{"1.0.0", "2.0.0"})

	payload, err := st.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot error: %v", err)
	}

	var loaded Store
	if err := json.Unmarshal(payload, &loaded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	if loaded.Meta.SchemaVersion != helpers.StoreSnapshotSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", helpers.StoreSnapshotSchemaVersion, loaded.Meta.SchemaVersion)
	}
	depsEntry, ok := loaded.DepsCache["deps"]
	if !ok || depsEntry.Deps["a.b"] != testDepsConstraint || depsEntry.FetchedAt.IsZero() {
		t.Fatalf("unexpected deps cache entry after JSON round trip: %#v (ok=%v)", depsEntry, ok)
	}
	versionsEntry, ok := loaded.Versions["versions"]
	if !ok || len(versionsEntry.List) != 2 || versionsEntry.FetchedAt.IsZero() {
		t.Fatalf("unexpected versions cache entry after JSON round trip: %#v (ok=%v)", versionsEntry, ok)
	}

	assertObjectShape(t, payload, "versions_cache", "versions", "fetched_at", "list")
	assertObjectShape(t, payload, "deps_cache", "deps", "fetched_at", "deps")
}

// assertObjectShape confirms that raw[bucket][key] decodes as a JSON object
// carrying both expected field names, rather than a bare array or map (the
// pre-v4 shape for versions_cache and deps_cache respectively).
func assertObjectShape(t *testing.T, payload []byte, bucket, key, firstField, secondField string) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("json.Unmarshal raw payload error: %v", err)
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw[bucket], &entries); err != nil {
		t.Fatalf("json.Unmarshal %s error: %v", bucket, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(entries[key], &fields); err != nil {
		t.Fatalf("%s[%q] is not a JSON object: %v", bucket, key, err)
	}
	if _, ok := fields[firstField]; !ok {
		t.Fatalf("%s[%q] missing field %q: %s", bucket, key, firstField, entries[key])
	}
	if _, ok := fields[secondField]; !ok {
		t.Fatalf("%s[%q] missing field %q: %s", bucket, key, secondField, entries[key])
	}
}

// TestSnapshotPrunesStaleAndFutureAcrossAllBuckets pins that both persist
// paths drop stale and future-stamped entries from all four age-eviction
// buckets, Warmed under its own WarmedEntryMaxAge, and keep fresh ones.
func TestSnapshotPrunesStaleAndFutureAcrossAllBuckets(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	stale := now.Add(-40 * 24 * time.Hour)
	fresh := now.Add(-1 * 24 * time.Hour)
	future := now.Add(365 * 24 * time.Hour)

	build := func() *Store {
		st := New()
		st.APICache["api-stale"] = APICacheEntry{URL: "stale", FetchedAt: stale}
		st.APICache["api-fresh"] = APICacheEntry{URL: "fresh", FetchedAt: fresh}
		st.APICache["api-future"] = APICacheEntry{URL: "future", FetchedAt: future}
		st.DepsCache["deps-stale"] = DepsCacheEntry{FetchedAt: stale, Deps: map[string]string{"a.b": "1"}}
		st.DepsCache["deps-fresh"] = DepsCacheEntry{FetchedAt: fresh, Deps: map[string]string{"a.b": "1"}}
		st.DepsCache["deps-future"] = DepsCacheEntry{FetchedAt: future, Deps: map[string]string{"a.b": "1"}}
		st.Versions["versions-stale"] = VersionsEntry{FetchedAt: stale, List: []string{"1.0.0"}}
		st.Versions["versions-fresh"] = VersionsEntry{FetchedAt: fresh, List: []string{"1.0.0"}}
		st.Versions["versions-future"] = VersionsEntry{FetchedAt: future, List: []string{"1.0.0"}}
		st.Warmed["warmed-stale"] = WarmedEntry{WarmedAt: stale, ArtifactSHA256: "sha-stale"}
		st.Warmed["warmed-fresh"] = WarmedEntry{WarmedAt: fresh, ArtifactSHA256: "sha-fresh"}
		st.Warmed["warmed-future"] = WarmedEntry{WarmedAt: future, ArtifactSHA256: "sha-future"}
		return st
	}

	t.Run("bolt", func(t *testing.T) {
		t.Parallel()
		dbs := openTestDBs(t)
		mustSave(t, dbs, build())
		assertStalePruned(t, mustLoad(t, dbs))
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		payload, err := build().MarshalSnapshot()
		if err != nil {
			t.Fatalf("MarshalSnapshot error: %v", err)
		}
		var loaded Store
		if err := json.Unmarshal(payload, &loaded); err != nil {
			t.Fatalf("json.Unmarshal error: %v", err)
		}
		assertStalePruned(t, &loaded)
	})
}

// bucketHasKey reports whether key is present in m; generic so
// assertStalePruned loops over four differently typed buckets within its
// cyclomatic complexity budget.
func bucketHasKey[T any](m map[string]T, key string) bool {
	_, ok := m[key]
	return ok
}

// assertStalePruned checks that every "-stale" and "-future" entry seeded by
// TestSnapshotPrunesStaleAndFutureAcrossAllBuckets's build helper is gone and
// every "-fresh" entry survived, across all four age-eviction buckets.
func assertStalePruned(t *testing.T, loaded *Store) {
	t.Helper()
	cases := []struct {
		bucket                        string
		hasStale, hasFresh, hasFuture bool
	}{
		{
			"api cache",
			bucketHasKey(loaded.APICache, "api-stale"),
			bucketHasKey(loaded.APICache, "api-fresh"),
			bucketHasKey(loaded.APICache, "api-future"),
		},
		{
			"deps cache",
			bucketHasKey(loaded.DepsCache, "deps-stale"),
			bucketHasKey(loaded.DepsCache, "deps-fresh"),
			bucketHasKey(loaded.DepsCache, "deps-future"),
		},
		{
			"versions cache",
			bucketHasKey(loaded.Versions, "versions-stale"),
			bucketHasKey(loaded.Versions, "versions-fresh"),
			bucketHasKey(loaded.Versions, "versions-future"),
		},
		{
			"warmed",
			bucketHasKey(loaded.Warmed, "warmed-stale"),
			bucketHasKey(loaded.Warmed, "warmed-fresh"),
			bucketHasKey(loaded.Warmed, "warmed-future"),
		},
	}
	for _, c := range cases {
		assertBucketRetention(t, c.bucket, c.hasStale, c.hasFresh, c.hasFuture)
	}
}

// assertBucketRetention fails, via Error so every failing bucket is reported,
// unless the bucket's stale and future entries were pruned and its fresh one
// survived.
func assertBucketRetention(t *testing.T, bucket string, hasStale, hasFresh, hasFuture bool) {
	t.Helper()
	if hasStale {
		t.Errorf("expected stale %s entry to be pruned", bucket)
	}
	if !hasFresh {
		t.Errorf("expected fresh %s entry to survive", bucket)
	}
	if hasFuture {
		t.Errorf("expected future %s entry to be pruned", bucket)
	}
}

// TestSnapshotBoundaryEntryIsKept pins that retentionWindow.isStale keeps a
// stamp exactly at either bound of [oldest, newest] and prunes one a nanosecond
// outside; the window is built directly since persist samples the wall clock.
func TestSnapshotBoundaryEntryIsKept(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	w := newRetentionWindow(now, time.Hour)

	if w.isStale(w.oldest) {
		t.Fatalf("expected an entry stamped exactly at the oldest bound to be kept")
	}
	if !w.isStale(w.oldest.Add(-time.Nanosecond)) {
		t.Fatalf("expected an entry stamped one nanosecond before the oldest bound to be pruned")
	}
	if w.isStale(w.newest) {
		t.Fatalf("expected an entry stamped exactly at the newest bound to be kept")
	}
	if !w.isStale(w.newest.Add(time.Nanosecond)) {
		t.Fatalf("expected an entry stamped one nanosecond after the newest bound (future) to be pruned")
	}
}

// TestLoadDropsV3BoltAndRebuilds pins that Load checks the schema before
// decoding any bucket: a v3 snapshot holding pre-v4 bare shapes is dropped for
// a fresh store rather than failing to decode.
func TestLoadDropsV3BoltAndRebuilds(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)

	err := dbs.db.Update(func(tx *bolt.Tx) error {
		metaBucket, err := tx.CreateBucketIfNotExists([]byte(helpers.StoreBucketMeta))
		if err != nil {
			return err
		}
		if err := metaBucket.Put([]byte(helpers.StoreMetaSchemaVersion), []byte(strconv.Itoa(3))); err != nil {
			return err
		}

		versionsBucket, err := tx.CreateBucketIfNotExists([]byte(helpers.StoreBucketVersions))
		if err != nil {
			return err
		}
		if err := versionsBucket.Put([]byte("versions"), []byte(`["1.0.0","2.0.0"]`)); err != nil {
			return err
		}

		depsBucket, err := tx.CreateBucketIfNotExists([]byte(helpers.StoreBucketDepsCache))
		if err != nil {
			return err
		}
		return depsBucket.Put([]byte("deps"), []byte(`{"a.b":">=1.0.0"}`))
	})
	if err != nil {
		t.Fatalf("failed to seed a v3-shape bolt file: %v", err)
	}

	loaded, err := Load(dbs)
	if err != nil {
		t.Fatalf("expected nil error dropping a v3 snapshot, got %v", err)
	}
	if !reflect.DeepEqual(loaded, New()) {
		t.Fatalf("expected a fresh store after dropping a v3 snapshot, got %#v", loaded)
	}
}

// TestLoadToleratesLegacyRootsBucket pins that a leftover legacy "roots" Bolt
// bucket breaks neither Load nor a following Save; whether the bucket survives
// is deliberately not asserted.
func TestLoadToleratesLegacyRootsBucket(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)

	st := New()
	st.SetInstalled("a.b@1.0.0", InstalledEntry{InstallPath: "/tmp/a/b", ArtifactSHA256: testArtifactSHA})
	mustSave(t, dbs, st)

	err := dbs.db.Update(func(tx *bolt.Tx) error {
		// "roots" is spelled out: Store has no roots field, so no constant.
		rootsBucket, err := tx.CreateBucketIfNotExists([]byte("roots"))
		if err != nil {
			return err
		}
		return rootsBucket.Put([]byte("last_run"), []byte(`["a.b@1.0.0"]`))
	})
	if err != nil {
		t.Fatalf("failed to plant a legacy roots bucket: %v", err)
	}

	loaded := mustLoad(t, dbs)
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry: %#v (ok=%v)", installed, ok)
	}

	if err := Save(dbs, loaded); err != nil {
		t.Fatalf("expected Save to succeed with a legacy roots bucket present, got %v", err)
	}
}

// TestSetWarmedIgnoresEmptyKeyOrSHA pins that SetWarmed silently ignores an
// empty key or an empty artifact sha, an entry that could protect nothing.
func TestSetWarmedIgnoresEmptyKeyOrSHA(t *testing.T) {
	t.Parallel()
	st := New()

	st.SetWarmed("", "sha-1")
	st.SetWarmed("a.b@1.0.0", "")

	if got := st.WarmedArtifactSHAByKey(); len(got) != 0 {
		t.Fatalf("expected no warmed entries after empty-key/empty-sha calls, got %#v", got)
	}
}

// TestWarmedArtifactSHAByKeyExcludesStaleEntry pins that
// WarmedArtifactSHAByKey applies the WarmedEntryMaxAge window persist prunes
// against, rather than returning every entry regardless of age.
func TestWarmedArtifactSHAByKeyExcludesStaleEntry(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetWarmed("fresh.key@1.0.0", "sha-fresh")
	// Seed the stale entry directly: SetWarmed always stamps time.Now, so a
	// genuinely stale WarmedAt can only be produced by writing the map field.
	st.Warmed["stale.key@1.0.0"] = WarmedEntry{
		WarmedAt:       time.Now().UTC().Add(-40 * 24 * time.Hour),
		ArtifactSHA256: "sha-stale",
	}

	got := st.WarmedArtifactSHAByKey()
	if _, ok := got["stale.key@1.0.0"]; ok {
		t.Fatalf("expected the stale warmed entry to be excluded, got %#v", got)
	}
	if got["fresh.key@1.0.0"] != "sha-fresh" {
		t.Fatalf("expected the fresh warmed entry to survive, got %#v", got)
	}
}

// TestWarmedArtifactSHAByKeyExcludesFutureEntry pins that a future-stamped
// warmed entry is excluded too, so a corrupt or clock-skewed stamp cannot keep
// its extracted tree forever.
func TestWarmedArtifactSHAByKeyExcludesFutureEntry(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetWarmed("fresh.key@1.0.0", "sha-fresh")
	// Seed the future entry directly: SetWarmed always stamps time.Now, so a
	// future WarmedAt can only be produced by writing the map field.
	st.Warmed["future.key@1.0.0"] = WarmedEntry{
		WarmedAt:       time.Now().UTC().Add(365 * 24 * time.Hour),
		ArtifactSHA256: "sha-future",
	}

	got := st.WarmedArtifactSHAByKey()
	if _, ok := got["future.key@1.0.0"]; ok {
		t.Fatalf("expected the future warmed entry to be excluded, got %#v", got)
	}
	if got["fresh.key@1.0.0"] != "sha-fresh" {
		t.Fatalf("expected the fresh warmed entry to survive, got %#v", got)
	}
}

// TestWarmedArtifactSHAByKeyReturnsIndependentMap pins that the returned map
// is a fresh copy whose mutation never reaches the store's Warmed state.
func TestWarmedArtifactSHAByKeyReturnsIndependentMap(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetWarmed("a.b@1.0.0", "sha-1")

	got := st.WarmedArtifactSHAByKey()
	got["a.b@1.0.0"] = "tampered"
	got["c.d@2.0.0"] = "injected"

	fresh := st.WarmedArtifactSHAByKey()
	if fresh["a.b@1.0.0"] != "sha-1" {
		t.Fatalf("expected the store's own warmed entry to be unaffected by mutating a returned map, got %#v", fresh)
	}
	if _, ok := fresh["c.d@2.0.0"]; ok {
		t.Fatalf("expected an injected key in a returned map to never appear in the store, got %#v", fresh)
	}
}

// assertStoreMapsNonNil fails, via Error so every field is checked and the
// caller still goes on to the mutators, for each of Store's twelve map fields
// that is still nil.
func assertStoreMapsNonNil(t *testing.T, st *Store) {
	t.Helper()
	fields := []struct {
		name  string
		isNil bool
	}{
		{"APICache", st.APICache == nil},
		{"DepsCache", st.DepsCache == nil},
		{"Installed", st.Installed == nil},
		{"Graph", st.Graph == nil},
		{"Requirements", st.Requirements == nil},
		{"Resolved", st.Resolved == nil},
		{"Versions", st.Versions == nil},
		{"GitPins", st.GitPins == nil},
		{"Warmed", st.Warmed == nil},
		{"InstalledRoles", st.InstalledRoles == nil},
		{"RolePins", st.RolePins == nil},
		{"URLPins", st.URLPins == nil},
	}
	for _, f := range fields {
		if f.isNil {
			t.Errorf("expected %s to be non-nil after decode", f.name)
		}
	}
}

// TestUnmarshalJSONRestoresEveryNilMap pins that a payload with every map
// bucket an explicit JSON null decodes into a Store whose mutators still work,
// which only Store.ensureMaps running after the decode makes true.
func TestUnmarshalJSONRestoresEveryNilMap(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	payload := fmt.Sprintf(`{
		"meta": {"schema_version": %d},
		"api_cache": null,
		"deps_cache": null,
		"installed": null,
		"graph": null,
		"requirements": null,
		"resolved": null,
		"versions_cache": null,
		"warmed": null,
		"git_pins": null,
		"installed_roles": null,
		"role_pins": null,
		"url_pins": null
	}`, helpers.StoreSnapshotSchemaVersion)

	st := New()
	if err := json.Unmarshal([]byte(payload), st); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	assertStoreMapsNonNil(t, st)

	populateTestStore(st, fixed)

	assertAPICache(t, st)
	assertDepsCache(t, st)
	assertInstalled(t, st)
	assertGraph(t, st)
	assertRequirements(t, st)
	assertResolved(t, st)
	assertVersions(t, st)
	assertWarmed(t, st)
	assertGitPin(t, st)
	assertInstalledRole(t, st)
	assertRolePin(t, st)
	assertURLPin(t, st)
}

// TestUnmarshalJSONKeepsDecodedData pins that ensureMaps fills only a bucket
// the decode nilled: decoded buckets and Meta survive untouched and the null
// warmed bucket ends non-nil and empty.
func TestUnmarshalJSONKeepsDecodedData(t *testing.T) {
	t.Parallel()
	payload := fmt.Sprintf(`{
		"meta": {
			"schema_version": %d,
			"requirements_hash": "req-hash",
			"server": "https://example.com",
			"last_snapshot": "2024-01-02T03:04:05Z"
		},
		"api_cache": {"api": {"url": "https://example.com/api", "etag": "etag", "body": "eyJvayI6dHJ1ZX0="}},
		"deps_cache": {"deps": {"fetched_at": "2024-01-02T03:04:05Z", "deps": {"a.b": ">=1.0.0"}}},
		"installed": {"a.b@1.0.0": {"artifact_sha256": "abc"}},
		"graph": {"a.b@1.0.0": ["c.d@1.2.3"]},
		"requirements": {"a.b": {"constraint": "1.0.0"}},
		"resolved": {"a.b": {"version": "1.0.0"}},
		"versions_cache": {"versions": {"list": ["1.0.0", "2.0.0"]}},
		"warmed": null
	}`, helpers.StoreSnapshotSchemaVersion)

	st := New()
	if err := json.Unmarshal([]byte(payload), st); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	assertMeta(t, st)
	assertAPICache(t, st)
	assertDepsCache(t, st)
	assertInstalled(t, st)
	assertGraph(t, st)
	assertRequirements(t, st)
	assertResolved(t, st)
	assertVersions(t, st)

	if st.Warmed == nil {
		t.Fatal("expected Warmed to be non-nil after decoding an explicit null")
	}
	if len(st.Warmed) != 0 {
		t.Fatalf("expected Warmed to be empty, got %#v", st.Warmed)
	}
}

// TestUnmarshalJSONPropagatesDecodeError pins that UnmarshalJSON returns a
// decode failure; the payload is type-invalid, not malformed, since
// encoding/json rejects malformed input before calling the Unmarshaler.
func TestUnmarshalJSONPropagatesDecodeError(t *testing.T) {
	t.Parallel()
	st := New()
	err := json.Unmarshal([]byte(`{"installed": 42}`), st)
	if err == nil {
		t.Fatal("expected an error decoding a type-invalid installed bucket, got nil")
	}
}
