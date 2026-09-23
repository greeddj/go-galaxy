package s3

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// testArtifactSHA is the shared artifact sha value seeded across several
// installed-entry test fixtures.
const testArtifactSHA = "abc"

// TestLoadStoreRejectsNewerSchema mirrors the local backend's contract: a
// snapshot stamped with a schema version newer than this binary supports
// must be reported as an error rather than partially trusted.
func TestLoadStoreRejectsNewerSchema(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	putStoreObject(ctx, t, b, helpers.StoreSnapshotSchemaVersion+1, nil)

	_, err := b.LoadStore(ctx)
	if !errors.Is(err, helpers.ErrUnsupportedSchemaVersion) {
		t.Fatalf("expected ErrUnsupportedSchemaVersion, got %v", err)
	}
}

// TestLoadStoreDropsOlderSchema mirrors the local backend's contract: a
// snapshot stamped with a schema version older than current must be
// dropped and rebuilt, returning a fresh empty Store with a nil error.
func TestLoadStoreDropsOlderSchema(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	putStoreObject(ctx, t, b, helpers.StoreSnapshotSchemaVersion-1, func(st *store.Store) {
		st.SetAPICache("api", store.APICacheEntry{URL: "https://example.com/api"})
	})

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("expected nil error for outdated schema, got %v", err)
	}
	fresh := store.New()
	if loaded.Meta.SchemaVersion != fresh.Meta.SchemaVersion {
		t.Fatalf("expected fresh schema version %d, got %d", fresh.Meta.SchemaVersion, loaded.Meta.SchemaVersion)
	}
	if len(loaded.APICache) != 0 {
		t.Fatalf("expected an empty store after dropping an outdated schema, got %#v", loaded)
	}
}

// TestLoadStoreDropsV3ShapeAndRebuilds proves the schema is checked before the
// full decode: a v3-shape snapshot whose buckets no longer decode into the
// current Store is dropped and rebuilt with a nil error.
func TestLoadStoreDropsV3ShapeAndRebuilds(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	rawJSON := []byte(`{
		"meta": {"schema_version": 3, "last_snapshot": "2024-01-02T03:04:05Z"},
		"api_cache": {},
		"deps_cache": {"a.b": {"c.d": ">=1.0.0"}},
		"installed": {},
		"graph": {},
		"requirements": {},
		"roots": {},
		"resolved": {},
		"versions_cache": {"a.b": ["1.0.0", "2.0.0"]}
	}`)
	putRawStoreObject(ctx, t, b, rawJSON)

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("expected nil error dropping a v3-shape snapshot, got %v", err)
	}
	fresh := store.New()
	if loaded.Meta.SchemaVersion != fresh.Meta.SchemaVersion {
		t.Fatalf("expected fresh schema version %d, got %d", fresh.Meta.SchemaVersion, loaded.Meta.SchemaVersion)
	}
	if len(loaded.DepsCache) != 0 || len(loaded.Versions) != 0 {
		t.Fatalf("expected an empty store after dropping a v3-shape snapshot, got %#v", loaded)
	}
}

// TestLoadStoreCurrentSchemaCorruptDataErrors proves drop-and-rebuild is only
// for an outdated schema: a malformed bucket under the current schema version
// is a decode error, never a silently empty store.
func TestLoadStoreCurrentSchemaCorruptDataErrors(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	rawJSON := fmt.Appendf(nil, `{
		"meta": {"schema_version": %d, "last_snapshot": "2024-01-02T03:04:05Z"},
		"api_cache": {},
		"deps_cache": {},
		"installed": {},
		"graph": {},
		"requirements": {},
		"roots": {},
		"resolved": {},
		"versions_cache": {"a.b": ["1.0.0", "2.0.0"]}
	}`, helpers.StoreSnapshotSchemaVersion)
	putRawStoreObject(ctx, t, b, rawJSON)

	loaded, err := b.LoadStore(ctx)
	if err == nil {
		t.Fatalf("expected an error decoding a current-schema snapshot with a corrupt versions_cache bucket, got a store: %#v", loaded)
	}
	if loaded != nil {
		t.Fatalf("expected a nil store alongside the decode error, got %#v", loaded)
	}
}

