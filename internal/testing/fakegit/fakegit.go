// Package fakegit is an in-process git remote test double: smart-HTTP and ssh
// upload-pack over deterministic in-memory repositories, with fault injection.
// Constructors take testing.TB so no production code can reach it.
package fakegit

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// Endpoint identifies one of the exchanges the fake answers, for fault
// injection (Fail), request counting (Count) and auth capture (SeenAuth).
type Endpoint int

// The three endpoints: the two halves of the smart-HTTP exchange and the
// single ssh exec that carries both halves over one channel.
const (
	EndpointInfoRefs Endpoint = iota
	EndpointUploadPack
	EndpointSSHExec
)

// endpointCount is the number of distinct Endpoint values, sizing Server's
// per-endpoint arrays.
const endpointCount = 3

// Path pieces of the smart-HTTP routes: a repository lives at "/<name>.git",
// with "/info/refs?service=git-upload-pack" and "/git-upload-pack" beneath it.
const (
	repoSuffix     = ".git"
	infoRefsPath   = "/info/refs"
	uploadPackPath = "/" + uploadPackService
	serviceQuery   = "service"
)

// Content types the smart-HTTP protocol names for the two responses. go-git
// does not check either, but a real git client does.
const (
	advertisementContentType = "application/x-git-upload-pack-advertisement"
	resultContentType        = "application/x-git-upload-pack-result"
)

// Capabilities are the per-repository toggles of what the server advertises
// and honors. In the zero value a deepen is a bad request, never a silent full
// pack, and a want that is not an advertised tip is refused with an ERR line.
type Capabilities struct {
	Shallow            bool
	AllowReachableSHA1 bool
}

// Fault is a scripted failure for one Fail rule; Count works as in fakegalaxy
// and a zero Count never fires. Redirect, Status, Hang and StallAfterBytes win
// in that order; ssh honors only Hang, StallAfterBytes and ServeCommit.
type Fault struct {
	Redirect        string
	Status          int
	Count           int
	StallAfterBytes int
	ServeCommit     plumbing.Hash
	Hang            bool
}

// Server is the in-process git remote. It must only be constructed via New.
type Server struct {
	srv            *httptest.Server
	repos          map[string]*Repo
	caps           map[string]Capabilities
	ssh            *SSHServer
	tb             testing.TB
	baseURL        string
	requiredAuth   string
	faults         []faultRule
	authSeen       [endpointCount]capturedAuth
	mu             sync.Mutex
	counts         [endpointCount]int
	authFailStatus int
}

// capturedAuth is the Authorization header state one endpoint's most recent
// request carried: present distinguishes a request that carried no header
// at all from one that carried an empty value.
type capturedAuth struct {
	value   string
	present bool
}

// faultRule is one armed Fail call: it fires for requests to ep whose
// repository name matches (an empty name is a wildcard), consuming one Count
// per match.
type faultRule struct {
	name  string
	fault Fault
	ep    Endpoint
}

// New starts the HTTP half of the remote and registers its shutdown with
// tb.Cleanup; the ssh half starts lazily on the first SSH call. See the
// package comment for why the constructor takes testing.TB.
func New(tb testing.TB) *Server {
	tb.Helper()
	s := &Server{
		tb:    tb,
		repos: make(map[string]*Repo),
		caps:  make(map[string]Capabilities),
	}
	// The server is started before baseURL is known so ServeHTTP can be its
	// handler; no request can arrive before New returns s to the caller.
	srv := httptest.NewServer(s)
	tb.Cleanup(s.Close)
	s.srv = srv
	s.baseURL = srv.URL
	return s
}

// URL returns the HTTP base URL, e.g. "http://127.0.0.1:PORT".
func (s *Server) URL() string {
	return s.baseURL
}

// RepoURL returns the HTTP clone URL of the repository registered as name:
// URL()+"/"+name+".git".
func (s *Server) RepoURL(name string) string {
	return s.baseURL + "/" + name + repoSuffix
}

