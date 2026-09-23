package store

import (
	"encoding/json"
	"testing"
	"time"
)

// TestDirtyIsFalseAfterEveryLoadPath pins that Dirty starts false after New, a
// local Save-then-Load and an unmarshal of MarshalSnapshot (the S3 load shape),
// and that one SetGraph on the same store then flips it.
func TestDirtyIsFalseAfterEveryLoadPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		build func(t *testing.T) *Store
		name  string
	}{
		{name: "New", build: func(t *testing.T) *Store {
			t.Helper()
			return New()
		}},
		{name: "Save-then-Load", build: func(t *testing.T) *Store {
			t.Helper()
			dbs := openTestDBs(t)
			fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
			mustSave(t, dbs, buildTestStore(fixed))
			return mustLoad(t, dbs)
		}},
		{name: "MarshalSnapshot-then-Unmarshal", build: func(t *testing.T) *Store {
			t.Helper()
			fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
			data, err := buildTestStore(fixed).MarshalSnapshot()
			if err != nil {
				t.Fatalf("MarshalSnapshot error: %v", err)
			}
			decoded := New()
			if err := json.Unmarshal(data, decoded); err != nil {
				t.Fatalf("json.Unmarshal error: %v", err)
			}
			return decoded
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := tc.build(t)
			if st.Dirty() {
				t.Fatalf("expected Dirty() == false immediately after %s, got true", tc.name)
			}

			// Positive control: a single mutator call on this same store must
			// flip Dirty to true.
			st.SetGraph("a.b@1.0.0", []string{"c.d@1.2.3"})
			if !st.Dirty() {
				t.Fatalf("expected Dirty() == true after SetGraph on the %s fixture, got false", tc.name)
			}
		})
	}
}

// dirtyMutatorCase is one row of TestEveryMutatorMarksDirty: a mutator call
// against a fresh store, and the name reported on failure.
type dirtyMutatorCase struct {
	call func(st *Store)
	name string
}

// dirtyMutatorCases lists every write-locked Store mutator, one row each, kept
// in sync by hand; TestEveryWriteLockedStoreMethodMarksDirty is its structural
// backstop for the mutators in snapshot.go.
func dirtyMutatorCases() []dirtyMutatorCase {
	return []dirtyMutatorCase{
		{name: "SetInstalled", call: func(st *Store) {
			st.SetInstalled("a.b@1.0.0", InstalledEntry{ArtifactSHA256: testArtifactSHA})
		}},
		{name: "DeleteInstalled", call: func(st *Store) {
			st.DeleteInstalled("a.b@1.0.0")
		}},
		{name: "SetWarmed", call: func(st *Store) {
			st.SetWarmed("a.b@1.0.0", "warmed-sha")
		}},
		{name: "SetGitPin", call: func(st *Store) {
			st.SetGitPin(testGitPinKey, GitPinEntry{Commit: testGitPinCommit})
		}},
		{name: "SetInstalledRole", call: func(st *Store) {
			st.SetInstalledRole(testRoleName, InstalledRoleEntry{ArtifactSHA256: testRoleArtifactSHA})
		}},
		{name: "DeleteInstalledRole", call: func(st *Store) {
			st.DeleteInstalledRole(testRoleName)
		}},
		{name: "SetRolePin", call: func(st *Store) {
			st.SetRolePin(testRolePinKey, RolePinEntry{Commit: testGitPinCommit})
		}},
		{name: "DeleteRolePin", call: func(st *Store) {
			st.DeleteRolePin(testRolePinKey)
		}},
		{name: "SetURLPin", call: func(st *Store) { st.SetURLPin(testURLPinKey, URLPinEntry{SHA256: testURLPinSHA}) }},
		{name: "SetDepsCache", call: func(st *Store) {
			st.SetDepsCache("deps", map[string]string{"a.b": testDepsConstraint})
		}},
		{name: "DeleteDepsCache", call: func(st *Store) {
			st.DeleteDepsCache("deps")
		}},
		{name: "SetAPICache", call: func(st *Store) {
			st.SetAPICache("api", APICacheEntry{URL: "https://example.com/api"})
		}},
		{name: "ClearCaches", call: func(st *Store) {
			st.ClearCaches()
		}},
		{name: "SetVersionsCache", call: func(st *Store) {
			st.SetVersionsCache("versions", []string{"1.0.0"})
		}},
		{name: "SetResolvedAll", call: func(st *Store) {
			st.SetResolvedAll(map[string]ResolvedEntry{"a.b": {Version: "1.0.0"}})
		}},
		{name: "SetGraph", call: func(st *Store) {
			st.SetGraph("a.b@1.0.0", []string{"c.d@1.2.3"})
		}},
		{name: "DeleteGraph", call: func(st *Store) {
			st.DeleteGraph("a.b@1.0.0")
		}},
		{name: "SetGraphSnapshot", call: func(st *Store) {
			st.SetGraphSnapshot(map[string][]string{"a.b@1.0.0": {"c.d@1.2.3"}})
		}},
		{name: "SetRequirements", call: func(st *Store) {
			st.SetRequirements(map[string]RequirementSpec{"a.b": {Constraint: "1.0.0"}})
		}},
		{name: "SetMetaRequirements", call: func(st *Store) {
			st.SetMetaRequirements("req-hash", "https://example.com")
		}},
	}
}

// TestEveryMutatorMarksDirty pins that one call to each write-locked mutator on
// a fresh store sets Dirty, even a delete of an absent key: the flag is set
// unconditionally, not only when a value changed.
func TestEveryMutatorMarksDirty(t *testing.T) {
	t.Parallel()

	for _, tc := range dirtyMutatorCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := New()
			if st.Dirty() {
				t.Fatalf("expected a fresh New() store to report Dirty() == false before %s", tc.name)
			}
			tc.call(st)
			if !st.Dirty() {
				t.Fatalf("expected Dirty() == true after %s, got false", tc.name)
			}
		})
	}
}
