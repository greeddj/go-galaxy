package fakegalaxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// RoleVersion is one version AddRole registers for a role: the tag name the
// v1 API lists, and the commit the server recorded for it ("" for null,
// which galaxy.ansible.com sends for most of its history).
type RoleVersion struct {
	Name      string
	CommitSHA string
}

// fakeRole is the registry entry for one owner.name role: its GitHub
// repository, default branch, and versions listed unsorted in registration
// order, as galaxy.ansible.com lists oldest first.
type fakeRole struct {
	owner    string
	name     string
	user     string
	repo     string
	branch   string
	versions []RoleVersion
	id       int64
}

// rolePageSize is the v1 versions page size when a request names no
// page_size; a request's own page_size wins, as on galaxy.ansible.com.
const rolePageSize = 10

// AddRole registers owner.name as imported from github.com/<user>/<repo> and
// returns the id the versions route is keyed by; until the first AddRole the
// server answers no v1 route at all.
func (s *Server) AddRole(owner, name, user, repo, branch string, versions []RoleVersion) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := owner + "." + name
	role, ok := s.roles[key]
	if !ok {
		role = &fakeRole{owner: owner, name: name, id: int64(len(s.roles) + 1)}
		s.roles[key] = role
		s.rolesByID[role.id] = role
	}
	role.user, role.repo, role.branch = user, repo, branch
	role.versions = append([]RoleVersion(nil), versions...)
	return role.id
}

// roleRouteSegments returns what follows "api/v1/roles" (galaxy shape) or
// "v1/roles" (hub shape) under the base path, reporting false while no role
// is registered.
func (s *Server) roleRouteSegments(segments []string) ([]string, bool) {
	s.mu.Lock()
	hasRoles := len(s.roles) > 0
	s.mu.Unlock()
	if !hasRoles {
		return nil, false
	}
	v1Prefix := []string{v1Segment, rolesSegment}
	if len(s.apiRoot) > 0 && s.apiRoot[0] == apiSegment {
		v1Prefix = []string{apiSegment, v1Segment, rolesSegment}
	}
	return stripPrefixSegments(segments, v1Prefix)
}

// dispatchRoles routes the two v1 role shapes: "" (the lookup, filtered by
// query) and "<id>/versions".
func (s *Server) dispatchRoles(w http.ResponseWriter, r *http.Request, rest []string) {
	switch {
	case len(rest) == 0 || len(rest) == 1 && rest[0] == "":
		s.incr(EndpointRoleLookup)
		if s.checkAuth(w, r, EndpointRoleLookup) {
			return
		}
		s.handleRoleLookup(w, r)
	case len(rest) >= 2 && rest[1] == versionsSegment:
		s.incr(EndpointRoleVersions)
		if s.checkAuth(w, r, EndpointRoleVersions) {
			return
		}
		s.handleRoleVersions(w, r, rest[0])
	default:
		http.NotFound(w, r)
	}
}

// handleRoleLookup answers "api/v1/roles/?owner__username=&name=" with the
// matching role, or an empty results list.
func (s *Server) handleRoleLookup(w http.ResponseWriter, r *http.Request) {
	owner, name := r.URL.Query().Get("owner__username"), r.URL.Query().Get("name")
	if s.applyFault(w, r, EndpointRoleLookup, owner, name) {
		return
	}
	s.mu.Lock()
	role, ok := s.roles[owner+"."+name]
	results := []roleLookupResult{}
	if ok {
		results = append(results, roleLookupResult{
			ID: role.id, GitHubUser: role.user, GitHubRepo: role.repo, GitHubBranch: role.branch, Name: role.name, Username: role.owner,
		})
	}
	s.mu.Unlock()
	body, err := json.Marshal(roleLookupPage{Count: len(results), Results: results})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONWithETag(w, r, body)
}

// roleLookupPage and roleLookupResult are the v1 lookup response shape, with
// the fields a real record carries that this tool reads plus the two names
// it ignores, so a decoder that reads more than it should is exercised.
type roleLookupPage struct {
	Next     *string            `json:"next"`
	Previous *string            `json:"previous"`
	Results  []roleLookupResult `json:"results"`
	Count    int                `json:"count"`
}

type roleLookupResult struct {
	GitHubUser   string `json:"github_user"`
	GitHubRepo   string `json:"github_repo"`
	GitHubBranch string `json:"github_branch"`
	Name         string `json:"name"`
	Username     string `json:"username"`
	ID           int64  `json:"id"`
}

// roleVersionsPage and roleVersionResult are the v1 versions response shape:
// next_link carries a path, as Galaxy NG sends it; commit_sha and
// download_url are nullable, as galaxy.ansible.com sends them.
type roleVersionsPage struct {
	Next         *string             `json:"next"`
	NextLink     *string             `json:"next_link"`
	Previous     *string             `json:"previous"`
	PreviousLink *string             `json:"previous_link"`
	Results      []roleVersionResult `json:"results"`
	Count        int                 `json:"count"`
}

type roleVersionResult struct {
	CommitSHA   *string `json:"commit_sha"`
	DownloadURL *string `json:"download_url"`
	Name        string  `json:"name"`
	Version     string  `json:"version"`
}

// handleRoleVersions answers "api/v1/roles/<id>/versions/" a page at a time,
// with next_link carrying the path of the following page, as Galaxy NG's
// v1 does.
func (s *Server) handleRoleVersions(w http.ResponseWriter, r *http.Request, rawID string) {
	id, err := strconv.ParseInt(rawID, decimalBase, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	role, ok := s.rolesByID[id]
	var versions []RoleVersion
	if ok {
		versions = append(versions, role.versions...)
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if s.applyFault(w, r, EndpointRoleVersions, role.owner, role.name) {
		return
	}
	page, size := pageParams(r)
	start := min((page-1)*size, len(versions))
	end := min(start+size, len(versions))
	results := make([]roleVersionResult, 0, end-start)
	for _, v := range versions[start:end] {
		res := roleVersionResult{Name: v.Name, Version: strings.TrimPrefix(v.Name, "v")}
		if v.CommitSHA != "" {
			sha := v.CommitSHA
			res.CommitSHA = &sha
		}
		results = append(results, res)
	}
	resp := roleVersionsPage{Count: len(versions), Results: results}
	if end < len(versions) {
		next := r.URL.Path + "?page=" + strconv.Itoa(page+1) + "&page_size=" + strconv.Itoa(size)
		resp.NextLink = &next
	}
	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONWithETag(w, r, body)
}

// pageParams reads ?page= and ?page_size=, defaulting to the first page and
// rolePageSize.
func pageParams(r *http.Request) (int, int) {
	page, size := 1, rolePageSize
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("page_size")); err == nil && v > 0 {
		size = v
	}
	return page, size
}