// TestLoadStoreLoadsCurrentSchema confirms a snapshot stamped with the
// current schema version round-trips its data unchanged through LoadStore.
func TestLoadStoreLoadsCurrentSchema(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	putStoreObject(ctx, t, b, helpers.StoreSnapshotSchemaVersion, func(st *store.Store) {
		st.SetAPICache("api", store.APICacheEntry{URL: "https://example.com/api"})
		st.SetInstalled("a.b@1.0.0", store.InstalledEntry{ArtifactSHA256: testArtifactSHA})
	})

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore error: %v", err)
	}
	entry, ok := loaded.GetAPICache("api")
	if !ok || entry.URL != "https://example.com/api" {
		t.Fatalf("unexpected api cache entry: %#v (ok=%v)", entry, ok)
	}
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry: %#v (ok=%v)", installed, ok)
	}
}

// TestLoadStoreToleratesLegacyRootsKey proves a current-schema payload still
// carrying the removed "roots" bucket decodes with its real data intact:
// unknown keys are skipped, not rejected.
func TestLoadStoreToleratesLegacyRootsKey(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	rawJSON := fmt.Appendf(nil, `{
		"meta": {"schema_version": %d, "last_snapshot": "2024-01-02T03:04:05Z"},
		"api_cache": {},
		"deps_cache": {},
		"installed": {"a.b@1.0.0": {"install_path": "/tmp/a/b", "artifact_sha256": "abc"}},
		"graph": {},
		"requirements": {},
		"roots": {"last_run": ["a.b@1.0.0"]},
		"resolved": {},
		"versions_cache": {},
		"warmed": {}
	}`, helpers.StoreSnapshotSchemaVersion)
	putRawStoreObject(ctx, t, b, rawJSON)

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore error: %v", err)
	}
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry: %#v (ok=%v)", installed, ok)
	}
}

// TestLoadStoreToleratesNullRootsKey pins that an explicit "roots": null, a
// different decoder input from {}, is tolerated as an unknown key and decodes
// with the real data intact.
func TestLoadStoreToleratesNullRootsKey(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	rawJSON := fmt.Appendf(nil, `{
		"meta": {"schema_version": %d, "last_snapshot": "2024-01-02T03:04:05Z"},
		"api_cache": {},
		"deps_cache": {},
		"installed": {"a.b@1.0.0": {"install_path": "/tmp/a/b", "artifact_sha256": "abc"}},
		"graph": {},
		"requirements": {},
		"roots": null,
		"resolved": {},
		"versions_cache": {},
		"warmed": {}
	}`, helpers.StoreSnapshotSchemaVersion)
	putRawStoreObject(ctx, t, b, rawJSON)

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore error: %v", err)
	}
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry: %#v (ok=%v)", installed, ok)
	}
}

// TestLoadStoreToleratesNullBuckets pins that explicit JSON null buckets load
// and their mutators stay usable: without store.Store.UnmarshalJSON
// re-allocating nilled maps, the Set calls below would panic.
func TestLoadStoreToleratesNullBuckets(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	rawJSON := fmt.Appendf(nil, `{
		"meta": {"schema_version": %d, "last_snapshot": "2024-01-02T03:04:05Z"},
		"api_cache": null,
		"deps_cache": null,
		"installed": null,
		"graph": null,
		"requirements": null,
		"resolved": null,
		"versions_cache": null,
		"warmed": null,
		"git_pins": null
	}`, helpers.StoreSnapshotSchemaVersion)
	putRawStoreObject(ctx, t, b, rawJSON)

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore error: %v", err)
	}

	loaded.SetInstalled("a.b@1.0.0", store.InstalledEntry{ArtifactSHA256: testArtifactSHA})
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry: %#v (ok=%v)", installed, ok)
	}

	loaded.SetWarmed("a.b@1.0.0", "sha-1")
	if warmed := loaded.WarmedArtifactSHAByKey(); warmed["a.b@1.0.0"] != "sha-1" {
		t.Fatalf("unexpected warmed entry: %#v", warmed)
	}

	const pinCommit = "0123456789abcdef0123456789abcdef01234567"
	loaded.SetGitPin("url\nref\n", store.GitPinEntry{Commit: pinCommit})
	if pin, ok := loaded.GetGitPin("url\nref\n"); !ok || pin.Commit != pinCommit {
		t.Fatalf("unexpected git pin: %#v (ok=%v)", pin, ok)
	}
}

