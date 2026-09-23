package collections

// Tests that a prefetched temp reaches its install worker through Wait and is
// reused, not fetched again, against an S3-style store stub and a real
// extracted.Store, so the temp still passes the same ingest checks.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// wrongPinSHA256 is a well-formed but deliberately incorrect sha256 hex
// string, standing in for a lockfile pin that does not match the real
// artifact - see TestPrefetchedArtifactFailsClosedOnPinMismatch.
const wrongPinSHA256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// s3StyleArtifacts is a cacheManager.ArtifactStore stub with the S3 backend's
// semantics: Commit copies without consuming the temp, Fetch always copies
// into a fresh temp. Has, Fetch and Cleanup calls are counted per key.
type s3StyleArtifacts struct {
	fetchCount   map[string]int
	cleanupCount map[string]int
	hasCount     map[string]int
	committed    map[string]string
	tmpBase      string
	bucketDir    string
	// commitOrder is every Commit key in call order, which
	// TestPrefetchQueueOrderedByLevel reads as the prefetch queue order.
	commitOrder []string
	mu          sync.Mutex
}

// newS3StyleArtifacts builds an s3StyleArtifacts rooted at two fresh
// directories under t.TempDir(): tmpBase stands in for wherever an S3 backend
// stages its local temps, bucketDir stands in for the S3 bucket itself.
func newS3StyleArtifacts(t *testing.T) *s3StyleArtifacts {
	t.Helper()
	root := t.TempDir()
	tmpBase := filepath.Join(root, "tmp")
	bucketDir := filepath.Join(root, "bucket")
	if err := os.MkdirAll(tmpBase, helpers.DirMod); err != nil {
		t.Fatalf("mkdir tmpBase: %v", err)
	}
	if err := os.MkdirAll(bucketDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir bucketDir: %v", err)
	}
	return &s3StyleArtifacts{
		tmpBase:      tmpBase,
		bucketDir:    bucketDir,
		fetchCount:   make(map[string]int),
		cleanupCount: make(map[string]int),
		hasCount:     make(map[string]int),
		committed:    make(map[string]string),
	}
}

// Has reports whether key is in the stub bucket, counting the call so a test
// can assert how many probes a key saw.
func (a *s3StyleArtifacts) Has(_ context.Context, key string) (bool, error) {
	a.mu.Lock()
	a.hasCount[key]++
	a.mu.Unlock()
	_, err := os.Stat(filepath.Join(a.bucketDir, key))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Meta is never exercised by this file's own tests (the prefetch handoff
// never calls it); it mirrors Has's own presence check and reports no
// metadata, consistent with this stub bucket never recording any.
func (a *s3StyleArtifacts) Meta(ctx context.Context, key string) (map[string]string, bool, error) {
	found, err := a.Has(ctx, key)
	return nil, found, err
}

// TempFile stages a fresh temp file under tmpBase, mirroring the real S3
// backend's own TempFile.
func (a *s3StyleArtifacts) TempFile(_ context.Context, prefix string) (*os.File, func(), error) {
	f, err := os.CreateTemp(a.tmpBase, prefix)
	if err != nil {
		return nil, nil, err
	}
	path := f.Name()
	return f, func() { _ = os.Remove(path) }, nil
}

// Commit copies tmpPath into the stub bucket without consuming it, like the
// S3 backend; its Cleanup is counted so a test can assert the temp was
// released exactly once.
func (a *s3StyleArtifacts) Commit(_ context.Context, key, tmpPath string, meta map[string]string) (cacheManager.ArtifactFile, error) {
	if err := copyFile(tmpPath, filepath.Join(a.bucketDir, key)); err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	a.mu.Lock()
	a.committed[key] = tmpPath
	a.commitOrder = append(a.commitOrder, key)
	a.mu.Unlock()
	return cacheManager.ArtifactFile{Path: tmpPath, Meta: meta, Cleanup: a.countingCleanup(key, tmpPath)}, nil
}

// Fetch counts one object-store GET for key - the signal this stub exists to
// let a test observe - then copies the bucket object into a fresh temp file,
// matching the real S3 backend's own re-download-on-Fetch behavior.
func (a *s3StyleArtifacts) Fetch(_ context.Context, key string) (cacheManager.ArtifactFile, error) {
	a.mu.Lock()
	a.fetchCount[key]++
	a.mu.Unlock()
	f, err := os.CreateTemp(a.tmpBase, "fetch-")
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return cacheManager.ArtifactFile{}, err
	}
	if err := copyFile(filepath.Join(a.bucketDir, key), path); err != nil {
		_ = os.Remove(path)
		return cacheManager.ArtifactFile{}, err
	}
	return cacheManager.ArtifactFile{Path: path, Cleanup: a.countingCleanup(key, path)}, nil
}

