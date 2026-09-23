package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// These tests walk the artifact round trip against this package's fake,
// proving Commit and Fetch agree on the object key and the recorded digest.

// artifactRoundTripKey is the cache key every test in this file commits and
// fetches under.
const artifactRoundTripKey = "abcdef01.acme-widgets-1.0.0.tar.gz"

// artifactRoundTripBody is the payload committed and read back.
const artifactRoundTripBody = "tarball bytes for the round trip"

// openRoundTripBackend returns an opened backend with a temp base of its own,
// so a test can assert on what TempFile leaves behind without seeing files
// from anywhere else.
func openRoundTripBackend(t *testing.T) *Backend {
	t.Helper()

	b := newTestBackend(t)
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	b.artifacts.tmpBase = t.TempDir()
	return b
}

// commitRoundTripArtifact stages artifactRoundTripBody through TempFile and
// commits it under artifactRoundTripKey with meta, returning the temp path
// Commit reports.
func commitRoundTripArtifact(t *testing.T, b *Backend, meta map[string]string) string {
	t.Helper()

	ctx := context.Background()
	tmpFile, cleanup, err := b.artifacts.TempFile(ctx, ".download-")
	if err != nil {
		t.Fatalf("TempFile: %v", err)
	}
	defer cleanupIfNeeded(cleanup)
	if _, err := tmpFile.WriteString(artifactRoundTripBody); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}

	committed, err := b.artifacts.Commit(ctx, artifactRoundTripKey, tmpFile.Name(), meta)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return committed.Path
}