// Add registers r at "/<name>.git" on both transports, replacing any
// repository of the same name. Capabilities stay whatever SetCapabilities
// last set for name, the zero value otherwise.
func (s *Server) Add(name string, r *Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[name] = r
}

// SetCapabilities sets what the repository registered as name advertises
// and honors. It may be called before or after Add.
func (s *Server) SetCapabilities(name string, c Capabilities) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.caps[name] = c
}

// Fail arms a fault rule for requests to ep on the repository name, or on
// every repository when name is empty. Rules are scanned in registration order.
func (s *Server) Fail(ep Endpoint, name string, f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, faultRule{name: name, ep: ep, fault: f})
}

// RequireAuth demands an exact Authorization header on every HTTP request; ""
// means anonymous. The check runs after auth capture and armed faults, so a
// redirect or hang is observed even unauthenticated; ssh ignores it.
func (s *Server) RequireAuth(expected string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requiredAuth = expected
}

// AuthFailStatus overrides the status an HTTP auth failure answers with,
// e.g. http.StatusForbidden in place of the default http.StatusUnauthorized.
func (s *Server) AuthFailStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authFailStatus = status
}

// SeenAuth reports what ep's latest request authenticated with: the HTTP
// Authorization header and its presence, or over ssh the accepted key's SHA256
// fingerprint. It reports ("", false) before any request.
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
// and captured auth are unaffected.
func (s *Server) ResetCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts = [endpointCount]int{}
}

// Close stops both transports. It is registered with tb.Cleanup by New and
// is safe to call again.
func (s *Server) Close() {
	s.mu.Lock()
	sshSrv := s.ssh
	s.mu.Unlock()
	if sshSrv != nil {
		sshSrv.close()
	}
	s.srv.Close()
}