// Delete removes key's object from the stub bucket, ignoring an
// already-absent object exactly like a real backend's idempotent delete.
func (a *s3StyleArtifacts) Delete(_ context.Context, key string) error {
	err := os.Remove(filepath.Join(a.bucketDir, key))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// countingCleanup returns a Cleanup func that increments key's cleanup
// counter before removing path.
func (a *s3StyleArtifacts) countingCleanup(key, path string) func() {
	return func() {
		a.mu.Lock()
		a.cleanupCount[key]++
		a.mu.Unlock()
		_ = os.Remove(path)
	}
}

// fetchCountFor reports how many times Fetch has been called for key.
func (a *s3StyleArtifacts) fetchCountFor(key string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fetchCount[key]
}

// cleanupCountFor reports how many times a Cleanup returned for key has run.
func (a *s3StyleArtifacts) cleanupCountFor(key string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cleanupCount[key]
}

// hasCountFor reports how many times Has has been called for key.
func (a *s3StyleArtifacts) hasCountFor(key string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hasCount[key]
}

// committedPath returns the tmpPath Commit last recorded for key, or "" if
// key was never committed.
func (a *s3StyleArtifacts) committedPath(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.committed[key]
}

// commitOrderSnapshot returns a copy of the keys passed to Commit, in call
// order, so a test can assert on commit sequencing without racing a
// concurrent Commit call.
func (a *s3StyleArtifacts) commitOrderSnapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	order := make([]string, len(a.commitOrder))
	copy(order, a.commitOrder)
	return order
}

// copyFile copies src's bytes to dst, creating (or truncating) dst with
// helpers.FileMod permissions.
func copyFile(src, dst string) error {
	//nolint:gosec // both paths are this test's own temp dirs.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	//nolint:gosec // dst is this test's own temp dir.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, helpers.FileMod)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// assertPathAbsent fails the test unless path does not exist.
func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected %s to be absent", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error on %s: %v", path, err)
	}
}

// prefetchHandoffFixture bundles the pieces every test in this file needs to
// drive installLevels directly against a stub S3-style artifact store and a
// real content-addressable extracted store.
type prefetchHandoffFixture struct {
	cfg          *config.Config
	runtime      *infra.Infra
	st           *store.Store
	artifacts    *s3StyleArtifacts
	extractStore *extracted.Store
	// root is the real collections root opened for cfg.DownloadPath, threaded
	// into every newPrefetchDeps/newInstallDeps call this fixture drives - both
	// now build an installTarget from it on every collection.
	root *os.Root
}

// newPrefetchHandoffFixture builds a fixture wired to srv with the given
// worker count and NoDeps set; dependency resolution is not under test here.
func newPrefetchHandoffFixture(t *testing.T, srv *fakegalaxy.Server, workers int) *prefetchHandoffFixture {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	cfg := &config.Config{
		Server:       srv.URL(),
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      workers,
		NoDeps:       true,
	}
	return &prefetchHandoffFixture{
		cfg:          cfg,
		runtime:      infra.New(noopPrinter{}, srv.Client()),
		st:           store.New(),
		artifacts:    newS3StyleArtifacts(t),
		extractStore: extracted.NewStore(cacheDir),
		root:         newTestCollectionsRoot(t, downloadPath),
	}
}

// installPath returns the ansible_collections install path installLevels
// would use for col.
func (f *prefetchHandoffFixture) installPath(col collection) string {
	return filepath.Join(f.cfg.DownloadPath, "ansible_collections", col.Namespace, col.Name)
}

