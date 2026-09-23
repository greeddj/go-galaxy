package collections

// Tests for buildPrefetchTasks' parallel, DownloadWorkers-bounded scan and
// the set it schedules. The stubs serve only Has: every other ArtifactStore
// method returns errStubNotImplemented so an accidental call fails loudly.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// errStubNotImplemented is returned by the stub ArtifactStore methods this
// file's tests never exercise, so an unexpected call fails the test loudly
// instead of returning a misleadingly successful zero value.
var errStubNotImplemented = errors.New("stub: method not implemented")

// concurrentProbeArtifacts is a stub ArtifactStore whose Has blocks until
// `target` probes are in flight at once, recording peak concurrency; a
// sequential scan never reaches target and escapes only via ctx.Done().
type concurrentProbeArtifacts struct {
	gate       chan struct{}
	mu         sync.Mutex
	target     int
	inFlight   int
	peak       int
	gateClosed bool
}

// Has blocks until target calls are in flight at once (or ctx is done),
// tracking a.peak. The gate closes only once: inFlight can reach target again
// in a later batch, and closing a closed channel would panic.
func (a *concurrentProbeArtifacts) Has(ctx context.Context, _ string) (bool, error) {
	a.mu.Lock()
	a.inFlight++
	if a.inFlight > a.peak {
		a.peak = a.inFlight
	}
	if a.inFlight == a.target && !a.gateClosed {
		a.gateClosed = true
		close(a.gate)
	}
	a.mu.Unlock()

	select {
	case <-a.gate:
	case <-ctx.Done():
	}

	a.mu.Lock()
	a.inFlight--
	a.mu.Unlock()
	return false, nil
}

func (a *concurrentProbeArtifacts) Meta(context.Context, string) (map[string]string, bool, error) {
	return nil, false, errStubNotImplemented
}

