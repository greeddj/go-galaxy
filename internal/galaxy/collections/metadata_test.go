package collections

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// v2FallThroughVersionsURL is a marker only the v2 candidate's response body
// carries, so a test can prove the returned root came from that candidate
// and not from any other one that might otherwise satisfy the request.
const v2FallThroughVersionsURL = "http://v2-candidate.example/versions/"

// v2FallThroughBody is a minimal but valid types.GalaxyCollection root
// metadata document, just enough for loadRootMetadataCached to unmarshal and
// return without error.
const v2FallThroughBody = `{"versions_url":"` + v2FallThroughVersionsURL +
	`","highest_version":{"href":"http://v2-candidate.example/versions/9.9.9/","version":"9.9.9"}}`

// newFallThroughServer 404s every "/api/v3/" path, then serves body for a path
// containing successMatch, and 404s the rest; "/api/v3/" is checked first so
// successMatch may be "/v3/". It records request paths in order.
func newFallThroughServer(t *testing.T, successMatch, body string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()

		switch {
		case strings.Contains(r.URL.Path, "/api/v3/"):
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, successMatch):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
	return srv, seen
}

// newRootMetadataFallThroughServer starts an httptest.Server that answers 404
// for any candidate URL under "/api/v3/" and 200 with v2FallThroughBody for
// any candidate under "/api/v2/" (any other path also 404s).
func newRootMetadataFallThroughServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	return newFallThroughServer(t, "/api/v2/", v2FallThroughBody)
}

// hubBasePath is the simulated Galaxy NG / Automation Hub mount point; as in
// a real deployment its v3 API sits at "<base>/v3/...", not "<base>/api/v3".
const hubBasePath = "/api/automation-hub"

// TestLoadRootMetadataCachedResolvesHubShapedBase pins that a Galaxy NG /
// Automation Hub base, which 404s "<base>/api/v3", resolves through the bare
// "<base>/v3" fallback candidate.
func TestLoadRootMetadataCachedResolvesHubShapedBase(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.NewAtBasePath(t, hubBasePath)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)
	base := srv.URL() + hubBasePath

	cfg := &config.Config{Server: base}
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root, _, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("expected the hub-shaped base to resolve via the bare /v3 candidate, got error: %v", err)
	}
	if root == nil {
		t.Fatal("expected a non-nil root, got nil")
	}
	if want := base + "/v3/collections/acme/widgets/versions/"; root.VersionsURL != want {
		t.Fatalf("expected the root served by the bare /v3 candidate (versions_url=%q), got versions_url=%q",
			want, root.VersionsURL)
	}
	// The hub counts only its own "/v3" route, so one counted request proves
	// the walk stopped at the first candidate the hub serves.
	if got := srv.Count(fakegalaxy.EndpointRootMetadata); got != 1 {
		t.Fatalf("expected exactly 1 served root metadata request against the hub, got %d", got)
	}
}

// TestLoadRootMetadataCachedGalaxyShapeResolvesOnFirstCandidateNoRegression
// pins that a galaxy.ansible.com-shaped base resolves on the first
// "<base>/api/v3" candidate with one request, paying nothing for the hub ones.
func TestLoadRootMetadataCachedGalaxyShapeResolvesOnFirstCandidateNoRegression(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)

	cfg := &config.Config{Server: srv.URL()}
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root, _, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root == nil {
		t.Fatal("expected a non-nil root, got nil")
	}
	if got := srv.Count(fakegalaxy.EndpointRootMetadata); got != 1 {
		t.Fatalf("expected exactly 1 root metadata request for the galaxy.ansible.com shape, got %d", got)
	}
}

