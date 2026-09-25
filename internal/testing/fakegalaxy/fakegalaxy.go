// Package fakegalaxy is an in-memory Galaxy v3 (and v1 role) API double with
// deterministic artifacts, fault injection, request counting and auth capture.
// Its constructors take testing.TB, so production code can never reach it.
package fakegalaxy

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/psvmcc/hub/pkg/types"
)

// Endpoint identifies one of the routes the fake server answers, for fault
// injection (Fail) and request counting (Count).
type Endpoint int

// The routes the fake server answers: four v3 collection routes, two v1 role
// routes (404 until AddRole registers a role, as on an Automation Hub) and
// the free-path tarball route AddTarball and AddRedirect register.
const (
	EndpointRootMetadata Endpoint = iota
	EndpointVersionsList
	EndpointVersionDetail
	EndpointArtifact
	EndpointRoleLookup
	EndpointRoleVersions
	EndpointTarball
)

// endpointCount is the number of distinct Endpoint values, sizing Server's
// per-endpoint counters array.
const endpointCount = 7

// Path segment names used by ServeHTTP's routing and the URLs this package
// builds. Named rather than repeated string literals, since "api" alone
// appears in three different route predicates.
const (
	apiSegment         = "api"
	v3Segment          = "v3"
	v1Segment          = "v1"
	rolesSegment       = "roles"
	collectionsSegment = "collections"
	versionsSegment    = "versions"
	downloadSegment    = "download"
)

// decimalBase is the radix parseDigits parses in.
const decimalBase = 10

// tarFileMode is the fixed permission bits stamped on every generated tar
// entry, keeping the artifact's bytes independent of the environment it
// was built in.
const tarFileMode = 0o644

// Names and values a generated artifact's chain-of-trust documents carry,
// spelled locally rather than imported from helpers or manifest: a double
// sharing them with the code under test could not catch that code drifting.
const (
	// filesManifestFileName is the per-file digest listing MANIFEST.json names.
	filesManifestFileName = "FILES.json"
	// filesEntryTypeFile is the ftype of a FILES.json row for a regular file.
	filesEntryTypeFile = "file"
	// chksumTypeSHA256 is the checksum algorithm both documents name.
	chksumTypeSHA256 = "sha256"
	// signatureFieldKey is the key a server's version-metadata signatures
	// carry, as collections.serverSignatureField reads them.
	signatureFieldKey = "signature"
)

// Fixed points in time, never time.Now, so every byte this package produces
// and its sha256 are identical on every run and every machine.
//
//nolint:gochecknoglobals // see above: fixed, not runtime-mutable, state.
var (
	// fixedModTime is used for every generated tar entry's ModTime and for
	// the fixed Last-Modified header on JSON responses.
	fixedModTime = time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	// fixedLastModified is fixedModTime pre-formatted the way net/http
	// expects it on a Last-Modified header.
	fixedLastModified = fixedModTime.UTC().Format(http.TimeFormat)
)

// Server is an in-memory Ansible Galaxy v3 API test double. It must only be
// constructed via New or NewAtBasePath.
type Server struct {
	srv            *httptest.Server
	collections    map[string]*fakeCollection
	artifacts      map[string]fakeArtifact
	roles          map[string]*fakeRole
	rolesByID      map[int64]*fakeRole
	tarballs       map[string][]byte
	redirects      map[string]string
	baseURL        string
	basePathPrefix string
	apiRootPrefix  string
	requiredAuth   string
	basePath       []string
	apiRoot        []string
	faults         []faultRule
	authSeen       [endpointCount]capturedAuth
	mu             sync.Mutex
	counts         [endpointCount]int
	authFailStatus int
}

// capturedAuth is the Authorization header one endpoint's most recent
// request carried; present tells an absent header from an empty value.
type capturedAuth struct {
	value   string
	present bool
}

// fakeCollection is the registry entry for one namespace/name pair: every
// registered version plus the premarshaled root metadata body, rebuilt as
// versions are added because it names the highest one.
type fakeCollection struct {
	namespace      string
	name           string
	versions       map[string]*fakeVersionEntry
	sortedVersions []string
	rootBody       []byte
}

// fakeVersionEntry is one registered collection version: its detail URL,
// artifact MANIFEST.json bytes, and the detail document with its marshaled
// body; info and detailBody must change together (AddVersion, SignVersion).
type fakeVersionEntry struct {
	info         *types.GalaxyCollectionVersionInfo
	version      string
	href         string
	sha256       string
	detailBody   []byte
	manifestJSON []byte
}

// fakeArtifact is one registered download and the namespace/name that
// produced it, so a Fail rule on EndpointArtifact can match by identity
// although the download route carries only a flat filename.
type fakeArtifact struct {
	namespace string
	name      string
	data      []byte
}

