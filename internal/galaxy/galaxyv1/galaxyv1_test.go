package galaxyv1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// fakeFetch answers URLs from a table of JSON bodies; an unknown URL is a
// 404, the status every probe of an absent API root gets.
type fakeFetch struct {
	bodies map[string]string
	status map[string]int
	seen   []string
}

func (f *fakeFetch) fetch(_ context.Context, u string, out any, _ cacheManager.Policy) error {
	f.seen = append(f.seen, u)
	if code, ok := f.status[u]; ok {
		return &cacheManager.HTTPStatusError{URL: u, Status: http.StatusText(code), Code: code}
	}
	body, ok := f.bodies[u]
	if !ok {
		return &cacheManager.HTTPStatusError{URL: u, Status: "404 Not Found", Code: http.StatusNotFound}
	}
	return json.Unmarshal([]byte(body), out)
}

const (
	testBase     = "https://galaxy.example"
	lookupURL    = testBase + "/api/v1/roles/?owner__username=geerlingguy&name=docker&page_size=50"
	lookupURLNG  = testBase + "/v1/roles/?owner__username=geerlingguy&name=docker&page_size=50"
	versionsURL  = testBase + "/api/v1/roles/10923/versions/?page_size=50"
	versionsURL2 = testBase + "/api/v1/roles/10923/versions/?page=2&page_size=50"
	roleBody     = `{"count":1,"results":[{"id":10923,"github_user":"geerlingguy",` +
		`"github_repo":"ansible-role-docker","github_branch":"master"}]}`
)

func TestLooseLess(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		a, b     string
		less, ok bool
	}{
		{"1.0.0", "1.1.0", true, true},
		{"1.10.0", "1.9.0", false, true},
		{"1.0", "1.0.0", true, true},
		{"1.0.0", "1.0", false, true},
		{"1.2.3", "1.2.3", false, true},
		// LooseVersion's known quirk: a prerelease suffix sorts after the
		// release, since a longer list compares greater after a common prefix.
		{"1.2.3rc1", "1.2.3", false, true},
		{"1.2.3", "1.2.3rc1", true, true},
		{"v1.2.3", "1.2.4", false, false},
		{"v1.2.3", "v1.10.0", true, true},
		{"1.2.3a", "1.2.3b", true, true},
		{"2.0", "10.0", true, true},
		{"1.0.0-beta", "1.0.0-rc", true, true},
	} {
		less, ok := LooseLess(tt.a, tt.b)
		if less != tt.less || ok != tt.ok {
			t.Errorf("LooseLess(%q, %q) = (%t, %t), want (%t, %t)", tt.a, tt.b, less, ok, tt.less, tt.ok)
		}
	}
}

func TestSelect(t *testing.T) {
	t.Parallel()
	tags := []Version{{Name: "1.0.0"}, {Name: "1.10.0", CommitSHA: "a"}, {Name: "1.9.0"}}
	for _, tt := range []struct {
		wantErr   error
		name      string
		requested string
		branch    string
		wantTag   string
		wantSHA   string
		versions  []Version
	}{
		{name: "highest loose", versions: tags, wantTag: "1.10.0", wantSHA: "a"},
		{name: "requested listed", versions: tags, requested: "1.9.0", wantTag: "1.9.0"},
		{name: "requested missing", versions: tags, requested: "2.0.0", wantErr: helpers.ErrRoleVersionNotFound},
		{name: "requested is the branch", versions: tags, requested: "main", branch: "main", wantTag: "main"},
		{name: "requested is master", versions: tags, requested: "master", branch: "main", wantTag: "master"},
		{name: "no tags falls back to branch", branch: "main", wantTag: "main"},
		{name: "no tags and no branch falls back to master", wantTag: "master"},
		{name: "no tags accepts any requested", requested: "anything", wantTag: "anything"},
		{name: "incomparable", versions: []Version{{Name: "v1.0"}, {Name: "1.1"}}, wantErr: helpers.ErrRoleVersionsIncomparable},
		{name: "loose tie breaks lexicographically", versions: []Version{{Name: "1.0"}, {Name: "1.0.0"}, {Name: "1.00"}}, wantTag: "1.0.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tag, sha, err := Select(tt.versions, tt.requested, tt.branch)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if tag != tt.wantTag || sha != tt.wantSHA {
				t.Fatalf("= (%q, %q), want (%q, %q)", tag, sha, tt.wantTag, tt.wantSHA)
			}
		})
	}
}