// runLevels starts a prefetcher and drives installLevels against it,
// returning the prefetcher still open: the caller Closes it after asserting.
func (f *prefetchHandoffFixture) runLevels(
	collections map[string]collection,
	graph map[string][]string,
	levels [][]string,
) (*prefetcher, failureSummary, error) {
	prefetch := startPrefetcher(context.Background(), newPrefetchDeps(f.cfg, f.runtime, f.st, f.artifacts, f.root), collections, levels)
	deps := newInstallDeps(f.cfg, f.runtime, f.st, f.artifacts, f.extractStore, f.root, prefetch.cachedArtifacts(), nil)
	plan := &installPlan{
		collections: collections,
		graph:       graph,
		levels:      levels,
		prefetch:    prefetch,
	}
	failures, err := installLevels(context.Background(), deps, plan)
	return prefetch, failures, err
}

// TestPrefetchedArtifactReusedNotRefetched pins that the install worker reuses
// the prefetched temp with no Fetch, that the scan's Has is the key's only
// probe, and that the temp is cleaned up exactly once.
func TestPrefetchedArtifactReusedNotRefetched(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	fx := newPrefetchHandoffFixture(t, srv, 1)
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}
	collections := map[string]collection{col.key(): col}
	graph := map[string][]string{col.key(): {}}
	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels: %v", err)
	}

	prefetch, summary, err := fx.runLevels(collections, graph, levels)
	if err != nil {
		t.Fatalf("installLevels: %v", err)
	}
	if summary.count != 0 {
		t.Fatalf("failures = %d, want 0", summary.count)
	}
	// Positive control for TestPrefetchedArtifactFailsClosedOnPinMismatch's
	// cause assertion below: a successful install records no cause at all, not
	// just a zero count.
	if summary.cause != nil {
		t.Fatalf("summary.cause = %v, want nil on a successful install", summary.cause)
	}
	assertFileContent(t, filepath.Join(fx.installPath(col), "README.md"), "# acme.app\n")

	if got := srv.Count(fakegalaxy.EndpointArtifact); got != 1 {
		t.Fatalf("EndpointArtifact count = %d, want 1 (downloaded once, by the prefetcher)", got)
	}
	key := artifactKey(col)
	if got := fx.artifacts.fetchCountFor(key); got != 0 {
		t.Fatalf("fetchCount = %d, want 0 (the prefetched temp must be reused, not refetched)", got)
	}
	if got := fx.artifacts.hasCountFor(key); got != 1 {
		t.Fatalf("hasCount = %d, want exactly 1 (one scan probe, no prefetchOne re-probe, no install-worker probe)", got)
	}

	prefetch.Close()
	if got := fx.artifacts.cleanupCountFor(key); got != 1 {
		t.Fatalf("cleanupCount = %d, want exactly 1 (consumed once by installCollection, not double-removed by Close)", got)
	}
}

// TestCachedArtifactProbedOnceAcrossScanAndInstall pins that a cached,
// uninstalled artifact is left unscheduled and its install worker reuses the
// scan's answer through deps.presence instead of probing Has again.
func TestCachedArtifactProbedOnceAcrossScanAndInstall(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	fx := newPrefetchHandoffFixture(t, srv, 1)
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}
	collections := map[string]collection{col.key(): col}
	graph := map[string][]string{col.key(): {}}
	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels: %v", err)
	}

	content := buildTarGzWithEntry(t, "README.md", []byte("# acme.app\n"))
	tmpPath := filepath.Join(t.TempDir(), "precached.tar.gz")
	if err := os.WriteFile(tmpPath, content, helpers.FileMod); err != nil {
		t.Fatalf("write pre-cached artifact: %v", err)
	}
	key := artifactKey(col)
	if _, err := fx.artifacts.Commit(context.Background(), key, tmpPath, nil); err != nil {
		t.Fatalf("pre-commit artifact into the stub bucket: %v", err)
	}

	prefetch, summary, err := fx.runLevels(collections, graph, levels)
	if err != nil {
		t.Fatalf("installLevels: %v", err)
	}
	if summary.count != 0 {
		t.Fatalf("failures = %d, want 0", summary.count)
	}
	assertFileContent(t, filepath.Join(fx.installPath(col), "README.md"), "# acme.app\n")

	if got := fx.artifacts.hasCountFor(key); got != 1 {
		t.Fatalf("hasCount = %d, want exactly 1 (one scan probe, no install-worker re-probe)", got)
	}
	if got := srv.Count(fakegalaxy.EndpointArtifact); got != 0 {
		t.Fatalf("EndpointArtifact count = %d, want 0 (the artifact was already cached, never downloaded from the origin)", got)
	}

	prefetch.Close()
}

