// Package s3 implements the cache backend on an S3-compatible store with its
// own SigV4 client and no AWS SDK. Its distributed lock rests on conditional
// writes, so Open refuses a store that does not enforce them.
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
	"strings"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/gzipstream"
	"github.com/klauspost/pgzip"
)

// Backend provides an S3-backed cache backend.
type Backend struct {
	client     *Client
	httpClient *http.Client
	artifacts  *Artifacts
	prefix     string
	tempDir    string
	cfg        config.S3CacheConfig
	lock       lockTiming
}

// New creates an S3-backed cache backend for the given config.
func New(cfg config.S3CacheConfig, httpClient *http.Client, tempDir string) (*Backend, error) {
	if cfg.Bucket == "" {
		return nil, errS3BucketEmpty
	}
	if httpClient == nil {
		return nil, errS3HTTPClientNil
	}
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	return &Backend{
		cfg:        cfg,
		httpClient: httpClient,
		prefix:     strings.Trim(cfg.Prefix, "/"),
		tempDir:    tempDir,
		lock: lockTiming{
			ttl:                lockTTL,
			heartbeatInterval:  heartbeatInterval,
			heartbeatOpTimeout: heartbeatOpTimeout,
			releaseTimeout:     lockReleaseTimeout,
			waitCeiling:        lockWaitCeiling,
			backoffBase:        lockBackoffBase,
			backoffCap:         lockBackoffCap,
		},
	}, nil
}