// TestArtifactRoundTrip walks stage, commit, read back and remove, proving
// both ends agree on the object key and that Fetch accepts the digest Commit
// recorded.
func TestArtifactRoundTrip(t *testing.T) {
	t.Parallel()
	b := openRoundTripBackend(t)
	ctx := context.Background()

	commitRoundTripArtifact(t, b, nil)

	found, err := b.artifacts.Has(ctx, artifactRoundTripKey)
	if err != nil || !found {
		t.Fatalf("Has after Commit = (%v, %v), want (true, nil)", found, err)
	}

	fetched, err := b.artifacts.Fetch(ctx, artifactRoundTripKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer cleanupIfNeeded(fetched.Cleanup)
	got, err := os.ReadFile(fetched.Path)
	if err != nil {
		t.Fatalf("read fetched artifact: %v", err)
	}
	if string(got) != artifactRoundTripBody {
		t.Errorf("fetched body = %q, want %q", got, artifactRoundTripBody)
	}

	if err := b.artifacts.Delete(ctx, artifactRoundTripKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	found, err = b.artifacts.Has(ctx, artifactRoundTripKey)
	if err != nil || found {
		t.Fatalf("Has after Delete = (%v, %v), want (false, nil)", found, err)
	}
}

// TestCommitRecordsTheDigestFetchVerifies pins that the digest Commit derives
// itself is the one Fetch checks: bytes overwritten under that recorded digest
// are refused with helpers.ErrSHA256Mismatch.
func TestCommitRecordsTheDigestFetchVerifies(t *testing.T) {
	t.Parallel()
	b := openRoundTripBackend(t)
	ctx := context.Background()

	commitRoundTripArtifact(t, b, nil)

	// Positive control first: the object Commit wrote does come back.
	fetched, err := b.artifacts.Fetch(ctx, artifactRoundTripKey)
	if err != nil {
		t.Fatalf("Fetch of the committed object: %v", err)
	}
	cleanupIfNeeded(fetched.Cleanup)

	// Now replace the bytes while keeping the digest Commit derived, which is
	// the shape a rotted or tampered object has.
	sum := sha256.Sum256([]byte(artifactRoundTripBody))
	putArtifactWithRecordedDigest(ctx, t, b, artifactRoundTripKey, []byte("different bytes entirely"), hex.EncodeToString(sum[:]))

	if _, err := b.artifacts.Fetch(ctx, artifactRoundTripKey); !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("Fetch of drifted bytes = %v, want errors.Is helpers.ErrSHA256Mismatch", err)
	}
}

// TestCommitHonorsASuppliedDigest pins that a caller-supplied sha256, which
// every real download passes, is recorded as given and accepted by Fetch.
func TestCommitHonorsASuppliedDigest(t *testing.T) {
	t.Parallel()
	b := openRoundTripBackend(t)
	ctx := context.Background()

	sum := sha256.Sum256([]byte(artifactRoundTripBody))
	supplied := hex.EncodeToString(sum[:])
	commitRoundTripArtifact(t, b, map[string]string{"sha256": supplied})

	recorded, found, err := b.artifacts.Meta(ctx, artifactRoundTripKey)
	if err != nil || !found {
		t.Fatalf("Meta = (%v, %v, %v), want found", recorded, found, err)
	}
	if recorded["sha256"] != supplied {
		t.Errorf("recorded sha256 = %q, want the supplied %q", recorded["sha256"], supplied)
	}

	fetched, err := b.artifacts.Fetch(ctx, artifactRoundTripKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	cleanupIfNeeded(fetched.Cleanup)
}

// TestTempFileLeavesNothingBehind pins that a staged temp file still exists
// when Commit is called and is gone once its cleanup runs, whether the commit
// succeeded or the fake refused the PUT.
func TestTempFileLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	t.Run("after a successful commit", func(t *testing.T) {
		t.Parallel()
		b := openRoundTripBackend(t)
		tmpPath := commitRoundTripArtifact(t, b, nil)
		// Commit reports the temp path and a cleanup of its own; the caller
		// runs it once the bytes are no longer needed.
		if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
			t.Fatalf("removing the committed temp: %v", err)
		}
		assertNoLeftoverTempFiles(t, b.artifacts.tmpBase)
	})

	t.Run("after a refused commit", func(t *testing.T) {
		t.Parallel()
		b, fake := newTestBackendAndFake(t)
		if err := b.Open(context.Background()); err != nil {
			t.Fatalf("Open: %v", err)
		}
		b.artifacts.tmpBase = t.TempDir()
		ctx := context.Background()

		tmpFile, cleanup, err := b.artifacts.TempFile(ctx, ".download-")
		if err != nil {
			t.Fatalf("TempFile: %v", err)
		}
		if _, err := tmpFile.WriteString(artifactRoundTripBody); err != nil {
			t.Fatalf("write temp file: %v", err)
		}
		if err := tmpFile.Close(); err != nil {
			t.Fatalf("close temp file: %v", err)
		}
		// The file must exist here: it is what Commit is about to open, and a
		// cleanup that already ran would make the refusal below meaningless.
		if _, err := os.Stat(tmpFile.Name()); err != nil {
			t.Fatalf("temp file missing before Commit: %v", err)
		}

		fake.failNext(b.artifacts.objectKey(artifactRoundTripKey), "PUT", 500, -1)
		if _, err := b.artifacts.Commit(ctx, artifactRoundTripKey, tmpFile.Name(), nil); err == nil {
			t.Fatal("expected Commit to fail against a refusing bucket")
		}

		cleanup()
		assertNoLeftoverTempFiles(t, b.artifacts.tmpBase)
	})
}

// TestDeleteOfAnAbsentObjectIsNotAnError pins that deleting an absent key
// succeeds, since eviction can race another runner's own eviction; deleting
// a committed object is the control.
func TestDeleteOfAnAbsentObjectIsNotAnError(t *testing.T) {
	t.Parallel()
	b := openRoundTripBackend(t)
	ctx := context.Background()

	if err := b.artifacts.Delete(ctx, "abcdef01.never-committed-1.0.0.tar.gz"); err != nil {
		t.Fatalf("Delete of an absent object: %v", err)
	}

	commitRoundTripArtifact(t, b, nil)
	if err := b.artifacts.Delete(ctx, artifactRoundTripKey); err != nil {
		t.Fatalf("Delete of a committed object: %v", err)
	}
	found, err := b.artifacts.Has(ctx, artifactRoundTripKey)
	if err != nil || found {
		t.Fatalf("Has after Delete = (%v, %v), want (false, nil)", found, err)
	}
}