func TestResolveWalksBothRequests(t *testing.T) {
	t.Parallel()
	f := &fakeFetch{bodies: map[string]string{
		lookupURL: roleBody,
		versionsURL: `{"next_link":"/api/v1/roles/10923/versions/?page=2&page_size=50",` +
			`"results":[{"name":"1.0.0","commit_sha":null},{"name":"bad name","commit_sha":null}]}`,
		versionsURL2: `{"next_link":null,"results":[{"name":"1.1.0","commit_sha":"0123456789ABCDEF0123456789abcdef01234567"}]}`,
	}}
	res, found, warnings, err := Resolve(context.Background(), f.fetch, testBase, "geerlingguy", "docker", "", cacheManager.Policy{})
	if err != nil || !found {
		t.Fatalf("Resolve: found=%t err=%v", found, err)
	}
	want := Resolution{
		RepoURL: res.RepoURL, Ref: res.Ref, Version: "1.1.0",
		GalaxySHA: "0123456789abcdef0123456789abcdef01234567", Versions: []string{"1.0.0", "1.1.0"},
	}
	if res.RepoURL.String() != "https://github.com/geerlingguy/ansible-role-docker" || res.Ref.Name != "refs/tags/1.1.0" ||
		fmt.Sprintf("%+v", res) != fmt.Sprintf("%+v", want) {
		t.Fatalf("resolution = %+v, want %+v", res, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "bad name") {
		t.Fatalf("warnings = %q", warnings)
	}
	if len(f.seen) != 3 {
		t.Fatalf("requests = %v, want lookup plus two pages", f.seen)
	}
}

func TestResolveFallsBackToTheNGRoot(t *testing.T) {
	t.Parallel()
	f := &fakeFetch{bodies: map[string]string{
		lookupURLNG: roleBody,
		testBase + "/v1/roles/10923/versions/?page_size=50": `{"results":[]}`,
	}}
	res, found, _, err := Resolve(context.Background(), f.fetch, testBase, "geerlingguy", "docker", "", cacheManager.Policy{})
	if err != nil || !found {
		t.Fatalf("Resolve: found=%t err=%v", found, err)
	}
	if res.Ref.Name != "refs/heads/master" || res.Version != "master" {
		t.Fatalf("no tags: ref = %q version = %q, want the github_branch as a qualified ref", res.Ref.Name, res.Version)
	}
}

func TestLookupRoleClassifiesAnswers(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		wantErr error
		name    string
		body    string
		status  int
	}{
		{name: "no v1 anywhere", status: http.StatusNotFound, wantErr: helpers.ErrGalaxyRoleAPIUnavailable},
		{name: "auth", status: http.StatusUnauthorized, wantErr: helpers.ErrGalaxyAuthFailed},
		{name: "unavailable", status: http.StatusBadGateway, wantErr: helpers.ErrGalaxyServerUnavailable},
		{name: "bad request", status: http.StatusBadRequest, wantErr: helpers.ErrGalaxyServerUnavailable},
		{name: "not implemented", status: http.StatusNotImplemented, wantErr: helpers.ErrGalaxyServerUnavailable},
		{name: "invalid record", body: `{"results":[{"id":1,"github_user":"../x","github_repo":"r"}]}`, wantErr: helpers.ErrGalaxyRoleInvalid},
		{name: "invalid branch", body: `{"results":[{"id":1,"github_user":"u","github_repo":"r","github_branch":"a b"}]}`,
			wantErr: helpers.ErrGalaxyRoleInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeFetch{bodies: map[string]string{}, status: map[string]int{}}
			if tt.body != "" {
				f.bodies[lookupURL] = tt.body
			} else {
				f.status[lookupURL], f.status[lookupURLNG] = tt.status, tt.status
			}
			_, found, err := LookupRole(context.Background(), f.fetch, testBase, "geerlingguy", "docker", cacheManager.Policy{})
			if !errors.Is(err, tt.wantErr) || found {
				t.Fatalf("found=%t err=%v, want %v", found, err, tt.wantErr)
			}
		})
	}
}

