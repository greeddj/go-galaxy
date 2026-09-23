package collections

// Fuzzing checkManifestAttribution, which walks attacker-chosen JSON from a
// signed MANIFEST.json and decides which collection the artifact is about.

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// fuzzAttributionIdentity is the collection every fuzzed document is judged
// against, spelled out so a change to testSignedCollection cannot move it.
//
//nolint:gochecknoglobals // a fixed fixture identity, not mutable shared state.
var fuzzAttributionIdentity = collection{Namespace: "acme", Name: "app", Version: "1.0.0"}

// fuzzAttributionSeeds returns the accepted document, degenerate shapes for the
// decode's error arms, and foldingBypassShapes closed into whole documents (the
// table leaves them open for buildArtifactWithManifest's chain pointer).
func fuzzAttributionSeeds() []string {
	degenerate := []string{
		`{"collection_info":{"namespace":"acme","name":"app","version":"1.0.0"}}`,
		``,
		`{}`,
		`null`,
		`"a bare string"`,
		`{"collection_info":null}`,
		`{"collection_info":{"namespace":1,"name":"app","version":"1.0.0"}}`,
		`{"collection_info":{"namespace":"acme","name":"app","version":"1.0.0"},"collection_info":{}}`,
		strings.Repeat(`{"collection_info":`, 200) + `{}` + strings.Repeat(`}`, 200),
	}

	seeds := make([]string, 0, len(degenerate)+len(foldingBypassShapes))
	seeds = append(seeds, degenerate...)
	for _, shape := range foldingBypassShapes {
		seeds = append(seeds, shape.manifest+`}`)
	}

	return seeds
}

// FuzzCheckManifestAttribution pins that checkManifestAttribution never panics,
// that a document it accepts is refused for an identity differing in any one
// component, and that every refusal wraps helpers.ErrSignatureAttributionMismatch.
func FuzzCheckManifestAttribution(f *testing.F) {
	for _, seed := range fuzzAttributionSeeds() {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, manifestJSON []byte) {
		err := checkManifestAttribution(fuzzAttributionIdentity, manifestJSON)
		if err != nil {
			if !errors.Is(err, helpers.ErrSignatureAttributionMismatch) {
				t.Fatalf("refusal = %v, want errors.Is helpers.ErrSignatureAttributionMismatch", err)
			}

			return
		}
		for _, other := range perturbedIdentities(fuzzAttributionIdentity) {
			if checkManifestAttribution(other, manifestJSON) == nil {
				t.Fatalf("a document accepted for %s was also accepted for %s: an accept must be an exact agreement",
					fuzzAttributionIdentity.key(), other.key())
			}
		}
	})
}

// perturbedIdentities returns col with one component changed at a time, which
// is what makes the acceptance invariant a per-component statement rather than
// a claim about the triple as a whole.
func perturbedIdentities(col collection) []collection {
	namespace, name, version := col, col, col
	namespace.Namespace += "x"
	name.Name += "x"
	version.Version += "1"

	return []collection{namespace, name, version}
}
