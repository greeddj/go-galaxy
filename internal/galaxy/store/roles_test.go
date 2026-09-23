package store

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// mutatedReadMarker is written into a slice a Get* call returned, to prove
// the read handed back a copy; mutatedMarker plays that part for a write.
const mutatedReadMarker = "mutated-read"

// testRoleDep is the one dependency the role fixtures in this file declare.
const testRoleDep = "common"

// TestInstalledRoleCloneOnWriteAndRead pins that SetInstalledRole and
// GetInstalledRole both clone Deps, the contract SetInstalled and GetInstalled
// hold for a collection.
func TestInstalledRoleCloneOnWriteAndRead(t *testing.T) {
	t.Parallel()
	st := New()
	deps := []string{testRoleDep}
	st.SetInstalledRole(testRoleName, InstalledRoleEntry{ArtifactSHA256: testRoleArtifactSHA, Deps: deps})
	deps[0] = mutatedMarker

	got, ok := st.GetInstalledRole(testRoleName)
	if !ok || got.Deps[0] != testRoleDep {
		t.Fatalf("SetInstalledRole did not clone Deps: %#v (ok=%t)", got, ok)
	}
	got.Deps[0] = mutatedReadMarker
	again, _ := st.GetInstalledRole(testRoleName)
	if again.Deps[0] != testRoleDep {
		t.Fatalf("GetInstalledRole did not clone Deps: %#v", again)
	}
}

// TestInstalledRoleMissingAndNil pins the miss shape: an unknown name answers
// a zero entry and false, a nil store answers the same and ignores a write.
func TestInstalledRoleMissingAndNil(t *testing.T) {
	t.Parallel()
	st := New()
	if got, ok := st.GetInstalledRole("absent"); ok || got.Deps != nil {
		t.Fatalf("unknown role answered %#v (ok=%t)", got, ok)
	}
	var nilStore *Store
	nilStore.SetInstalledRole(testRoleName, InstalledRoleEntry{ArtifactSHA256: testRoleArtifactSHA})
	nilStore.DeleteInstalledRole(testRoleName)
	if _, ok := nilStore.GetInstalledRole(testRoleName); ok {
		t.Fatalf("nil store answered a role")
	}
	if nilStore.InstalledRolesSnapshot() != nil || nilStore.InstalledRoleArtifactSHAs() != nil {
		t.Fatalf("nil store answered a non-nil map")
	}
}

// TestDeleteInstalledRoleRemovesOnlyThatName proves a delete is keyed: the
// named role goes, its neighbor stays.
func TestDeleteInstalledRoleRemovesOnlyThatName(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetInstalledRole(testRoleName, InstalledRoleEntry{ArtifactSHA256: testRoleArtifactSHA})
	st.SetInstalledRole(testRoleDep, InstalledRoleEntry{ArtifactSHA256: "common-sha"})
	st.DeleteInstalledRole(testRoleName)
	if _, ok := st.GetInstalledRole(testRoleName); ok {
		t.Fatalf("deleted role still answered")
	}
	if _, ok := st.GetInstalledRole(testRoleDep); !ok {
		t.Fatalf("a neighbor was deleted too")
	}
}

// TestInstalledRolesSnapshotIsDeepCopy proves the map InstalledRolesSnapshot
// returns is independent of the store down to each entry's Deps slice, so
// cleanup can walk and mutate it freely.
func TestInstalledRolesSnapshotIsDeepCopy(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetInstalledRole(testRoleName, InstalledRoleEntry{ArtifactSHA256: testRoleArtifactSHA, Deps: []string{testRoleDep}})

	snap := st.InstalledRolesSnapshot()
	snap[testRoleName].Deps[0] = mutatedMarker
	delete(snap, testRoleName)
	snap["injected"] = InstalledRoleEntry{}

	got, ok := st.GetInstalledRole(testRoleName)
	if !ok || got.Deps[0] != testRoleDep {
		t.Fatalf("mutating the snapshot reached the store: %#v (ok=%t)", got, ok)
	}
	if _, ok := st.GetInstalledRole("injected"); ok {
		t.Fatalf("a key injected into the snapshot appeared in the store")
	}
}

