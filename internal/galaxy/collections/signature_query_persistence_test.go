package collections

// A signature source's query must survive neither into buildRequirementsSpec's
// output nor into the snapshot it feeds, and a store holding an uncut entry
// must not fail a run.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// signatureCapabilityMarker is the sensitive part of the query below, matched
// alone because json.Marshal escapes '&' as \u0026, so a needle spanning the
// '&' would never appear in serialized bytes even if the query survived.
const signatureCapabilityMarker = "X-Amz-Signature=deadbeefcapability"

// signatureCapabilityQuery is a presigned-URL-shaped query string, standing
// in for the bearer capability normalizeSignatures' query cut exists to keep
// out of persisted state.
const signatureCapabilityQuery = signatureCapabilityMarker + "&X-Amz-Expires=3600"

// signatureSourceWithQuery and signatureSourceStripped are the same source
// before and after the cut: scheme, host and path identical, query present
// on one and absent on the other.
const (
	signatureSourceWithQuery = "https://sigs.example.com/acme-app.asc?" + signatureCapabilityQuery
	signatureSourceStripped  = "https://sigs.example.com/acme-app.asc"
)

// TestBuildRequirementsSpecCutsSignatureQuery pins that buildRequirementsSpec
// cuts a signature source's query and only the query: scheme, host and path
// survive byte for byte, so an emptied value cannot pass.
func TestBuildRequirementsSpecCutsSignatureQuery(t *testing.T) {
	t.Parallel()
	roots := []collection{{
		Namespace:  "acme",
		Name:       "app",
		Constraint: "*",
		Source:     "https://galaxy.example.com",
		Signatures: []string{signatureSourceWithQuery},
	}}

	spec := buildRequirementsSpec(roots)
	got := spec["acme.app"].Signatures
	if len(got) != 1 {
		t.Fatalf("stored signatures = %v, want exactly one entry", got)
	}
	if strings.Contains(got[0], "?") {
		t.Fatalf("stored signature %q still carries a query", got[0])
	}

	// Positive control: the scheme, host and path are not merely absent, they
	// are the exact bytes the source declared - proving the cut removed only
	// the query rather than the whole value.
	if got[0] != signatureSourceStripped {
		t.Fatalf("stored signature = %q, want %q (scheme/host/path preserved byte for byte)", got[0], signatureSourceStripped)
	}
}

// TestRequirementsSnapshotDoesNotCarrySignatureQueryCapability pins the sink:
// a serialized snapshot never holds the capability, whichever cut removed it;
// the producer and store persist cuts each have their own test.
func TestRequirementsSnapshotDoesNotCarrySignatureQueryCapability(t *testing.T) {
	t.Parallel()
	roots := []collection{{
		Namespace:  "acme",
		Name:       "app",
		Constraint: "*",
		Source:     "https://galaxy.example.com",
		Signatures: []string{signatureSourceWithQuery},
	}}
	spec := buildRequirementsSpec(roots)

	st := store.New()
	st.SetRequirements(spec)
	blob, err := st.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot: %v", err)
	}

	if bytes.Contains(blob, []byte(signatureCapabilityMarker)) {
		t.Fatalf("snapshot contains a signature capability %q", signatureCapabilityMarker)
	}

	// Positive control: the source's informational part is still present, so
	// the absence above is the cut working, not the field going missing
	// entirely.
	if !bytes.Contains(blob, []byte(signatureSourceStripped)) {
		t.Fatalf("snapshot payload does not contain the stripped signature source %q at all: %s", signatureSourceStripped, blob)
	}

	var decoded struct {
		Requirements map[string]store.RequirementSpec `json:"requirements"`
	}
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got := decoded.Requirements["acme.app"].Signatures; len(got) != 1 || got[0] != signatureSourceStripped {
		t.Fatalf("round-tripped signatures = %v, want [%q]", got, signatureSourceStripped)
	}
}