// ServeHTTP routes info/refs and git-upload-pack; anything else, receive-pack
// included, is 404. A request is counted and its auth captured before faults
// and the auth check run, so Count and SeenAuth reflect every request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, infoRefsPath):
		name, ok := repoName(strings.TrimSuffix(r.URL.Path, infoRefsPath))
		if !ok || r.URL.Query().Get(serviceQuery) != uploadPackService {
			http.NotFound(w, r)
			return
		}
		s.handleInfoRefs(w, r, name)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, uploadPackPath):
		name, ok := repoName(strings.TrimSuffix(r.URL.Path, uploadPackPath))
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.handleUploadPack(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

// handleInfoRefs answers the advertisement for name.
func (s *Server) handleInfoRefs(w http.ResponseWriter, r *http.Request, name string) {
	s.incr(EndpointInfoRefs)
	s.captureHeader(r, EndpointInfoRefs)
	fault, matched := s.consumeFault(EndpointInfoRefs, name)
	if matched && s.enactHTTPFault(w, r, fault) {
		return
	}
	if s.rejectAuth(w, r) {
		return
	}
	repo, caps, ok := s.lookup(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", advertisementContentType)
	w.WriteHeader(http.StatusOK)
	if err := advertise(w, repo, caps, true); err != nil {
		s.tb.Errorf("fakegit: advertise %s: %v", name, err)
	}
}

// handleUploadPack answers one upload-pack request for name. The body is read
// first: net/http notices a client going away only once the body is consumed,
// so a Hang over an unread body would hold the server open.
func (s *Server) handleUploadPack(w http.ResponseWriter, r *http.Request, name string) {
	s.incr(EndpointUploadPack)
	s.captureHeader(r, EndpointUploadPack)
	body, readErr := io.ReadAll(r.Body)
	fault, matched := s.consumeFault(EndpointUploadPack, name)
	if matched && s.enactHTTPFault(w, r, fault) {
		return
	}
	if s.rejectAuth(w, r) {
		return
	}
	repo, caps, ok := s.lookup(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if readErr != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	req, closed, err := readUploadRequest(bytes.NewReader(body))
	if err != nil || closed {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	reply, err := buildReply(repo, caps, req, fault)
	if err != nil {
		s.tb.Errorf("fakegit: build reply for %s: %v", name, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if reply.badRequest != "" {
		http.Error(w, reply.badRequest, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", resultContentType)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeReply(w, flusher, r.Context().Done(), reply, fault.StallAfterBytes)
}

// lookup returns the repository registered as name and its capabilities.
func (s *Server) lookup(name string) (*Repo, Capabilities, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repo, ok := s.repos[name]
	return repo, s.caps[name], ok
}

// incr counts one request to ep.
func (s *Server) incr(ep Endpoint) {
	s.mu.Lock()
	s.counts[ep]++
	s.mu.Unlock()
}

// captureHeader records the Authorization header r carried for ep.
func (s *Server) captureHeader(r *http.Request, ep Endpoint) {
	value, present := "", false
	if values, ok := r.Header["Authorization"]; ok {
		present = true
		if len(values) > 0 {
			value = values[0]
		}
	}
	s.recordAuth(ep, value, present)
}

// recordAuth stores what ep's latest request authenticated with.
func (s *Server) recordAuth(ep Endpoint, value string, present bool) {
	s.mu.Lock()
	s.authSeen[ep] = capturedAuth{value: value, present: present}
	s.mu.Unlock()
}

// rejectAuth enforces RequireAuth on r: it writes the failure status and
// reports true when a required header is missing or differs, and reports
// false, writing nothing, when no auth is required or it matches.
func (s *Server) rejectAuth(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	required := s.requiredAuth
	failStatus := s.authFailStatus
	s.mu.Unlock()
	if required == "" {
		return false
	}
	if values, ok := r.Header["Authorization"]; ok && len(values) > 0 && values[0] == required {
		return false
	}
	if failStatus == 0 {
		failStatus = http.StatusUnauthorized
	}
	w.WriteHeader(failStatus)
	return true
}

// consumeFault scans armed rules in registration order for the first one
// matching ep/name whose Count is not exhausted, consumes one use of it
// (unless Count is negative) and returns it.
func (s *Server) consumeFault(ep Endpoint, name string) (Fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.faults {
		rule := &s.faults[i]
		if rule.ep != ep || rule.fault.Count == 0 || (rule.name != "" && rule.name != name) {
			continue
		}
		if rule.fault.Count > 0 {
			rule.fault.Count--
		}
		return rule.fault, true
	}
	return Fault{}, false
}

// enactHTTPFault enacts Redirect, Status or Hang, in that precedence, and
// reports whether it answered; StallAfterBytes and ServeCommit need the pack
// and are left to the upload-pack handler.
func (s *Server) enactHTTPFault(w http.ResponseWriter, r *http.Request, fault Fault) bool {
	switch {
	case fault.Redirect != "":
		w.Header().Set("Location", fault.Redirect)
		w.WriteHeader(http.StatusFound)
		return true
	case fault.Status != 0:
		w.WriteHeader(fault.Status)
		return true
	case fault.Hang:
		<-r.Context().Done()
		return true
	}
	return false
}

// writeReply writes reply to w: the header, then - when stallAfter is
// positive - only that many pack bytes followed by a flush and a block until
// done is closed, otherwise the whole pack. flusher may be nil.
func writeReply(w io.Writer, flusher http.Flusher, done <-chan struct{}, reply uploadPackReply, stallAfter int) {
	if _, err := w.Write(reply.header); err != nil {
		return
	}
	if stallAfter > 0 {
		n := min(stallAfter, len(reply.pack))
		if _, err := w.Write(reply.pack[:n]); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		<-done
		return
	}
	_, _ = w.Write(reply.pack)
}

// repoName extracts name from a path of the form "/<name>.git". A name with
// a further slash or an empty name is refused, so a repository can only be
// addressed at the top level it was registered at.
func repoName(p string) (string, bool) {
	if !strings.HasPrefix(p, "/") || !strings.HasSuffix(p, repoSuffix) {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(p, "/"), repoSuffix)
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}