// TestUncachedArtifactStillProbedByInstallWorkerAfterFailedPrefetch is the
// positive control for the probe count: a scheduled key gets no presence
// hint, so after its prefetch fails the install worker probes Has itself.
func TestUncachedArtifactStillProbedByInstallWorkerAfterFailedPrefetch(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)
	srv.Fail(fakegalaxy.EndpointArtifact, "acme", "app", fakegalaxy.Fault{Status: http.StatusInternalServerError, Count: -1})

	fx := newPrefetchHandoffFixture(t, srv, 1)
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}
	collections := map[string]collection{col.key(): col}
	graph := map[string][]string{col.key(): {}}
	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels: %v", err)
	}

	prefetch, summary, err := fx.runLevels(collections, graph, levels)
	if err != nil {
		t.Fatalf("installLevels: %v", err)
	}
	if summary.count == 0 {
		t.Fatalf("expected failures > 0 from the persistently failing artifact download")
	}

	key := artifactKey(col)
	if got := fx.artifacts.hasCountFor(key); got != 2 {
		t.Fatalf("hasCount = %d, want exactly 2 (one scan probe, one install-worker probe after the failed prefetch)", got)
	}

	prefetch.Close()
	assertPathAbsent(t, fx.installPath(col))
}

// TestUnconsumedPrefetchTempReclaimedOnLevelFailure pins that a prefetched
// temp whose level never ran, after an earlier level failed, is reclaimed
// exactly once by Close rather than leaked.
func TestUnconsumedPrefetchTempReclaimedOnLevelFailure(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", map[string]string{"acme.lib": ">=1.0.0"})
	srv.AddVersion("acme", "lib", "1.0.0", nil)
	srv.Fail(fakegalaxy.EndpointArtifact, "acme", "lib", fakegalaxy.Fault{Status: http.StatusInternalServerError, Count: -1})

	fx := newPrefetchHandoffFixture(t, srv, 4)
	app := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}
	lib := collection{Namespace: "acme", Name: "lib", Version: "1.0.0"}
	collections := map[string]collection{app.key(): app, lib.key(): lib}
	graph := map[string][]string{
		app.key(): {lib.key()},
		lib.key(): {},
	}
	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels: %v", err)
	}
	if len(levels) != 2 || len(levels[0]) != 1 || levels[0][0] != lib.key() {
		t.Fatalf("unexpected levels, want [[lib],[app]]: %#v", levels)
	}

	prefetch, summary, err := fx.runLevels(collections, graph, levels)
	if err != nil {
		t.Fatalf("installLevels: %v", err)
	}
	if summary.count == 0 {
		t.Fatalf("expected failures > 0 from acme.lib's persistently failing artifact download")
	}

	appKey := artifactKey(app)
	appTemp := fx.artifacts.committedPath(appKey)
	if appTemp == "" {
		t.Fatalf("expected the prefetcher to have committed acme.app's artifact before level 1 was reached")
	}
	assertExists(t, appTemp)

	prefetch.Close()
	assertReclaimedExactlyOnce(t, fx.artifacts, appKey, appTemp)

	// acme.lib's own artifact GET failed with a 500 before ever reaching
	// writeDownloadToTemp, so its prefetch never produced a temp to reclaim.
	libKey := artifactKey(lib)
	if got := fx.artifacts.cleanupCountFor(libKey); got != 0 {
		t.Fatalf("lib cleanupCount = %d, want 0 (its prefetch download never produced a temp)", got)
	}

	assertPathAbsent(t, fx.installPath(app))
	assertPathAbsent(t, fx.installPath(lib))
}