// TestSaveStoreConcurrentMutationIsRaceFree proves SaveStore never reads the
// live maps unlocked: under -race, it runs repeatedly while a goroutine keeps
// mutating the same store.
func TestSaveStoreConcurrentMutationIsRaceFree(t *testing.T) {
	b := newTestBackend(t)
	ctx := t.Context()
	st := store.New()

	// Reuse a small, fixed set of keys so the store's maps stay bounded:
	// an ever-growing key set would make each SaveStore's deep copy
	// progressively more expensive, snowballing into a runaway loop.
	const mutatorKeys = 8

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			key := fmt.Sprintf("mutator.key%d", i%mutatorKeys)
			st.SetInstalled(key, store.InstalledEntry{ArtifactSHA256: "sha"})
			st.SetAPICache("api", store.APICacheEntry{URL: fmt.Sprintf("https://example.com/%d", i)})
		}
	})

	const saveCount = 20
	var lastErr error
	for range saveCount {
		lastErr = b.SaveStore(ctx, st)
		if lastErr != nil {
			break
		}
	}
	close(done)
	wg.Wait()

	if lastErr != nil {
		t.Fatalf("expected SaveStore to succeed under concurrent mutation, got %v", lastErr)
	}
}

// TestSaveStoreStampsSchemaAndTimestamp confirms SaveStore stamps the
// current schema version and a non-zero last-snapshot time via
// MarshalSnapshot, exactly as the local backend's Save does.
func TestSaveStoreStampsSchemaAndTimestamp(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	st := store.New()
	st.SetAPICache("api", store.APICacheEntry{URL: "https://example.com/api"})

	if err := b.SaveStore(ctx, st); err != nil {
		t.Fatalf("SaveStore error: %v", err)
	}

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore error: %v", err)
	}
	if loaded.Meta.SchemaVersion != helpers.StoreSnapshotSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", helpers.StoreSnapshotSchemaVersion, loaded.Meta.SchemaVersion)
	}
	if loaded.Meta.LastSnapshot.IsZero() {
		t.Fatalf("expected a non-zero last_snapshot timestamp")
	}
}

// TestSaveStoreRoundTripsDataShape confirms a populated store, warmed bucket
// included, survives the gzipped SaveStore and LoadStore round trip exactly.
func TestSaveStoreRoundTripsDataShape(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	st := store.New()
	// A zero FetchedAt would be pruned by CacheEntryMaxAge at persist time,
	// hiding the entry regardless of the behavior under test.
	st.SetAPICache("api", store.APICacheEntry{URL: "https://example.com/api", ETag: "etag", FetchedAt: time.Now().UTC()})
	st.SetInstalled("a.b@1.0.0", store.InstalledEntry{ArtifactSHA256: testArtifactSHA})
	st.SetWarmed("c.d@1.0.0", "warmed-sha")

	if err := b.SaveStore(ctx, st); err != nil {
		t.Fatalf("SaveStore error: %v", err)
	}

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore error: %v", err)
	}
	entry, ok := loaded.GetAPICache("api")
	if !ok || entry.URL != "https://example.com/api" || entry.ETag != "etag" {
		t.Fatalf("unexpected api cache entry after round trip: %#v (ok=%v)", entry, ok)
	}
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry after round trip: %#v (ok=%v)", installed, ok)
	}
	if got := loaded.WarmedArtifactSHAByKey()["c.d@1.0.0"]; got != "warmed-sha" {
		t.Fatalf("unexpected warmed entry after round trip: %q", got)
	}
}

