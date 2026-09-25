package cache

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// webUIPage is what galaxy.ansible.com serves with 200 at a /v3 path its
// API does not own: the web UI's HTML shell.
const webUIPage = "<!doctype html><html><head><title>Galaxy</title></head><body></body></html>"

// classifyFixtureSecret rides in a fixture URL's userinfo and query, so a
// substring search for it in a rendered message is an answer.
const classifyFixtureSecret = "s3cr3t-must-not-be-rendered"

// errTestMarkupText is an error whose text reads as markup, which IsWebPage
// must still refuse: it judges a response body, never a message.
var errTestMarkupText = errors.New("<!doctype html>")

// unreachableBase is a host no fixture ever dials: the refusing clients below
// fail the dial themselves, so no port is freed and possibly reused.
const unreachableBase = "http://galaxy.invalid"

// newBodyServer serves body with status 200 and contentType on every path,
// counting requests.
func newBodyServer(t *testing.T, contentType, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("ETag", "v2")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// refusingClient is a client with no proxy whose every dial fails with
// dialErr, the shape net.Dialer gives a refused connection or a failed lookup.
func refusingClient(dialErr error) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: dialErr}
		},
	}}
}

// TestMetadataHTMLBodyIsNotJSONAndIsNeverCached pins that a 200 whose body is
// a web page is helpers.ErrMetadataNotJSON marked as a web page, keeps the
// *json.SyntaxError, names the URL cut, and leaves no API cache entry behind.
func TestMetadataHTMLBodyIsNotJSONAndIsNeverCached(t *testing.T) {
	t.Parallel()
	srv, requests := newBodyServer(t, "text/html", webUIPage)
	raw := srv.URL + "/v3/collections/ns/x/?token=" + classifyFixtureSecret

	for _, tt := range []struct {
		st   *store.Store
		name string
	}{
		{name: "cached policy", st: store.New()},
		{name: "no store", st: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out map[string]any
			policy := Policy{Read: true, Write: true, TTL: time.Minute}
			err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), raw, tt.st, &out, policy, 0)
			if !errors.Is(err, helpers.ErrMetadataNotJSON) || !IsWebPage(err) {
				t.Fatalf("err = %v, want helpers.ErrMetadataNotJSON marked as a web page", err)
			}
			if _, ok := errors.AsType[*json.SyntaxError](err); !ok {
				t.Errorf("err = %v, want the *json.SyntaxError still reachable", err)
			}
			if strings.Contains(err.Error(), classifyFixtureSecret) {
				t.Errorf("err names the URL's query: %v", err)
			}
			if !strings.Contains(err.Error(), "/v3/collections/ns/x/") {
				t.Errorf("err does not name the URL it fetched: %v", err)
			}
			if tt.st != nil {
				if _, ok := tt.st.GetAPICache(apiCacheKey(raw)); ok {
					t.Error("a body that did not decode was stored in the API cache")
				}
			}
		})
	}
	t.Cleanup(func() {
		if got := requests.Load(); got != 2 {
			t.Errorf("requests = %d, want 2 (one per subtest, a decode failure is not retried)", got)
		}
	})
}

// TestMetadataNotJSONMarksOnlyMarkupAsAWebPage pins that every body that is no
// JSON is helpers.ErrMetadataNotJSON, but only markup is a web page: a cut-off
// or empty body from a real API root must never read as that root's absence.
func TestMetadataNotJSONMarksOnlyMarkupAsAWebPage(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		body        string
		wantWebPage bool
	}{
		{name: "html", body: webUIPage, wantWebPage: true},
		{name: "html after whitespace", body: "\r\n  \t" + webUIPage, wantWebPage: true},
		{name: "xml", body: `<?xml version="1.0"?><error/>`, wantWebPage: true},
		{name: "truncated json", body: `{"highest_version":{"href":"/api/v3/`},
		{name: "empty body", body: ""},
		{name: "plain text", body: "Service Unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, _ := newBodyServer(t, "text/plain", tt.body)
			st := store.New()
			var out map[string]any
			policy := Policy{Read: true, Write: true, TTL: time.Minute}
			err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), srv.URL, st, &out, policy, 0)
			if !errors.Is(err, helpers.ErrMetadataNotJSON) {
				t.Fatalf("err = %v, want errors.Is helpers.ErrMetadataNotJSON", err)
			}
			if got := IsWebPage(err); got != tt.wantWebPage {
				t.Errorf("IsWebPage(%v) = %t, want %t", err, got, tt.wantWebPage)
			}
			if _, ok := st.GetAPICache(apiCacheKey(srv.URL)); ok {
				t.Error("a body that did not decode was stored in the API cache")
			}
		})
	}
}

