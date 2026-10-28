package store

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
)

// mutateNilStore calls every mutator on a nil *Store; each must return
// without touching memory.
func mutateNilStore(s *Store) {
	s.SetInstalled("a.b@1.0.0", InstalledEntry{ArtifactSHA256: "abc"})
	s.DeleteInstalled("a.b@1.0.0")
	s.SetDepsCache("a.b@1.0.0", map[string]string{"c.d": testDepsConstraint})
	s.DeleteDepsCache("a.b@1.0.0")
	s.SetAPICache("k", APICacheEntry{URL: "https://example.com"})
	s.ClearCaches()
	s.SetVersionsCache("a.b", []string{"1.0.0"})
	s.SetResolvedAll(map[string]ResolvedEntry{"a.b": {Version: "1.0.0"}})
	s.SetGraph("a.b@1.0.0", []string{"c.d@1.0.0"})
	s.DeleteGraph("a.b@1.0.0")
	s.SetGraphSnapshot(map[string][]string{"a.b@1.0.0": nil})
	s.SetRequirements(map[string]RequirementSpec{"a.b": {Constraint: "*"}})
	s.SetMetaRequirements("hash", "https://galaxy.example")
}

// TestNilStoreIsInert pins the nil-receiver contract: mutators do nothing and
// readers answer empty, so a caller without a snapshot never panics and never
// mistakes a nil store for one holding records.
func TestNilStoreIsInert(t *testing.T) {
	t.Parallel()
	var s *Store
	mutateNilStore(s)

	_, installedHit := s.GetInstalled("a.b@1.0.0")
	_, depsHit := s.GetDepsCache("a.b@1.0.0")
	_, apiHit := s.GetAPICache("k")
	_, versionsHit := s.GetVersionsCache("a.b")
	violations := map[string]bool{
		"GetInstalled reported a hit":                  installedHit,
		"GetDepsCache reported a hit":                  depsHit,
		"GetAPICache reported a hit":                   apiHit,
		"GetVersionsCache reported a hit":              versionsHit,
		"InstalledArtifactSHAByKey is non-nil":         s.InstalledArtifactSHAByKey() != nil,
		"WarmedArtifactSHAByKey is non-nil":            s.WarmedArtifactSHAByKey() != nil,
		"ResolvedSnapshot is non-nil":                  s.ResolvedSnapshot() != nil,
		"GraphSnapshot is non-nil":                     s.GraphSnapshot() != nil,
		"RequirementsSnapshot is non-nil":              s.RequirementsSnapshot() != nil,
		"MetaSnapshot is not the zero value":           s.MetaSnapshot() != (SnapshotMeta{}),
		"HasRecordedContent reported recorded content": s.HasRecordedContent(),
		"hasContentEntries reported entries":           s.hasContentEntries(),
		"Dirty reported a mutation":                    s.Dirty(),
	}
	for violation, happened := range violations {
		if happened {
			t.Errorf("nil store: %s", violation)
		}
	}
}

// TestCacheReadsMissWithoutAnEntry pins that the deps and versions caches
// answer a miss as (nil, false), not an empty hit a caller would trust.
func TestCacheReadsMissWithoutAnEntry(t *testing.T) {
	t.Parallel()
	st := New()
	if deps, ok := st.GetDepsCache("a.b@1.0.0"); ok || deps != nil {
		t.Errorf("GetDepsCache miss = %v, %v; want nil, false", deps, ok)
	}
	if versions, ok := st.GetVersionsCache("a.b"); ok || versions != nil {
		t.Errorf("GetVersionsCache miss = %v, %v; want nil, false", versions, ok)
	}
}

// TestMetaSnapshotReportsTheCurrentMeta pins that MetaSnapshot carries the
// schema of New and what SetMetaRequirements recorded.
func TestMetaSnapshotReportsTheCurrentMeta(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetMetaRequirements("req-hash", "https://galaxy.example")
	meta := st.MetaSnapshot()
	if meta.SchemaVersion != helpers.StoreSnapshotSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", meta.SchemaVersion, helpers.StoreSnapshotSchemaVersion)
	}
	if meta.RequirementsHash != "req-hash" || meta.Server != "https://galaxy.example" {
		t.Errorf("MetaSnapshot = %+v, want the recorded hash and server", meta)
	}
}

// TestLoadWithoutADatabaseIsAFreshStore pins that Load of no handle is an
// empty current-schema store that claims no persisted snapshot.
func TestLoadWithoutADatabaseIsAFreshStore(t *testing.T) {
	t.Parallel()
	for name, dbs := range map[string]*DBs{"nil DBs": nil, "DBs without a handle": {}} {
		st, err := Load(dbs)
		if err != nil {
			t.Fatalf("%s: Load = %v, want nil", name, err)
		}
		if st.Meta.SchemaVersion != helpers.StoreSnapshotSchemaVersion || st.WasPersisted() || st.Dirty() {
			t.Fatalf("%s: Load = %+v, want a fresh, unpersisted, clean store", name, st.Meta)
		}
	}
}