// TestLoadRootMetadataCachedFallsThroughOn404 pins that a 404 on the v3
// root-metadata candidate means "try the next candidate": the loader asks v3
// first, then advances to v2 and returns its root metadata.
func TestLoadRootMetadataCachedFallsThroughOn404(t *testing.T) {
	t.Parallel()
	srv, seenPaths := newRootMetadataFallThroughServer(t)

	cfg := &config.Config{Server: srv.URL}
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root, _, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("expected the 404 on v3 to fall through to v2, got error: %v", err)
	}
	if root == nil {
		t.Fatal("expected a non-nil root, got nil")
	}
	if root.VersionsURL != v2FallThroughVersionsURL {
		t.Fatalf("expected the root served by the v2 candidate (versions_url=%q), got versions_url=%q",
			v2FallThroughVersionsURL, root.VersionsURL)
	}

	paths := seenPaths()
	if len(paths) == 0 {
		t.Fatal("expected the server to have received at least one request")
	}
	if !strings.Contains(paths[0], "/api/v3/") {
		t.Fatalf("expected the first request to hit the v3 candidate, got %q (all requests: %v)", paths[0], paths)
	}

	var hitV2 bool
	for _, p := range paths {
		if strings.Contains(p, "/api/v2/") {
			hitV2 = true
			break
		}
	}
	if !hitV2 {
		t.Fatalf("expected a request to hit the v2 candidate after the v3 404, got requests: %v", paths)
	}
}

// TestLoadRootMetadataCachedAllCandidates404ReturnsLastError pins that when
// every candidate 404s the last 404 is returned as an HTTPStatusError, so the
// caller's message reflects the real upstream response.
func TestLoadRootMetadataCachedAllCandidates404ReturnsLastError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Server: srv.URL}
	col := collection{Namespace: "acme", Name: "gadgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root, _, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{})
	if err == nil {
		t.Fatal("expected an error when every candidate 404s, got nil")
	}
	if root != nil {
		t.Fatalf("expected a nil root on failure, got %+v", root)
	}

	var statusErr *cacheManager.HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected errors.As to *cacheManager.HTTPStatusError, got %T: %v", err, err)
	}
	if statusErr.Code != http.StatusNotFound {
		t.Fatalf("expected status code %d, got %d", http.StatusNotFound, statusErr.Code)
	}
}

// TestLoadRootMetadataCachedMemoizesWinningAPIRootAcrossCollections pins that
// once apiRootMemo learns a server's winning API root, a second collection on
// that server sends no further request to the losing v3 candidate.
func TestLoadRootMetadataCachedMemoizesWinningAPIRootAcrossCollections(t *testing.T) {
	t.Parallel()
	var v3Requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/v3/"):
			v3Requests.Add(1)
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "/api/v2/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(v2FallThroughBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Server: srv.URL}
	runtime := infra.New(noopPrinter{}, srv.Client())
	// One collectionDeps (and therefore one apiRootMemo) is shared across
	// both fetches below, mirroring how a single phase's memo is shared by
	// value-copy across every worker resolving collections on the same server.
	deps := newCollectionDeps(cfg, runtime, store.New())

	first := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	if _, _, err := loadRootMetadataCached(context.Background(), deps, first, cacheManager.Policy{}); err != nil {
		t.Fatalf("first collection: unexpected error: %v", err)
	}
	afterFirst := v3Requests.Load()
	if afterFirst == 0 {
		t.Fatal("expected the first collection to probe v3 at least once before falling through to v2")
	}

	second := collection{Namespace: "acme", Name: "gadgets", Version: "1.0.0"}
	if _, _, err := loadRootMetadataCached(context.Background(), deps, second, cacheManager.Policy{}); err != nil {
		t.Fatalf("second collection: unexpected error: %v", err)
	}
	if afterSecond := v3Requests.Load(); afterSecond != afterFirst {
		t.Fatalf(
			"expected no additional v3 probes for a second collection under the same server "+
				"(the memoized apiRoot should skip the v3 probe entirely): had %d after the first fetch, %d after the second",
			afterFirst, afterSecond,
		)
	}
}

// TestLoadRootMetadataCachedNonNotFoundErrorAbortsImmediately pins that a
// non-404 error aborts the candidate walk on the first candidate. It uses 400,
// not a retryable 5xx, so retries cannot inflate the request count.
func TestLoadRootMetadataCachedNonNotFoundErrorAbortsImmediately(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Server: srv.URL}
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root, _, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{})
	if err == nil {
		t.Fatal("expected an error from the 400 response, got nil")
	}
	if root != nil {
		t.Fatalf("expected a nil root on failure, got %+v", root)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request (no probing further candidates after a non-404 error), got %d", got)
	}
}
