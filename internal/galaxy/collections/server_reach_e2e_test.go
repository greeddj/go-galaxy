package collections_test

// End-to-end tests of how the server walk classifies a server it cannot use:
// one serving its web UI beside a real 404, one serving only web pages, and
// one that cannot be reached at all.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// galaxyWebUIPage is what galaxy.ansible.com answers with 200 at a /v3 or /v2
// path its API does not own: the web UI's HTML shell.
const galaxyWebUIPage = "<!doctype html><html><head><title>Galaxy</title></head><body></body></html>"

// webUIServerShape is how a newWebUIServer answers a path that is not its
// login page.
type webUIServerShape int

const (
	// webUIBeside404 answers a JSON 404 under /api/ and the web UI elsewhere,
	// as galaxy.ansible.com does for a collection or role it lacks.
	webUIBeside404 webUIServerShape = iota
	// webUIEverywhere answers the web UI page on every path.
	webUIEverywhere
	// webUISSOFront redirects every path to /login, which answers the web UI
	// page, as a hub behind single sign-on does.
	webUISSOFront
)

// newWebUIServer serves shape's answers, counting nothing: the tests below
// judge it by what the walk does next.
func newWebUIServer(t *testing.T, shape webUIServerShape) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case shape == webUIBeside404 && strings.HasPrefix(r.URL.Path, "/api/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"status":"404","code":"not_found"}]}`))
			return
		case shape == webUISSOFront && r.URL.Path != "/login":
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(galaxyWebUIPage))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestUnknownCollectionBesideAWebUIIsUnknownPackage pins that a collection the
// server lacks resolves to the unknown-package path, ExitResolution, when the
// roots past its 404 answer the web UI page, as galaxy.ansible.com's /v3 does.
func TestUnknownCollectionBesideAWebUIIsUnknownPackage(t *testing.T) {
	t.Parallel()
	srv := newWebUIServer(t, webUIBeside404)
	servers := []config.Server{{ID: "a", URL: srv.URL}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.ghost"}}))

	err := collections.Start(context.Background(), cfg, multiServerRuntime(cfg))
	if _, ok := errors.AsType[*solver.ConflictError](err); !ok {
		t.Fatalf("expected a *solver.ConflictError, got %T: %v", err, err)
	}
	if errors.Is(err, helpers.ErrMetadataNotJSON) {
		t.Errorf("err = %v: a passed-over probe must not surface", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitResolution {
		t.Fatalf("exitcode.FromError(err) = %d, want ExitResolution (%d): %v", got, exitcode.ExitResolution, err)
	}
}

// TestServerOfWebPagesAloneIsNeverPassedOver pins that a first server with no
// 404 among its web pages aborts the walk, ExitNetwork, before the next server
// is asked: routing around an SSO front would hand the collection onward.
func TestServerOfWebPagesAloneIsNeverPassedOver(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		shape webUIServerShape
	}{
		{name: "web UI on every path", shape: webUIEverywhere},
		{name: "SSO redirect to a login page", shape: webUISSOFront},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srvA := newWebUIServer(t, tt.shape)
			srvB := fakegalaxy.New(t)
			srvB.AddVersion("ns", "x", "1.0.0", nil)

			servers := []config.Server{{ID: "hub", URL: srvA.URL}, {ID: "public", URL: srvB.URL()}}
			cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))

			err := collections.Start(context.Background(), cfg, multiServerRuntime(cfg))
			if !errors.Is(err, helpers.ErrMetadataNotJSON) {
				t.Fatalf("err = %v, want errors.Is helpers.ErrMetadataNotJSON", err)
			}
			if !strings.Contains(err.Error(), "server hub answers a web page at every API root") {
				t.Errorf("err = %v, want it to name server hub", err)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitNetwork {
				t.Fatalf("exitcode.FromError(err) = %d, want ExitNetwork (%d): %v", got, exitcode.ExitNetwork, err)
			}
			if got := srvB.Total(); got != 0 {
				t.Fatalf("srvB.Total() = %d, want 0 (web pages alone are no evidence of absence)", got)
			}
		})
	}
}

// TestServerWithA404BesideWebPagesIsPassedOver pins that a first server whose
// API root answers 404, its other roots the web UI, lets the walk move on to
// the next server, which then owns the collection.
func TestServerWithA404BesideWebPagesIsPassedOver(t *testing.T) {
	t.Parallel()
	srvA := newWebUIServer(t, webUIBeside404)
	srvB := fakegalaxy.New(t)
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "x")
	if e := findLockEntry(t, msLockFile(t, cfg, runtime), "ns.x"); e.Source != srvB.URL() {
		t.Fatalf("ns.x lockfile source = %q, want %q", e.Source, srvB.URL())
	}
}

