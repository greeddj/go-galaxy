package collections

// Tests binding a verified signature to the collection it was verified for, and
// bounding the manifest- and server-chosen strings its messages render. Every
// artifact here verifies and walks; only its declared identity differs.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/psvmcc/hub/pkg/types"
)

// TestSignedManifestMustNameTheCollection pins that a verified signature over a
// manifest naming another collection, or an older version of this one (a signed
// downgrade), is refused; the last row is the accepting control.
func TestSignedManifestMustNameTheCollection(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name      string
		namespace string
		collName  string
		version   string
		wantErr   bool
	}{
		{name: "another collection entirely", namespace: "trusted", collName: "lib", version: "2.0.0", wantErr: true},
		{name: "a signed downgrade of the same collection", namespace: "acme", collName: "app", version: "0.0.1", wantErr: true},
		{name: "the collection being installed", namespace: "acme", collName: "app", version: "1.0.0"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			tarPath, manifestJSON := buildSignedArtifactAs(t, row.namespace, row.collName, row.version, false)
			fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
			meta := serverSignatureMeta(signTestBytes(t, manifestJSON))

			err := verifyCollectionSignatures(
				context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
			if !row.wantErr {
				if err != nil {
					t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
				}

				return
			}
			if !errors.Is(err, helpers.ErrSignatureAttributionMismatch) {
				t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureAttributionMismatch", err)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitSignature {
				t.Fatalf("exit code = %d, want %d", got, exitcode.ExitSignature)
			}
			if want := row.namespace + "." + row.collName + "@" + row.version; !strings.Contains(err.Error(), want) {
				t.Fatalf("verifyCollectionSignatures() = %v, want it to name what the manifest vouches for (%s)", err, want)
			}
		})
	}
}

// TestAttributionIsNotCheckedWithoutAVerifiedSignature pins that attribution is
// checked only once a signature verified: on an unsigned manifest an attacker
// chooses both sides of the comparison, so refusing there would prove nothing.
func TestAttributionIsNotCheckedWithoutAVerifiedSignature(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifactAs(t, "trusted", "lib", "2.0.0", false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)

	if err := verifyCollectionSignatures(
		context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
		t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
	}
}

