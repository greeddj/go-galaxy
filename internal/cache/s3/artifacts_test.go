package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestVerifyArtifactSHAWrapsBothSentinels proves a read-time sha256 mismatch
// wraps both errArtifactSHA256Mismatch and helpers.ErrSHA256Mismatch, and that
// a matching sum passes.
func TestVerifyArtifactSHAWrapsBothSentinels(t *testing.T) {
	t.Parallel()

	expectedSum := sha256.Sum256([]byte("real artifact bytes"))
	expected := hex.EncodeToString(expectedSum[:])
	wrongSum := sha256.Sum256([]byte("different bytes entirely"))

	err := verifyArtifactSHA(map[string]string{"sha256": expected}, wrongSum[:])
	if err == nil {
		t.Fatalf("expected a mismatch error, got nil")
	}
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Errorf("errors.Is(err, helpers.ErrSHA256Mismatch) = false, want true (err: %v)", err)
	}
	if !errors.Is(err, errArtifactSHA256Mismatch) {
		t.Errorf("errors.Is(err, errArtifactSHA256Mismatch) = false, want true (err: %v)", err)
	}

	if err := verifyArtifactSHA(map[string]string{"sha256": expected}, expectedSum[:]); err != nil {
		t.Errorf("matching sum: got %v, want nil", err)
	}
}

// TestVerifyArtifactSHARejectsCaseOnlyDifference proves the comparison is
// exact, not EqualFold: a recorded digest differing only by case is a
// non-canonical sidecar and is rejected as helpers.ErrSHA256Mismatch.
func TestVerifyArtifactSHARejectsCaseOnlyDifference(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("real artifact bytes"))
	actual := hex.EncodeToString(sum[:])
	upper := strings.ToUpper(actual)

	err := verifyArtifactSHA(map[string]string{"sha256": upper}, sum[:])
	if err == nil {
		t.Fatal("expected a case-only difference to be rejected, got nil")
	}
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Errorf("errors.Is(err, helpers.ErrSHA256Mismatch) = false, want true (err: %v)", err)
	}
}

// TestFetchRefusesAnObjectWhoseRecordedDigestDisagreesWithItsBytes proves
// Fetch checks the recorded digest against the downloaded bytes before
// returning the file, and that the refusal leaves no temp file under tmpBase.
func TestFetchRefusesAnObjectWhoseRecordedDigestDisagreesWithItsBytes(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if b.artifacts.tmpBase == "" {
		b.artifacts.tmpBase = t.TempDir()
	}

	const key = "digest-mismatch.tar.gz"
	body := []byte("tarball bytes for a digest mismatch test")
	wrongSum := sha256.Sum256([]byte("some other, unrelated bytes"))
	putArtifactWithRecordedDigest(ctx, t, b, key, body, hex.EncodeToString(wrongSum[:]))

	_, err := b.artifacts.Fetch(ctx, key)
	if err == nil {
		t.Fatal("expected Fetch to refuse an object whose recorded digest disagrees with its bytes")
	}
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Errorf("errors.Is(err, helpers.ErrSHA256Mismatch) = false, want true (err: %v)", err)
	}
	assertNoLeftoverTempFiles(t, b.artifacts.tmpBase)

	// Positive control: the same fixture with a matching digest is accepted.
	correctSum := sha256.Sum256(body)
	putArtifactWithRecordedDigest(ctx, t, b, key, body, hex.EncodeToString(correctSum[:]))

	file, err := b.artifacts.Fetch(ctx, key)
	if err != nil {
		t.Fatalf("Fetch with a matching recorded digest: %v", err)
	}
	got, err := os.ReadFile(file.Path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", file.Path, err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("fetched bytes = %q, want %q", got, body)
	}
	file.Cleanup()
}

// putArtifactWithRecordedDigest stores body under key with x-amz-meta-sha256
// set to digest, failing the test on any put error.
func putArtifactWithRecordedDigest(ctx context.Context, t *testing.T, b *Backend, key string, body []byte, digest string) {
	t.Helper()
	if err := b.client.putObject(ctx, b.artifacts.objectKey(key), bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "application/gzip", meta: map[string]string{"sha256": digest}}, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}
}

// assertNoLeftoverTempFiles fails the test if tmpBase contains any entry,
// pinning Fetch's cleanupIfNeeded call on its digest-mismatch refusal arm.
func assertNoLeftoverTempFiles(t *testing.T, tmpBase string) {
	t.Helper()
	entries, err := os.ReadDir(tmpBase)
	if err != nil {
		t.Fatalf("ReadDir(tmpBase): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no leftover temp file under tmpBase after a refused Fetch, found %v", entries)
	}
}