// faultRule is one armed Fail call, firing for requests to ep whose
// namespace/name match (empty is a wildcard); ruleMatches and consumeFault
// define how its Count is spent.
type faultRule struct {
	namespace string
	name      string
	ep        Endpoint
	fault     Fault
}

// Version is one registered collection version plus the sha256 of the
// artifact generated for it, so tests never hardcode a checksum.
type Version struct {
	Namespace   string
	Name        string
	Version     string
	SHA256      string
	DownloadURL string
}

// Fault is a scripted failure for Fail: one of Status, Hang, StallAfterBytes
// (artifact and tarball only) or DripInterval, firing while Count is nonzero
// (negative is unlimited). A test must abort a hang, stall or drip itself.
type Fault struct {
	Status          int
	Count           int
	StallAfterBytes int
	DripInterval    time.Duration
	Hang            bool
}

// New starts a fake shaped like galaxy.ansible.com, serving "/api/v3" at the
// root, and closes it through tb.Cleanup. testing.TB is implementable only by
// the testing package, which keeps the double out of production code.
func New(tb testing.TB) *Server {
	tb.Helper()
	return newServer(tb, "", []string{apiSegment, v3Segment})
}

// NewAtBasePath starts a fake shaped like a Galaxy NG / Automation Hub, with
// "v3" directly under basePath (which may be empty) and a 404 for "/api/v3",
// so a client's API-root probing meets what a real hub answers.
func NewAtBasePath(tb testing.TB, basePath string) *Server {
	tb.Helper()
	return newServer(tb, basePath, []string{v3Segment})
}

// newServer builds and starts a server serving its collection routes under
// apiRoot's segments, itself nested under basePath's. It is the shared body
// of New and NewAtBasePath, which differ only in those two values.
func newServer(tb testing.TB, basePath string, apiRoot []string) *Server {
	tb.Helper()

	trimmed := strings.Trim(basePath, "/")
	var segments []string
	var prefix string
	if trimmed != "" {
		segments = strings.Split(trimmed, "/")
		prefix = "/" + trimmed
	}

	s := &Server{
		collections:    make(map[string]*fakeCollection),
		artifacts:      make(map[string]fakeArtifact),
		roles:          make(map[string]*fakeRole),
		rolesByID:      make(map[int64]*fakeRole),
		tarballs:       make(map[string][]byte),
		redirects:      make(map[string]string),
		basePath:       segments,
		basePathPrefix: prefix,
		apiRoot:        apiRoot,
		apiRootPrefix:  "/" + strings.Join(apiRoot, "/"),
	}
	// Assigning s.srv and s.baseURL after the server starts is safe: nobody
	// knows its URL, so no request can arrive before newServer returns.
	srv := httptest.NewServer(s)
	tb.Cleanup(srv.Close)
	s.srv = srv
	s.baseURL = srv.URL

	return s
}

// URL returns the fake server's base URL, e.g. "http://127.0.0.1:PORT".
// AddVersion uses it internally to build every absolute URL it returns or
// embeds in a response body.
func (s *Server) URL() string {
	return s.baseURL
}

// Client returns an *http.Client wired to talk to the fake server,
// following httptest.Server's own conventions for the underlying
// transport.
func (s *Server) Client() *http.Client {
	return s.srv.Client()
}

// AddVersion registers one version of namespace/name, creating the
// collection on first use and recomputing its highest_version, and returns
// the generated artifact's sha256 with its identity.
func (s *Server) AddVersion(namespace, name, version string, deps map[string]string) Version {
	data, sum, manifestJSON := buildArtifact(namespace, name, version, deps)
	filename := fmt.Sprintf("%s-%s-%s.tar.gz", namespace, name, version)
	// Every URL handed out carries the base path and API root the server
	// routes on; the download route sits beside the API root, not under it.
	urlBase := s.baseURL + s.basePathPrefix
	collectionsBase := urlBase + s.apiRootPrefix
	href := fmt.Sprintf("%s/collections/%s/%s/versions/%s/", collectionsBase, namespace, name, version)
	downloadURL := fmt.Sprintf("%s/download/%s", urlBase, filename)
	info := buildVersionDetailBody(namespace, name, version, href, downloadURL, filename, sum, len(data), deps)
	detailBody := marshalVersionDetail(info)

	s.mu.Lock()
	defer s.mu.Unlock()

	key := collectionKey(namespace, name)
	col, ok := s.collections[key]
	if !ok {
		col = &fakeCollection{
			namespace: namespace,
			name:      name,
			versions:  make(map[string]*fakeVersionEntry),
		}
		s.collections[key] = col
	}
	col.versions[version] = &fakeVersionEntry{
		version: version, href: href, sha256: sum, detailBody: detailBody, manifestJSON: manifestJSON, info: info,
	}
	col.sortedVersions = sortedVersionKeys(col.versions)
	recomputeRootBody(col, collectionsBase)

	s.artifacts[filename] = fakeArtifact{namespace: namespace, name: name, data: data}

	return Version{Namespace: namespace, Name: name, Version: version, SHA256: sum, DownloadURL: downloadURL}
}