func TestLookupRoleNotFoundAndAPISuffix(t *testing.T) {
	t.Parallel()
	f := &fakeFetch{bodies: map[string]string{lookupURL: `{"results":[]}`}}
	_, found, err := LookupRole(context.Background(), f.fetch, testBase, "geerlingguy", "docker", cacheManager.Policy{})
	if err != nil || found {
		t.Fatalf("found=%t err=%v, want not found and no error", found, err)
	}

	g := &fakeFetch{bodies: map[string]string{testBase + "/api/v1/roles/?owner__username=a&name=b&page_size=50": `{"results":[]}`}}
	if _, _, err := LookupRole(context.Background(), g.fetch, testBase+"/api/", "a", "b", cacheManager.Policy{}); err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(g.seen) != 1 {
		t.Fatalf("requests = %v, want one: a base ending in /api has one v1 root", g.seen)
	}
}

func TestListVersionsRefusesAnEscapingNextLink(t *testing.T) {
	t.Parallel()
	f := &fakeFetch{bodies: map[string]string{
		versionsURL: `{"next":"https://evil.example/api/v1/roles/10923/versions/?page=2","results":[{"name":"1.0.0"}]}`,
	}}
	_, _, err := ListVersions(context.Background(), f.fetch, testBase, 10923, cacheManager.Policy{})
	if !errors.Is(err, helpers.ErrGalaxyRoleInvalid) {
		t.Fatalf("error = %v", err)
	}
}

func TestListVersionsCapsThePageWalk(t *testing.T) {
	t.Parallel()
	f := &fakeFetch{bodies: map[string]string{
		versionsURL: `{"next_link":"/api/v1/roles/10923/versions/?page_size=50","results":[{"name":"1.0.0"}]}`,
	}}
	_, _, err := ListVersions(context.Background(), f.fetch, testBase, 10923, cacheManager.Policy{})
	if !errors.Is(err, helpers.ErrVersionsPagingExceeded) {
		t.Fatalf("error = %v", err)
	}
	if len(f.seen) != helpers.RoleVersionsMaxPages {
		t.Fatalf("requests = %d, want the cap %d", len(f.seen), helpers.RoleVersionsMaxPages)
	}
}

func TestIsGitHubName(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   string
		want bool
	}{
		{"geerlingguy", true}, {"ansible-role-docker", true}, {"a.b_c", true}, {"", false}, {"-x", false}, {".x", false},
		{"..", false}, {"a/b", false}, {"a b", false}, {strings.Repeat("a", gitHubNameMaxLen+1), false},
	} {
		if got := isGitHubName(tt.in); got != tt.want {
			t.Errorf("isGitHubName(%q) = %t, want %t", tt.in, got, tt.want)
		}
	}
}

// TestValidateVersionAcceptsNumericTags pins that an all-digit tag, which the
// unqualified ref grammar would read as an abbreviated commit, is a tag here.
func TestValidateVersionAcceptsNumericTags(t *testing.T) {
	t.Parallel()
	f := &fakeFetch{bodies: map[string]string{
		lookupURL:   roleBody,
		versionsURL: `{"results":[{"name":"20230601"},{"name":"20240101"},{"name":"1.0.0"}]}`,
	}}
	res, _, warnings, err := Resolve(context.Background(), f.fetch, testBase, "geerlingguy", "docker", "", cacheManager.Policy{})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Resolve: err=%v warnings=%q", err, warnings)
	}
	if res.Version != "20240101" || res.Ref.Name != "refs/tags/20240101" {
		t.Fatalf("resolution = %+v, want the numeric date tag", res)
	}
}

func TestValidateRepository(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		raw string
		ok  bool
	}{
		{"https://github.com/geerlingguy/ansible-role-docker", true},
		{"https://github.com/geerlingguy/ansible-role-docker.git", true},
		{"https://evil.example/geerlingguy/ansible-role-docker", false},
		{"http://github.com/geerlingguy/ansible-role-docker", false},
		{"https://github.com/geerlingguy", false},
		{"https://github.com/geerlingguy/a/b", false},
		{"ssh://git@github.com/geerlingguy/ansible-role-docker", false},
	} {
		u, err := gitsource.ParseURL(tt.raw)
		if err != nil {
			t.Fatalf("ParseURL(%q): %v", tt.raw, err)
		}
		if got := ValidateRepository(u) == nil; got != tt.ok {
			t.Errorf("ValidateRepository(%q) ok = %t, want %t", tt.raw, got, tt.ok)
		}
	}
}