// Open initializes the S3 client, ensures the bucket exists, and probes that
// the store enforces the conditional writes the lock needs, so a store that
// ignores them fails here rather than letting two processes hold the lock.
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
	if err := b.probeConditionalPut(ctx); err != nil {
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

// Lock acquires an S3-based distributed lock. The returned holder context is
// canceled with a cause matching helpers.ErrCacheLockLost as soon as the
// heartbeat sees another acquirer's token on the lock object.
func (b *Backend) Lock(ctx context.Context) (context.Context, func() error, error) {
	if err := b.Open(ctx); err != nil {
		return nil, nil, err
	}
	lockKey := b.key(locksPrefix, lockObject)
	return b.acquireLock(ctx, lockKey)
}

// LoadStore loads the snapshot from S3: a newer schema is an error, an older
// one is dropped and rebuilt. The schema is probed before the full decode,
// since an outdated snapshot's buckets may no longer decode into *store.Store.
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

	var probe struct {
		Meta store.SnapshotMeta `json:"meta"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	switch verr := store.ValidateSchema(probe.Meta.SchemaVersion); {
	case errors.Is(verr, helpers.ErrOutdatedSchemaVersion):
		return store.New(), nil
	case verr != nil:
		return nil, verr
	}

	st := store.New()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, err
	}
	return st, nil
}

// SaveStore persists the snapshot to S3 as gzipped JSON. It must marshal via
// Store.MarshalSnapshot, which copies under the store's RLock, since other
// goroutines may still be mutating st.
func (b *Backend) SaveStore(ctx context.Context, st *store.Store) error {
	if st == nil {
		return nil
	}
	if err := b.Open(ctx); err != nil {
		return err
	}
	payload, err := st.MarshalSnapshot()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := pgzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		_ = zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	key := b.key(statePrefix, storeObject)
	reader := bytes.NewReader(buf.Bytes())
	return b.client.putObject(ctx, key, reader, int64(buf.Len()),
		putObjectAttrs{contentType: "application/json", contentEncoding: "gzip"}, putCondition{})
}

// ClearFiles removes cached artifacts from S3, batching the deletes via
// DeleteObjects (one batch per list page) rather than issuing one DELETE per
// object.
func (b *Backend) ClearFiles(ctx context.Context) error {
	if err := b.Open(ctx); err != nil {
		return err
	}
	return b.client.deleteAllUnderPrefix(ctx, b.key(artifactsPrefix))
}

// RecordProject records the project metadata in S3. The record itself is
// built by store.NewProjectRecord, the same function the local backend's
// registry goes through, so the object and the file hold one shape.
func (b *Backend) RecordProject(ctx context.Context, requirementsFile, downloadPath, rolesPath string) error {
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
	projectPath, record := store.NewProjectRecord(requirementsFile, downloadPath, rolesPath)
	registry.Projects[projectPath] = record
	return b.saveProjectRegistry(ctx, registry)
}

// LoadProjectRegistry loads the project registry from S3. A missing object is
// an empty registry, but one that fails to decode is an error: read as empty,
// it would make cleanup treat nothing as reachable and delete everything.
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
		return nil, fmt.Errorf("%w at %s: %w (remove the object or clear the cache to rebuild the registry)",
			helpers.ErrCorruptProjectRegistry, key, err)
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

// SweepTemp is a no-op for the S3 backend: its download temps live under the
// OS temp directory or a configured base, never in the shared cache.
func (b *Backend) SweepTemp(_ context.Context) error {
	return nil
}

// probeConditionalPut writes a unique probe object with If-None-Match: * and
// requires a second such write to be refused, then hands the object to
// probeCompareAndSwap; it owns the object's cleanup for both probes.
func (b *Backend) probeConditionalPut(ctx context.Context) error {
	suffix, err := generateLockToken()
	if err != nil {
		return err
	}
	key := b.key(locksPrefix, conditionalProbeObject+"-"+suffix)
	body := []byte("probe")

	if err := b.client.putObject(ctx, key, bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "text/plain"}, putCondition{ifNoneMatch: true}); err != nil {
		return err
	}
	//nolint:contextcheck // best-effort cleanup deliberately uses a fresh context, not ctx:
	// ctx may be near its own deadline by the time Open runs the probe, but a leftover probe
	// object is harmless (its random suffix avoids colliding with the next Open) so it is not
	// worth failing Open over.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), lockReleaseTimeout)
		defer cancel()
		_ = b.client.deleteObject(cleanupCtx, key)
	}()

	switch putErr := b.client.putObject(ctx, key, bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "text/plain"}, putCondition{ifNoneMatch: true}); {
	case putErr == nil:
		return errS3ConditionalPutUnsupported
	case errors.Is(putErr, errS3PreconditionFailed):
		return b.probeCompareAndSwap(ctx, key, body)
	default:
		return putErr
	}
}

// probeCompareAndSwap verifies If-Match, which reclaimIfExpired relies on: the
// HEAD must name an ETag, a swap on staleProbeETag must be refused, and one on
// the current ETag must succeed, or a dead holder's lock is never reclaimed.
func (b *Backend) probeCompareAndSwap(ctx context.Context, key string, body []byte) error {
	headers, err := b.client.headObject(ctx, key)
	if err != nil {
		return err
	}
	etag := strings.TrimSpace(headers.Get("ETag"))
	if etag == "" {
		return errS3CompareAndSwapUnsupported
	}

	switch staleErr := b.client.putObject(ctx, key, bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "text/plain"}, putCondition{ifMatch: staleProbeETag}); {
	case staleErr == nil:
		return errS3CompareAndSwapUnsupported
	case errors.Is(staleErr, errS3PreconditionFailed):
		// The refusal this probe requires; fall through to the positive half.
	default:
		return staleErr
	}

	switch currentErr := b.client.putObject(ctx, key, bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "text/plain"}, putCondition{ifMatch: etag}); {
	case currentErr == nil:
		return nil
	case errors.Is(currentErr, errS3PreconditionFailed), errors.Is(currentErr, errS3NotFound):
		return errS3CompareAndSwapUnsupported
	default:
		return currentErr
	}
}

// readObject downloads a cache-state object (snapshot or project registry),
// inflating gzip if needed under size caps on both sides. An oversized object
// or an empty gzip member is reclassified as corrupt cache state.
func (b *Backend) readObject(ctx context.Context, key string) ([]byte, error) {
	resp, err := b.client.getObject(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	data, err := readAllCapped(ctx, resp.Body, resp.Header, key,
		helpers.StateObjectMaxCompressedSize, helpers.StateObjectMaxDecompressedSize)
	if err != nil {
		// The cause is %v, not %w, to drop helpers.ErrResponseTooLarge: kept
		// reachable, the error would match ExitNetwork and ExitCacheCorrupt at
		// once and exitcode.FromError's check order would pick the exit code.
		if errors.Is(err, helpers.ErrResponseTooLarge) {
			//nolint:errorlint // deliberately %v, not %w: see the comment above.
			return nil, fmt.Errorf("%w: state object %s: %v", helpers.ErrStateObjectTooLarge, key, err)
		}
		// An empty gzip member means this is not what SaveStore writes. It is
		// reclassified here, not in an exitcode predicate, since the sentinel's
		// other readers must keep their own class; %w is safe as it has none.
		if errors.Is(err, helpers.ErrEmptyGzipMember) {
			return nil, fmt.Errorf("%w: state object %s: %w", helpers.ErrCorruptStateObject, key, err)
		}
		return nil, err
	}
	return data, nil
}

// readAllCapped reads a body, inflating gzip through internal/gzipstream under
// ctx and capping both sizes with helpers.ErrResponseTooLarge: the read runs
// under the distributed lock, so a hostile object would stall every runner.
func readAllCapped(
	ctx context.Context,
	body io.Reader,
	header http.Header,
	key string,
	compressedCap, decompressedCap int64,
) ([]byte, error) {
	limited := helpers.NewSizeLimitedReader(body, compressedCap)
	shouldGzip := isGzip(header) || strings.HasSuffix(key, ".gz")
	if !shouldGzip {
		return io.ReadAll(limited)
	}
	buffered := bufio.NewReader(limited)
	if isGzipStream(buffered) {
		gz, err := gzipstream.NewReader(ctx, buffered)
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = gz.Close()
		}()
		return io.ReadAll(helpers.NewSizeLimitedReader(gz, decompressedCap))
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
	return b.client.putObject(ctx, key, reader, int64(len(payload)),
		putObjectAttrs{contentType: "application/json"}, putCondition{})
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