// ManifestJSON returns a copy of the MANIFEST.json the artifact of
// namespace/name/version carries, or nil if it was never registered; a
// signature over these bytes verifies against the artifact actually served.
func (s *Server) ManifestJSON(namespace, name, version string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	col, ok := s.collections[collectionKey(namespace, name)]
	if !ok {
		return nil
	}
	entry, ok := col.versions[version]
	if !ok {
		return nil
	}
	return slices.Clone(entry.manifestJSON)
}

// SignVersion appends signature to a registered version's detail, served as
// signatures[i].signature the way collections.serverSignatureBlobs reads it;
// an unregistered version fails the calling test through tb.
func (s *Server) SignVersion(tb testing.TB, namespace, name, version string, signature []byte) {
	tb.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()

	col, ok := s.collections[collectionKey(namespace, name)]
	if !ok {
		tb.Fatalf("fakegalaxy: SignVersion: %s.%s was never registered with AddVersion", namespace, name)
	}
	entry, ok := col.versions[version]
	if !ok {
		tb.Fatalf("fakegalaxy: SignVersion: %s.%s@%s was never registered with AddVersion", namespace, name, version)
	}

	// Signatures is written only here, so a failed assertion means "not
	// signed yet" (nil), never a foreign shape.
	existing, _ := entry.info.Signatures.([]map[string]string)
	existing = append(existing, map[string]string{signatureFieldKey: string(signature)})
	entry.info.Signatures = existing
	entry.detailBody = marshalVersionDetail(entry.info)
}

// Fail arms f for requests to ep matching namespace/name (empty is a
// wildcard). Rules are scanned in registration order and a second rule for
// the same endpoint adds to the first rather than replacing it.
func (s *Server) Fail(ep Endpoint, namespace, name string, f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, faultRule{namespace: namespace, name: name, ep: ep, fault: f})
}

// RequireAuth demands the exact Authorization value expected on every
// endpoint ("" means anonymous); a mismatch gets 401, or AuthFailStatus's
// status, before any route handler or armed Fail rule runs.
func (s *Server) RequireAuth(expected string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requiredAuth = expected
}

// AuthFailStatus replaces the default 401 an auth failure answers with; it
// matters only once RequireAuth has a non-empty value, set in either order.
func (s *Server) AuthFailStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authFailStatus = status
}

// SeenAuth reports the Authorization value ep's most recent request carried
// and whether it carried the header at all, so an absent header differs
// from an empty one; ("", false) before ep has seen a request.
func (s *Server) SeenAuth(ep Endpoint) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.authSeen[ep]
	return seen.value, seen.present
}

// Count reports how many requests ep has received since the server started
// or since the last ResetCounts.
func (s *Server) Count(ep Endpoint) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[ep]
}

// Total reports how many requests every endpoint has received combined.
func (s *Server) Total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, c := range s.counts {
		total += c
	}
	return total
}

// ResetCounts zeroes every endpoint's request counter. Armed fault rules
// are unaffected; use a fresh Server to reset those too.
func (s *Server) ResetCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts = [endpointCount]int{}
}

// ServeHTTP routes the known endpoints and 404s every other path, API-root
// probes included. Each dispatch counts first, checks auth second and applies
// faults last, so an armed Fault can never mask a missing or wrong header.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	segments, ok := stripPrefixSegments(segments, s.basePath)
	if !ok {
		// Outside the base path there is no endpoint to count against.
		http.NotFound(w, r)
		return
	}

	// A registered tarball path is matched first and exactly, the way a
	// url-source URL sits beside every other route.
	if s.dispatchTarball(w, r, strings.Join(segments, "/")) {
		return
	}

	// The download route sits beside the API root rather than under it, so
	// it is matched before the API root is stripped.
	if isDownloadPath(segments) {
		s.dispatchArtifact(w, r, segments[1])
		return
	}

	if roleSegments, ok := s.roleRouteSegments(segments); ok {
		s.dispatchRoles(w, r, roleSegments)
		return
	}

	collectionSegments, ok := stripPrefixSegments(segments, s.apiRoot)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch {
	case isRootMetadataPath(collectionSegments):
		s.dispatchRootMetadata(w, r, collectionSegments[1], collectionSegments[2])
	case isVersionsListPath(collectionSegments):
		s.dispatchVersionsList(w, r, collectionSegments[1], collectionSegments[2])
	case isVersionDetailPath(collectionSegments):
		s.dispatchVersionDetail(w, r, collectionSegments[1], collectionSegments[2], collectionSegments[4])
	default:
		http.NotFound(w, r)
	}
}

