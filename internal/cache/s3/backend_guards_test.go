package s3

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// TestNewRefusesAMissingBucketOrClient pins New's two refusals.
func TestNewRefusesAMissingBucketOrClient(t *testing.T) {
	t.Parallel()
	if _, err := New(config.S3CacheConfig{}, &http.Client{}, ""); !errors.Is(err, errS3BucketEmpty) {
		t.Errorf("New without a bucket = %v, want errS3BucketEmpty", err)
	}
	if _, err := New(config.S3CacheConfig{Bucket: "b"}, nil, ""); !errors.Is(err, errS3HTTPClientNil) {
		t.Errorf("New without an HTTP client = %v, want errS3HTTPClientNil", err)
	}
}

// TestNewNormalizesPrefixAndTempDir pins New's temp dir default and its
// prefix trim, which every object key is built under.
func TestNewNormalizesPrefixAndTempDir(t *testing.T) {
	t.Parallel()
	b, err := New(config.S3CacheConfig{Bucket: "b", Prefix: "/team/cache/"}, &http.Client{}, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.tempDir != os.TempDir() {
		t.Errorf("tempDir = %q, want os.TempDir() %q", b.tempDir, os.TempDir())
	}
	if got := b.key(); got != "team/cache" {
		t.Errorf("key() = %q, want the trimmed prefix", got)
	}
	if got := b.key(statePrefix, storeObject); got != "team/cache/state/store.json.gz" {
		t.Errorf("key(state, store) = %q, want team/cache/state/store.json.gz", got)
	}

	bare, err := New(config.S3CacheConfig{Bucket: "b"}, &http.Client{}, "/custom/tmp")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if bare.key() != "" || bare.key(locksPrefix, lockObject) != "locks/cache.lock" || bare.tempDir != "/custom/tmp" {
		t.Errorf("unprefixed backend: key() %q, lock key %q, tempDir %q", bare.key(), bare.key(locksPrefix, lockObject), bare.tempDir)
	}
}

// TestUnopenableBackendFailsEveryOperation pins that an endpoint Open refuses
// fails each operation needing the bucket with that refusal, leaves the
// backend unopened, and that Close and a nil SaveStore stay no-ops.
func TestUnopenableBackendFailsEveryOperation(t *testing.T) {
	t.Parallel()
	b, err := New(config.S3CacheConfig{Bucket: "b", Endpoint: "file:///srv/cache"}, &http.Client{}, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	calls := map[string]func() error{
		"Open": func() error { return b.Open(ctx) },
		"Lock": func() error {
			_, _, err := b.Lock(ctx)
			return err
		},
		"LoadStore": func() error {
			_, err := b.LoadStore(ctx)
			return err
		},
		"SaveStore":     func() error { return b.SaveStore(ctx, store.New()) },
		"ClearFiles":    func() error { return b.ClearFiles(ctx) },
		"RecordProject": func() error { return b.RecordProject(ctx, store.ProjectRun{RequirementsFile: "requirements.yml"}) },
		"LoadProjectRegistry": func() error {
			_, err := b.LoadProjectRegistry(ctx)
			return err
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, errS3InvalidEndpoint) || !errors.Is(err, helpers.ErrCacheBackendUnusable) {
			t.Errorf("%s = %v, want errS3InvalidEndpoint", name, err)
		}
	}
	if b.client != nil || b.artifacts != nil {
		t.Fatal("a refused Open left the backend opened")
	}
	if err := b.SaveStore(ctx, nil); err != nil {
		t.Errorf("SaveStore(nil) = %v, want nil without opening", err)
	}
	if err := b.Close(ctx); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

// TestLoadStoreOfAnEmptyBucketIsAFreshStore pins that a bucket with no
// snapshot object loads as a new, unpersisted, current-schema store.
func TestLoadStoreOfAnEmptyBucketIsAFreshStore(t *testing.T) {
	t.Parallel()
	st, err := newTestBackend(t).LoadStore(context.Background())
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if st.WasPersisted() || st.MetaSnapshot().SchemaVersion != helpers.StoreSnapshotSchemaVersion {
		t.Fatalf("LoadStore of an empty bucket = %+v, want a fresh store", st.MetaSnapshot())
	}
}

// TestRegistryReadFailuresStopTheRecord pins that a registry object that
// cannot be read or does not decode fails RecordProject without a PUT,
// so the object cleanup reads is never replaced by a guess.
func TestRegistryReadFailuresStopTheRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	run := store.ProjectRun{RequirementsFile: "/project/requirements.yml"}

	b, fake := newTestBackendAndFake(t)
	key := b.key(statePrefix, projectsObject)
	putProjectsObject(ctx, t, b, []byte("{not json"))
	if err := b.RecordProject(ctx, run); !errors.Is(err, helpers.ErrCorruptProjectRegistry) {
		t.Errorf("RecordProject over a corrupt registry = %v, want ErrCorruptProjectRegistry", err)
	}
	if got := fake.requestCount(key, http.MethodPut); got != 1 {
		t.Errorf("%d PUTs of the registry, want only the seeding one", got)
	}

	b, fake = newTestBackendAndFake(t)
	fake.failNextWithBody(key, http.MethodGet, http.StatusForbidden, -1, []byte(accessDeniedErrorBody))
	registry, err := b.LoadProjectRegistry(ctx)
	if !errors.Is(err, errS3GetFailed) || registry != nil {
		t.Errorf("LoadProjectRegistry on a 403 = %v, %v; want nil and errS3GetFailed", registry, err)
	}
	if err := b.RecordProject(ctx, run); !errors.Is(err, errS3GetFailed) {
		t.Errorf("RecordProject on a 403 = %v, want errS3GetFailed", err)
	}
	if got := fake.requestCount(key, http.MethodPut); got != 0 {
		t.Errorf("%d PUTs of the registry after a refused read, want none", got)
	}
}

// TestLoadProjectRegistryRestoresANullProjectsMap pins that a registry whose
// projects key is null loads with an empty map and its location set.
func TestLoadProjectRegistryRestoresANullProjectsMap(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	putProjectsObject(context.Background(), t, b, []byte(`{"projects":null}`))
	registry, err := b.LoadProjectRegistry(context.Background())
	if err != nil {
		t.Fatalf("LoadProjectRegistry: %v", err)
	}
	if registry.Projects == nil || len(registry.Projects) != 0 {
		t.Fatalf("Projects = %#v, want an empty non-nil map", registry.Projects)
	}
	if want := "s3://test/state/projects.json"; registry.Location != want {
		t.Fatalf("Location = %q, want %q", registry.Location, want)
	}
}

// TestSaveProjectRegistrySkipsANilRegistry pins that nil sends no PUT.
func TestSaveProjectRegistrySkipsANilRegistry(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.saveProjectRegistry(context.Background(), nil); err != nil {
		t.Fatalf("saveProjectRegistry(nil) = %v, want nil", err)
	}
	if got := fake.requestCount(b.key(statePrefix, projectsObject), http.MethodPut); got != 0 {
		t.Fatalf("%d PUTs of the registry, want none", got)
	}
}

// TestArtifactsAreBuiltByOpen pins that Open builds the artifact store under
// the artifacts prefix, staging temps in the backend's temp dir.
func TestArtifactsAreBuiltByOpen(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	artifacts, ok := b.Artifacts().(*Artifacts)
	if !ok || artifacts == nil {
		t.Fatalf("Artifacts() = %#v, want the *Artifacts Open built", b.Artifacts())
	}
	if artifacts.prefix != artifactsPrefix || artifacts.tmpBase != b.tempDir || artifacts.client != b.client {
		t.Fatalf("Artifacts = prefix %q, tmpBase %q; want %q under the backend's client and temp dir",
			artifacts.prefix, artifacts.tmpBase, artifactsPrefix)
	}
}

// TestReadAllCappedShortAndTruncatedGzip pins that a body too short to carry
// gzip magic is read as is, while one that opens a gzip member and breaks
// off fails.
func TestReadAllCappedShortAndTruncatedGzip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	data, err := readAllCapped(ctx, bytes.NewReader([]byte("x")), http.Header{}, "state/store.json.gz", 1<<10, 1<<10)
	if err != nil || string(data) != "x" {
		t.Fatalf("readAllCapped of a one-byte body = %q, %v; want it read as is", data, err)
	}
	header := http.Header{"Content-Encoding": {"gzip"}}
	if _, err := readAllCapped(ctx, bytes.NewReader([]byte{0x1f, 0x8b, 0x08}), header, "obj", 1<<10, 1<<10); err == nil {
		t.Fatal("readAllCapped of a truncated gzip header = nil error, want a failure")
	}
}
