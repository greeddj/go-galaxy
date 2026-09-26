package collections

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// errTestAbsent stands in for the 404 a root walk ends with.
var errTestAbsent = errors.New("404 Not Found")

// hostCounter counts every request to one host, routed by the fake or not.
type hostCounter struct {
	base  http.RoundTripper
	host  string
	count atomic.Int32
}

func (c *hostCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == c.host {
		c.count.Add(1)
	}
	return c.base.RoundTrip(r)
}

// TestAbsentMemoKeysByServerAndCollection pins that a recorded 404 answers only
// for its own server and collection, and that a nil memo records nothing.
func TestAbsentMemoKeysByServerAndCollection(t *testing.T) {
	t.Parallel()
	notFound := errTestAbsent
	m := newAbsentMemo()
	m.record("https://a.example", "acme.one", notFound)

	if got := m.lookup("https://a.example", "acme.one"); !errors.Is(got, notFound) {
		t.Fatalf("lookup(a, acme.one) = %v, want the recorded 404", got)
	}
	if got := m.lookup("https://a.example", "acme.two"); got != nil {
		t.Fatalf("lookup(a, acme.two) = %v, want nil: another collection", got)
	}
	if got := m.lookup("https://b.example", "acme.one"); got != nil {
		t.Fatalf("lookup(b, acme.one) = %v, want nil: another server", got)
	}
	var none *absentMemo
	none.record("https://a.example", "acme.one", notFound)
	if got := none.lookup("https://a.example", "acme.one"); got != nil {
		t.Fatalf("nil memo lookup = %v, want nil", got)
	}
}

// TestResolveAsksAServerWithoutACollectionOnce pins that prewarm and the solve
// share one walk's 404s: server A, which has none of the roots, answers each
// root candidate once per collection, with dependencies and under --no-deps.
func TestResolveAsksAServerWithoutACollectionOnce(t *testing.T) {
	t.Parallel()
	for name, noDeps := range map[string]bool{"with dependencies": false, "no-deps": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srvA := fakegalaxy.New(t)
			srvB := fakegalaxy.New(t)
			const pins = 4
			roots := exactPinnedPrewarmRoots(srvB, "", pins)
			cfg := &config.Config{
				Servers: []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}},
				Server:  srvA.URL(), Workers: pins, NoDeps: noDeps,
			}
			hostA, err := url.Parse(srvA.URL())
			if err != nil {
				t.Fatalf("parse A: %v", err)
			}
			counter := &hostCounter{base: srvA.Client().Transport, host: hostA.Host}
			deps := newCollectionDeps(cfg, infra.New(noopPrinter{}, &http.Client{Transport: counter}), store.New())

			if _, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial); err != nil {
				t.Fatalf("resolveCollectionsInternal: %v", err)
			}
			candidates := len(rootMetadataURLCandidates(srvA.URL(), roots[0], newAPIRootMemo()))
			if got, want := int(counter.count.Load()), pins*candidates; got != want {
				t.Fatalf("requests to A = %d, want %d: one %d-candidate walk per collection", got, want, candidates)
			}
		})
	}
}

// TestResolveTrustsALive404OverAStaleCachedRoot pins that once a resolve met a
// server's live 404 for a collection, its later walks there do not rebind the
// collection to that server from a root document an earlier run cached.
func TestResolveTrustsALive404OverAStaleCachedRoot(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.AddVersion("acme", "one", "1.0.0", nil)
	srvB.AddVersion("acme", "one", "1.0.0", nil)
	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	roots := []collection{{Namespace: "acme", Name: "one", Constraint: ">=1.0.0"}}
	runtime := infra.New(noopPrinter{}, srvA.Client())
	st := store.New()

	first := newCollectionDeps(&config.Config{Servers: servers, Server: srvA.URL(), Workers: 1}, runtime, st)
	if resolved, _, err := resolveCollectionsInternal(context.Background(), first, roots, resolveNestedPartial); err != nil ||
		resolved["acme.one"].Source != srvA.URL() {
		t.Fatalf("first run: source %q, err %v; want A, which caches its root document", resolved["acme.one"].Source, err)
	}

	srvA.Fail(fakegalaxy.EndpointRootMetadata, "acme", "one", fakegalaxy.Fault{Status: http.StatusNotFound, Count: -1})
	second := newCollectionDeps(&config.Config{Servers: servers, Server: srvA.URL(), Workers: 1, Refresh: true}, runtime, st)
	resolved, _, err := resolveCollectionsInternal(context.Background(), second, roots, resolveNestedPartial)
	if err != nil || resolved["acme.one"].Source != srvB.URL() {
		t.Fatalf("second run: source %q, err %v; want B, since A answered 404 live", resolved["acme.one"].Source, err)
	}
}

// TestResolveNeverMemoizesAnAbort pins that only a 404 is remembered: a server
// that refused prewarm's walk aborts the solve's walk too, and the collection
// never falls through to the next server.
func TestResolveNeverMemoizesAnAbort(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.Fail(fakegalaxy.EndpointRootMetadata, "", "", fakegalaxy.Fault{Status: http.StatusBadRequest, Count: -1})
	roots := unpinnedPrewarmRoots(srvB, "", 2)
	cfg := &config.Config{
		Servers: []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}},
		Server:  srvA.URL(), Workers: 2,
	}
	deps := newCollectionDeps(cfg, infra.New(noopPrinter{}, srvA.Client()), store.New())

	_, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial)
	if !errors.Is(err, helpers.ErrGalaxyServerUnavailable) {
		t.Fatalf("err = %v, want A's refusal to abort the resolve", err)
	}
	if got := srvB.Count(fakegalaxy.EndpointRootMetadata); got != 0 {
		t.Fatalf("B root requests = %d, want 0: nothing may route around A's refusal", got)
	}
}
