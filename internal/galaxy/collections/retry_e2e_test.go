package collections_test

// End-to-end retry behavior of the Galaxy API GET and the artifact download,
// driven by fakegalaxy.Fault injection rather than the internal predicates.

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// newRetryFixture registers acme.<name> 1.0.0 on a fresh fake server. noCache
// disables the prefetcher, so artifact requests have one call site to count;
// metadata tests keep the API cache, which serves the install's second fetch.
func newRetryFixture(t *testing.T, name string, noCache bool) (*config.Config, *infra.Infra, *fakegalaxy.Server) {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme."+name)

	s := fakegalaxy.New(t)
	s.AddVersion("acme", name, "1.0.0", nil)

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
		NoCache:          noCache,
	}
	return cfg, infra.New(noopPrinter{}, s.Client()), s
}

// TestMetadataFetchRetriesTransientFailureThenSucceeds pins that a version
// detail GET recovers from two 503s in exactly three requests.
func TestMetadataFetchRetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newRetryFixture(t, "metaretry", false)
	s.Fail(fakegalaxy.EndpointVersionDetail, "acme", "metaretry", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: 2})

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "metaretry")
	if got := s.Count(fakegalaxy.EndpointVersionDetail); got != 3 {
		t.Errorf("EndpointVersionDetail count = %d, want 3 (2 failures + 1 success)", got)
	}
}

// TestArtifactDownloadRetriesTransientFailureThenSucceeds mirrors
// TestMetadataFetchRetriesTransientFailureThenSucceeds for the artifact
// download endpoint.
func TestArtifactDownloadRetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newRetryFixture(t, "artretry", true)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "artretry", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: 2})

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "artretry")
	if got := s.Count(fakegalaxy.EndpointArtifact); got != 3 {
		t.Errorf("EndpointArtifact count = %d, want 3 (2 failures + 1 success)", got)
	}
}

// TestArtifactDownloadRetrySuccessCountsOneMissNotOnePerAttempt pins that a
// download taking three attempts counts one cache miss: the miss is counted
// in downloadCollectionToCache after the retry loop, not per attempt.
func TestArtifactDownloadRetrySuccessCountsOneMissNotOnePerAttempt(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newRetryFixture(t, "artretry", true)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "artretry", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: 2})

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "artretry")
	if got := s.Count(fakegalaxy.EndpointArtifact); got != 3 {
		t.Errorf("EndpointArtifact count = %d, want 3 (2 failures + 1 success)", got)
	}
	totals := runtime.Metrics.Totals()
	if totals.CacheMisses != 1 {
		t.Errorf("CacheMisses = %d, want 1 (one per retry-bounded acquisition, not one per the 3 HTTP attempts above)", totals.CacheMisses)
	}
	if totals.CacheHits != 0 {
		t.Errorf("CacheHits = %d, want 0 (a fresh origin download is never a cache hit)", totals.CacheHits)
	}
}

// TestMetadataFetchExhaustsRetriesAndFails pins that an always-failing
// metadata GET makes FetchRetryMaxAttempts requests and fails during
// resolution with the HTTPStatusError reachable, not ErrInstallationFailed.
func TestMetadataFetchExhaustsRetriesAndFails(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newRetryFixture(t, "metafail", false)
	s.Fail(fakegalaxy.EndpointVersionDetail, "acme", "metafail", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	var statusErr *cacheManager.HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected errors.As to *cacheManager.HTTPStatusError, got %v", err)
	}
	if statusErr.Code != http.StatusServiceUnavailable {
		t.Errorf("HTTPStatusError.Code = %d, want %d", statusErr.Code, http.StatusServiceUnavailable)
	}
	if got := s.Count(fakegalaxy.EndpointVersionDetail); got != helpers.FetchRetryMaxAttempts {
		t.Errorf("EndpointVersionDetail count = %d, want helpers.FetchRetryMaxAttempts=%d", got, helpers.FetchRetryMaxAttempts)
	}
}

// TestArtifactDownloadExhaustsRetriesAndFails mirrors
// TestMetadataFetchExhaustsRetriesAndFails for the artifact download
// endpoint.
func TestArtifactDownloadExhaustsRetriesAndFails(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newRetryFixture(t, "artfail", true)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "artfail", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}
	if got := s.Count(fakegalaxy.EndpointArtifact); got != helpers.FetchRetryMaxAttempts {
		t.Errorf("EndpointArtifact count = %d, want helpers.FetchRetryMaxAttempts=%d", got, helpers.FetchRetryMaxAttempts)
	}
}
