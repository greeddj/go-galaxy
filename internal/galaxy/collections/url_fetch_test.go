package collections

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// urlRefetchFixture is one url refetch scenario: a server holding acme.kafka's
// tarball, deps whose url client and temp directory are observable, and the
// pinned collection the refetch is for.
type urlRefetchFixture struct {
	server  *fakegalaxy.Server
	deps    installDeps
	tempDir string
	col     collection
}

// newURLRefetchFixture serves the bytes of acme.name@0.24.0 at the path col
// pins, so a test picks whether the served manifest matches col.
func newURLRefetchFixture(t *testing.T, name string) *urlRefetchFixture {
	t.Helper()
	server := fakegalaxy.New(t)
	data, sha := fakegalaxy.BuildArtifact("acme", name, "0.24.0", nil)
	tarballURL := server.AddTarball("releases/acme-kafka-0.24.0.tar.gz", data)
	deps, _ := newRefetchDeps(t, nil)
	deps.runtime.URLHTTP = fetch.NewURLDownload(time.Minute, false, nil)
	tempDir := t.TempDir()
	deps.runtime.TempDir = func() string { return tempDir }
	return &urlRefetchFixture{
		server:  server,
		deps:    deps,
		tempDir: tempDir,
		col: collection{
			Namespace: "acme", Name: "kafka", Version: "0.24.0",
			Source: urlsource.Locator{URL: tarballURL, SHA256: sha}.String(),
		},
	}
}

// assertTempDirEmpty fails the test when a download left anything behind.
func (f *urlRefetchFixture) assertTempDirEmpty(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(f.tempDir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp dir holds %d entries after the refetch, want none", len(entries))
	}
}

// TestURLFetchToCacheRefusesWithoutAPin pins that the refetch judges the
// locator before any request: a source that is not a url locator, and one
// with no sha256 pin, are both ErrInvalidURLLocator.
func TestURLFetchToCacheRefusesWithoutAPin(t *testing.T) {
	t.Parallel()
	f := newURLRefetchFixture(t, "kafka")
	loc, err := urlSourceOf(f.col)
	if err != nil {
		t.Fatalf("urlSourceOf(pinned) error = %v", err)
	}

	for name, source := range map[string]string{
		"server source": "https://hub.example/",
		"unpinned":      urlsource.Locator{URL: loc.URL}.String(),
	} {
		col := f.col
		col.Source = source
		if _, err := urlFetchToCache(context.Background(), f.deps, col, true); !errors.Is(err, helpers.ErrInvalidURLLocator) {
			t.Errorf("%s: urlFetchToCache() error = %v, want ErrInvalidURLLocator", name, err)
		}
		if _, err := urlSourceOf(col); !errors.Is(err, helpers.ErrInvalidURLLocator) {
			t.Errorf("%s: urlSourceOf() error = %v, want ErrInvalidURLLocator", name, err)
		}
	}
	if n := f.server.Count(fakegalaxy.EndpointTarball); n != 0 {
		t.Fatalf("tarball requests = %d, want 0 before a pin is judged", n)
	}
}

// TestURLFetchToCacheRefusals pins the refusals after the request: a 404, and
// bytes that match the pin but name another collection, released rather than
// cached.
func TestURLFetchToCacheRefusals(t *testing.T) {
	t.Parallel()

	t.Run("origin answers 404", func(t *testing.T) {
		t.Parallel()
		f := newURLRefetchFixture(t, "kafka")
		col := f.col
		col.Source = strings.Replace(col.Source, "acme-kafka-0.24.0", "acme-kafka-gone", 1)
		if _, err := urlFetchToCache(context.Background(), f.deps, col, true); !errors.Is(err, helpers.ErrDownloadFailed) {
			t.Fatalf("urlFetchToCache() error = %v, want ErrDownloadFailed", err)
		}
		f.assertTempDirEmpty(t)
	})

	t.Run("manifest names another collection", func(t *testing.T) {
		t.Parallel()
		f := newURLRefetchFixture(t, "zookeeper")
		_, err := urlFetchToCache(context.Background(), f.deps, f.col, true)
		if !errors.Is(err, helpers.ErrURLArtifactIdentityMismatch) {
			t.Fatalf("urlFetchToCache() error = %v, want ErrURLArtifactIdentityMismatch", err)
		}
		if !strings.Contains(err.Error(), "acme.zookeeper@0.24.0 for acme.kafka@0.24.0") {
			t.Fatalf("urlFetchToCache() error = %v, want both identities named", err)
		}
		f.assertTempDirEmpty(t)
		if _, statErr := os.Stat(filepath.Join(f.deps.cfg.CacheDir, artifactKey(f.col))); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("cache slot stat error = %v, want nothing cached", statErr)
		}
	})
}

// TestURLFetchToCacheArms pins the two arms past the checks: without the
// cache the verified download is handed back, and a failed commit releases
// it.
func TestURLFetchToCacheArms(t *testing.T) {
	t.Parallel()

	t.Run("no cache", func(t *testing.T) {
		t.Parallel()
		f := newURLRefetchFixture(t, "kafka")
		loc, err := urlSourceOf(f.col)
		if err != nil {
			t.Fatalf("urlSourceOf() error = %v", err)
		}
		got, err := urlFetchToCache(context.Background(), f.deps, f.col, false)
		if err != nil {
			t.Fatalf("urlFetchToCache() error = %v", err)
		}
		if filepath.Dir(got.Path) != f.tempDir || got.SHA != loc.SHA256 {
			t.Fatalf("urlFetchToCache() = (%q, %q), want a temp file in %s with sha %s", got.Path, got.SHA, f.tempDir, loc.SHA256)
		}
		assertFileSHA256(t, got.Path, loc.SHA256)
		if misses := f.deps.runtime.Metrics.Totals().CacheMisses; misses != 0 {
			t.Fatalf("cache misses = %d, want 0 without a commit", misses)
		}
		got.Cleanup()
		f.assertTempDirEmpty(t)
	})

	t.Run("commit fails", func(t *testing.T) {
		t.Parallel()
		f := newURLRefetchFixture(t, "kafka")
		f.deps.artifacts = local.NewArtifacts(filepath.Join(t.TempDir(), "absent"))
		if _, err := urlFetchToCache(context.Background(), f.deps, f.col, true); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("urlFetchToCache() error = %v, want the store's fs.ErrNotExist", err)
		}
		f.assertTempDirEmpty(t)
	})
}
