package store

import (
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

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
	assertRoots(t, loaded)
	assertResolved(t, loaded)
	assertVersions(t, loaded)
}

func openTestDBs(t *testing.T) *DBs {
	t.Helper()
	dir := t.TempDir()
	dbs, err := OpenDBs(dir)
	if err != nil {
		t.Fatalf("OpenDBs error: %v", err)
	}
	t.Cleanup(func() {
		_ = dbs.Close()
	})
	return dbs
}

func buildTestStore(fixed time.Time) *Store {
	st := New()
	st.SetMetaRequirements("req-hash", "https://example.com")
	st.SetAPICache("api", APICacheEntry{
		URL:       "https://example.com/api",
		ETag:      "etag",
		FetchedAt: fixed,
		TTL:       time.Minute,
		Body:      []byte(`{"ok":true}`),
	})
	st.SetDepsCache("deps", map[string]string{"a.b": ">=1.0.0"})
	st.SetInstalled("a.b@1.0.0", InstalledEntry{
		InstallPath:    "/tmp/a/b",
		Source:         "https://example.com",
		ArtifactSHA256: "abc",
		InstalledAt:    fixed,
		Deps:           []string{"c.d@1.2.3"},
	})
	st.SetGraph("a.b@1.0.0", []string{"c.d@1.2.3"})
	st.SetRequirements(map[string]RequirementSpec{
		"a.b": {Constraint: "1.0.0", Source: "https://example.com", Type: "galaxy"},
	})
	st.SetRoots("last_run", []string{"a.b@1.0.0"})
	st.SetResolvedAll(map[string]ResolvedEntry{
		"a.b": {Version: "1.0.0", Source: "https://example.com"},
	})
	st.SetVersionsCache("versions", []string{"1.0.0", "2.0.0"})
	return st
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
	if !ok || deps["a.b"] != ">=1.0.0" {
		t.Fatalf("unexpected deps cache: %#v", deps)
	}
}

func assertInstalled(t *testing.T, loaded *Store) {
	t.Helper()
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != "abc" {
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

func assertRoots(t *testing.T, loaded *Store) {
	t.Helper()
	roots := loaded.Roots["last_run"]
	if len(roots) != 1 || roots[0] != "a.b@1.0.0" {
		t.Fatalf("unexpected roots: %#v", roots)
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
