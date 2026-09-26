package galaxyv1

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// versionsPageTwoBody is a first versions page whose next_link names page two.
const versionsPageTwoBody = `{"next_link":"/api/v1/roles/10923/versions/?page=2&page_size=50","results":[{"name":"1.0.0"}]}`

// TestListVersionsClassifiesStatus pins that a versions page answering any
// status but 404, first page or later, aborts carrying its own class, named
// once, and never reads as a server without v1.
func TestListVersionsClassifiesStatus(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		wantErr error
		name    string
		later   bool
		status  int
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantErr: helpers.ErrGalaxyAuthFailed},
		{name: "forbidden", status: http.StatusForbidden, wantErr: helpers.ErrGalaxyAuthFailed},
		{name: "bad request", status: http.StatusBadRequest, wantErr: helpers.ErrGalaxyServerUnavailable},
		{name: "unavailable", status: http.StatusServiceUnavailable, wantErr: helpers.ErrGalaxyServerUnavailable},
		{name: "later page failing", status: http.StatusInternalServerError, later: true, wantErr: helpers.ErrGalaxyServerUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeFetch{bodies: map[string]string{}, status: map[string]int{}}
			if tt.later {
				f.bodies[versionsURL] = versionsPageTwoBody
				f.status[versionsURL2] = tt.status
			} else {
				f.status[versionsURL] = tt.status
			}
			_, _, err := ListVersions(context.Background(), f.fetch, testBase, 10923, cacheManager.Policy{})
			if !errors.Is(err, tt.wantErr) || errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable) {
				t.Fatalf("err = %v, want %v and not the routed-around class", err, tt.wantErr)
			}
			if got := strings.Count(err.Error(), tt.wantErr.Error()); got != 1 {
				t.Errorf("%q names %q %d times, want once", err.Error(), tt.wantErr.Error(), got)
			}
		})
	}
}

// TestResolveReadsAFoundRoleWithoutVersionsAsNotFound pins that a 404 for the
// versions of a role the lookup just found is helpers.ErrRoleVersionNotFound, chaining
// the 404 of the root that listed it and naming a later page by number.
func TestResolveReadsAFoundRoleWithoutVersionsAsNotFound(t *testing.T) {
	t.Parallel()
	const versionsURLNG = testBase + "/v1/roles/10923/versions/?page_size=50"
	for _, tt := range []struct {
		bodies   map[string]string
		name     string
		want404  string
		wantText string
		notAsked string
	}{
		{name: "first page", bodies: map[string]string{lookupURL: roleBody}, want404: versionsURL},
		{
			name:     "later page",
			bodies:   map[string]string{lookupURL: roleBody, versionsURL: versionsPageTwoBody},
			want404:  versionsURL2,
			wantText: "versions page 2: ",
		},
		{
			name:     "listed at the Galaxy NG root",
			bodies:   map[string]string{lookupURLNG: roleBody},
			want404:  versionsURLNG,
			notAsked: versionsURL,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeFetch{bodies: tt.bodies}
			_, found, _, err := Resolve(context.Background(), f.fetch, testBase, "geerlingguy", "docker", "", cacheManager.Policy{})
			if !found || !errors.Is(err, helpers.ErrRoleVersionNotFound) || errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable) {
				t.Fatalf("found=%t err=%v, want found with helpers.ErrRoleVersionNotFound and not the routed-around class", found, err)
			}
			assertVersionsGone(t, err, tt.want404, tt.wantText)
			if tt.notAsked != "" && slices.Contains(f.seen, tt.notAsked) {
				t.Errorf("requests = %q: a root that did not list the role was asked for its versions", f.seen)
			}
		})
	}
}

// assertVersionsGone fails unless err chains the 404 of want404 and names the
// role's versions as not published at testBase, plus wantText when set.
func assertVersionsGone(t *testing.T, err error, want404, wantText string) {
	t.Helper()
	if statusErr, ok := errors.AsType[*cacheManager.HTTPStatusError](err); !ok || statusErr.URL != want404 {
		t.Errorf("err = %v, want the 404 of %s chained", err, want404)
	}
	for _, want := range []string{"geerlingguy.docker: its versions are not published at " + testBase, wantText} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
}

// TestResolveLookupAll404StaysWithoutV1 pins the lookup half, which the
// versions rule leaves alone: a 404 at every v1 root is a server without v1.
func TestResolveLookupAll404StaysWithoutV1(t *testing.T) {
	t.Parallel()
	f := &fakeFetch{bodies: map[string]string{}}
	_, found, _, err := Resolve(context.Background(), f.fetch, testBase, "geerlingguy", "docker", "", cacheManager.Policy{})
	if found || !errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable) || errors.Is(err, helpers.ErrRoleVersionNotFound) {
		t.Fatalf("found=%t err=%v, want helpers.ErrGalaxyRoleAPIUnavailable alone", found, err)
	}
}