// TestAttributionMessageIsBounded pins that the refusal truncates a
// manifest-declared value, which only helpers.ManifestScanMaxBytes bounds, and
// says that it did.
func TestAttributionMessageIsBounded(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("n", 64<<10)
	tarPath, manifestJSON := buildSignedArtifactAs(t, huge, "lib", "2.0.0", false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
	meta := serverSignatureMeta(signTestBytes(t, manifestJSON))

	err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
	if !errors.Is(err, helpers.ErrSignatureAttributionMismatch) {
		t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureAttributionMismatch", err)
	}
	if len(err.Error()) > 2*helpers.MessageValueMaxLen {
		t.Fatalf("the refusal renders %d bytes for a %d-byte declared namespace, want it bounded", len(err.Error()), len(huge))
	}
	if !strings.Contains(err.Error(), "bytes)") {
		t.Fatalf("the refusal = %q, want it to say the value was truncated", err.Error())
	}
}

// TestServerBlobOriginIsBounded pins the same cap on a version-metadata href,
// which is copied onto every gathered blob and rendered once per failure; an
// ordinary href passes through unchanged.
func TestServerBlobOriginIsBounded(t *testing.T) {
	t.Parallel()

	long := &types.GalaxyCollectionVersionInfo{Href: "https://sigs.example/" + strings.Repeat("p", 64<<10)}
	long.Signatures = []any{map[string]any{"signature": "-----BEGIN PGP SIGNATURE-----\nx\n-----END PGP SIGNATURE-----"}}
	blobs, _ := serverSignatureBlobs(long)
	if len(blobs) != 1 {
		t.Fatalf("serverSignatureBlobs() returned %d blobs, want 1", len(blobs))
	}
	if got := len(blobs[0].Origin); got > 2*helpers.MessageValueMaxLen {
		t.Fatalf("blob origin is %d bytes for a %d-byte href, want it bounded", got, len(long.Href))
	}
	if !strings.Contains(blobs[0].Origin, "bytes)") {
		t.Fatalf("blob origin = %q, want it to say the value was truncated", blobs[0].Origin)
	}

	href := "https://galaxy.example/api/v3/collections/acme/app/versions/1.0.0/"
	short := &types.GalaxyCollectionVersionInfo{Href: href}
	short.Signatures = long.Signatures
	if got, _ := serverSignatureBlobs(short); len(got) != 1 || got[0].Origin != href {
		t.Fatalf("serverSignatureBlobs() origin = %v, want the href unchanged", got)
	}
}

// blobOriginSignatures is one shape-valid server signature entry, enough for
// serverSignatureBlobs to produce a blob whose Origin a test reads.
func blobOriginSignatures() []any {
	return []any{map[string]any{"signature": "-----BEGIN PGP SIGNATURE-----\nx\n-----END PGP SIGNATURE-----"}}
}

// originOf drives serverSignatureBlobs over one href and returns the single
// blob's Origin.
func originOf(t *testing.T, href string) string {
	t.Helper()
	meta := &types.GalaxyCollectionVersionInfo{Href: href}
	meta.Signatures = blobOriginSignatures()
	blobs, _ := serverSignatureBlobs(meta)
	if len(blobs) != 1 {
		t.Fatalf("serverSignatureBlobs(%q) returned %d blobs, want 1", href, len(blobs))
	}

	return blobs[0].Origin
}

// TestServerBlobOriginCutsCredentials pins that a blob Origin drops the href's
// userinfo and query yet still names the metadata document; the cut sits at the
// producer because internal/galaxy/signature reports Origin verbatim.
func TestServerBlobOriginCutsCredentials(t *testing.T) {
	t.Parallel()

	const password = "sup3rsecret"
	const clean = "https://galaxy.example/api/v3/collections/acme/app/versions/1.0.0/"
	origin := originOf(t, "https://u:"+password+"@galaxy.example/api/v3/collections/acme/app/versions/1.0.0/"+
		"?X-Amz-Signature=deadbeefcafe")

	if strings.Contains(origin, password) {
		t.Errorf("blob origin carries the password: %s", origin)
	}
	if strings.Contains(origin, "u:") {
		t.Errorf("blob origin carries the userinfo prefix %q: %s", "u:", origin)
	}
	if strings.Contains(origin, "X-Amz-Signature") {
		t.Errorf("blob origin carries the presigned query: %s", origin)
	}
	if !strings.Contains(origin, clean) {
		t.Errorf("blob origin does not name the metadata document it came from: %s", origin)
	}
}

// TestServerBlobOriginCutsBeforeTruncating pins cut-then-truncate in
// serverBlobOrigin: truncating first drops the "@" of a credential longer than
// helpers.MessageValueMaxLen, so nothing is cut and the password prefix shows.
func TestServerBlobOriginCutsBeforeTruncating(t *testing.T) {
	t.Parallel()

	password := strings.Repeat("s", helpers.MessageValueMaxLen)
	origin := originOf(t, "https://u:"+password+"@galaxy.example/versions/1.0.0/")

	if at := strings.Index(origin, "ssss"); at >= 0 {
		t.Errorf("blob origin carries the truncated password at byte %d of %d", at, len(origin))
	}
	if !strings.Contains(origin, "https://galaxy.example/versions/1.0.0/") {
		t.Errorf("blob origin (%d bytes) does not name the metadata document it came from", len(origin))
	}
}

// TestMetadataUnavailableIsADifferentLine pins that a vacuous pass says whether
// the collection carried no signatures or its metadata could not be loaded; the
// verdict is identical, so the fact rides on the payload, not on a nil meta.
func TestMetadataUnavailableIsADifferentLine(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifact(t, false)

	t.Run("metadata was in hand and carried no signatures", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		if err := verifyCollectionSignatures(
			context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
		}
		if fx.printer.hasWarnContaining("version metadata was unavailable") {
			t.Fatalf("an ordinary vacuous pass must not blame the metadata; warns=%v", fx.printer.warns)
		}
	})

	t.Run("metadata could not be loaded at all", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		payload := verifyPayload(nil, tarPath)
		payload.metaUnavailable = true

		if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, payload); err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
		}
		if !fx.printer.hasWarnContaining("version metadata was unavailable") {
			t.Fatalf("no warning naming the unavailable metadata; warns=%v", fx.printer.warns)
		}
	})
}

