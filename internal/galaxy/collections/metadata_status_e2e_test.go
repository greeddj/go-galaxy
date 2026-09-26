package collections_test

// End-to-end tests of how a failed Galaxy metadata request in a resolve or lock
// exits: any status but 404 or a document of the wrong shape is the server
// failing, exit 4, and a 404 is the source lacking what was asked for, exit 3.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// statusCase is one metadata status and what a run meeting it must return.
type statusCase struct {
	want     error
	name     string
	status   int
	wantCode int
}

// notPublishedCase is a Galaxy collection's 404: the source lacks it, exit 3.
//
//nolint:gochecknoglobals // a fixed case shared by the tests below, not mutable shared state
var notPublishedCase = statusCase{
	name: "not found", status: http.StatusNotFound, want: helpers.ErrNoSemverCandidates, wantCode: exitcode.ExitResolution,
}

// metadataStatusCases are the answers every collection test here drives a
// document to: refused credentials and an outage exit 4, a 404 exits 3.
//
//nolint:gochecknoglobals // a fixed table shared by the tests below, not mutable shared state
var metadataStatusCases = []statusCase{
	{name: "unauthorized", status: http.StatusUnauthorized, want: helpers.ErrGalaxyAuthFailed, wantCode: exitcode.ExitNetwork},
	{name: "unavailable", status: http.StatusServiceUnavailable, want: helpers.ErrGalaxyServerUnavailable, wantCode: exitcode.ExitNetwork},
	notPublishedCase,
}

// newStatusFixture registers acme.name at versions on a fresh fake and writes
// a requirements.yml asking for acme.name at constraint.
func newStatusFixture(t *testing.T, name, constraint string, versions ...string) (*config.Config, *infra.Infra, *fakegalaxy.Server) {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	body := "collections:\n  - name: acme." + name + "\n    version: \"" + constraint + "\"\n"
	if err := os.WriteFile(reqPath, []byte(body), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
	s := fakegalaxy.New(t)
	for _, v := range versions {
		s.AddVersion("acme", name, v, nil)
	}
	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
	}
	return cfg, infra.New(noopPrinter{}, s.Client()), s
}

// assertMetadataExit fails unless err carries tt's sentinel and exits with
// tt's code, a class sentinel named once and a 404 read as unpublished.
func assertMetadataExit(t *testing.T, err error, tt statusCase, unpublished string) {
	t.Helper()
	if !errors.Is(err, tt.want) {
		t.Fatalf("err = %v, want errors.Is %v", err, tt.want)
	}
	if got := exitcode.FromError(err); got != tt.wantCode {
		t.Fatalf("exitcode.FromError(err) = %d, want %d: %v", got, tt.wantCode, err)
	}
	if errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("err = %v: a resolve-phase failure must not hide behind the install headline", err)
	}
	if tt.status != http.StatusNotFound {
		if got := strings.Count(err.Error(), tt.want.Error()); got != 1 {
			t.Errorf("%q names %q %d times, want once", err.Error(), tt.want.Error(), got)
		}
		return
	}
	if want := unpublished + " is not published at its server"; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to contain %q", err, want)
	}
}

// TestVersionsPageStatusExitCode pins the solver's versions list: a constraint
// that excludes highest_version sends the resolve to the versions page, whose
// failure is the server's (4) unless it is a 404 (3).
func TestVersionsPageStatusExitCode(t *testing.T) {
	t.Parallel()
	for _, tt := range metadataStatusCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, runtime, s := newStatusFixture(t, "pick", "<2.0.0", testVersion100, "2.0.0")
			s.Fail(fakegalaxy.EndpointVersionsList, "acme", "pick", fakegalaxy.Fault{Status: tt.status, Count: -1})

			err := collections.Start(context.Background(), cfg, runtime)
			assertMetadataExit(t, err, tt, "acme.pick")
			if s.Count(fakegalaxy.EndpointVersionsList) == 0 {
				t.Fatal("the versions page was never requested")
			}
		})
	}
}

// TestResolveVersionDocumentStatusExitCode pins the solver's dependency read:
// a version document failing mid-resolve is the server's (4) unless it is a
// 404 (3), as lock already read one.
func TestResolveVersionDocumentStatusExitCode(t *testing.T) {
	t.Parallel()
	for _, tt := range metadataStatusCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, runtime, s := newStatusFixture(t, "deps", "*", testVersion100)
			s.Fail(fakegalaxy.EndpointVersionDetail, "acme", "deps", fakegalaxy.Fault{Status: tt.status, Count: -1})

			err := collections.Start(context.Background(), cfg, runtime)
			assertMetadataExit(t, err, tt, "acme.deps "+testVersion100)
			if s.Count(fakegalaxy.EndpointVersionDetail) == 0 {
				t.Fatal("the version document was never requested")
			}
		})
	}
}