// legacyRequirementsSignature is requirementsSignatureFromSpec without the
// query cut, so a test can seed a store as a binary without the cut left it:
// an uncut spec with a hash consistent with it.
func legacyRequirementsSignature(t *testing.T, spec map[string]store.RequirementSpec, noDeps bool, serversSig string) string {
	t.Helper()
	parts := make([]string, 0, len(spec))
	for fqdn, entry := range spec {
		constraint := entry.Constraint
		if constraint == "" {
			constraint = "*"
		}
		sigs := make([]string, len(entry.Signatures))
		for i, value := range entry.Signatures {
			sigs[i] = strings.TrimSpace(value)
		}
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%s", fqdn, constraint, entry.Source, entry.Type, strings.Join(sigs, ",")))
	}
	header := fmt.Sprintf("no-deps=%t\nservers=%s", noDeps, serversSig)
	sum := sha256.Sum256([]byte(header + "\n" + strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// TestUnstrippedPersistedSignatureQuerySelfHeals pins that an uncut stored spec
// no longer matches the recomputed hash, so the run falls back to a full
// resolve instead of failing and rewrites the spec in cut form.
func TestUnstrippedPersistedSignatureQuerySelfHeals(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", testVersion100, nil)

	cfg := &config.Config{Server: srv.URL(), Workers: 2}
	runtime := infra.New(noopPrinter{}, srv.Client())
	st := store.New()

	root := collection{
		Namespace: "acme", Name: "app", Constraint: "^1.0.0",
		Source: srv.URL(), Signatures: []string{signatureSourceWithQuery},
	}

	oldSpec := map[string]store.RequirementSpec{
		"acme.app": {Constraint: "^1.0.0", Source: srv.URL(), Signatures: []string{signatureSourceWithQuery}},
	}
	oldHash := legacyRequirementsSignature(t, oldSpec, cfg.NoDeps, serversSignature(cfg))
	st.SetMetaRequirements(oldHash, cfg.Server)
	st.SetRequirements(oldSpec)

	resolved, _, err := resolveCollectionsInternal(
		context.Background(), newCollectionDeps(cfg, runtime, st), []collection{root}, resolveTopLevel,
	)
	if err != nil {
		t.Fatalf("resolveCollectionsInternal (unstripped persisted spec) = %v, want nil", err)
	}
	if resolved["acme.app"].Version != testVersion100 {
		t.Fatalf("resolved[acme.app].Version = %q, want %q", resolved["acme.app"].Version, testVersion100)
	}

	got := st.RequirementsSnapshot()["acme.app"].Signatures
	if len(got) != 1 || got[0] != signatureSourceStripped {
		t.Fatalf("persisted spec after self-heal = %v, want [%q]", got, signatureSourceStripped)
	}
}

// TestLegacyRequirementsSignatureAgreesOnlyOnAStrippedSpec pins that the
// formula without the cut agrees with requirementsSignatureFromSpec on a
// query-free spec, so an older binary accepts the hash, and differs otherwise.
func TestLegacyRequirementsSignatureAgreesOnlyOnAStrippedSpec(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: "https://galaxy.example.com"}
	serversSig := serversSignature(cfg)

	strippedSpec := map[string]store.RequirementSpec{
		"acme.app": {Constraint: "^1.0.0", Source: cfg.Server, Signatures: []string{signatureSourceStripped}},
	}
	legacyStripped := legacyRequirementsSignature(t, strippedSpec, cfg.NoDeps, serversSig)
	currentStripped := requirementsSignatureFromSpec(strippedSpec, cfg.NoDeps, serversSig)
	if legacyStripped != currentStripped {
		t.Fatalf("legacy = %s, current = %s, want equal over an already-stripped spec (the downgrade direction's own agreement)",
			legacyStripped, currentStripped)
	}

	// Positive control: with the query present the two formulas must
	// disagree, or the equality above proved nothing about the cut.
	unstrippedSpec := map[string]store.RequirementSpec{
		"acme.app": {Constraint: "^1.0.0", Source: cfg.Server, Signatures: []string{signatureSourceWithQuery}},
	}
	legacyUnstripped := legacyRequirementsSignature(t, unstrippedSpec, cfg.NoDeps, serversSig)
	currentUnstripped := requirementsSignatureFromSpec(unstrippedSpec, cfg.NoDeps, serversSig)
	if legacyUnstripped == currentUnstripped {
		t.Fatalf("legacy = %s, current = %s, want different over an unstripped spec (the upgrade direction's own mismatch)",
			legacyUnstripped, currentUnstripped)
	}
}