// foldingBypass is one manifest whose collection_info this tool must refuse to
// read a single identity out of.
type foldingBypass struct {
	name     string
	manifest string
}

// foldingBypassShapes enumerates manifests declaring their identity more than
// once, each of which a struct decode accepts, including one level down.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var foldingBypassShapes = []foldingBypass{
	{
		name: "a folded top-level shadow",
		manifest: `{"collection_info":{"namespace":"evil","name":"lib","version":"6.6.6"},` +
			`"COLLECTION_INFO":{"namespace":"acme","name":"app","version":"1.0.0"}`,
	},
	{
		name: "an identity assembled across three spellings",
		manifest: `{"collection_info":{"namespace":"evil","name":"lib","version":"6.6.6"},` +
			`"CoLlEcTiOn_InFo":{"namespace":"acme"},` +
			`"COLLECTION_INFO":{"name":"app","version":"1.0.0"}`,
	},
	{
		name: "two exact duplicate keys",
		manifest: `{"collection_info":{"namespace":"evil","name":"lib","version":"6.6.6"},` +
			`"collection_info":{"namespace":"acme","name":"app","version":"1.0.0"}`,
	},
	{
		name:     "a folded shadow one level down",
		manifest: `{"collection_info":{"namespace":"evil","NAMESPACE":"acme","name":"app","version":"1.0.0"}`,
	},
	{
		// Every value matches, so only the decode can refuse it: an identity
		// named twice is a function of the parser, whatever the values say.
		name: "a folded shadow whose values all match",
		manifest: `{"collection_info":{"namespace":"acme","name":"app","version":"1.0.0"},` +
			`"COLLECTION_INFO":{"namespace":"acme","name":"app","version":"1.0.0"}`,
	},
}

// TestFoldedIdentityKeysAreRefused pins the decode half of attribution: a
// signed manifest this tool cannot read one identity out of is refused, however
// its values compare.
func TestFoldedIdentityKeysAreRefused(t *testing.T) {
	t.Parallel()

	for _, shape := range foldingBypassShapes {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			tarPath, manifestJSON := buildArtifactWithManifest(t, shape.manifest)
			fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
			meta := serverSignatureMeta(signTestBytes(t, manifestJSON))

			err := verifyCollectionSignatures(
				context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
			if !errors.Is(err, helpers.ErrSignatureAttributionMismatch) {
				t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureAttributionMismatch", err)
			}
		})
	}
}

// TestAttributionMessageQuotesTheDeclaredIdentity pins the %q rendering of a
// declared identity: internal/safeout passes a newline through, so an unquoted
// version string could forge an output line of its own.
func TestAttributionMessageQuotesTheDeclaredIdentity(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifactAs(t, "acme", "app", "9.9.9\nSuccessfully installed acme.app@1.0.0", false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
	meta := serverSignatureMeta(signTestBytes(t, manifestJSON))

	err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
	if !errors.Is(err, helpers.ErrSignatureAttributionMismatch) {
		t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureAttributionMismatch", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("the refusal carries a real newline from the manifest: %q", err.Error())
	}
	if !strings.Contains(err.Error(), `\n`) {
		t.Fatalf("the refusal = %q, want the declared version quoted", err.Error())
	}
}