// TestInstalledRoleArtifactSHAsOmitsEmptyAndIsIndependent proves the keep-set
// half for roles skips an entry with no sha, as InstalledArtifactSHAByKey
// does, and hands back a map the caller may mutate.
func TestInstalledRoleArtifactSHAsOmitsEmptyAndIsIndependent(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetInstalledRole(testRoleName, InstalledRoleEntry{ArtifactSHA256: testRoleArtifactSHA})
	st.SetInstalledRole("no-sha", InstalledRoleEntry{InstallPath: "/tmp/roles/no-sha"})

	got := st.InstalledRoleArtifactSHAs()
	if want := map[string]string{testRoleName: testRoleArtifactSHA}; !reflect.DeepEqual(got, want) {
		t.Fatalf("InstalledRoleArtifactSHAs = %#v, want %#v", got, want)
	}
	got[testRoleName] = "tampered"
	if again := st.InstalledRoleArtifactSHAs(); again[testRoleName] != testRoleArtifactSHA {
		t.Fatalf("mutating the returned map reached the store: %#v", again)
	}
}

// TestRolePinCloneOnWriteAndRead proves SetRolePin and GetRolePin both
// deep-copy Deps, and that a zero FetchedAt is stamped with the current time.
func TestRolePinCloneOnWriteAndRead(t *testing.T) {
	t.Parallel()
	st := New()
	entry := RolePinEntry{
		Repository: testRoleRepository,
		Commit:     testGitPinCommit,
		Version:    "v1.2.3",
		Deps:       []RolePinDep{{Src: "acme.common", Name: testRoleDep}},
	}
	st.SetRolePin(testRolePinKey, entry)
	entry.Deps[0].Name = mutatedMarker

	got, ok := st.GetRolePin(testRolePinKey)
	if !ok || got.Deps[0].Name != testRoleDep {
		t.Fatalf("SetRolePin did not clone Deps: %#v (ok=%t)", got, ok)
	}
	got.Deps[0].Name = mutatedReadMarker
	again, _ := st.GetRolePin(testRolePinKey)
	if again.Deps[0].Name != testRoleDep {
		t.Fatalf("GetRolePin did not clone Deps: %#v", again)
	}
	if again.FetchedAt.IsZero() || time.Since(again.FetchedAt) > time.Minute {
		t.Fatalf("FetchedAt not stamped with the current time: %v", again.FetchedAt)
	}
}

// TestRolePinKeepsACallerFetchedAt proves a non-zero FetchedAt is kept as
// given, so a caller replaying a pin it already holds does not rewrite its
// stamp.
func TestRolePinKeepsACallerFetchedAt(t *testing.T) {
	t.Parallel()
	st := New()
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	st.SetRolePin(testRolePinKey, RolePinEntry{Commit: testGitPinCommit, FetchedAt: fixed})
	got, ok := st.GetRolePin(testRolePinKey)
	if !ok || !got.FetchedAt.Equal(fixed) {
		t.Fatalf("FetchedAt = %v (ok=%t), want %v", got.FetchedAt, ok, fixed)
	}
}

// TestRolePinIgnoresEmptyKeyOrCommit proves a pin naming no commit, or no
// key, is never recorded: such an entry would replay into a failure.
func TestRolePinIgnoresEmptyKeyOrCommit(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetRolePin("", RolePinEntry{Commit: testGitPinCommit})
	st.SetRolePin(testRolePinKey, RolePinEntry{Repository: testRoleRepository})
	if len(st.RolePins) != 0 || st.Dirty() {
		t.Fatalf("an empty key or commit was recorded: %#v", st.RolePins)
	}
	if _, ok := st.GetRolePin(testRolePinKey); ok {
		t.Fatalf("GetRolePin found a pin that was never set")
	}
	var nilStore *Store
	nilStore.SetRolePin(testRolePinKey, RolePinEntry{Commit: testGitPinCommit})
	nilStore.DeleteRolePin(testRolePinKey)
	if _, ok := nilStore.GetRolePin(testRolePinKey); ok {
		t.Fatalf("nil store answered a pin")
	}
}

// TestDeleteRolePinRemovesOnlyThatKey proves a delete is keyed.
func TestDeleteRolePinRemovesOnlyThatKey(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetRolePin(testRolePinKey, RolePinEntry{Commit: testGitPinCommit})
	st.SetRolePin("other", RolePinEntry{Commit: testGitPinCommit})
	st.DeleteRolePin(testRolePinKey)
	if _, ok := st.GetRolePin(testRolePinKey); ok {
		t.Fatalf("deleted pin still answered")
	}
	if _, ok := st.GetRolePin("other"); !ok {
		t.Fatalf("a neighbor was deleted too")
	}
}

