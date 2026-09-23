package store

import "testing"

// mutatedMarker is written into a caller-owned slice after a Set* call to
// prove the stored copy does not alias it.
const mutatedMarker = "mutated"

// These tests read stored state through the Store's fields, not its getters,
// so each proves only that a Set* clones the caller's slice or map on the way
// in; a getter that aliased on the way out cannot mask or fake the result.

// TestSetGraphClonesInput proves SetGraph does not alias the caller's deps
// slice.
func TestSetGraphClonesInput(t *testing.T) {
	t.Parallel()
	st := New()
	deps := []string{"c.d@1.0.0", "e.f@2.0.0"}

	st.SetGraph("a.b@1.0.0", deps)
	deps[0] = mutatedMarker

	stored := st.Graph["a.b@1.0.0"]
	if len(stored) != 2 || stored[0] != "c.d@1.0.0" || stored[1] != "e.f@2.0.0" {
		t.Fatalf("expected stored graph to be unaffected by caller mutation, got %#v", stored)
	}
}

// TestSetInstalledClonesDeps proves SetInstalled does not alias the
// caller's InstalledEntry.Deps slice, even though the entry itself is
// passed by value.
func TestSetInstalledClonesDeps(t *testing.T) {
	t.Parallel()
	st := New()
	entry := InstalledEntry{
		ArtifactSHA256: "abc",
		Deps:           []string{"c.d@1.2.3"},
	}

	st.SetInstalled("a.b@1.0.0", entry)
	entry.Deps[0] = mutatedMarker

	stored := st.Installed["a.b@1.0.0"]
	if len(stored.Deps) != 1 || stored.Deps[0] != "c.d@1.2.3" {
		t.Fatalf("expected stored installed entry to be unaffected by caller mutation, got %#v", stored)
	}
}

// TestSetAPICacheClonesBody proves SetAPICache does not alias the caller's
// APICacheEntry.Body buffer, even though the entry itself is passed by
// value.
func TestSetAPICacheClonesBody(t *testing.T) {
	t.Parallel()
	st := New()
	entry := APICacheEntry{
		URL:  "https://example.com/api",
		Body: []byte(`{"ok":true}`),
	}

	st.SetAPICache("api", entry)
	entry.Body[0] = 'X'

	stored := st.APICache["api"]
	if string(stored.Body) != `{"ok":true}` {
		t.Fatalf("expected stored api cache body to be unaffected by caller mutation, got %q", stored.Body)
	}
}

// TestSetRequirementsClonesSignatures pins that SetRequirements deep-copies each
// RequirementSpec.Signatures slice, which a shallow maps.Copy of the spec map
// would leave aliased to the caller's backing array.
func TestSetRequirementsClonesSignatures(t *testing.T) {
	t.Parallel()
	st := New()
	spec := map[string]RequirementSpec{
		"a.b": {
			Constraint: "1.0.0",
			Signatures: []string{"sig1", "sig2"},
		},
	}

	st.SetRequirements(spec)
	spec["a.b"].Signatures[0] = mutatedMarker

	stored := st.Requirements["a.b"]
	if len(stored.Signatures) != 2 || stored.Signatures[0] != "sig1" || stored.Signatures[1] != "sig2" {
		t.Fatalf("expected stored requirement signatures to be unaffected by caller mutation, got %#v", stored.Signatures)
	}
}

// TestGetInstalledClonesDepsOnRead proves GetInstalled clones Deps before
// returning, so mutating the returned entry's slice cannot corrupt the
// stored snapshot state observed by a later caller.
func TestGetInstalledClonesDepsOnRead(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetInstalled("k", InstalledEntry{Deps: []string{"a"}})

	e1, ok := st.GetInstalled("k")
	if !ok {
		t.Fatalf("expected installed entry to exist")
	}
	e1.Deps[0] = mutatedMarker

	e2, ok := st.GetInstalled("k")
	if !ok || e2.Deps[0] != "a" {
		t.Fatalf("expected a later GetInstalled to be unaffected by the first caller's mutation, got %#v (ok=%v)", e2, ok)
	}
}

// TestInstalledArtifactSHAByKeyOmitsEmptySHA pins that an installed entry with
// an empty ArtifactSHA256 is left out, so the extracted-store sweep never
// treats "" as a digest to keep.
func TestInstalledArtifactSHAByKeyOmitsEmptySHA(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetInstalled("a.b@1.0.0", InstalledEntry{ArtifactSHA256: "sha-a"})
	st.SetInstalled("c.d@2.0.0", InstalledEntry{ArtifactSHA256: ""})

	got := st.InstalledArtifactSHAByKey()
	if len(got) != 1 {
		t.Fatalf("expected exactly one entry, got %d: %#v", len(got), got)
	}
	if got["a.b@1.0.0"] != "sha-a" {
		t.Fatalf("expected a.b@1.0.0 -> sha-a, got %#v", got)
	}
	if _, ok := got["c.d@2.0.0"]; ok {
		t.Fatalf("expected c.d@2.0.0 with an empty SHA to be omitted, got %#v", got)
	}
}

// TestRequirementsSnapshotIsIndependentDeepCopy proves RequirementsSnapshot
// returns a fully independent deep copy: mutating one snapshot's Signatures
// slice cannot leak into a later, independently taken snapshot.
func TestRequirementsSnapshotIsIndependentDeepCopy(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetRequirements(map[string]RequirementSpec{
		"k": {Constraint: "1.0.0", Signatures: []string{"s1"}},
	})

	snap1 := st.RequirementsSnapshot()
	sig := snap1["k"].Signatures
	sig[0] = mutatedMarker

	snap2 := st.RequirementsSnapshot()
	if snap2["k"].Signatures[0] != "s1" {
		t.Fatalf("expected an independently taken snapshot to be unaffected, got %#v", snap2["k"])
	}
}