// TestIsWebPageRefusesOtherErrors pins that IsWebPage answers no for every
// error but a markup body, a 404 and a wrapped sentinel included.
func TestIsWebPageRefusesOtherErrors(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		nil,
		helpers.ErrMetadataNotJSON,
		&HTTPStatusError{Status: "404 Not Found", Code: http.StatusNotFound},
		errTestMarkupText,
	} {
		if IsWebPage(err) {
			t.Errorf("IsWebPage(%v) = true, want false", err)
		}
	}
}

// TestMetadataWrongShapeJSONIsNotStoredOrNotJSON pins that valid JSON of the
// wrong shape fails as before, not as helpers.ErrMetadataNotJSON, and is not
// stored either: only a body that decoded is kept.
func TestMetadataWrongShapeJSONIsNotStoredOrNotJSON(t *testing.T) {
	t.Parallel()
	srv, _ := newBodyServer(t, "application/json", `[1,2,3]`)
	st := store.New()

	var out map[string]any
	policy := Policy{Read: true, Write: true, TTL: time.Minute}
	err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), srv.URL, st, &out, policy, 0)
	if err == nil {
		t.Fatal("an array decoded into a map, so this fixture proves nothing")
	}
	if errors.Is(err, helpers.ErrMetadataNotJSON) || IsWebPage(err) {
		t.Errorf("err = %v: a well-formed document is not ErrMetadataNotJSON", err)
	}
	if _, ok := st.GetAPICache(apiCacheKey(srv.URL)); ok {
		t.Error("a body that did not decode was stored in the API cache")
	}
}

// TestMetadataRevalidationKeepsTheEntryOnANonJSONAnswer pins that an expired
// entry whose revalidation answers HTML with 200 fails as ErrMetadataNotJSON
// and keeps the earlier entry, never the HTML.
func TestMetadataRevalidationKeepsTheEntryOnANonJSONAnswer(t *testing.T) {
	t.Parallel()
	srv, requests := newBodyServer(t, "text/html", webUIPage)
	st := store.New()
	key := apiCacheKey(srv.URL)
	st.SetAPICache(key, store.APICacheEntry{
		URL:       srv.URL,
		Body:      []byte(`{"ok":true}`),
		ETag:      "v1",
		FetchedAt: time.Now().Add(-time.Hour).UTC(),
		TTL:       time.Minute,
	})

	var out map[string]any
	policy := Policy{Read: true, Write: true, TTL: time.Minute}
	err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), srv.URL, st, &out, policy, 0)
	if !errors.Is(err, helpers.ErrMetadataNotJSON) {
		t.Fatalf("err = %v, want errors.Is helpers.ErrMetadataNotJSON", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 (the expired entry was revalidated once)", got)
	}
	entry, ok := st.GetAPICache(key)
	if !ok || string(entry.Body) != `{"ok":true}` || entry.ETag != "v1" {
		t.Fatalf("entry = %+v (present %t), want the earlier JSON entry untouched", entry, ok)
	}
}