// AddTarball serves data as application/gzip at the free-form path under the
// base path, matched exactly, and returns its absolute URL. Faults, counting
// and auth capture address it as EndpointTarball with empty namespace/name.
func (s *Server) AddTarball(path string, data []byte) string {
	key := strings.Trim(path, "/")
	s.mu.Lock()
	s.tarballs[key] = data
	s.mu.Unlock()
	return s.baseURL + s.basePathPrefix + "/" + key
}

// AddRedirect answers path under the base path with a 302 to location, the
// release-asset shape, and returns the redirecting path's absolute URL.
func (s *Server) AddRedirect(path, location string) string {
	key := strings.Trim(path, "/")
	s.mu.Lock()
	s.redirects[key] = location
	s.mu.Unlock()
	return s.baseURL + s.basePathPrefix + "/" + key
}

// dispatchArtifact counts, then auth-gates, a request to the download
// route before handing it to handleArtifact. See ServeHTTP for why this
// ordering is load-bearing.
func (s *Server) dispatchArtifact(w http.ResponseWriter, r *http.Request, filename string) {
	s.incr(EndpointArtifact)
	if s.checkAuth(w, r, EndpointArtifact) {
		return
	}
	s.handleArtifact(w, r, filename)
}

// dispatchTarball serves a path AddTarball or AddRedirect registered, in
// ServeHTTP's count, auth, fault order, and reports false for any other path.
func (s *Server) dispatchTarball(w http.ResponseWriter, r *http.Request, path string) bool {
	s.mu.Lock()
	data, isTarball := s.tarballs[path]
	location, isRedirect := s.redirects[path]
	s.mu.Unlock()
	if !isTarball && !isRedirect {
		return false
	}
	s.incr(EndpointTarball)
	if s.checkAuth(w, r, EndpointTarball) {
		return true
	}
	if s.serveTarballFault(w, r, data, isTarball) {
		return true
	}
	if isRedirect {
		http.Redirect(w, r, location, http.StatusFound)
		return true
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	return true
}

// serveTarballFault consumes and enacts an armed EndpointTarball fault, and
// reports whether it answered the request.
func (s *Server) serveTarballFault(w http.ResponseWriter, r *http.Request, data []byte, isTarball bool) bool {
	fault, matched := s.consumeFault(EndpointTarball, "", "")
	if !matched {
		return false
	}
	if fault.StallAfterBytes > 0 && isTarball {
		serveArtifactStall(w, r, data, fault.StallAfterBytes)
		return true
	}
	if fault.DripInterval > 0 && isTarball && len(data) > 0 {
		serveArtifactDrip(w, r, data, fault.DripInterval)
		return true
	}
	return enactFault(w, r, fault)
}

// dispatchRootMetadata counts, then auth-gates, a request to the root
// metadata route before handing it to handleRootMetadata. See ServeHTTP
// for why this ordering is load-bearing.
func (s *Server) dispatchRootMetadata(w http.ResponseWriter, r *http.Request, namespace, name string) {
	s.incr(EndpointRootMetadata)
	if s.checkAuth(w, r, EndpointRootMetadata) {
		return
	}
	s.handleRootMetadata(w, r, namespace, name)
}

// dispatchVersionsList counts, then auth-gates, a request to the versions
// list route before handing it to handleVersionsList. See ServeHTTP for
// why this ordering is load-bearing.
func (s *Server) dispatchVersionsList(w http.ResponseWriter, r *http.Request, namespace, name string) {
	s.incr(EndpointVersionsList)
	if s.checkAuth(w, r, EndpointVersionsList) {
		return
	}
	s.handleVersionsList(w, r, namespace, name)
}

// dispatchVersionDetail counts, then auth-gates, a request to the version
// detail route before handing it to handleVersionDetail. See ServeHTTP for
// why this ordering is load-bearing.
func (s *Server) dispatchVersionDetail(w http.ResponseWriter, r *http.Request, namespace, name, version string) {
	s.incr(EndpointVersionDetail)
	if s.checkAuth(w, r, EndpointVersionDetail) {
		return
	}
	s.handleVersionDetail(w, r, namespace, name, version)
}

// stripPrefixSegments removes prefix (a base path or API root) from the
// front of segments, reporting false, and so a 404, when it is not there.
// An empty prefix always matches.
func stripPrefixSegments(segments, prefix []string) ([]string, bool) {
	if len(segments) < len(prefix) {
		return nil, false
	}
	for i, want := range prefix {
		if segments[i] != want {
			return nil, false
		}
	}
	return segments[len(prefix):], true
}

// checkAuth records r's Authorization header for SeenAuth and enforces
// RequireAuth, writing the failure status and reporting true on a mismatch.
// It runs after incr and before any fault handling; see ServeHTTP.
func (s *Server) checkAuth(w http.ResponseWriter, r *http.Request, ep Endpoint) bool {
	value, present := "", false
	if values, ok := r.Header["Authorization"]; ok {
		present = true
		if len(values) > 0 {
			value = values[0]
		}
	}

	s.mu.Lock()
	s.authSeen[ep] = capturedAuth{value: value, present: present}
	required := s.requiredAuth
	failStatus := s.authFailStatus
	s.mu.Unlock()

	if required == "" || (present && value == required) {
		return false
	}
	if failStatus == 0 {
		failStatus = http.StatusUnauthorized
	}
	w.WriteHeader(failStatus)
	return true
}

// isDownloadPath reports whether segments is "download/{file}".
func isDownloadPath(segments []string) bool {
	return len(segments) == 2 && segments[0] == downloadSegment
}

// isCollectionPath reports whether segments, already stripped of base path
// and API root, starts with "collections/{ns}/{name}".
func isCollectionPath(segments []string) bool {
	return len(segments) >= 3 && segments[0] == collectionsSegment
}

// isRootMetadataPath reports whether segments is exactly
// "collections/{ns}/{name}", relative to the API root.
func isRootMetadataPath(segments []string) bool {
	return len(segments) == 3 && isCollectionPath(segments)
}

// isVersionsListPath reports whether segments is
// "collections/{ns}/{name}/versions", relative to the API root.
func isVersionsListPath(segments []string) bool {
	return len(segments) == 4 && isCollectionPath(segments) && segments[3] == versionsSegment
}

// isVersionDetailPath reports whether segments is
// "collections/{ns}/{name}/versions/{version}", relative to the API root.
func isVersionDetailPath(segments []string) bool {
	return len(segments) == 5 && isCollectionPath(segments) && segments[3] == versionsSegment
}

// incr counts a request to ep. It runs before auth and fault handling, so
// Count and Total include requests a fault, an auth failure or a 404 ended.
func (s *Server) incr(ep Endpoint) {
	s.mu.Lock()
	s.counts[ep]++
	s.mu.Unlock()
}

// handleRootMetadata answers "api/v3/collections/{ns}/{name}".
func (s *Server) handleRootMetadata(w http.ResponseWriter, r *http.Request, namespace, name string) {
	if s.applyFault(w, r, EndpointRootMetadata, namespace, name) {
		return
	}
	s.mu.Lock()
	col, ok := s.collections[collectionKey(namespace, name)]
	var body []byte
	if ok {
		body = col.rootBody
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSONWithETag(w, r, body)
}

// handleVersionsList answers "api/v3/collections/{ns}/{name}/versions",
// honoring ?limit=&offset= pagination over the registered versions sorted
// ascending.
func (s *Server) handleVersionsList(w http.ResponseWriter, r *http.Request, namespace, name string) {
	if s.applyFault(w, r, EndpointVersionsList, namespace, name) {
		return
	}
	s.mu.Lock()
	col, ok := s.collections[collectionKey(namespace, name)]
	var sortedVersions []string
	var entries map[string]*fakeVersionEntry
	if ok {
		sortedVersions = col.sortedVersions
		entries = col.versions
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	total := len(sortedVersions)
	limit := parseQueryInt(r, "limit", total)
	offset := parseQueryInt(r, "offset", 0)
	page := paginate(sortedVersions, offset, limit)

	var resp types.GalaxyCollectionVersions
	resp.Meta.Count = total
	resp.Data = make([]types.GalaxyCollectionVersion, len(page))
	for i, v := range page {
		entry := entries[v]
		resp.Data[i] = types.GalaxyCollectionVersion{Version: entry.version, Href: entry.href}
	}

	body, err := json.Marshal(&resp)
	if err != nil {
		// Strings and an int cannot fail to marshal; an error is a harness bug.
		panic(fmt.Sprintf("fakegalaxy: marshal versions list: %v", err))
	}
	writeJSONWithETag(w, r, body)
}

// handleVersionDetail answers
// "api/v3/collections/{ns}/{name}/versions/{version}".
func (s *Server) handleVersionDetail(w http.ResponseWriter, r *http.Request, namespace, name, version string) {
	if s.applyFault(w, r, EndpointVersionDetail, namespace, name) {
		return
	}
	s.mu.Lock()
	col, ok := s.collections[collectionKey(namespace, name)]
	var body []byte
	if ok {
		var entry *fakeVersionEntry
		entry, ok = col.versions[version]
		if ok {
			body = entry.detailBody
		}
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSONWithETag(w, r, body)
}

// handleArtifact answers "download/{file}" with raw bytes and no cache
// validators. Stall and drip faults are enacted here, then Status and Hang,
// because the first two need the artifact's bytes enactFault lacks.
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request, filename string) {
	s.mu.Lock()
	art, ok := s.artifacts[filename]
	s.mu.Unlock()

	// The fault matches the artifact's registered identity, since the
	// download URL carries only a flat filename.
	if fault, matched := s.consumeFault(EndpointArtifact, art.namespace, art.name); matched {
		if fault.StallAfterBytes > 0 && ok {
			serveArtifactStall(w, r, art.data, fault.StallAfterBytes)
			return
		}
		if fault.DripInterval > 0 && ok && len(art.data) > 0 {
			serveArtifactDrip(w, r, art.data, fault.DripInterval)
			return
		}
		if enactFault(w, r, fault) {
			return
		}
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(art.data)
}

// serveArtifactStall flushes the first after bytes of data, chunked so no
// Content-Length reveals the end, then blocks until the client aborts; no
// wall clock is involved, so it stays deterministic under -race.
func serveArtifactStall(w http.ResponseWriter, r *http.Request, data []byte, after int) {
	n := min(after, len(data))
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data[:n])
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	<-r.Context().Done()
}

// serveArtifactDrip flushes non-empty data one byte per interval, cycling
// forever until r's context ends: it never stops making progress, so only a
// whole-transfer deadline, never a read-inactivity watchdog, catches it.
func serveArtifactDrip(w http.ResponseWriter, r *http.Request, data []byte, interval time.Duration) {
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for i := 0; ; i++ {
		if _, err := w.Write([]byte{data[i%len(data)]}); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(interval):
		}
	}
}

// applyFault consumes the first armed fault rule matching ep/namespace/name,
// if any, and enacts it against w/r. It reports whether it did (in which
// case the caller must not write any further response).
func (s *Server) applyFault(w http.ResponseWriter, r *http.Request, ep Endpoint, namespace, name string) bool {
	fault, matched := s.consumeFault(ep, namespace, name)
	if !matched {
		return false
	}
	return enactFault(w, r, fault)
}

// consumeFault returns the first rule, in Fail order, matching
// ep/namespace/name, spending one use of a positive Count; false when none
// matches.
func (s *Server) consumeFault(ep Endpoint, namespace, name string) (Fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.faults {
		rule := &s.faults[i]
		if !ruleMatches(rule, ep, namespace, name) {
			continue
		}
		if rule.fault.Count > 0 {
			rule.fault.Count--
		}
		return rule.fault, true
	}
	return Fault{}, false
}

// ruleMatches reports whether rule fires for ep/namespace/name: same
// endpoint, a nonzero Count (zero is the disabled zero value, negative is
// unlimited), and namespace/name empty or equal.
func ruleMatches(rule *faultRule, ep Endpoint, namespace, name string) bool {
	if rule.ep != ep || rule.fault.Count == 0 {
		return false
	}
	if rule.namespace != "" && rule.namespace != namespace {
		return false
	}
	return rule.name == "" || rule.name == name
}

// enactFault carries out Status, then Hang, then a JSON drip, reporting
// whether it answered; false (a StallAfterBytes fault here, say) lets the
// caller serve its normal response.
func enactFault(w http.ResponseWriter, r *http.Request, fault Fault) bool {
	if fault.Status != 0 {
		w.WriteHeader(fault.Status)
		return true
	}
	if fault.Hang {
		<-r.Context().Done()
		return true
	}
	if fault.DripInterval > 0 {
		dripJSONResponse(w, r, fault.DripInterval)
		return true
	}
	return false
}

// dripJSONResponse answers 200 with "{" and then one flushed space per
// interval until r's context ends: a JSON document that never completes and
// never goes idle, so only the client's deadline or cancellation ends it.
func dripJSONResponse(w http.ResponseWriter, r *http.Request, interval time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if _, err := w.Write([]byte("{")); err != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(interval):
		}
		if _, err := w.Write([]byte(" ")); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// writeJSONWithETag serves body as JSON with an ETag over its bytes and
// answers a matching If-None-Match with 304; If-Modified-Since is ignored.
func writeJSONWithETag(w http.ResponseWriter, r *http.Request, body []byte) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:])[:16] + `"`

	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("ETag", etag)
	header.Set("Last-Modified", fixedLastModified)

	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// collectionKey builds the registry key for a namespace/name pair. Test
