package collections

// These tests pin loadVersionsListCached's loop-level metadata budget: the
// server picks the page count through meta.count, so a per-request budget
// alone would not bound the paging loop.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// versionsPagingDeadlineTotalPages is the number of full pages
// newVersionsPagingDelayServer's meta.count forces the loop to want, sized
// well above what the loop-level budget below can complete.
const versionsPagingDeadlineTotalPages = 8

// versionsPagingDeadlineBudget is the loop-level
// deps.runtime.MetadataDeadline() budget every test in this file uses.
const versionsPagingDeadlineBudget = 500 * time.Millisecond

// newVersionsPagingDelayServer serves a full page after delay, declaring
// versionsPagingDeadlineTotalPages pages; served is counted before the
// delay so a request the client aborts still counts.
func newVersionsPagingDelayServer(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var served atomic.Int32

	var page types.GalaxyCollectionVersions
	page.Meta.Count = versionsPagingDeadlineTotalPages * versionLimit
	page.Data = make([]types.GalaxyCollectionVersion, versionLimit)
	for i := range page.Data {
		page.Data[i] = types.GalaxyCollectionVersion{Version: fmt.Sprintf("1.0.%d", i)}
	}
	body, err := json.Marshal(&page)
	if err != nil {
		t.Fatalf("marshal fixture page: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &served
}

// TestVersionsPagingSharesOneMetadataBudget pins that a server answering
// every full page in 150ms trips the loop-level budget with
// helpers.ErrMetadataFetchDeadline before all pages are served.
func TestVersionsPagingSharesOneMetadataBudget(t *testing.T) {
	t.Parallel()
	const pageDelay = 150 * time.Millisecond

	srv, served := newVersionsPagingDelayServer(t, pageDelay)
	cfg := &config.Config{Server: srv.URL}
	runtime := infra.New(noopPrinter{}, srv.Client())
	runtime.MetadataFetchDeadline = versionsPagingDeadlineBudget
	deps := newCollectionDeps(cfg, runtime, store.New())

	versionsURL := srv.URL + "/versions/"
	_, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if !errors.Is(err, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("unexpected error: %v, want errors.Is ErrMetadataFetchDeadline", err)
	}
	if got := served.Load(); got >= versionsPagingDeadlineTotalPages {
		t.Fatalf("served = %d pages, want fewer than %d (the loop-level budget must end the run before all pages complete)",
			got, versionsPagingDeadlineTotalPages)
	}
}

// TestVersionsPagingDeadlineFixturePositiveControl pins that the same
// fixture with no delay completes every page under the same budget, so the
// budget alone does not fail TestVersionsPagingSharesOneMetadataBudget.
func TestVersionsPagingDeadlineFixturePositiveControl(t *testing.T) {
	t.Parallel()
	srv, served := newVersionsPagingDelayServer(t, 0)
	cfg := &config.Config{Server: srv.URL}
	runtime := infra.New(noopPrinter{}, srv.Client())
	runtime.MetadataFetchDeadline = versionsPagingDeadlineBudget
	deps := newCollectionDeps(cfg, runtime, store.New())

	versionsURL := srv.URL + "/versions/"
	versions, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("loadVersionsListCached: %v", err)
	}
	if want := versionsPagingDeadlineTotalPages * versionLimit; len(versions) != want {
		t.Fatalf("len(versions) = %d, want %d", len(versions), want)
	}
	if got := served.Load(); got != versionsPagingDeadlineTotalPages {
		t.Fatalf("served = %d pages, want exactly %d", got, versionsPagingDeadlineTotalPages)
	}
}