func (a *concurrentProbeArtifacts) Fetch(context.Context, string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (a *concurrentProbeArtifacts) TempFile(context.Context, string) (*os.File, func(), error) {
	return nil, nil, errStubNotImplemented
}

func (a *concurrentProbeArtifacts) Commit(context.Context, string, string, map[string]string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (a *concurrentProbeArtifacts) Delete(context.Context, string) error {
	return errStubNotImplemented
}

// TestBuildPrefetchTasksProbesConcurrentlyBoundedByDownloadWorkers pins that
// the Has probes run in parallel bounded by cfg.DownloadWorkers, not Workers:
// Workers=1 makes a sequential or Workers-bounded scan stall at peak 1.
func TestBuildPrefetchTasksProbesConcurrentlyBoundedByDownloadWorkers(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	const target = 4
	art := &concurrentProbeArtifacts{gate: make(chan struct{}), target: target}
	cfg := &config.Config{Workers: 1, DownloadWorkers: target, DownloadPath: t.TempDir()}
	root := newTestCollectionsRoot(t, cfg.DownloadPath)
	deps := newPrefetchDeps(cfg, infra.New(noopPrinter{}, http.DefaultClient), store.New(), art, root)

	const n = 6
	collections := make(map[string]collection, n)
	for i := range n {
		col := collection{Namespace: "acme", Name: fmt.Sprintf("col%d", i), Version: "1.0.0", Type: "galaxy"}
		collections[col.key()] = col
	}

	p := &prefetcher{done: make(map[string]chan struct{})}
	tasks := buildPrefetchTasks(ctx, deps, collections, p)

	if len(tasks) != n {
		t.Fatalf("len(tasks) = %d, want %d", len(tasks), n)
	}
	if art.peak != target {
		t.Fatalf("peak concurrent Has calls = %d, want %d (scan must be DownloadWorkers-bounded parallel, "+
			"not cfg.Workers=%d)", art.peak, target, cfg.Workers)
	}
}

// presenceArtifacts is a deterministic stub cacheManager.ArtifactStore keyed
// by artifact key: present reports which keys are already cached, and errKeys
// reports which keys' Has call should fail instead.
type presenceArtifacts struct {
	present map[string]bool
	errKeys map[string]bool
}

// errStubHas is the error presenceArtifacts.Has returns for a key listed in
// errKeys, standing in for a transient cache-probe failure.
var errStubHas = errors.New("stub: has probe failed")

func (a *presenceArtifacts) Has(_ context.Context, key string) (bool, error) {
	if a.errKeys[key] {
		return false, errStubHas
	}
	return a.present[key], nil
}

func (a *presenceArtifacts) Meta(context.Context, string) (map[string]string, bool, error) {
	return nil, false, errStubNotImplemented
}

func (a *presenceArtifacts) Fetch(context.Context, string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (a *presenceArtifacts) TempFile(context.Context, string) (*os.File, func(), error) {
	return nil, nil, errStubNotImplemented
}

func (a *presenceArtifacts) Commit(context.Context, string, string, map[string]string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (a *presenceArtifacts) Delete(context.Context, string) error {
	return errStubNotImplemented
}

// seedAlreadyInstalled makes installRecordMatches report col as installed:
// install directory, valid extract marker, the <ns>.<name>-<version>.info
// GALAXY.yml sidecar, and a matching installed entry in st.
func seedAlreadyInstalled(t *testing.T, cfg *config.Config, st *store.Store, col collection) {
	t.Helper()
	const installedSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	target := newTestInstallTarget(t, cfg, col)
	if err := os.MkdirAll(target.path, helpers.DirMod); err != nil {
		t.Fatalf("mkdir installPath: %v", err)
	}
	seedValidExtractMarker(t, target, installedSHA)
	infoDir := filepath.Join(cfg.DownloadPath, "ansible_collections", col.Namespace+"."+col.Name+"-"+col.Version+".info")
	if err := os.MkdirAll(infoDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir infoDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(infoDir, "GALAXY.yml"), sidecarFor(col), helpers.FileMod); err != nil {
		t.Fatalf("write GALAXY.yml: %v", err)
	}
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: installedSHA,
		InstalledAt:    time.Now().UTC(),
	})
}

// TestBuildPrefetchTasksSchedulesExactlyTheRightSet pins that absent and
// Has-erroring (fail-open) rows are scheduled, present, installed and
// unresolvable-type rows are not, and only rows probed present enter presence.
func TestBuildPrefetchTasksSchedulesExactlyTheRightSet(t *testing.T) {
	t.Parallel()
	c1 := collection{Namespace: "acme", Name: "absent", Version: "1.0.0", Type: "galaxy"}
	c2 := collection{Namespace: "acme", Name: "present", Version: "1.0.0", Type: "galaxy"}
	c3 := collection{Namespace: "acme", Name: "erroring", Version: "1.0.0", Type: "galaxy"}
	c4 := collection{
		Namespace: "acme", Name: "fromgit", Version: "1.0.0", Type: "git",
		Source: "git+https://h.example/acme/fromgit.git#@0123456789abcdef0123456789abcdef01234567",
	}
	c5 := collection{Namespace: "acme", Name: "installed", Version: "1.0.0", Type: "galaxy"}
	c6 := collection{Namespace: "acme", Name: "nongalaxy", Version: "1.0.0", Type: "file"}

	art := &presenceArtifacts{
		present: map[string]bool{artifactKey(c2): true, artifactKey(c4): true},
		errKeys: map[string]bool{artifactKey(c3): true},
	}
	cfg := &config.Config{Workers: 2, DownloadPath: t.TempDir()}
	st := store.New()
	seedAlreadyInstalled(t, cfg, st, c5)
	root := newTestCollectionsRoot(t, cfg.DownloadPath)
	deps := newPrefetchDeps(cfg, infra.New(noopPrinter{}, http.DefaultClient), st, art, root)

	collections := map[string]collection{
		c1.key(): c1,
		c2.key(): c2,
		c3.key(): c3,
		c4.key(): c4,
		c5.key(): c5,
		c6.key(): c6,
	}

	p := &prefetcher{done: make(map[string]chan struct{})}
	tasks := buildPrefetchTasks(t.Context(), deps, collections, p)

	want := map[string]bool{c1.key(): true, c3.key(): true}
	got := make(map[string]bool, len(tasks))
	for _, col := range tasks {
		got[col.key()] = true
	}
	if len(got) != len(tasks) {
		t.Fatalf("buildPrefetchTasks returned duplicate keys: %v", tasks)
	}
	if len(got) != len(want) || !got[c1.key()] || !got[c3.key()] {
		t.Fatalf("scheduled set = %v, want %v", got, want)
	}
	if got[c5.key()] {
		t.Fatalf("c5 (already installed) must not be scheduled: %v", got)
	}

	if len(p.done) != len(want) {
		t.Fatalf("p.done has %d entries, want %d: %v", len(p.done), len(want), p.done)
	}
	for key := range want {
		if _, ok := p.done[key]; !ok {
			t.Fatalf("p.done missing registered key %s", key)
		}
	}

	assertPresenceNamesOnlyC2(t, p, c1, c2, c3)
	assertPresenceNamesGitRow(t, p, c4)
}

// assertPresenceNamesGitRow checks that the git row's locator-keyed artifact,
// probed present, made it into buildPrefetchTasks' presence set.
func assertPresenceNamesGitRow(t *testing.T, p *prefetcher, c4 collection) {
	t.Helper()
	if !p.presence[artifactKey(c4)] {
		t.Fatalf("presence must name c4: its locator-keyed artifact was found cached")
	}
}

// assertPresenceNamesOnlyC2 checks the presence set excludes c1 and c3 and
// names c2, then its size (counting the git row); specific checks come first
// so a broader one cannot mask them.
func assertPresenceNamesOnlyC2(t *testing.T, p *prefetcher, c1, c2, c3 collection) {
	t.Helper()
	if p.presence[artifactKey(c1)] {
		t.Fatalf("presence must not name c1: it was scheduled for prefetch, not left unscheduled")
	}
	if p.presence[artifactKey(c3)] {
		t.Fatalf("presence must not name c3: its Has probe errored, so it answers nothing to trust")
	}
	if !p.presence[artifactKey(c2)] {
		t.Fatalf("presence must name c2: its probe found the artifact cached and left it unscheduled")
	}
	if len(p.presence) != 2 {
		t.Fatalf("len(p.presence) = %d, want 2 (c2 and the cached git row)", len(p.presence))
	}
}