// collection names never contain a slash, so this cannot collide.
func collectionKey(namespace, name string) string {
	return namespace + "/" + name
}

// parseQueryInt reads the named query parameter as a non-negative integer,
// returning def when it is absent or malformed.
func parseQueryInt(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, ok := parseDigits(raw)
	if !ok {
		return def
	}
	return n
}

// paginate returns up to limit versions from offset within the slice's
// bounds; a negative offset counts as zero, a negative limit as no limit, and
// an offset+limit that overflows int yields none.
func paginate(versions []string, offset, limit int) []string {
	if offset < 0 {
		offset = 0
	}
	if offset > len(versions) {
		offset = len(versions)
	}
	end := offset + limit
	if limit < 0 || end > len(versions) {
		end = len(versions)
	}
	if end < offset {
		end = offset
	}
	return versions[offset:end]
}

// sortedVersionKeys returns versions' keys sorted ascending by
// compareDottedVersions. The slice is pre-sized to len(versions) since its
// final length is already known.
func sortedVersionKeys(versions map[string]*fakeVersionEntry) []string {
	keys := make([]string, 0, len(versions))
	for v := range versions {
		keys = append(keys, v)
	}
	slices.SortFunc(keys, compareDottedVersions)
	return keys
}

// compareDottedVersions orders dot-separated versions component by
// component, numerically where both parse as digits and lexically otherwise;
// enough for plain semver, with no pre-release or build-metadata handling.
func compareDottedVersions(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	for i := 0; i < len(aParts) && i < len(bParts); i++ {
		if c := compareDottedComponent(aParts[i], bParts[i]); c != 0 {
			return c
		}
	}
	return len(aParts) - len(bParts)
}

