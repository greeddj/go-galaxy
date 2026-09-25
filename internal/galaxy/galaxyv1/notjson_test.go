package galaxyv1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// webUIPage is the HTML shell a Galaxy web UI serves with 200 at a path its
// API does not own, as galaxy.ansible.com does under /v3.
const webUIPage = "<!doctype html><html><head><title>Galaxy</title></head><body></body></html>"

// truncatedBody is a JSON document cut short, as a dropped connection or a
// proxy leaves one: not JSON, and not a web page either.
const truncatedBody = `{"count":1,"results":[{"id":10923,`

const (
	lookupPath   = "/roles/?owner__username=geerlingguy&name=docker&page_size=50"
	versionsPath = "/roles/10923/versions/?page_size=50"
	versionsBody = `{"results":[{"name":"1.0.0"}]}`
)

// routeFetch is a FetchJSON over a real httptest server and the production
// cacheManager.FetchJSONWithCachePolicy, so every error has its real shape;
// seen lists the URLs asked, in order.
type routeFetch struct {
	fetch FetchJSON
	base  string
	seen  []string
}

// newRouteFetch serves routes (request URI to a body sent as JSON) and
// webUIPage for a path in html, both with 200, else 404.
func newRouteFetch(t *testing.T, routes map[string]string, html map[string]bool) *routeFetch {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := routes[r.URL.RequestURI()]; ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		if html[r.URL.Path] {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(webUIPage))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	rf := &routeFetch{base: srv.URL}
	rf.fetch = func(ctx context.Context, u string, out any, policy cacheManager.Policy) error {
		rf.seen = append(rf.seen, strings.TrimPrefix(u, srv.URL))
		return cacheManager.FetchJSONWithCachePolicy(ctx, srv.Client(), u, nil, out, policy, 0)
	}
	return rf
}

// rootWalkWant is what a v1 walk over two roots must end in: the answer from
// the /v1 root, the routed-around 404, or an abort on a body that is not JSON.
type rootWalkWant int

const (
	wantAnswer rootWalkWant = iota
	wantNoRoleAPI
	wantWebPageAbort
	wantNotJSONAbort
)

// assertRootWalk fails unless err is what want names, and never renders a
// formatting fault such as a nil cause.
func assertRootWalk(t *testing.T, err error, want rootWalkWant) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), "%!") {
		t.Errorf("err renders a formatting fault: %v", err)
	}
	switch want {
	case wantAnswer:
		if err != nil {
			t.Fatalf("err = %v, want the /v1 root's answer", err)
		}
	case wantNoRoleAPI:
		assertRoutedAround(t, err)
	case wantWebPageAbort, wantNotJSONAbort:
		assertNotJSONAbort(t, err, want == wantWebPageAbort)
	}
}

// assertRoutedAround fails unless err is helpers.ErrGalaxyRoleAPIUnavailable
// carrying the 404, with no skipped web page surfacing.
func assertRoutedAround(t *testing.T, err error) {
	t.Helper()
	statusErr, ok := errors.AsType[*cacheManager.HTTPStatusError](err)
	if !errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable) || !ok || statusErr.Code != http.StatusNotFound {
		t.Fatalf("err = %v, want helpers.ErrGalaxyRoleAPIUnavailable carrying the 404", err)
	}
	if errors.Is(err, helpers.ErrMetadataNotJSON) {
		t.Errorf("err = %v: a skipped web page must not surface", err)
	}
}

// assertNotJSONAbort fails unless err is helpers.ErrMetadataNotJSON, never the
// routed-around class, and is a web page exactly when webPage is set.
func assertNotJSONAbort(t *testing.T, err error, webPage bool) {
	t.Helper()
	if !errors.Is(err, helpers.ErrMetadataNotJSON) || errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable) {
		t.Fatalf("err = %v, want helpers.ErrMetadataNotJSON, never the routed-around class", err)
	}
	if got := cacheManager.IsWebPage(err); got != webPage {
		t.Errorf("IsWebPage(%v) = %t, want %t", err, got, webPage)
	}
}