// TestMetadataUnreachableServerIsServerUnavailable pins that a refused dial and
// a failed lookup are helpers.ErrGalaxyServerUnavailable with the *url.Error
// still reachable and the URL's credentials still cut by CutTransportURL.
func TestMetadataUnreachableServerIsServerUnavailable(t *testing.T) {
	t.Parallel()
	raw := strings.Replace(unreachableBase, "http://", "http://u:"+classifyFixtureSecret+"@", 1) +
		"/api/v3/collections/ns/x/?sig=" + classifyFixtureSecret

	for _, tt := range []struct {
		dialErr error
		name    string
	}{
		{name: "connection refused", dialErr: syscall.ECONNREFUSED},
		{name: "no such host", dialErr: &net.DNSError{Err: "no such host", Name: "galaxy.invalid", IsNotFound: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out map[string]any
			policy := Policy{Read: true, Write: true, TTL: time.Minute}
			err := FetchJSONWithCachePolicy(context.Background(), refusingClient(tt.dialErr), raw, store.New(), &out, policy, 0)
			if !errors.Is(err, helpers.ErrGalaxyServerUnavailable) {
				t.Fatalf("err = %v, want errors.Is helpers.ErrGalaxyServerUnavailable", err)
			}
			if _, ok := errors.AsType[*url.Error](err); !ok {
				t.Errorf("err = %v, want the *url.Error still reachable", err)
			}
			if !errors.Is(err, tt.dialErr) {
				t.Errorf("err = %v, want the dial's own cause %v still reachable", err, tt.dialErr)
			}
			if strings.Contains(err.Error(), classifyFixtureSecret) {
				t.Errorf("err names the URL's credentials: %v", err)
			}
			if !strings.Contains(err.Error(), "/api/v3/collections/ns/x/") {
				t.Errorf("err does not name the URL it failed to reach: %v", err)
			}
		})
	}
}

// TestMetadataCanceledContextStaysCanceled pins that the caller's own Ctrl-C
// is context.Canceled alone, never relabeled as an unreachable server.
func TestMetadataCanceledContextStaysCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out map[string]any
	err := FetchJSONWithCachePolicy(ctx, refusingClient(syscall.ECONNREFUSED), unreachableBase+"/api/v3/", nil, &out, Policy{}, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is context.Canceled", err)
	}
	if errors.Is(err, helpers.ErrGalaxyServerUnavailable) {
		t.Fatalf("err = %v: a canceled caller is not an unreachable server", err)
	}
}

// TestMetadataCallerDeadlineIsLeftToTheCaller pins that a fetch ended by the
// caller's own deadline, as the versions pager's shared budget ends one, is
// neither relabeled as an unreachable server nor claimed as its own deadline.
func TestMetadataCallerDeadlineIsLeftToTheCaller(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), metadataDripBudget)
	defer cancel()

	var out map[string]any
	err := FetchJSONWithCachePolicy(ctx, srv.Client(), srv.URL, nil, &out, Policy{}, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want errors.Is context.DeadlineExceeded", err)
	}
	if errors.Is(err, helpers.ErrGalaxyServerUnavailable) || errors.Is(err, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("err = %v: the caller's deadline is the caller's to classify", err)
	}
}

// TestMetadataDeadlineStaysTheFetchDeadline pins that a server answering no
// headers within the budget is helpers.ErrMetadataFetchDeadline alone, never
// relabeled as an unreachable server.
func TestMetadataDeadlineStaysTheFetchDeadline(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), srv.URL, nil, &out, Policy{}, metadataDripBudget)
	if !errors.Is(err, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("err = %v, want errors.Is helpers.ErrMetadataFetchDeadline", err)
	}
	if errors.Is(err, helpers.ErrGalaxyServerUnavailable) {
		t.Fatalf("err = %v: the request's own deadline is not an unreachable server", err)
	}
}

// TestMetadataOfflineRefusalStaysOffline pins that --offline's refusal, which
// client.Do hands back inside a *url.Error and so as a net.Error, is never
// relabeled as an unreachable server.
func TestMetadataOfflineRefusalStaysOffline(t *testing.T) {
	t.Parallel()
	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), fetch.NewOffline(time.Second), unreachableBase+"/api/v3/", nil, &out, Policy{}, 0)
	if !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("err = %v, want errors.Is helpers.ErrOfflineMode", err)
	}
	if errors.Is(err, helpers.ErrGalaxyServerUnavailable) {
		t.Fatalf("err = %v: an offline refusal is not an unreachable server", err)
	}
}