// compareDottedComponent compares one dot-separated component of two
// versions, numerically if both sides parse as plain digit strings, or
// lexically otherwise.
func compareDottedComponent(a, b string) int {
	an, aOK := parseDigits(a)
	bn, bOK := parseDigits(b)
	if aOK && bOK {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a, b)
}

// parseDigits parses s as a non-negative base-10 integer only when every
// rune is an ASCII digit, so unlike "%d" it refuses "12abc".
func parseDigits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*decimalBase + int(r-'0')
	}
	return n, true
}

// recomputeRootBody rebuilds col's root metadata so highest_version names
// the highest version. Call it with s.mu held and col.sortedVersions current;
// collectionsBase already carries the base path and API root.
func recomputeRootBody(col *fakeCollection, collectionsBase string) {
	highest := col.sortedVersions[len(col.sortedVersions)-1]

	var root types.GalaxyCollection
	root.Href = fmt.Sprintf("%s/collections/%s/%s/", collectionsBase, col.namespace, col.name)
	root.Namespace = col.namespace
	root.Name = col.name
	root.VersionsURL = fmt.Sprintf("%s/collections/%s/%s/versions/", collectionsBase, col.namespace, col.name)
	root.HighestVersion.Href = col.versions[highest].href
	root.HighestVersion.Version = highest

	body, err := json.Marshal(&root)
	if err != nil {
		// root is built entirely from strings; marshaling it cannot fail.
		panic(fmt.Sprintf("fakegalaxy: marshal root metadata: %v", err))
	}
	col.rootBody = body
}