// TestSaveRefusesAMissingDatabaseOrStore pins the two guard sentinels.
func TestSaveRefusesAMissingDatabaseOrStore(t *testing.T) {
	t.Parallel()
	if err := Save(nil, New()); !errors.Is(err, helpers.ErrDbNil) {
		t.Errorf("Save(nil DBs) = %v, want helpers.ErrDbNil", err)
	}
	if err := Save(&DBs{}, New()); !errors.Is(err, helpers.ErrDbNil) {
		t.Errorf("Save(DBs without a handle) = %v, want helpers.ErrDbNil", err)
	}
	if err := Save(openTestDBs(t), nil); !errors.Is(err, helpers.ErrStoreNil) {
		t.Errorf("Save(nil store) = %v, want helpers.ErrStoreNil", err)
	}
}

// TestLoadOfANewDatabaseKeepsTheCurrentSchema pins that a database with no
// meta bucket yet, a first run's, loads as an empty current-schema store.
func TestLoadOfANewDatabaseKeepsTheCurrentSchema(t *testing.T) {
	t.Parallel()
	st := mustLoad(t, openTestDBs(t))
	if st.Meta.SchemaVersion != helpers.StoreSnapshotSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", st.Meta.SchemaVersion, helpers.StoreSnapshotSchemaVersion)
	}
	if st.WasPersisted() || len(st.Installed) != 0 || len(st.APICache) != 0 {
		t.Fatalf("Load of a new database = %+v, want an empty unpersisted store", st.Meta)
	}
}

// putMetaValues writes values into the meta bucket, creating it, so a test can
// plant exactly the keys an older or damaged writer would leave.
func putMetaValues(t *testing.T, dbs *DBs, values map[string]string) {
	t.Helper()
	err := dbs.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(helpers.StoreBucketMeta))
		if err != nil {
			return err
		}
		for key, value := range values {
			if err := bucket.Put([]byte(key), []byte(value)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("putMetaValues: %v", err)
	}
}

// TestLoadRejectsDamagedMetaValues pins that a meta value that does not parse
// fails Load with an error naming which value, never a guessed default.
func TestLoadRejectsDamagedMetaValues(t *testing.T) {
	t.Parallel()
	current := strconv.Itoa(helpers.StoreSnapshotSchemaVersion)
	cases := []struct {
		values map[string]string
		want   string
	}{
		{values: map[string]string{helpers.StoreMetaSchemaVersion: "four"}, want: "invalid schema version"},
		{
			values: map[string]string{helpers.StoreMetaSchemaVersion: current, helpers.StoreMetaLastSnapshot: "yesterday"},
			want:   "invalid snapshot time",
		},
		{
			values: map[string]string{helpers.StoreMetaSchemaVersion: current, helpers.StoreMetaContentRecorded: "never"},
			want:   "invalid content-recorded time",
		},
	}
	for _, tc := range cases {
		dbs := openTestDBs(t)
		putMetaValues(t, dbs, tc.values)
		st, err := Load(dbs)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Load with meta %v = %v, want an error containing %q", tc.values, err, tc.want)
		}
		if st != nil {
			t.Errorf("Load with meta %v returned a store alongside its error", tc.values)
		}
	}
}

// TestLoadDropsAnUnstampedSnapshot pins that a meta bucket without a schema
// key reads as version 0 and is dropped, its data never trusted.
func TestLoadDropsAnUnstampedSnapshot(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	st := New()
	st.SetInstalled("a.b@1.0.0", InstalledEntry{ArtifactSHA256: testArtifactSHA})
	mustSave(t, dbs, st)
	err := dbs.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(helpers.StoreBucketMeta)).Delete([]byte(helpers.StoreMetaSchemaVersion))
	})
	if err != nil {
		t.Fatalf("removing the schema key: %v", err)
	}

	loaded := mustLoad(t, dbs)
	if _, ok := loaded.GetInstalled("a.b@1.0.0"); ok {
		t.Fatal("an unstamped snapshot's installed entry survived Load, want it dropped")
	}
	if loaded.Meta.SchemaVersion != helpers.StoreSnapshotSchemaVersion || loaded.WasPersisted() {
		t.Fatalf("Load of an unstamped snapshot = %+v, want a fresh store", loaded.Meta)
	}
}

// TestSaveLoadKeepsMetaTimesToTheNanosecond pins that LastSnapshot and
// ContentRecorded round-trip through Bolt at full precision.
func TestSaveLoadKeepsMetaTimesToTheNanosecond(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	putMetaValues(t, dbs, map[string]string{
		helpers.StoreMetaSchemaVersion:   strconv.Itoa(helpers.StoreSnapshotSchemaVersion),
		helpers.StoreMetaLastSnapshot:    stamp.Format(time.RFC3339Nano),
		helpers.StoreMetaContentRecorded: stamp.Format(time.RFC3339Nano),
	})
	loaded := mustLoad(t, dbs)
	if !loaded.Meta.LastSnapshot.Equal(stamp) || !loaded.Meta.ContentRecorded.Equal(stamp) {
		t.Fatalf("meta times = %v, %v; want %v for both", loaded.Meta.LastSnapshot, loaded.Meta.ContentRecorded, stamp)
	}
	if !loaded.WasPersisted() || !loaded.HasRecordedContent() {
		t.Fatal("a loaded snapshot with both stamps reports no persisted snapshot or no recorded content")
	}
}
