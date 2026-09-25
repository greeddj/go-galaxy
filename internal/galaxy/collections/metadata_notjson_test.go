package collections

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// webUIShell is the HTML a Galaxy web UI answers with 200 at a path its API
// does not own, as galaxy.ansible.com does under /v3.
const webUIShell = "<!doctype html><html><head><title>Galaxy</title></head><body></body></html>"

// newWebUIRootServer serves jsonBody under jsonPrefix, 404 under notFoundPrefix
// and webUIShell with 200 on every other path; an empty prefix matches nothing
// and jsonPrefix is checked first. It counts every request.
func newWebUIRootServer(t *testing.T, notFoundPrefix, jsonPrefix, jsonBody string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case jsonPrefix != "" && strings.HasPrefix(r.URL.Path, jsonPrefix):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(jsonBody))
		case notFoundPrefix != "" && strings.HasPrefix(r.URL.Path, notFoundPrefix):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(webUIShell))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// webUIRootDeps builds collectionDeps for the one server at srv's URL.
func webUIRootDeps(srv *httptest.Server) collectionDeps {
	cfg := &config.Config{Server: srv.URL}
	return newCollectionDeps(cfg, infra.New(noopPrinter{}, srv.Client()), store.New())
}

// TestLoadRootMetadataCachedPassesOverWebPagesKeepingThe404 pins that web UI
// answers after a 404 are passed over without replacing it, so the walk ends
// in the unknown-package path and no candidate is memoized as the winner.
func TestLoadRootMetadataCachedPassesOverWebPagesKeepingThe404(t *testing.T) {
	t.Parallel()
	srv, requests := newWebUIRootServer(t, "/api/v3/", "", "")
	deps := webUIRootDeps(srv)
	col := collection{Namespace: "nosuchns", Name: "nosuchcoll"}
	policy := cacheManager.Policy{Read: true, Write: true}

	_, _, err := loadRootMetadataCached(context.Background(), deps, col, policy)
	statusErr, ok := errors.AsType[*cacheManager.HTTPStatusError](err)
	if !ok || statusErr.Code != http.StatusNotFound {
		t.Fatalf("err = %v, want the /api/v3 404 as the walk's last error", err)
	}
	if errors.Is(err, helpers.ErrMetadataNotJSON) || !isUnknownPackageError(err) {
		t.Fatalf("err = %v, want the unknown-package path, not a web page abort", err)
	}
	if got, want := int(requests.Load()), len(rootMetadataURLCandidates(srv.URL, col, newAPIRootMemo())); got != want {
		t.Errorf("requests = %d, want %d: every candidate is tried past a web UI answer", got, want)
	}
	if root, ok := deps.apiRoots.winner(srv.URL); ok {
		t.Errorf("apiRoot %q was memoized from a web UI answer", root)
	}
}

// TestLoadRootMetadataCachedAbortsOnAServerOfWebPagesAlone pins that a server
// answering a web page on every candidate, as an SSO front does, aborts the
// walk naming the server: without a 404 it is no evidence col is absent.
func TestLoadRootMetadataCachedAbortsOnAServerOfWebPagesAlone(t *testing.T) {
	t.Parallel()
	srv, _ := newWebUIRootServer(t, "", "", "")
	deps := webUIRootDeps(srv)
	col := collection{Namespace: "nosuchns", Name: "nosuchcoll"}

	_, _, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{Read: true, Write: true})
	if !errors.Is(err, helpers.ErrMetadataNotJSON) || !cacheManager.IsWebPage(err) {
		t.Fatalf("err = %v, want a web page's helpers.ErrMetadataNotJSON", err)
	}
	if isUnknownPackageError(err) {
		t.Fatalf("err = %v: a server of web pages alone must not read as the collection's absence", err)
	}
	if want := "server " + srv.URL + " answers a web page at every API root"; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to contain %q", err, want)
	}
	if root, ok := deps.apiRoots.winner(srv.URL); ok {
		t.Errorf("apiRoot %q was memoized from a web UI answer", root)
	}
}

// TestLoadRootMetadataCachedAbortsOnATruncatedRoot pins that a body cut short
// at an API root aborts the walk even beside 404s: only markup is passed over,
// so a real root that failed to answer is never read as the collection absent.
func TestLoadRootMetadataCachedAbortsOnATruncatedRoot(t *testing.T) {
	t.Parallel()
	srv, requests := newWebUIRootServer(t, "/", "/api/v3/", `{"highest_version":{"href":"/api/v3/`)
	deps := webUIRootDeps(srv)
	col := collection{Namespace: "acme", Name: "widgets"}

	_, _, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{Read: true, Write: true})
	if !errors.Is(err, helpers.ErrMetadataNotJSON) || cacheManager.IsWebPage(err) {
		t.Fatalf("err = %v, want helpers.ErrMetadataNotJSON for a body that is not a web page", err)
	}
	if isUnknownPackageError(err) {
		t.Fatalf("err = %v: a truncated root must not read as the collection's absence", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1: the walk stops at the truncated root", got)
	}
}

// TestLoadRootMetadataCachedResolvesPastAWebUIRoot pins that a web UI answer
// at /api/v3 does not stop the walk from resolving at /v3, which alone is
// memoized as the winning API root.
func TestLoadRootMetadataCachedResolvesPastAWebUIRoot(t *testing.T) {
	t.Parallel()
	srv, _ := newWebUIRootServer(t, "", "/v3/", v2FallThroughBody)
	deps := webUIRootDeps(srv)
	col := collection{Namespace: "acme", Name: "widgets"}

	root, base, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{Read: true, Write: true})
	if err != nil {
		t.Fatalf("loadRootMetadataCached: %v", err)
	}
	if root.VersionsURL != v2FallThroughVersionsURL || base != srv.URL {
		t.Fatalf("root = %+v from %q, want the /v3 document from %q", root, base, srv.URL)
	}
	if winner, ok := deps.apiRoots.winner(srv.URL); !ok || winner != srv.URL+"/v3" {
		t.Fatalf("memoized apiRoot = %q (%t), want %q", winner, ok, srv.URL+"/v3")
	}
}