// buildVersionDetailBody builds a version's detail document, deps set in
// both metadata.dependencies and manifest.collection_info.dependencies; it
// returns the live value because SignVersion mutates it later.
func buildVersionDetailBody(
	namespace, name, version, href, downloadURL, filename, sha string,
	size int,
	deps map[string]string,
) *types.GalaxyCollectionVersionInfo {
	var info types.GalaxyCollectionVersionInfo
	info.Version = version
	info.Href = href
	info.Name = name
	info.Namespace.Name = namespace
	info.DownloadURL = downloadURL
	info.Artifact.Filename = filename
	info.Artifact.Sha256 = sha
	info.Artifact.Size = int64(size)
	info.Metadata.Dependencies = deps
	info.Manifest.CollectionInfo.Namespace = namespace
	info.Manifest.CollectionInfo.Name = name
	info.Manifest.CollectionInfo.Version = version
	info.Manifest.CollectionInfo.Dependencies = deps

	return &info
}

// marshalVersionDetail renders info as the body handleVersionDetail serves,
// the one marshal AddVersion and SignVersion share.
func marshalVersionDetail(info *types.GalaxyCollectionVersionInfo) []byte {
	body, err := json.Marshal(info)
	if err != nil {
		// Strings, an int64 and string maps cannot fail to marshal.
		panic(fmt.Sprintf("fakegalaxy: marshal version detail: %v", err))
	}
	return body
}