// TestLockVersionDocumentStatusExitCode pins lock's own pin read: under
// --no-deps the resolve asks for no version document, so lock's is the first,
// and it reads a status as the resolve does.
func TestLockVersionDocumentStatusExitCode(t *testing.T) {
	t.Parallel()
	for _, tt := range metadataStatusCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, runtime, s := newStatusFixture(t, "lk", "*", testVersion100)
			cfg.NoDeps = true
			s.Fail(fakegalaxy.EndpointVersionDetail, "acme", "lk", fakegalaxy.Fault{Status: tt.status, Count: -1})

			err := collections.Lock(context.Background(), cfg, runtime)
			assertMetadataExit(t, err, tt, "acme.lk "+testVersion100)
			if s.Count(fakegalaxy.EndpointVersionDetail) == 0 {
				t.Fatal("the version document was never requested")
			}
		})
	}
}

// TestExactPinnedUnknownCollectionExitsResolution pins that an exact pin the
// server does not have, which the solver takes straight to its dependency
// read, exits 3 as the same name unconstrained does, on one server or two.
func TestExactPinnedUnknownCollectionExitsResolution(t *testing.T) {
	t.Parallel()
	const requirements = "collections:\n  - name: acme.ghost\n    version: \"1.0.0\"\n"
	for _, tt := range []struct {
		name string
		ids  []string
	}{
		{name: "one server", ids: []string{"a"}},
		{name: "two servers", ids: []string{"a", "b"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			servers := make([]config.Server, 0, len(tt.ids))
			for _, id := range tt.ids {
				s := fakegalaxy.New(t)
				s.AddVersion("acme", "other", testVersion100, nil)
				servers = append(servers, config.Server{ID: id, URL: s.URL()})
			}
			cfg := newMultiServerConfig(t, servers, requirements)

			err := collections.Start(context.Background(), cfg, multiServerRuntime(cfg))
			assertMetadataExit(t, err, notPublishedCase, "acme.ghost 1.0.0")
		})
	}
}

// TestWrongShapeRootDocumentAbortsTheWalk pins that an API root answering JSON
// of the wrong shape, a timestamp that does not parse included, aborts the walk
// as helpers.ErrMetadataMalformed, exit 4: only a 404 or a web page is passed over.
func TestWrongShapeRootDocumentAbortsTheWalk(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, body string }{
		{name: "string for an object", body: `{"highest_version":"1.0.0"}`},
		{name: "timestamp that does not parse", body: `{"created_at":"yesterday"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srvA.Close)
			srvB := fakegalaxy.New(t)
			srvB.AddVersion("ns", "x", testVersion100, nil)

			servers := []config.Server{{ID: "a", URL: srvA.URL}, {ID: "b", URL: srvB.URL()}}
			cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))

			err := collections.Start(context.Background(), cfg, multiServerRuntime(cfg))
			if !errors.Is(err, helpers.ErrMetadataMalformed) || errors.Is(err, helpers.ErrMetadataNotJSON) {
				t.Fatalf("err = %v, want helpers.ErrMetadataMalformed and not helpers.ErrMetadataNotJSON", err)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitNetwork {
				t.Fatalf("exitcode.FromError(err) = %d, want ExitNetwork (%d): %v", got, exitcode.ExitNetwork, err)
			}
			if got := srvB.Total(); got != 0 {
				t.Fatalf("srvB.Total() = %d, want 0 (a document of the wrong shape is no evidence of absence)", got)
			}
		})
	}
}

// TestGalaxyRoleVersionsStatusExitCode pins the v1 versions pages of a role
// the first server found: a 404 is the role missing there (3) and any other
// status the server failing (4), neither passing the server over as without v1.
func TestGalaxyRoleVersionsStatusExitCode(t *testing.T) {
	t.Parallel()
	for _, tt := range []statusCase{
		{name: "not found", status: http.StatusNotFound, want: helpers.ErrRoleVersionNotFound, wantCode: exitcode.ExitResolution},
		{name: "unavailable", status: http.StatusServiceUnavailable, want: helpers.ErrGalaxyServerUnavailable, wantCode: exitcode.ExitNetwork},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newGalaxyRoleFixture(t)
			f.galaxy.Fail(fakegalaxy.EndpointRoleVersions, "geerlingguy", "docker", fakegalaxy.Fault{Status: tt.status, Count: -1})
			next := fakegalaxy.New(t)
			next.AddRole("geerlingguy", "docker", "geerlingguy", "ansible-role-docker", "master", []fakegalaxy.RoleVersion{{Name: "1.0.0"}})
			f.cfg.Servers = []config.Server{{ID: "galaxy", URL: f.galaxy.URL()}, {ID: "next", URL: next.URL()}}
			f.writeRequirements(t, "roles:\n  - geerlingguy.docker\n")

			err := f.install(t)
			if !errors.Is(err, tt.want) || errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable) {
				t.Fatalf("err = %v, want %v and not a server without v1", err, tt.want)
			}
			if got := exitcode.FromError(err); got != tt.wantCode {
				t.Fatalf("exitcode.FromError(err) = %d, want %d: %v", got, tt.wantCode, err)
			}
			if got := next.Total(); got != 0 {
				t.Fatalf("requests to the next server = %d, want 0 (the first server serves v1)", got)
			}
		})
	}
}
