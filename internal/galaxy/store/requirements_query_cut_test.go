package store

import (
	"bytes"
	"testing"
)

// requirementsQueryCapabilityMarker is the sensitive part of the query below,
// searched for alone because json.Marshal escapes '&' as \u0026, so a needle
// spanning the '&' would never match serialized bytes even when left uncut.
const requirementsQueryCapabilityMarker = "X-Amz-Signature=deadbeefcapability"

// requirementsQueryCapabilityQuery is a presigned-URL-shaped query, the bearer
// capability the persist-side cut (copyRequirementsCutQuery) keeps out of a
// shared snapshot object.
const requirementsQueryCapabilityQuery = requirementsQueryCapabilityMarker + "&X-Amz-Expires=3600"

// requirementsSourceWithQuery and requirementsSourceStripped are the same
// source before and after the cut: scheme, host and path identical, query
// present on one and absent on the other.
const (
	requirementsSourceWithQuery = "https://sigs.example.com/acme-app.asc?" + requirementsQueryCapabilityQuery
	requirementsSourceStripped  = "https://sigs.example.com/acme-app.asc"
)

// TestMarshalSnapshotCutsRequirementsSignatureQueryWrittenDirectly pins that an
// entry that never went through SetRequirements, like one a --frozen run keeps
// from an older snapshot, still loses its signature query when persisted.
func TestMarshalSnapshotCutsRequirementsSignatureQueryWrittenDirectly(t *testing.T) {
	t.Parallel()
	st := New()
	st.Requirements["acme.app"] = RequirementSpec{
		Constraint: "1.0.0",
		Source:     "https://galaxy.example.com",
		Signatures: []string{requirementsSourceWithQuery},
	}

	blob, err := st.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot error: %v", err)
	}

	if bytes.Contains(blob, []byte(requirementsQueryCapabilityMarker)) {
		t.Fatalf("snapshot contains a requirements signature capability %q", requirementsQueryCapabilityMarker)
	}

	// Positive control: the source's informational part is still present, so
	// the absence above is the cut working, not the field going missing
	// entirely.
	if !bytes.Contains(blob, []byte(requirementsSourceStripped)) {
		t.Fatalf("snapshot payload does not contain the stripped source %q at all: %s", requirementsSourceStripped, blob)
	}

	// The live store's own value must stay exactly what was written: the cut
	// runs only over snapshotData's RLock-protected copy, and must never
	// write through the aliased slice this entry's Signatures still is.
	live := st.Requirements["acme.app"].Signatures
	if len(live) != 1 || live[0] != requirementsSourceWithQuery {
		t.Fatalf("live store Requirements mutated by MarshalSnapshot: got %v, want [%q]", live, requirementsSourceWithQuery)
	}
}