// buildArtifact returns a deterministic collection tar.gz whose MANIFEST.json,
// FILES.json and README.md form a closed digest chain manifest.VerifyChain
// accepts, plus its sha256 hex and the MANIFEST.json bytes.
func buildArtifact(namespace, name, version string, deps map[string]string) ([]byte, string, []byte) {
	readme := []byte("# " + namespace + "." + name + "\n")
	filesJSON := buildFilesJSON(readme)
	manifest := buildManifestJSON(namespace, name, version, deps, sha256Hex(filesJSON))

	// strings.Builder accepts arbitrary bytes, so it serves as the sink.
	var sink strings.Builder
	gz := gzip.NewWriter(&sink)
	tw := tar.NewWriter(gz)

	// MANIFEST.json leads, as `ansible-galaxy collection build` writes it.
	writeTarFile(tw, "MANIFEST.json", manifest)
	writeTarFile(tw, filesManifestFileName, filesJSON)
	writeTarFile(tw, "README.md", readme)

	// An in-memory sink with sizes matching every header cannot fail to close.
	_ = tw.Close()
	_ = gz.Close()

	built := []byte(sink.String())
	return built, sha256Hex(built), manifest
}

// writeTarFile appends one regular file entry to tw with a fixed mode,
// owner, and mod time, so the resulting tar.gz - and therefore its sha256
// - never depends on the environment it was built in.
func writeTarFile(tw *tar.Writer, name string, content []byte) {
	header := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     int64(len(content)),
		Mode:     tarFileMode,
		ModTime:  fixedModTime,
	}
	// See buildArtifact: this in-memory pairing cannot fail.
	_ = tw.WriteHeader(header)
	_, _ = tw.Write(content)
}

// buildManifestJSON renders a generated artifact's MANIFEST.json through the
// psvmcc/hub wire type, with filesSHA, the FILES.json digest, recorded under
// file_manifest_file for manifest.VerifyChain's pointer check.
func buildManifestJSON(namespace, name, version string, deps map[string]string, filesSHA string) []byte {
	var manifest types.GalaxyCollectionVersionInfoManifest
	manifest.Format = 1
	manifest.CollectionInfo.Namespace = namespace
	manifest.CollectionInfo.Name = name
	manifest.CollectionInfo.Version = version
	manifest.CollectionInfo.Dependencies = deps
	manifest.FileManifestFile.Name = filesManifestFileName
	manifest.FileManifestFile.Ftype = filesEntryTypeFile
	manifest.FileManifestFile.Format = 1
	manifest.FileManifestFile.ChksumType = chksumTypeSHA256
	manifest.FileManifestFile.ChksumSha256 = filesSHA

	body, err := json.Marshal(&manifest)
	if err != nil {
		// manifest is built entirely from strings and an int; marshaling it
		// cannot fail.
		panic(fmt.Sprintf("fakegalaxy: marshal MANIFEST.json: %v", err))
	}
	return body
}

// appendRow appends a zero element to *target and returns a pointer to it,
// valid until the next append, so no caller spells the element type out;
// see buildFilesJSON for why that matters.
func appendRow[T any](target *[]T) *T {
	*target = append(*target, *new(T))
	return &(*target)[len(*target)-1]
}

// buildFilesJSON renders FILES.json: the "." directory row and README.md's
// real digest. Rows grow through appendRow: a respelled anonymous struct
// type must keep psvmcc/hub's field order, which fieldalignment -fix breaks.
func buildFilesJSON(readme []byte) []byte {
	var files types.GalaxyCollectionVersionInfoFiles
	files.Format = 1

	dir := appendRow(&files.Files)
	dir.Name = "."
	dir.Ftype = "dir"

	readmeRow := appendRow(&files.Files)
	readmeRow.Name = "README.md"
	readmeRow.Ftype = filesEntryTypeFile
	readmeRow.ChksumType = chksumTypeSHA256
	readmeRow.ChksumSha256 = sha256Hex(readme)

	body, err := json.Marshal(&files)
	if err != nil {
		// files is built entirely from strings, an int, and a sha256 hex
		// string; marshaling it cannot fail.
		panic(fmt.Sprintf("fakegalaxy: marshal FILES.json: %v", err))
	}
	return body
}

// sha256Hex returns data's sha256 digest as lowercase hex, the shape both
// FILES.json's own per-file digests and MANIFEST.json's pointer at
// FILES.json use.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// BuildArtifact returns the bytes and sha256 AddVersion would serve for the
// same identity, for doubles such as the collections suite's in-memory git
// client; it needs no testing.TB because it touches no server state.
func BuildArtifact(namespace, name, version string, deps map[string]string) ([]byte, string) {
	data, sha, _ := buildArtifact(namespace, name, version, deps)
	return data, sha
}