// TestLoadProjectRegistryRejectsCorruptObject confirms an undecodable registry
// is ErrCorruptProjectRegistry, not an empty registry that would make cleanup
// delete everything.
func TestLoadProjectRegistryRejectsCorruptObject(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	putProjectsObject(ctx, t, b, []byte("{invalid"))

	_, err := b.LoadProjectRegistry(ctx)
	if !errors.Is(err, helpers.ErrCorruptProjectRegistry) {
		t.Fatalf("expected ErrCorruptProjectRegistry, got %v", err)
	}
}

// TestLoadProjectRegistryMissingObjectReturnsEmpty is a regression guard: a
// bucket that has never recorded a project must still return an empty,
// initialized registry with a nil error.
func TestLoadProjectRegistryMissingObjectReturnsEmpty(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	registry, err := b.LoadProjectRegistry(ctx)
	if err != nil {
		t.Fatalf("expected nil error for a missing registry object, got %v", err)
	}
	if registry == nil || registry.Projects == nil {
		t.Fatalf("expected an initialized empty registry, got %#v", registry)
	}
	if len(registry.Projects) != 0 {
		t.Fatalf("expected no projects, got %#v", registry.Projects)
	}
}

// TestBackendSweepTempNoOp confirms the S3 backend's SweepTemp is a genuine
// no-op: its download temps live under the OS temp directory rather than the
// shared S3 storage, so there is nothing for the backend itself to sweep.
func TestBackendSweepTempNoOp(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	if err := b.SweepTemp(ctx); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

// TestReadAllCappedRejectsOversizedRaw confirms the compressed-size ceiling
// applies on the non-gzip path: a plain body longer than compressedCap must
// fail with helpers.ErrResponseTooLarge before it is fully buffered.
func TestReadAllCappedRejectsOversizedRaw(t *testing.T) {
	t.Parallel()

	const compressedCap = 16
	data := bytes.Repeat([]byte("x"), compressedCap*4)

	_, err := readAllCapped(t.Context(), bytes.NewReader(data), http.Header{}, "state/store.json",
		compressedCap, helpers.StateObjectMaxDecompressedSize)
	if !errors.Is(err, helpers.ErrResponseTooLarge) {
		t.Fatalf("readAllCapped() error = %v, want ErrResponseTooLarge", err)
	}
}

// TestReadAllCappedRejectsGzipBomb confirms the decompressed-size ceiling holds
// on its own: a gzip stream well under compressedCap that inflates past
// decompressedCap fails with helpers.ErrResponseTooLarge.
func TestReadAllCappedRejectsGzipBomb(t *testing.T) {
	t.Parallel()

	const decompressedCap = 64
	// A few KB of zeros compresses to well under any reasonable compressedCap,
	// while comfortably exceeding decompressedCap once inflated, so the
	// decompressed ceiling - not the compressed one - is what trips.
	payload := make([]byte, 8<<10)
	gz := gzipBytes(t, payload)
	if int64(len(gz)) >= helpers.StateObjectMaxCompressedSize {
		t.Fatalf("test setup: gzip payload (%d bytes) is not comfortably under the compressed cap", len(gz))
	}

	header := http.Header{"Content-Encoding": []string{"gzip"}}
	_, err := readAllCapped(t.Context(), bytes.NewReader(gz), header, "state/store.json.gz",
		helpers.StateObjectMaxCompressedSize, decompressedCap)
	if !errors.Is(err, helpers.ErrResponseTooLarge) {
		t.Fatalf("readAllCapped() error = %v, want ErrResponseTooLarge", err)
	}
}

// TestReadAllCappedAcceptsNormal confirms both the gzip and non-gzip paths
// still round-trip their exact bytes through readAllCapped when the input
// stays comfortably under both ceilings.
func TestReadAllCappedAcceptsNormal(t *testing.T) {
	t.Parallel()

	t.Run("gzip", func(t *testing.T) {
		t.Parallel()
		want := []byte("a small cache-state payload")
		gz := gzipBytes(t, want)

		header := http.Header{"Content-Encoding": []string{"gzip"}}
		got, err := readAllCapped(t.Context(), bytes.NewReader(gz), header, "state/store.json.gz", 64, 64)
		if err != nil {
			t.Fatalf("readAllCapped() error = %v, want nil", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("readAllCapped() = %q, want %q", got, want)
		}
	})

	t.Run("raw", func(t *testing.T) {
		t.Parallel()
		want := []byte("a small non-gzip payload")

		got, err := readAllCapped(t.Context(), bytes.NewReader(want), http.Header{}, "state/store.json", 64, 64)
		if err != nil {
			t.Fatalf("readAllCapped() error = %v, want nil", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("readAllCapped() = %q, want %q", got, want)
		}
	})
}

// TestReadObjectReclassifiesOversizedStateObject proves a state object over
// the production compressed ceiling is ErrStateObjectTooLarge and not
// ErrResponseTooLarge, with a within-cap object as the positive control.
func TestReadObjectReclassifiesOversizedStateObject(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// One byte past the ceiling on the non-gzip path, generated as served
	// rather than stored, since materializing 256 MiB twice proves nothing.
	oversizedKey := b.key(statePrefix, "oversized-state-object.json")
	fake.serveSyntheticBody(oversizedKey, helpers.StateObjectMaxCompressedSize+1)

	_, err := b.readObject(ctx, oversizedKey)
	if !errors.Is(err, helpers.ErrStateObjectTooLarge) {
		t.Fatalf("readObject(oversized) error = %v, want errors.Is(err, ErrStateObjectTooLarge) = true", err)
	}
	// readAllCapped's failure carries ErrResponseTooLarge; readObject must
	// break that chain (%v, not %w) so only one exit class matches.
	if errors.Is(err, helpers.ErrResponseTooLarge) {
		t.Fatalf("readObject(oversized) error = %v, want errors.Is(err, ErrResponseTooLarge) = false", err)
	}

	withinCapKey := b.key(statePrefix, "within-cap-state-object.json")
	want := []byte(`{"projects":{}}`)
	if err := b.client.putObject(ctx, withinCapKey, bytes.NewReader(want), int64(len(want)),
		putObjectAttrs{contentType: "application/json"}, putCondition{}); err != nil {
		t.Fatalf("putObject(within cap): %v", err)
	}
	got, err := b.readObject(ctx, withinCapKey)
	if err != nil {
		t.Fatalf("readObject(within cap) error = %v, want nil", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("readObject(within cap) = %q, want %q", got, want)
	}
}

// emptyGzipMember is the smallest gzip member (header, an empty final block,
// trailer), hand-spelled as a hostile bucket writer would plant it.
func emptyGzipMember() []byte {
	member := make([]byte, 0, 20)
	member = append(member, "\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\xff"...)
	member = append(member, 0x03, 0x00)
	return append(member, 0, 0, 0, 0, 0, 0, 0, 0)
}

// TestReadObjectReclassifiesAStateObjectThatWillNotInflate proves an empty
// gzip member is ErrCorruptStateObject with ErrEmptyGzipMember still reachable,
// with an ordinary gzipped object as the positive control.
func TestReadObjectReclassifiesAStateObjectThatWillNotInflate(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackendAndFake(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The .gz suffix is what routes readAllCapped down its inflating arm, the
	// same way the real snapshot object's own key does.
	emptyMemberKey := b.key(statePrefix, "empty-member-state-object.json.gz")
	member := emptyGzipMember()
	if err := b.client.putObject(ctx, emptyMemberKey, bytes.NewReader(member), int64(len(member)),
		putObjectAttrs{contentType: "application/gzip"}, putCondition{}); err != nil {
		t.Fatalf("putObject(empty member): %v", err)
	}

	_, err := b.readObject(ctx, emptyMemberKey)
	if !errors.Is(err, helpers.ErrCorruptStateObject) {
		t.Fatalf("readObject(empty member) = %v, want ErrCorruptStateObject", err)
	}
	if !errors.Is(err, helpers.ErrEmptyGzipMember) {
		t.Fatalf("readObject(empty member) = %v, want its cause reachable", err)
	}

	inflatableKey := b.key(statePrefix, "inflatable-state-object.json.gz")
	want := []byte(`{"projects":{}}`)
	body := gzipBytes(t, want)
	if err := b.client.putObject(ctx, inflatableKey, bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "application/gzip"}, putCondition{}); err != nil {
		t.Fatalf("putObject(inflatable): %v", err)
	}
	got, err := b.readObject(ctx, inflatableKey)
	if err != nil {
		t.Fatalf("readObject(inflatable) error = %v, want nil", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("readObject(inflatable) = %q, want %q", got, want)
	}
}

// gzipBytes gzip-encodes data with compress/gzip, whose wire format is the
// one the production reader inflates.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// putProjectsObject seeds state/projects.json with raw bytes as plain JSON,
// the encoding saveProjectRegistry writes.
func putProjectsObject(ctx context.Context, t *testing.T, b *Backend, data []byte) {
	t.Helper()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := b.key(statePrefix, projectsObject)
	reader := bytes.NewReader(data)
	attrs := putObjectAttrs{contentType: "application/json"}
	if err := b.client.putObject(ctx, key, reader, int64(len(data)), attrs, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}
}

// putStoreObject seeds state/store.json.gz with a store stamped at
// schemaVersion, gzipped JSON as SaveStore writes it.
func putStoreObject(ctx context.Context, t *testing.T, b *Backend, schemaVersion int, mutate func(*store.Store)) {
	t.Helper()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	st := store.New()
	st.Meta.SchemaVersion = schemaVersion
	if mutate != nil {
		mutate(st)
	}

	payload, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	key := b.key(statePrefix, storeObject)
	reader := bytes.NewReader(buf.Bytes())
	attrs := putObjectAttrs{contentType: "application/json", contentEncoding: "gzip"}
	if err := b.client.putObject(ctx, key, reader, int64(buf.Len()), attrs, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}
}

// putRawStoreObject seeds state/store.json.gz with raw JSON gzipped as
// SaveStore would, so a test can plant a shape the current Store cannot emit.
func putRawStoreObject(ctx context.Context, t *testing.T, b *Backend, rawJSON []byte) {
	t.Helper()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(rawJSON); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	key := b.key(statePrefix, storeObject)
	reader := bytes.NewReader(buf.Bytes())
	attrs := putObjectAttrs{contentType: "application/json", contentEncoding: "gzip"}
	if err := b.client.putObject(ctx, key, reader, int64(buf.Len()), attrs, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}
}

// TestRecordProjectRoundTripsRolesPath proves the registry object carries the
// recorded roles path resolved against the project directory, and that a run
// recording none leaves the key out.
func TestRecordProjectRoundTripsRolesPath(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	projectDir := t.TempDir()
	reqPath := filepath.Join(projectDir, "requirements.yml")
	if err := b.RecordProject(ctx, reqPath, "collections", "roles"); err != nil {
		t.Fatalf("RecordProject: %v", err)
	}
	registry, err := b.LoadProjectRegistry(ctx)
	if err != nil {
		t.Fatalf("LoadProjectRegistry: %v", err)
	}
	record, ok := registry.Projects[projectDir]
	if !ok {
		t.Fatalf("no record under %q, got %#v", projectDir, registry.Projects)
	}
	if want := filepath.Join(projectDir, "roles"); record.RolesPath != want {
		t.Fatalf("RolesPath = %q, want %q", record.RolesPath, want)
	}
	if want := filepath.Join(projectDir, "collections"); record.CollectionsPath != want {
		t.Fatalf("CollectionsPath = %q, want %q", record.CollectionsPath, want)
	}

	if err := b.RecordProject(ctx, reqPath, "collections", ""); err != nil {
		t.Fatalf("RecordProject without a roles path: %v", err)
	}
	data, err := b.readObject(ctx, b.key(statePrefix, projectsObject))
	if err != nil {
		t.Fatalf("readObject: %v", err)
	}
	if bytes.Contains(data, []byte("roles_path")) {
		t.Fatalf("a record without a roles path carries roles_path: %s", data)
	}
}
