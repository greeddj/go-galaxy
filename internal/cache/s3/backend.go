package s3

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	gzip "github.com/klauspost/pgzip"
)

// Backend provides an S3-backed cache backend.
type Backend struct {
	cfg        config.S3CacheConfig
	client     *Client
	httpClient *http.Client
	prefix     string
	artifacts  *Artifacts
	tempDir    string
}

// New creates an S3-backed cache backend for the given config.
func New(cfg config.S3CacheConfig, httpClient *http.Client, tempDir string) (*Backend, error) {
	if cfg.Bucket == "" {
		return nil, errS3BucketIsEmpty
	}
	if httpClient == nil {
		return nil, errS3HttpClientIsNil
	}
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	return &Backend{
		cfg:        cfg,
		httpClient: httpClient,
		prefix:     strings.Trim(cfg.Prefix, "/"),
		tempDir:    tempDir,
	}, nil
}

// Open initializes the S3 client and ensures the bucket exists.
func (b *Backend) Open(ctx context.Context) error {
	if b.client != nil {
		return nil
	}
	client, err := newClient(b.cfg, b.httpClient)
	if err != nil {
		return err
	}
	b.client = client
	if err := b.client.ensureBucket(ctx); err != nil {
		b.client = nil
		return err
	}
	b.artifacts = &Artifacts{
		client:  client,
		prefix:  b.key(artifactsPrefix),
		tmpBase: b.tempDir,
	}
	return nil
}

// Close releases backend resources.
func (b *Backend) Close(_ context.Context) error {
	return nil
}

// Lock acquires an S3-based distributed lock.
func (b *Backend) Lock(ctx context.Context) (func() error, error) {
	if err := b.Open(ctx); err != nil {
		return nil, err
	}
	lockKey := b.key(locksPrefix, lockObject)
	return b.acquireLock(ctx, lockKey)
}

// LoadStore loads the snapshot store from S3.
func (b *Backend) LoadStore(ctx context.Context) (*store.Store, error) {
	if err := b.Open(ctx); err != nil {
		return nil, err
	}
	key := b.key(statePrefix, storeObject)
	data, err := b.readObject(ctx, key)
	if err != nil {
		if errors.Is(err, errS3NotFound) {
			return store.New(), nil
		}
		return nil, err
	}
	st := store.New()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, err
	}
	return st, nil
}

// SaveStore persists the snapshot store to S3.
func (b *Backend) SaveStore(ctx context.Context, st *store.Store) error {
	if st == nil {
		return nil
	}
	if err := b.Open(ctx); err != nil {
		return err
	}
	payload, err := json.Marshal(st)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		_ = zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	key := b.key(statePrefix, storeObject)
	reader := bytes.NewReader(buf.Bytes())
	return b.client.putObject(ctx, key, reader, int64(buf.Len()), "application/json", "gzip", nil, false, "")
}

