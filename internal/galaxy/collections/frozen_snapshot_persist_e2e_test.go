package collections_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// frozenPersistCapabilityMarker is the sensitive part of the seeded query,
// checked for on its own as signatureCapabilityMarker is.
const frozenPersistCapabilityMarker = "X-Amz-Signature=deadbeefcapability"

// frozenPersistSignatureSourceWithQuery and
// frozenPersistSignatureSourceStripped are the same signature source before
// and after the cut, seeded directly onto the store below.
const (
	frozenPersistSignatureSourceWithQuery = "https://sigs.example.com/acme-app.asc?" + frozenPersistCapabilityMarker + "&X-Amz-Expires=3600"
	frozenPersistSignatureSourceStripped  = "https://sigs.example.com/acme-app.asc"
)

// seedRequirementsWithQuerySignature persists a Requirements entry whose
// signature source carries a query, written straight onto the map past
// Store.SetRequirements, as a frozen install finds it and never rebuilds it.
func seedRequirementsWithQuerySignature(t *testing.T, cacheDir, source string) {
	t.Helper()
	ctx := context.Background()
	backend := local.New(cacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("seed backend.Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()

	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("seed backend.LoadStore: %v", err)
	}
	st.Requirements["acme.app"] = store.RequirementSpec{
		Constraint: "*",
		Source:     source,
		Signatures: []string{frozenPersistSignatureSourceWithQuery},
	}
	if err := backend.SaveStore(ctx, st); err != nil {
		t.Fatalf("seed backend.SaveStore: %v", err)
	}
}

// TestFrozenInstallPersistsRequirementsWithoutSignatureQuery pins that a real
// install --frozen, which never rebuilds the requirements spec, still commits
// the signature source with its query cut into the Bolt file on disk.
func TestFrozenInstallPersistsRequirementsWithoutSignatureQuery(t *testing.T) {
	f := newE2EFixture(t)
	newFrozenPinFixture(t, f)
	seedRequirementsWithQuerySignature(t, f.cfg.CacheDir, f.cfg.Server)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (frozen install): %v", err)
	}

	ctx := context.Background()
	backend := local.New(f.cfg.CacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("reload backend.Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()

	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("reload backend.LoadStore: %v", err)
	}

	got := st.RequirementsSnapshot()["acme.app"].Signatures
	if len(got) != 1 {
		t.Fatalf("persisted acme.app signatures = %v, want exactly one entry", got)
	}
	if strings.Contains(got[0], "?") {
		t.Fatalf("persisted acme.app signature still carries a query")
	}

	// Positive control: the cut removed only the query, not the whole value.
	if got[0] != frozenPersistSignatureSourceStripped {
		t.Fatalf("persisted acme.app signature = %q, want %q", got[0], frozenPersistSignatureSourceStripped)
	}

	// The re-marshaled reload lacks the capability but keeps the stripped
	// source, so the absence is the cut working, not the field going missing.
	blob, err := st.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot of the reloaded store: %v", err)
	}
	if bytes.Contains(blob, []byte(frozenPersistCapabilityMarker)) {
		t.Fatalf("re-marshaled store still contains a signature capability %q", frozenPersistCapabilityMarker)
	}
	if !bytes.Contains(blob, []byte(frozenPersistSignatureSourceStripped)) {
		t.Fatalf("re-marshaled store missing the stripped signature source %q", frozenPersistSignatureSourceStripped)
	}
}
