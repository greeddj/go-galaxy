package s3

// Tests for Artifacts.Meta: the cacheManager.ArtifactStore tri-state contract,
// Meta's found matching Has for the same key, and Has costing one HEAD.

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

// artifactsMetaTestKey is the artifact cache key every test in this file
// probes, factored out since none of them ever vary it.
const artifactsMetaTestKey = "ns.name-1.0.0.tar.gz"

// testSHA is a canonical 64-char lowercase hex digest, duplicated from the
// internal/cache/local tests because they are a different package.
const testSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// assertS3MetaFoundMatchesHas fails unless Has reports the same presence as
// metaFound, the equality dryRunArtifactMeta relies on to mirror isCacheHit.
func assertS3MetaFoundMatchesHas(t *testing.T, artifacts *Artifacts, metaFound bool) {
	t.Helper()
	hasFound, err := artifacts.Has(context.Background(), artifactsMetaTestKey)
	if err != nil {
		t.Fatalf("Has error: %v", err)
	}
	if hasFound != metaFound {
		t.Fatalf("Has found=%v, Meta found=%v, want equal", hasFound, metaFound)
	}
}

// TestArtifactsMetaAbsentReportsNotFound proves Meta reports found=false with
// a nil map and a nil error for a key that was never stored - the identical
// outcome Has itself reports for the same key.
func TestArtifactsMetaAbsentReportsNotFound(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	meta, found, err := b.artifacts.Meta(ctx, artifactsMetaTestKey)
	if err != nil {
		t.Fatalf("Meta error: %v", err)
	}
	if found {
		t.Fatalf("expected found=false for an absent key, got true (meta=%#v)", meta)
	}
	if meta != nil {
		t.Fatalf("expected a nil meta map for an absent key, got %#v", meta)
	}
	assertS3MetaFoundMatchesHas(t, b.artifacts, found)
}

// TestArtifactsMetaPresentWithValidDigestReturnsIt proves Meta surfaces a
// committed artifact's recorded sha256 exactly as it was written.
func TestArtifactsMetaPresentWithValidDigestReturnsIt(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	body := []byte("tarball bytes")
	if err := b.client.putObject(ctx, b.artifacts.objectKey(artifactsMetaTestKey), bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "application/gzip", meta: map[string]string{"sha256": testSHA}}, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}

	meta, found, err := b.artifacts.Meta(ctx, artifactsMetaTestKey)
	if err != nil {
		t.Fatalf("Meta error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for a stored key")
	}
	if got := meta["sha256"]; got != testSHA {
		t.Fatalf("expected Meta to report sha256 %q, got %q", testSHA, got)
	}
	assertS3MetaFoundMatchesHas(t, b.artifacts, found)
}

// TestArtifactsMetaPresentWithNoMetadataReportsFoundNilMeta proves an object
// with no x-amz-meta-sha256 (not written by Commit) is found with a nil map:
// the "cached, no recorded metadata" state.
func TestArtifactsMetaPresentWithNoMetadataReportsFoundNilMeta(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	body := []byte("tarball bytes")
	if err := b.client.putObject(ctx, b.artifacts.objectKey(artifactsMetaTestKey), bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "application/gzip"}, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}

	meta, found, err := b.artifacts.Meta(ctx, artifactsMetaTestKey)
	if err != nil {
		t.Fatalf("Meta error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for a stored key with no recorded metadata")
	}
	if meta != nil {
		t.Fatalf("expected a nil meta map with no recorded metadata, got %#v", meta)
	}
	assertS3MetaFoundMatchesHas(t, b.artifacts, found)
}

// TestArtifactsMetaPresentWithNonHexDigestReturnsItVerbatim proves Meta does
// not validate digest shape: a non-hex sha256 comes back verbatim, since
// callers such as dryRunPinVerdict check helpers.IsSHA256Hex themselves.
func TestArtifactsMetaPresentWithNonHexDigestReturnsItVerbatim(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	const nonHex = "not-a-hex-digest-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
	body := []byte("tarball bytes")
	if err := b.client.putObject(ctx, b.artifacts.objectKey(artifactsMetaTestKey), bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "application/gzip", meta: map[string]string{"sha256": nonHex}}, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}

	meta, found, err := b.artifacts.Meta(ctx, artifactsMetaTestKey)
	if err != nil {
		t.Fatalf("Meta error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for a stored key with a non-hex recorded digest")
	}
	if got := meta["sha256"]; got != nonHex {
		t.Fatalf("expected Meta to return the non-hex digest verbatim %q, got %q", nonHex, got)
	}
	assertS3MetaFoundMatchesHas(t, b.artifacts, found)
}

// TestArtifactsHeadArtifactPropagatesNonNotFoundError proves a non-404 HEAD
// failure surfaces as an error from Has and Meta, never as found=false. The
// fault spans s3RetryMaxAttempts so headObject's own retry cannot absorb it.
func TestArtifactsHeadArtifactPropagatesNonNotFoundError(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	objectKey := b.artifacts.objectKey(artifactsMetaTestKey)

	fake.failNext(objectKey, http.MethodHead, http.StatusInternalServerError, s3RetryMaxAttempts)
	if _, _, err := b.artifacts.Meta(ctx, artifactsMetaTestKey); err == nil {
		t.Fatal("expected Meta to return a non-nil error for a HEAD that exhausted its retries on a non-404 status")
	}

	// Re-armed: Meta's call already spent the previous fault rule.
	fake.failNext(objectKey, http.MethodHead, http.StatusInternalServerError, s3RetryMaxAttempts)
	if _, err := b.artifacts.Has(ctx, artifactsMetaTestKey); err == nil {
		t.Fatal("expected Has to return a non-nil error for a HEAD that exhausted its retries on a non-404 status")
	}
}

// TestArtifactsHasSharesHeadArtifactWithMetaOneHeadRequest proves Has costs
// exactly one HEAD for a present and an absent key, so a dry run calling Meta
// instead of Has costs the S3 backend nothing extra.
func TestArtifactsHasSharesHeadArtifactWithMetaOneHeadRequest(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	const presentKey = "present.tar.gz"
	const absentKey = "absent.tar.gz"
	body := []byte("tarball bytes")
	if err := b.client.putObject(ctx, b.artifacts.objectKey(presentKey), bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "application/gzip", meta: map[string]string{"sha256": testSHA}}, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}

	found, err := b.artifacts.Has(ctx, presentKey)
	if err != nil {
		t.Fatalf("Has(present) error: %v", err)
	}
	if !found {
		t.Fatal("expected Has(present) = true")
	}
	if got := fake.requestCount(b.artifacts.objectKey(presentKey), http.MethodHead); got != 1 {
		t.Errorf("HEAD request count for the present key = %d, want exactly 1", got)
	}

	found, err = b.artifacts.Has(ctx, absentKey)
	if err != nil {
		t.Fatalf("Has(absent) error: %v", err)
	}
	if found {
		t.Fatal("expected Has(absent) = false")
	}
	if got := fake.requestCount(b.artifacts.objectKey(absentKey), http.MethodHead); got != 1 {
		t.Errorf("HEAD request count for the absent key = %d, want exactly 1", got)
	}
}