// ClearFiles removes cached artifacts from S3.
func (b *Backend) ClearFiles(ctx context.Context) error {
	if err := b.Open(ctx); err != nil {
		return err
	}
	prefix := b.key(artifactsPrefix)
	keys, err := b.client.listObjects(ctx, prefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := b.client.deleteObject(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

// RecordProject records the project metadata in S3.
func (b *Backend) RecordProject(ctx context.Context, requirementsFile, downloadPath string) error {
	if err := b.Open(ctx); err != nil {
		return err
	}
	registry, err := b.LoadProjectRegistry(ctx)
	if err != nil {
		return err
	}
	if registry.Projects == nil {
		registry.Projects = make(map[string]store.ProjectRecord)
	}
	absReq, err := filepath.Abs(requirementsFile)
	if err != nil {
		absReq = requirementsFile
	}
	projectPath := filepath.Dir(absReq)
	collectionsPath := resolveCollectionsPath(projectPath, downloadPath)
	registry.Projects[projectPath] = store.ProjectRecord{
		RequirementsFile: absReq,
		CollectionsPath:  collectionsPath,
		LastRun:          time.Now().UTC(),
	}
	return b.saveProjectRegistry(ctx, registry)
}

// LoadProjectRegistry loads the project registry from S3.
func (b *Backend) LoadProjectRegistry(ctx context.Context) (*store.ProjectRegistry, error) {
	if err := b.Open(ctx); err != nil {
		return nil, err
	}
	key := b.key(statePrefix, projectsObject)
	data, err := b.readObject(ctx, key)
	if err != nil {
		if errors.Is(err, errS3NotFound) {
			return &store.ProjectRegistry{Projects: make(map[string]store.ProjectRecord)}, nil
		}
		return nil, err
	}
	var registry store.ProjectRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return &store.ProjectRegistry{Projects: make(map[string]store.ProjectRecord)}, nil
	}
	if registry.Projects == nil {
		registry.Projects = make(map[string]store.ProjectRecord)
	}
	return &registry, nil
}

// Artifacts returns the S3-backed artifact store.
func (b *Backend) Artifacts() cacheManager.ArtifactStore {
	return b.artifacts
}

// acquireLock creates or steals an expired lock in S3.
func (b *Backend) acquireLock(ctx context.Context, lockKey string) (func() error, error) {
	release := func() error {
		return b.client.deleteObject(ctx, lockKey)
	}
	if err := b.putLock(ctx, lockKey); err == nil {
		return release, nil
	} else if !errors.Is(err, errS3PreconditionFailed) {
		return nil, err
	}
	return b.handleExistingLock(ctx, lockKey, release)
}

// handleExistingLock resolves lock contention after precondition failure.
func (b *Backend) handleExistingLock(ctx context.Context, lockKey string, release func() error) (func() error, error) {
	headers, headErr := b.client.headObject(ctx, lockKey)
	if errors.Is(headErr, errS3NotFound) {
		if err := b.putLock(ctx, lockKey); err != nil {
			if errors.Is(err, errS3PreconditionFailed) {
				return nil, fmt.Errorf("%w: %s", errS3LockAlreadyIsExists, lockKey)
			}
			return nil, err
		}
		return release, nil
	}
	if headErr != nil {
		return nil, headErr
	}
	expired, err := lockExpired(headers, lockTTL)
	if err != nil {
		return nil, err
	}
	if !expired {
		return nil, fmt.Errorf("%w: %s", errS3LockAlreadyIsExists, lockKey)
	}
	if err := release(); err != nil {
		return nil, err
	}
	if err := b.putLock(ctx, lockKey); err != nil {
		if errors.Is(err, errS3PreconditionFailed) {
			return nil, fmt.Errorf("%w: %s", errS3LockAlreadyIsExists, lockKey)
		}
		return nil, err
	}
	return release, nil
}

// putLock writes a lock object with metadata for this process.
func (b *Backend) putLock(ctx context.Context, lockKey string) error {
	host, _ := os.Hostname()
	payload := fmt.Sprintf("pid=%d host=%s time=%s\n", os.Getpid(), host, time.Now().UTC().Format(time.RFC3339))
	meta := map[string]string{
		"pid":  strconv.Itoa(os.Getpid()),
		"host": host,
		"time": time.Now().UTC().Format(time.RFC3339),
	}
	reader := strings.NewReader(payload)
	return b.client.putObject(ctx, lockKey, reader, int64(len(payload)), "text/plain", "", meta, true, "")
}

// readObject downloads an object and transparently inflates gzip data if needed.
func (b *Backend) readObject(ctx context.Context, key string) ([]byte, error) {
	resp, err := b.client.getObject(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	shouldGzip := isGzip(resp.Header) || strings.HasSuffix(key, ".gz")
	if !shouldGzip {
		return io.ReadAll(resp.Body)
	}
	buffered := bufio.NewReader(resp.Body)
	if isGzipStream(buffered) {
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = gz.Close()
		}()
		return io.ReadAll(gz)
	}
	return io.ReadAll(buffered)
}

// saveProjectRegistry writes the project registry to S3.
func (b *Backend) saveProjectRegistry(ctx context.Context, registry *store.ProjectRegistry) error {
	if registry == nil {
		return nil
	}
	payload, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	key := b.key(statePrefix, projectsObject)
	reader := bytes.NewReader(payload)
	return b.client.putObject(ctx, key, reader, int64(len(payload)), "application/json", "", nil, false, "")
}

// key builds a key under the configured S3 prefix.
func (b *Backend) key(parts ...string) string {
	if len(parts) == 0 {
		return b.prefix
	}
	if b.prefix == "" {
		return path.Join(parts...)
	}
	all := make([]string, 0, len(parts)+1)
	all = append(all, b.prefix)
	all = append(all, parts...)
	return path.Join(all...)
}

// resolveCollectionsPath returns an absolute collections path for a project.
func resolveCollectionsPath(projectPath, downloadPath string) string {
	if downloadPath == "" {
		return ""
	}
	if filepath.IsAbs(downloadPath) {
		return downloadPath
	}
	return filepath.Join(projectPath, downloadPath)
}

// isGzip reports whether the headers indicate gzip encoding.
func isGzip(headers http.Header) bool {
	enc := strings.ToLower(strings.TrimSpace(headers.Get("Content-Encoding")))
	return strings.Contains(enc, "gzip")
}

// isGzipStream reports whether the stream begins with gzip magic bytes.
func isGzipStream(reader *bufio.Reader) bool {
	header, err := reader.Peek(peekBytes)
	if err != nil || len(header) < headerLength {
		return false
	}
	return header[0] == 0x1f && header[1] == 0x8b
}

// lockExpired reports whether the lock is older than ttl.
func lockExpired(headers http.Header, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, errS3LockTTLIsInvalid
	}
	timestamp, err := lockTimestamp(headers)
	if err != nil {
		return false, err
	}
	return time.Since(timestamp) > ttl, nil
}

// lockTimestamp extracts lock creation time from object metadata.
func lockTimestamp(headers http.Header) (time.Time, error) {
	if headers == nil {
		return time.Time{}, errS3LockHeaderIsMissing
	}
	if value := strings.TrimSpace(headers.Get("X-Amz-Meta-Time")); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return time.Time{}, err
		}
		return parsed, nil
	}
	if value := strings.TrimSpace(headers.Get("Last-Modified")); value != "" {
		parsed, err := http.ParseTime(value)
		if err != nil {
			return time.Time{}, err
		}
		return parsed, nil
	}
	return time.Time{}, errS3LockTimestampIsMissing
}
