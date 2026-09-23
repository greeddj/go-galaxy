package collections

// Tests prefetchOne's metadata-load failure: surfaced through Wait, then
// absorbed by the install worker's own reload. The fault is a 404 because a
// retryable 500 would be consumed inside loadCollectionMetadata's retries.

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestPrefetchMetadataErrorSurfacedByWait pins that a version-detail 404
// leaves the task completed with its error recorded, a nil meta and no
// download, all visible through Wait.
func TestPrefetchMetadataErrorSurfacedByWait(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)
	// Persistent (Count: -1): this test only exercises the prefetcher, so
	// nothing else consumes the fault afterward.
	srv.Fail(fakegalaxy.EndpointVersionDetail, "acme", "app", fakegalaxy.Fault{Status: http.StatusNotFound, Count: -1})

	fx := newPrefetchHandoffFixture(t, srv, 1)
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}
	collections := map[string]collection{col.key(): col}
	graph := map[string][]string{col.key(): {}}
	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels: %v", err)
	}

	prefetch := startPrefetcher(context.Background(), newPrefetchDeps(fx.cfg, fx.runtime, fx.st, fx.artifacts, fx.root), collections, levels)

	meta, dl, ok, waitErr := prefetch.Wait(col.key())
	if !ok {
		t.Fatalf("Wait ok = false, want true (a prefetch task was scheduled for %s)", col.key())
	}
	if waitErr == nil {
		t.Fatalf("Wait err = nil, want the metadata-load failure recorded by finish")
	}
	if meta != nil {
		t.Fatalf("Wait meta = %+v, want nil (prefetchOne must not return metadata on a metadata-load failure)", meta)
	}
	if dl.Path != "" {
		t.Fatalf("Wait dl.Path = %q, want empty (prefetchOne returns before ever downloading an artifact)", dl.Path)
	}

	prefetch.Close()
}

// TestPrefetchMetadataErrorAbsorbedInstallRecovers pins that a one-shot 404
// spent by the prefetch is absorbed: the install worker reloads metadata with
// a fresh GET, since the failure was never cached, and installs.
func TestPrefetchMetadataErrorAbsorbedInstallRecovers(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)
	srv.Fail(fakegalaxy.EndpointVersionDetail, "acme", "app", fakegalaxy.Fault{Status: http.StatusNotFound, Count: 1})

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
		t.Fatalf("failures = %d, want 0 (the install worker's own metadata reload must recover)", summary.count)
	}
	assertFileContent(t, filepath.Join(fx.installPath(col), "README.md"), "# acme.app\n")

	if got := srv.Count(fakegalaxy.EndpointVersionDetail); got != 2 {
		t.Fatalf("EndpointVersionDetail count = %d, want 2 (one failed prefetch attempt, one successful install attempt)", got)
	}

	prefetch.Close()
}