// TestLookupRolePassesOverAWebPageRoot pins that a v1 root answering a web
// page is passed over without replacing a real 404, that web pages alone
// abort, and that a truncated body aborts even beside a 404.
func TestLookupRolePassesOverAWebPageRoot(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		routes map[string]string
		html   map[string]bool
		name   string
		want   rootWalkWant
	}{
		{
			name:   "web page then the role",
			routes: map[string]string{"/v1" + lookupPath: roleBody},
			html:   map[string]bool{"/api/v1/roles/": true},
			want:   wantAnswer,
		},
		{name: "404 then web page", html: map[string]bool{"/v1/roles/": true}, want: wantNoRoleAPI},
		{name: "web page then 404", html: map[string]bool{"/api/v1/roles/": true}, want: wantNoRoleAPI},
		{
			name: "web page on every root",
			html: map[string]bool{"/api/v1/roles/": true, "/v1/roles/": true},
			want: wantWebPageAbort,
		},
		{
			name:   "truncated then 404",
			routes: map[string]string{"/api/v1" + lookupPath: truncatedBody},
			want:   wantNotJSONAbort,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rf := newRouteFetch(t, tt.routes, tt.html)
			role, found, err := LookupRole(context.Background(), rf.fetch, rf.base, "geerlingguy", "docker", cacheManager.Policy{})
			assertRootWalk(t, err, tt.want)
			if tt.want == wantAnswer && (!found || role.ID != 10923) {
				t.Fatalf("role=%+v found=%t, want the role from the /v1 root", role, found)
			}
			if tt.want != wantAnswer && found {
				t.Fatalf("found = true alongside err %v", err)
			}
		})
	}
}

// TestListVersionsPassesOverAWebPageRoot pins that ListVersions skips a root
// whose first page is a web page, as LookupRole does, but aborts on web pages
// alone and on a web page past the first page, a broken listing.
func TestListVersionsPassesOverAWebPageRoot(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		routes   map[string]string
		html     map[string]bool
		name     string
		wantSeen []string
		want     rootWalkWant
	}{
		{
			name:   "web page then versions",
			routes: map[string]string{"/v1" + versionsPath: versionsBody},
			html:   map[string]bool{"/api/v1/roles/10923/versions/": true},
			want:   wantAnswer,
		},
		{name: "404 then web page", html: map[string]bool{"/v1/roles/10923/versions/": true}, want: wantNoRoleAPI},
		{
			name: "web page on every root",
			html: map[string]bool{"/api/v1/roles/10923/versions/": true, "/v1/roles/10923/versions/": true},
			want: wantWebPageAbort,
		},
		{
			name: "web page on a later page",
			routes: map[string]string{
				"/api/v1" + versionsPath: `{"next_link":"/api/v1/roles/10923/versions/?page=2&page_size=50","results":[{"name":"1.0.0"}]}`,
				"/v1" + versionsPath:     versionsBody,
			},
			html:     map[string]bool{"/api/v1/roles/10923/versions/": true},
			want:     wantWebPageAbort,
			wantSeen: []string{"/api/v1" + versionsPath, "/api/v1/roles/10923/versions/?page=2&page_size=50"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rf := newRouteFetch(t, tt.routes, tt.html)
			versions, _, err := ListVersions(context.Background(), rf.fetch, rf.base, 10923, cacheManager.Policy{})
			assertRootWalk(t, err, tt.want)
			if tt.want == wantAnswer && (len(versions) != 1 || versions[0].Name != "1.0.0") {
				t.Fatalf("versions = %+v, want the /v1 root's 1.0.0", versions)
			}
			if tt.wantSeen != nil && strings.Join(rf.seen, " ") != strings.Join(tt.wantSeen, " ") {
				t.Fatalf("requests = %q, want %q: the walk stops at the broken listing", rf.seen, tt.wantSeen)
			}
		})
	}
}

// TestEmptyBaseNamesNoV1Root pins that a base naming no v1 root at all is
// helpers.ErrGalaxyRoleAPIUnavailable with a cause of its own, never a nil one.
func TestEmptyBaseNamesNoV1Root(t *testing.T) {
	t.Parallel()
	f := &fakeFetch{}
	_, found, err := LookupRole(context.Background(), f.fetch, " ", "geerlingguy", "docker", cacheManager.Policy{})
	if found || !errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable) || strings.Contains(err.Error(), "%!") {
		t.Fatalf("LookupRole: found=%t err=%v, want a clean helpers.ErrGalaxyRoleAPIUnavailable", found, err)
	}
	_, _, err = ListVersions(context.Background(), f.fetch, "", 10923, cacheManager.Policy{})
	if !errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable) || strings.Contains(err.Error(), "%!") {
		t.Fatalf("ListVersions: err=%v, want a clean helpers.ErrGalaxyRoleAPIUnavailable", err)
	}
	if len(f.seen) != 0 {
		t.Fatalf("requests = %v, want none: no root, no request", f.seen)
	}
}