// TestClearCachesDropsRolePinsKeepsInstalledRoles pins that ClearCaches drops a
// role pin, a remote's answer, but keeps an installed role, a record of content
// on disk, as it does for git pins and installed collections.
func TestClearCachesDropsRolePinsKeepsInstalledRoles(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetRolePin(testRolePinKey, RolePinEntry{Commit: testGitPinCommit})
	st.SetInstalledRole(testRoleName, InstalledRoleEntry{ArtifactSHA256: testRoleArtifactSHA})
	st.ClearCaches()
	if _, ok := st.GetRolePin(testRolePinKey); ok {
		t.Fatalf("ClearCaches kept the role pin")
	}
	if _, ok := st.GetInstalledRole(testRoleName); !ok {
		t.Fatalf("ClearCaches dropped the installed role")
	}
}

// TestRolePinsSurviveSnapshotWithoutAWindow pins that a role pin and an
// installed role older than every retention window still survive
// snapshotData: like git pins, they are never evicted by age.
func TestRolePinsSurviveSnapshotWithoutAWindow(t *testing.T) {
	t.Parallel()
	st := New()
	st.RolePins[testRolePinKey] = RolePinEntry{
		FetchedAt: time.Now().UTC().Add(-400 * 24 * time.Hour),
		Commit:    testGitPinCommit,
	}
	st.InstalledRoles[testRoleName] = InstalledRoleEntry{
		InstalledAt:    time.Now().UTC().Add(-400 * 24 * time.Hour),
		ArtifactSHA256: testRoleArtifactSHA,
	}
	data := st.snapshotData()
	if _, ok := data.RolePins[testRolePinKey]; !ok {
		t.Fatalf("an old role pin was evicted from the snapshot")
	}
	if _, ok := data.InstalledRoles[testRoleName]; !ok {
		t.Fatalf("an old installed role was evicted from the snapshot")
	}
}

// TestOnlyAnInstalledRoleCountsAsContent pins that an installed role alone
// makes a save stamp ContentRecorded, as an installed collection does, while a
// role pin alone, a remote's answer, does not.
func TestOnlyAnInstalledRoleCountsAsContent(t *testing.T) {
	t.Parallel()

	pinOnly := New()
	pinOnly.SetRolePin(testRolePinKey, RolePinEntry{Commit: testGitPinCommit})
	if pinOnly.hasContentEntries() {
		t.Fatalf("a role pin alone counted as content")
	}

	roleOnly := New()
	roleOnly.SetInstalledRole(testRoleName, InstalledRoleEntry{ArtifactSHA256: testRoleArtifactSHA})
	if !roleOnly.hasContentEntries() {
		t.Fatalf("an installed role alone did not count as content")
	}
	dbs := openTestDBs(t)
	mustSave(t, dbs, roleOnly)
	if loaded := mustLoad(t, dbs); !loaded.HasRecordedContent() {
		t.Fatalf("a save carrying only an installed role did not stamp ContentRecorded")
	}
}

// TestRoleBucketsRoundTripJSON proves the two role buckets survive
// MarshalSnapshot and a decode of its output - the S3 backend's path - and
// that an explicit null for either leaves the map writable.
func TestRoleBucketsRoundTripJSON(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	payload, err := buildTestStore(fixed).MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot error: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(payload, &keys); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	for _, bucket := range []string{helpers.StoreBucketInstalledRoles, helpers.StoreBucketRolePins} {
		if _, ok := keys[bucket]; !ok {
			t.Fatalf("MarshalSnapshot wrote no %q key: %s", bucket, payload)
		}
	}

	decoded := New()
	if err := json.Unmarshal(payload, decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	assertInstalledRole(t, decoded)
	assertRolePin(t, decoded)

	nulled := New()
	if err := json.Unmarshal([]byte(`{"installed_roles": null, "role_pins": null}`), nulled); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	if nulled.InstalledRoles == nil || nulled.RolePins == nil {
		t.Fatalf("an explicit null left a role map nil")
	}
	nulled.SetInstalledRole(testRoleName, InstalledRoleEntry{ArtifactSHA256: testRoleArtifactSHA})
	nulled.SetRolePin(testRolePinKey, RolePinEntry{Commit: testGitPinCommit})
}

// The schema-bump pin test lives with the bump that last moved the version:
// see TestSchemaIsNineAndASchemaEightSnapshotIsDropped in urlpins_test.go.
