package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
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

// TestArtifactsWithoutAClientRefuseEveryRemoteCall pins that an artifact store
// Open never built a client for refuses with errS3ClientNil instead of
// panicking.
func TestArtifactsWithoutAClientRefuseEveryRemoteCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := &Artifacts{tmpBase: t.TempDir()}
	_, fetchErr := s.Fetch(ctx, "k")
	_, commitErr := s.Commit(ctx, "k", "tmp", nil)
	_, hasErr := s.Has(ctx, "k")
	_, _, metaErr := s.Meta(ctx, "k")
	errs := map[string]error{"Fetch": fetchErr, "Commit": commitErr, "Delete": s.Delete(ctx, "k"), "Has": hasErr, "Meta": metaErr}
	for name, err := range errs {
		if !errors.Is(err, errS3ClientNil) {
			t.Errorf("%s = %v, want errS3ClientNil", name, err)
		}
	}
	assertNoLeftoverTempFiles(t, s.tmpBase)
}

// openedArtifacts opens a fake-backed backend and returns its artifact store
// with temps staged in a fresh directory.
func openedArtifacts(t *testing.T) (*Artifacts, *fakeS3) {
	t.Helper()
	b, fake := newTestBackendAndFake(t)
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	b.artifacts.tmpBase = t.TempDir()
	return b.artifacts, fake
}

// TestFetchOfAMissingObjectLeavesNoTemp pins that a missing artifact is
// errS3NotFound and its staged temp file is removed.
func TestFetchOfAMissingObjectLeavesNoTemp(t *testing.T) {
	t.Parallel()
	s, _ := openedArtifacts(t)
	file, err := s.Fetch(context.Background(), "absent.tar.gz")
	if !errors.Is(err, errS3NotFound) || file.Path != "" {
		t.Fatalf("Fetch of an absent key = %+v, %v; want no file and errS3NotFound", file, err)
	}
	assertNoLeftoverTempFiles(t, s.tmpBase)
}

// TestFetchWithoutAStagingDirSendsNothing pins that a staging dir that does
// not exist fails Fetch before any GET is sent.
func TestFetchWithoutAStagingDirSendsNothing(t *testing.T) {
	t.Parallel()
	s, fake := openedArtifacts(t)
	s.tmpBase = filepath.Join(s.tmpBase, "missing")
	if _, err := s.Fetch(context.Background(), "a.tar.gz"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Fetch with a missing staging dir = %v, want fs.ErrNotExist", err)
	}
	if got := fake.requestCount(s.objectKey("a.tar.gz"), http.MethodGet); got != 0 {
		t.Fatalf("%d GETs sent, want none", got)
	}
}

// TestFetchOfABodyThatBreaksOffLeavesNoTemp pins that a download cut short is
// named as the cached artifact object's failure and leaves no temp file.
func TestFetchOfABodyThatBreaksOffLeavesNoTemp(t *testing.T) {
	t.Parallel()
	client := newHandlerTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeTruncatedBody(w, "partial tarball")
	}, nil)
	s := &Artifacts{client: client, prefix: artifactsPrefix, tmpBase: t.TempDir()}
	_, err := s.Fetch(context.Background(), "a.tar.gz")
	if err == nil || !strings.Contains(err.Error(), "cached artifact object") {
		t.Fatalf("Fetch of a cut body = %v, want the cached artifact object's read failure", err)
	}
	assertNoLeftoverTempFiles(t, s.tmpBase)
}

// TestTempFileDefaultsToTheOSTempDir pins that a blank staging base falls back
// to os.TempDir and that the returned cleanup removes the file.
func TestTempFileDefaultsToTheOSTempDir(t *testing.T) {
	t.Parallel()
	file, cleanup, err := (&Artifacts{tmpBase: "  "}).TempFile(context.Background(), ".artifact-")
	if err != nil {
		t.Fatalf("TempFile: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := filepath.Dir(file.Name()); got != filepath.Clean(os.TempDir()) {
		t.Errorf("temp file in %q, want os.TempDir() %q", got, os.TempDir())
	}
	cleanup()
	if _, err := os.Stat(file.Name()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat after cleanup = %v, want the temp file removed", err)
	}
}

// TestCommitOfAMissingTempSendsNothing pins that Commit of a temp path that
// does not exist fails before any PUT.
func TestCommitOfAMissingTempSendsNothing(t *testing.T) {
	t.Parallel()
	s, fake := openedArtifacts(t)
	_, err := s.Commit(context.Background(), "a.tar.gz", filepath.Join(s.tmpBase, "never-written"), nil)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Commit of a missing temp = %v, want fs.ErrNotExist", err)
	}
	if got := fake.requestCount(s.objectKey("a.tar.gz"), http.MethodPut); got != 0 {
		t.Fatalf("%d PUTs sent, want none", got)
	}
}

// TestCommitCleanupRemovesTheStagedFile pins that the committed file's Cleanup
// deletes the local temp it was uploaded from.
func TestCommitCleanupRemovesTheStagedFile(t *testing.T) {
	t.Parallel()
	s, _ := openedArtifacts(t)
	tmpPath := filepath.Join(s.tmpBase, "staged")
	if err := os.WriteFile(tmpPath, []byte("tarball"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	file, err := s.Commit(context.Background(), "a.tar.gz", tmpPath, nil)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if file.Path != tmpPath {
		t.Fatalf("Commit Path = %q, want the staged file %q", file.Path, tmpPath)
	}
	file.Cleanup()
	if _, err := os.Stat(tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat after Cleanup = %v, want the staged file removed", err)
	}
}

// TestVerifyArtifactSHAWithoutARecordedDigest pins that no metadata, or no
// sha256 in it, verifies nothing and refuses nothing.
func TestVerifyArtifactSHAWithoutARecordedDigest(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte("bytes"))
	for name, meta := range map[string]map[string]string{"nil": nil, "no sha256": {"other": "x"}, "blank sha256": {"sha256": "  "}} {
		if err := verifyArtifactSHA(meta, sum[:]); err != nil {
			t.Errorf("%s meta: verifyArtifactSHA = %v, want nil", name, err)
		}
	}
}

// TestMetaFromHeadersKeepsOnlyUserMetadata pins that only X-Amz-Meta-* values
// are kept, lower-cased and trimmed, and that none at all is nil.
func TestMetaFromHeadersKeepsOnlyUserMetadata(t *testing.T) {
	t.Parallel()
	got := metaFromHeaders(map[string][]string{
		"X-Amz-Meta-Sha256": {" abc "},
		"X-Amz-Meta-Empty":  {},
		"Content-Type":      {"application/gzip"},
	})
	if len(got) != 1 || got["sha256"] != "abc" {
		t.Fatalf("metaFromHeaders = %v, want only sha256=abc", got)
	}
	if got := metaFromHeaders(map[string][]string{"X-Amz-Meta-Empty": {}, "Etag": {`"x"`}}); got != nil {
		t.Fatalf("metaFromHeaders without user metadata = %v, want nil", got)
	}
}
