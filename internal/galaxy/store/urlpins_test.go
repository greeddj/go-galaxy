package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestURLPinCloneOnWriteAndRead proves SetURLPin and GetURLPin both deep-copy:
// a caller mutating the value it passed in, or the one it got back, never
// reaches the stored entry - the contract SetGitPin holds for Collections.
func TestURLPinCloneOnWriteAndRead(t *testing.T) {
	t.Parallel()
	const name = "app"
	st := New()
	entry := URLPinEntry{
		SHA256:       testURLPinSHA,
		Namespace:    "acme",
		Name:         name,
		Version:      "1.2.3",
		Dependencies: map[string]string{"a.b": testDepsConstraint},
	}
	st.SetURLPin(testURLPinKey, entry)
	entry.Dependencies["a.b"] = mutatedMarker
	entry.Name = mutatedMarker

	got, ok := st.GetURLPin(testURLPinKey)
	if !ok {
		t.Fatalf("pin missing")
	}
	if got.Name != name || got.Dependencies["a.b"] != testDepsConstraint {
		t.Fatalf("SetURLPin did not clone: %#v", got)
	}
	got.Dependencies["a.b"] = mutatedReadMarker
	again, _ := st.GetURLPin(testURLPinKey)
	if again.Name != name || again.Dependencies["a.b"] != testDepsConstraint {
		t.Fatalf("GetURLPin did not clone: %#v", again)
	}
	if again.FetchedAt.IsZero() || time.Since(again.FetchedAt) > time.Minute {
		t.Fatalf("FetchedAt not stamped with the current time: %v", again.FetchedAt)
	}
}

// TestURLPinIgnoresEmptyKeyOrSHA proves a pin naming no sha256, or no key, is
// never recorded: such an entry would replay into a failure.
func TestURLPinIgnoresEmptyKeyOrSHA(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetURLPin("", URLPinEntry{SHA256: testURLPinSHA})
	st.SetURLPin(testURLPinKey, URLPinEntry{Namespace: "acme", Name: "app", Version: "1.2.3"})
	if len(st.URLPins) != 0 || st.Dirty() {
		t.Fatalf("an empty key or sha was recorded: %#v", st.URLPins)
	}
	if _, ok := st.GetURLPin(testURLPinKey); ok {
		t.Fatalf("GetURLPin found a pin that was never set")
	}
	var nilStore *Store
	nilStore.SetURLPin(testURLPinKey, URLPinEntry{SHA256: testURLPinSHA})
	if _, ok := nilStore.GetURLPin(testURLPinKey); ok {
		t.Fatalf("nil store answered a pin")
	}
}

// TestRolePinAcceptsSHAWithoutCommit pins the guard SetRolePin holds for url
// role pins: an entry naming a sha256 and no commit is a legal pin, while one
// naming neither is still ignored.
func TestRolePinAcceptsSHAWithoutCommit(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetRolePin(testRolePinKey, RolePinEntry{URL: testURLPinKey, SHA256: testURLPinSHA})
	if _, ok := st.GetRolePin(testRolePinKey); !ok {
		t.Fatalf("a url role pin naming a sha256 was not recorded")
	}
	st2 := New()
	st2.SetRolePin(testRolePinKey, RolePinEntry{URL: testURLPinKey})
	if len(st2.RolePins) != 0 {
		t.Fatalf("a role pin naming neither a commit nor a sha256 was recorded")
	}
}

// TestClearCachesDropsURLPins pins that --clear-cache forgets url pins along
// with the other remote answers, so the next resolve re-downloads.
func TestClearCachesDropsURLPins(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetURLPin(testURLPinKey, URLPinEntry{SHA256: testURLPinSHA})
	st.ClearCaches()
	if _, ok := st.GetURLPin(testURLPinKey); ok {
		t.Fatalf("ClearCaches kept the url pin")
	}
}

// TestURLPinsSurviveSnapshotWithoutAWindow proves a pin older than every
// retention window still round-trips through snapshotData: pins are
// invalidated by the requirements signature and --refresh, never by age.
func TestURLPinsSurviveSnapshotWithoutAWindow(t *testing.T) {
	t.Parallel()
	st := New()
	st.URLPins[testURLPinKey] = URLPinEntry{
		FetchedAt: time.Now().UTC().Add(-400 * 24 * time.Hour),
		SHA256:    testURLPinSHA,
	}
	data := st.snapshotData()
	if _, ok := data.URLPins[testURLPinKey]; !ok {
		t.Fatalf("an old url pin was evicted from the snapshot")
	}
}

// TestSchemaIsNineAndASchemaEightSnapshotIsDropped pins the bump that added
// url pins: the schema is 9 and a schema-8 Bolt snapshot is dropped on Load.
// The literal 8 is deliberate, so a later bump cannot keep this passing.
func TestSchemaIsNineAndASchemaEightSnapshotIsDropped(t *testing.T) {
	t.Parallel()
	if helpers.StoreSnapshotSchemaVersion != 9 {
		t.Fatalf("StoreSnapshotSchemaVersion = %d, want 9", helpers.StoreSnapshotSchemaVersion)
	}
	if err := ValidateSchema(8); !errors.Is(err, helpers.ErrOutdatedSchemaVersion) {
		t.Fatalf("ValidateSchema(8) = %v, want ErrOutdatedSchemaVersion", err)
	}

	dbs := openTestDBs(t)
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	st := buildTestStore(fixed)
	st.URLPins = make(map[string]URLPinEntry)
	mustSave(t, dbs, st)
	stampSchemaVersion(t, dbs, 8)

	loaded := mustLoad(t, dbs)
	if !reflect.DeepEqual(loaded, New()) {
		t.Fatalf("a schema-8 snapshot was not dropped: %#v", loaded)
	}
}
