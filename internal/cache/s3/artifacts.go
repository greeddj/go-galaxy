package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
)

// Artifacts implements ArtifactStore backed by S3 objects.
type Artifacts struct {
	client  *Client
	prefix  string
	tmpBase string
}

// Has reports whether the artifact exists in S3.
func (s *Artifacts) Has(ctx context.Context, key string) (bool, error) {
	if s.client == nil {
		return false, errS3ClientNil
	}
	_, err := s.client.headObject(ctx, s.objectKey(key))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, errS3NotFound) {
		return false, nil
	}
	return false, err
}

// Fetch downloads an artifact from S3 into a temporary file.
func (s *Artifacts) Fetch(ctx context.Context, key string) (cacheManager.ArtifactFile, error) {
	if s.client == nil {
		return cacheManager.ArtifactFile{}, errS3ClientNil
	}
	tmpFile, cleanup, err := s.TempFile(ctx, ".artifact-")
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	meta, sum, err := s.downloadToFile(ctx, key, tmpFile)
	if err != nil {
		_ = tmpFile.Close()
		cleanupIfNeeded(cleanup)
		return cacheManager.ArtifactFile{}, err
	}
	if err := tmpFile.Close(); err != nil {
		cleanupIfNeeded(cleanup)
		return cacheManager.ArtifactFile{}, err
	}
	if err := verifyArtifactSHA(meta, sum); err != nil {
		cleanupIfNeeded(cleanup)
		return cacheManager.ArtifactFile{}, err
	}
	return cacheManager.ArtifactFile{Path: tmpFile.Name(), Cleanup: cleanup, Meta: meta}, nil
}

func verifyArtifactSHA(meta map[string]string, sum []byte) error {
	if meta == nil {
		return nil
	}
	expected := strings.TrimSpace(meta["sha256"])
	if expected == "" {
		return nil
	}
	actual := hex.EncodeToString(sum)
	if strings.EqualFold(actual, expected) {
		return nil
	}
	return fmt.Errorf("%w: %s != %s", errArtifactSHA256Mismatch, actual, expected)
}

func cleanupIfNeeded(cleanup func()) {
	if cleanup != nil {
		cleanup()
	}
}

// TempFile creates a temporary file for staging an artifact.
func (s *Artifacts) TempFile(_ context.Context, prefix string) (*os.File, func(), error) {
	base := strings.TrimSpace(s.tmpBase)
	if base == "" {
		base = os.TempDir()
	}
	tmpFile, err := os.CreateTemp(base, prefix)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		_ = os.Remove(tmpFile.Name())
	}
	return tmpFile, cleanup, nil
}

// Commit uploads a temporary artifact to S3 and returns its file reference.
func (s *Artifacts) Commit(ctx context.Context, key, tmpPath string, meta map[string]string) (cacheManager.ArtifactFile, error) {
	if s.client == nil {
		return cacheManager.ArtifactFile{}, errS3ClientNil
	}
	//nolint:gosec // tmpPath is created by this process and is trusted.
	file, err := os.Open(tmpPath)
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	defer func() {
		_ = file.Close()
	}()
	info, err := file.Stat()
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	if meta == nil {
		meta = make(map[string]string)
	}
	payloadHash := strings.TrimSpace(meta["sha256"])
	if payloadHash == "" {
		hash, err := hashReader(file)
		if err != nil {
			return cacheManager.ArtifactFile{}, err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return cacheManager.ArtifactFile{}, err
		}
		payloadHash = hash
		meta["sha256"] = hash
	}
	if err := s.client.putObject(ctx, s.objectKey(key), file, info.Size(), "application/gzip", "", meta, false, payloadHash); err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	cleanup := func() {
		_ = os.Remove(tmpPath)
	}
	return cacheManager.ArtifactFile{Path: tmpPath, Cleanup: cleanup, Meta: meta}, nil
}

// Delete removes an artifact from S3.
func (s *Artifacts) Delete(ctx context.Context, key string) error {
	if s.client == nil {
		return errS3ClientNil
	}
	return s.client.deleteObject(ctx, s.objectKey(key))
}

// objectKey builds a full S3 object key for an artifact key.
func (s *Artifacts) objectKey(key string) string {
	trimmed := strings.TrimLeft(key, "/")
	return path.Join(s.prefix, trimmed)
}

// metaFromHeaders extracts user metadata from S3 response headers.
func metaFromHeaders(headers map[string][]string) map[string]string {
	meta := make(map[string]string)
	for name, values := range headers {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-meta-") {
			continue
		}
		if len(values) == 0 {
			continue
		}
		key := strings.TrimPrefix(lower, "x-amz-meta-")
		meta[key] = strings.TrimSpace(values[0])
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

func (s *Artifacts) downloadToFile(ctx context.Context, key string, file *os.File) (map[string]string, []byte, error) {
	resp, err := s.client.getObject(ctx, s.objectKey(key))
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	hasher := sha256.New()
	writer := io.MultiWriter(file, hasher)
	if _, err := io.Copy(writer, resp.Body); err != nil {
		return nil, nil, err
	}
	return metaFromHeaders(resp.Header), hasher.Sum(nil), nil
}
