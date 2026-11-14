package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// commitTimeSource is the git locator the commit-time fixtures record.
const commitTimeSource = "git+https://git.example/acme/app.git#@d4e99adaaa78873baaafbdb953a001315081c0f0"

// assertCommitTimes checks the records commitTimeStore wrote: the dated ones
// keep their time, the undated one stays zero.
func assertCommitTimes(t *testing.T, path string, st *Store, when time.Time) {
	t.Helper()
	if entry, ok := st.GetInstalled("acme.app@1.0.0"); !ok || !entry.CommitTime.Equal(when) {
		t.Fatalf("%s: collection record = (%+v, %v), want commit time %v", path, entry, ok, when)
	}
	if entry, ok := st.GetInstalled("acme.lib@1.0.0"); !ok || !entry.CommitTime.IsZero() {
		t.Fatalf("%s: Galaxy collection record = (%+v, %v), want no commit time", path, entry, ok)
	}
	if entry, ok := st.GetInstalledRole("app"); !ok || !entry.CommitTime.Equal(when) {
		t.Fatalf("%s: role record = (%+v, %v), want commit time %v", path, entry, ok, when)
	}
}

// TestInstalledCommitTimeRoundTrips pins that an install record's commit time
// survives both persistence paths, bbolt and MarshalSnapshot, the second of
// which is the S3 backend's.
func TestInstalledCommitTimeRoundTrips(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, time.October, 29, 13, 55, 0, 0, time.UTC)
	st := New()
	st.SetInstalled("acme.app@1.0.0", InstalledEntry{Source: commitTimeSource, ArtifactSHA256: testArtifactSHA, CommitTime: when})
	st.SetInstalled("acme.lib@1.0.0", InstalledEntry{Source: "https://galaxy.example", ArtifactSHA256: testArtifactSHA})
	st.SetInstalledRole("app", InstalledRoleEntry{Source: commitTimeSource, ArtifactSHA256: testArtifactSHA, CommitTime: when})

	dbs := openTestDBs(t)
	mustSave(t, dbs, st)
	assertCommitTimes(t, "bbolt", mustLoad(t, dbs), when)

	payload, err := st.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot error: %v", err)
	}
	decoded := New()
	if err := json.Unmarshal(payload, decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	assertCommitTimes(t, "JSON", decoded, when)
}

// TestInstalledCommitTimeIsOptional pins the no-bump reading: a zero time
// writes no key, and a record without the key, as older binaries write it,
// decodes to zero.
func TestInstalledCommitTimeIsOptional(t *testing.T) {
	t.Parallel()
	for name, entry := range map[string]any{"collection": InstalledEntry{}, "role": InstalledRoleEntry{}} {
		raw, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("%s: Marshal error: %v", name, err)
		}
		if strings.Contains(string(raw), "commit_time") {
			t.Fatalf("%s: a zero commit time was written: %s", name, raw)
		}
	}

	var col InstalledEntry
	if err := json.Unmarshal([]byte(`{"install_path":"/x","source":"s","artifact_sha256":"abc"}`), &col); err != nil {
		t.Fatalf("decode collection record: %v", err)
	}
	var role InstalledRoleEntry
	if err := json.Unmarshal([]byte(`{"install_path":"/x","source":"s","artifact_sha256":"abc"}`), &role); err != nil {
		t.Fatalf("decode role record: %v", err)
	}
	if !col.CommitTime.IsZero() || !role.CommitTime.IsZero() {
		t.Fatalf("records with no commit_time decoded to %v and %v, want zero", col.CommitTime, role.CommitTime)
	}
}
