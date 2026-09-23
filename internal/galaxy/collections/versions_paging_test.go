package collections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
	"github.com/psvmcc/hub/pkg/types"
)

// pagedVersions returns n version strings in the ascending order fakegalaxy
// and newDeclaredCountServer both page them out in, so a test can compare a
// collected list against the exact sequence an offset-ordered walk yields.
func pagedVersions(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("1.0.%d", i)
	}
	return out
}

// TestLoadVersionsListCachedPagesThroughAllVersions pins that three pages
// fetched concurrently (DownloadWorkers 4) assemble in offset order with
// exactly one request per page.
func TestLoadVersionsListCachedPagesThroughAllVersions(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)

	const wantVersions = 250
	want := pagedVersions(wantVersions)
	for _, v := range want {
		srv.AddVersion("acme", "widgets", v, nil)
	}
	versionsURL := srv.URL() + "/api/v3/collections/acme/widgets/versions/"

	cfg := &config.Config{Server: srv.URL(), DownloadWorkers: 4}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	versions, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(versions, want) {
		t.Fatalf("versions not in offset order: got %d entries, first %q, last %q; want %d entries %q..%q",
			len(versions), versions[0], versions[len(versions)-1], len(want), want[0], want[len(want)-1])
	}
	// 250 versions at versionLimit=100 per page means three requests: offset
	// 0 (100), then the declared total schedules exactly offsets 100 (100)
	// and 200 (50) - never a fourth, empty request.
	if got := srv.Count(fakegalaxy.EndpointVersionsList); got != 3 {
		t.Fatalf("Count(EndpointVersionsList) = %d, want 3", got)
	}
}

// TestLoadVersionsListCachedStopsAtMetaCount pins that two full pages stop
// at the declared meta.count after the second request, never issuing a
// third, empty one.
func TestLoadVersionsListCachedStopsAtMetaCount(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)

	const wantVersions = 2 * versionLimit
	for _, v := range pagedVersions(wantVersions) {
		srv.AddVersion("acme", "gadgets", v, nil)
	}
	versionsURL := srv.URL() + "/api/v3/collections/acme/gadgets/versions/"

	cfg := &config.Config{Server: srv.URL()}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	versions, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(versions) != wantVersions {
		t.Fatalf("len(versions) = %d, want %d", len(versions), wantVersions)
	}
	if got := srv.Count(fakegalaxy.EndpointVersionsList); got != 2 {
		t.Fatalf("Count(EndpointVersionsList) = %d, want 2", got)
	}
}

// newDeclaredCountServer pages actual by ?limit=&offset= while declaring
// count as meta.count, however far that diverges from what it serves. The
// handler only reads shared state, so concurrent fetches are safe.
func newDeclaredCountServer(t *testing.T, actual []string, count int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		start := min(max(offset, 0), len(actual))
		end := min(start+max(limit, 0), len(actual))

		var page types.GalaxyCollectionVersions
		page.Meta.Count = count
		page.Data = make([]types.GalaxyCollectionVersion, 0, end-start)
		for _, v := range actual[start:end] {
			page.Data = append(page.Data, types.GalaxyCollectionVersion{Version: v})
		}
		body, err := json.Marshal(&page)
		if err != nil {
			// Marshaling strings and an int cannot fail; an error is a bug
			// in this fixture, not a scenario to answer.
			panic(fmt.Sprintf("marshal fixture page: %v", err))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &served
}

// TestLoadVersionsListCachedExceedsPageCeilingFailsHard pins that endless
// full pages with no meta.count fail with helpers.ErrVersionsPagingExceeded
// after exactly maxVersionPages requests, never a truncated list.
func TestLoadVersionsListCachedExceedsPageCeilingFailsHard(t *testing.T) {
	t.Parallel()
	srv, served := newDeclaredCountServer(t, pagedVersions(maxVersionPages*versionLimit+versionLimit), 0)

	cfg := &config.Config{Server: srv.URL}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	versionsURL := srv.URL + "/versions/"
	_, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if !errors.Is(err, helpers.ErrVersionsPagingExceeded) {
		t.Fatalf("expected errors.Is(err, helpers.ErrVersionsPagingExceeded), got %v", err)
	}
	if got := served.Load(); got != int32(maxVersionPages) {
		t.Fatalf("requests = %d, want exactly maxVersionPages=%d", got, maxVersionPages)
	}
}

// TestLoadVersionsListCachedExcessiveTotalFailsUpFront pins that a page-0
// meta.count past the ceiling fails after that one request, before a hostile
// total can size an allocation; 1<<50 survives the JSON parse exactly.
func TestLoadVersionsListCachedExcessiveTotalFailsUpFront(t *testing.T) {
	t.Parallel()
	srv, served := newDeclaredCountServer(t, pagedVersions(versionLimit), 1<<50)

	cfg := &config.Config{Server: srv.URL, DownloadWorkers: 4}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	versionsURL := srv.URL + "/versions/"
	_, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if !errors.Is(err, helpers.ErrVersionsPagingExceeded) {
		t.Fatalf("expected errors.Is(err, helpers.ErrVersionsPagingExceeded), got %v", err)
	}
	if got := served.Load(); got != 1 {
		t.Fatalf("requests = %d, want exactly 1 (the verdict must precede any scheduled page)", got)
	}
}

// TestLoadVersionsListCachedToleratesLyingTotal pins that a meta.count
// promising pages the server lacks still ends the list at the first short or
// empty page, discarding the over-scheduled prefetches, with no error.
func TestLoadVersionsListCachedToleratesLyingTotal(t *testing.T) {
	t.Parallel()
	const declared = 5 * versionLimit
	cases := []struct {
		name   string
		actual int
	}{
		{name: "a short page ends the walk", actual: 2*versionLimit + 30},
		{name: "an empty page ends the walk", actual: 2 * versionLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := pagedVersions(tc.actual)
			srv, _ := newDeclaredCountServer(t, want, declared)

			cfg := &config.Config{Server: srv.URL, DownloadWorkers: 4}
			runtime := infra.New(noopPrinter{}, srv.Client())
			deps := newCollectionDeps(cfg, runtime, store.New())

			versionsURL := srv.URL + "/versions/"
			versions, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(versions, want) {
				t.Fatalf("len(versions) = %d, want the %d actually-served entries in offset order", len(versions), tc.actual)
			}
		})
	}
}

// TestLoadVersionsListCachedZeroTotalFallsBackSequential pins that a server
// reporting no total (meta.count 0) is paged on demand and ends on the short
// page: three requests for 250 entries, never a fourth.
func TestLoadVersionsListCachedZeroTotalFallsBackSequential(t *testing.T) {
	t.Parallel()
	const actual = 2*versionLimit + 50
	want := pagedVersions(actual)
	srv, served := newDeclaredCountServer(t, want, 0)

	cfg := &config.Config{Server: srv.URL, DownloadWorkers: 4}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	versionsURL := srv.URL + "/versions/"
	versions, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(versions, want) {
		t.Fatalf("len(versions) = %d, want all %d entries in offset order", len(versions), actual)
	}
	if got := served.Load(); got != 3 {
		t.Fatalf("requests = %d, want exactly 3 (page 0 plus the two pages a full predecessor demanded)", got)
	}
}