// refusingHostRuntime is multiServerRuntime's client, minus server auth, with
// every dial to host failing as dialErr: an unreachable server without a real
// closed port another test could be handed meanwhile.
func refusingHostRuntime(host string, dialErr error) *infra.Infra {
	dialer := &net.Dialer{}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == host {
				return nil, &net.OpError{Op: "dial", Net: network, Err: dialErr}
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}}
	return infra.New(noopPrinter{}, client)
}

// TestUnreachableServerExitsNetwork pins that a server that refuses the dial
// or has no DNS record aborts the walk as helpers.ErrGalaxyServerUnavailable,
// ExitNetwork, and is never routed around to the next server.
func TestUnreachableServerExitsNetwork(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		dialErr error
		name    string
	}{
		{name: "connection refused", dialErr: syscall.ECONNREFUSED},
		{name: "no such host", dialErr: &net.DNSError{Err: "no such host", Name: "galaxy-a.invalid", IsNotFound: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srvB := fakegalaxy.New(t)
			srvB.AddVersion("ns", "x", "1.0.0", nil)

			servers := []config.Server{{ID: "a", URL: "http://galaxy-a.invalid"}, {ID: "b", URL: srvB.URL()}}
			cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))

			err := collections.Start(context.Background(), cfg, refusingHostRuntime("galaxy-a.invalid:80", tt.dialErr))
			if !errors.Is(err, helpers.ErrGalaxyServerUnavailable) {
				t.Fatalf("err = %v, want errors.Is helpers.ErrGalaxyServerUnavailable", err)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitNetwork {
				t.Fatalf("exitcode.FromError(err) = %d, want ExitNetwork (%d): %v", got, exitcode.ExitNetwork, err)
			}
			if got := srvB.Total(); got != 0 {
				t.Fatalf("srvB.Total() = %d, want 0 (an unreachable server is not evidence of absence)", got)
			}
		})
	}
}

// TestUnexpectedRootStatusExitsNetwork pins that an API root answering a
// status no rule routes around aborts the walk as ErrGalaxyServerUnavailable,
// ExitNetwork, and is never routed around to the next server.
func TestUnexpectedRootStatusExitsNetwork(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			t.Cleanup(srvA.Close)
			srvB := fakegalaxy.New(t)
			srvB.AddVersion("ns", "x", "1.0.0", nil)

			servers := []config.Server{{ID: "a", URL: srvA.URL}, {ID: "b", URL: srvB.URL()}}
			cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))

			err := collections.Start(context.Background(), cfg, multiServerRuntime(cfg))
			if !errors.Is(err, helpers.ErrGalaxyServerUnavailable) {
				t.Fatalf("err = %v, want errors.Is helpers.ErrGalaxyServerUnavailable", err)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitNetwork {
				t.Fatalf("exitcode.FromError(err) = %d, want ExitNetwork (%d): %v", got, exitcode.ExitNetwork, err)
			}
			if got := srvB.Total(); got != 0 {
				t.Fatalf("srvB.Total() = %d, want 0 (only a 404 routes around a server)", got)
			}
		})
	}
}

// TestGalaxyRoleWalkTreatsWebPagesAsTheCollectionWalkDoes pins the role side:
// a first server of web pages alone aborts before the next is asked, while
// one whose /api/v1 answers 404 beside them is routed around as without v1.
func TestGalaxyRoleWalkTreatsWebPagesAsTheCollectionWalkDoes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		shape   webUIServerShape
		wantErr bool
	}{
		{name: "web UI on every path", shape: webUIEverywhere, wantErr: true},
		{name: "SSO redirect to a login page", shape: webUISSOFront, wantErr: true},
		{name: "404 beside the web UI", shape: webUIBeside404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newGalaxyRoleFixture(t)
			hub := newWebUIServer(t, tt.shape)
			f.cfg.Servers = []config.Server{{ID: "hub", URL: hub.URL}, {ID: "galaxy", URL: f.galaxy.URL()}}
			f.cfg.Server = hub.URL
			f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")

			err := f.install(t)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Start: %v", err)
				}
				assertFileContains(t, filepath.Join(f.rolePath("geerlingguy.docker"), "COMMIT"), fakeCommit("docker-3"))
				return
			}
			if !errors.Is(err, helpers.ErrMetadataNotJSON) || errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable) {
				t.Fatalf("err = %v, want helpers.ErrMetadataNotJSON and not a server without v1", err)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitNetwork {
				t.Fatalf("exitcode.FromError(err) = %d, want ExitNetwork (%d): %v", got, exitcode.ExitNetwork, err)
			}
			if got := f.galaxy.Count(fakegalaxy.EndpointRoleLookup); got != 0 {
				t.Fatalf("role lookups on the next server = %d, want 0", got)
			}
		})
	}
}