// assertReclaimedExactlyOnce fails the test unless key's temp at path was
// cleaned up exactly once (by Close's drain of an unconsumed prefetch, in
// this file's tests) and no longer exists on disk.
func assertReclaimedExactlyOnce(t *testing.T, artifacts *s3StyleArtifacts, key, path string) {
	t.Helper()
	if got := artifacts.cleanupCountFor(key); got != 1 {
		t.Fatalf("%s cleanupCount = %d, want exactly 1 (reclaimed once by Close)", key, got)
	}
	assertPathAbsent(t, path)
}

// TestPrefetchedArtifactFailsClosedOnPinMismatch pins that a reused prefetched
// temp still meets verifyPinnedSHA: a wrong lockfile pin fails the install
// closed, with no refetch from the object store.
func TestPrefetchedArtifactFailsClosedOnPinMismatch(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	fx := newPrefetchHandoffFixture(t, srv, 1)
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0", SHA256: wrongPinSHA256}
	collections := map[string]collection{col.key(): col}
	graph := map[string][]string{col.key(): {}}
	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels: %v", err)
	}

	prefetch, summary, err := fx.runLevels(collections, graph, levels)
	if err != nil {
		t.Fatalf("installLevels: %v", err)
	}
	if summary.count == 0 {
		t.Fatalf("expected failures > 0 from the pin mismatch")
	}
	if !errors.Is(summary.cause, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is(summary.cause, helpers.ErrSHA256Mismatch), got %v", summary.cause)
	}
	assertPathAbsent(t, fx.installPath(col))

	if got := srv.Count(fakegalaxy.EndpointArtifact); got != 1 {
		t.Fatalf("EndpointArtifact count = %d, want 1 (a single origin download, no futile refetch)", got)
	}
	key := artifactKey(col)
	if got := fx.artifacts.fetchCountFor(key); got != 0 {
		t.Fatalf("fetchCount = %d, want 0 (a pin mismatch must fail closed without hitting the object store)", got)
	}

	prefetch.Close()
	if got := fx.artifacts.cleanupCountFor(key); got != 1 {
		t.Fatalf("cleanupCount = %d, want exactly 1", got)
	}
}

// TestInstallCollectionSkipReleasesPrefetchedTempExactlyOnce pins that
// installCollection's already-installed skip branch releases a handed-in
// prefetched temp exactly once instead of leaking it until Close.
func TestInstallCollectionSkipReleasesPrefetchedTempExactlyOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	cfg := &config.Config{DownloadPath: downloadPath}
	target := newTestInstallTarget(t, cfg, col)
	const installedSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	if err := os.MkdirAll(target.path, helpers.DirMod); err != nil {
		t.Fatalf("mkdir installPath: %v", err)
	}
	seedValidExtractMarker(t, target, installedSHA)
	infoDir := filepath.Join(downloadPath, "ansible_collections", col.Namespace+"."+col.Name+"-"+col.Version+".info")
	if err := os.MkdirAll(infoDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir infoDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(infoDir, "GALAXY.yml"), sidecarFor(col), helpers.FileMod); err != nil {
		t.Fatalf("write GALAXY.yml: %v", err)
	}

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: installedSHA,
		InstalledAt:    time.Now().UTC(),
	})
	deps := installDeps{
		cfg:     cfg,
		runtime: infra.New(noopPrinter{}, http.DefaultClient),
		st:      st,
		root:    target.root,
	}

	tempPath := filepath.Join(t.TempDir(), "prefetched.tar.gz")
	if err := os.WriteFile(tempPath, []byte("prefetched tarball bytes"), helpers.FileMod); err != nil {
		t.Fatalf("seed prefetched temp: %v", err)
	}
	var cleanupCount atomic.Int32
	prefetched := downloadResult{
		Path: tempPath,
		SHA:  installedSHA,
		Cleanup: func() {
			cleanupCount.Add(1)
			_ = os.Remove(tempPath)
		},
	}

	if err := installCollection(context.Background(), col, deps, nil, nil, prefetched); err != nil {
		t.Fatalf("installCollection: %v", err)
	}

	if got := cleanupCount.Load(); got != 1 {
		t.Fatalf("cleanupCount = %d, want exactly 1 (released once by the skip branch, not leaked, not double-removed)", got)
	}
	assertPathAbsent(t, tempPath)
}
