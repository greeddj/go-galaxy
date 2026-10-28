package collections

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// urlRoleScenario is one url role served by a fake origin: deps with a real
// url client, a git artifact store and an observable temp directory.
type urlRoleScenario struct {
	server  *fakegalaxy.Server
	printer *capturingPrinter
	deps    installDeps
	url     string
	sha     string
	tempDir string
}

// newURLRoleScenario serves a role archive whose meta/main.yml is meta at a
// release-asset path.
func newURLRoleScenario(t *testing.T, meta string) *urlRoleScenario {
	t.Helper()
	server := fakegalaxy.New(t)
	data := buildTarGz(t, []tarEntry{
		{name: "meta/main.yml", body: []byte(meta)},
		{name: "tasks/main.yml", body: []byte("- name: hello\n  debug:\n    msg: hi\n")},
	})
	deps, printer := newRefetchDeps(t, nil)
	deps.runtime.URLHTTP = fetch.NewURLDownload(time.Minute, false, nil)
	tempDir := t.TempDir()
	deps.runtime.TempDir = func() string { return tempDir }
	deps.gitStore = local.NewArtifacts(deps.cfg.CacheDir)
	return &urlRoleScenario{
		server:  server,
		printer: printer,
		deps:    deps,
		url:     server.AddTarball("releases/web-1.0.0.tar.gz", data),
		sha:     sha256Hex(data),
		tempDir: tempDir,
	}
}

// role is the resolved role pinned to the served bytes.
func (s *urlRoleScenario) role() resolvedRole {
	return resolvedRole{
		Name: "web", Version: s.sha[:urlRoleVersionLen], Kind: typeURL,
		Source: urlsource.Locator{URL: s.url, SHA256: s.sha}.String(),
	}
}

// assertNoTempFiles fails the test when the extraction left anything in the
// temp directory or a download left a temp file in the artifact store.
func (s *urlRoleScenario) assertNoTempFiles(t *testing.T) {
	t.Helper()
	for _, dir := range []string{s.tempDir, s.deps.cfg.CacheDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if dir == s.tempDir || strings.HasPrefix(e.Name(), helpers.ArtifactDownloadTempPrefix) {
				t.Fatalf("%s holds %s, want no temp file left", dir, e.Name())
			}
		}
	}
}

type urlRoleRefusalCase struct {
	wantErr  error
	source   func(s *urlRoleScenario) string
	name     string
	meta     string
	requests int
	gone     bool
}

// urlRoleRefusalCases are the refetch's refusals with the tarball requests
// each costs; gone arms a 404 on the served tarball.
func urlRoleRefusalCases() []urlRoleRefusalCase {
	return []urlRoleRefusalCase{
		{
			name:    "torn locator",
			meta:    "dependencies: []\n",
			source:  func(*urlRoleScenario) string { return urlsource.LocatorPrefix + "x" },
			wantErr: helpers.ErrInvalidURLLocator,
		},
		{
			name:    "locator without a sha256",
			meta:    "dependencies: []\n",
			source:  func(s *urlRoleScenario) string { return urlsource.Locator{URL: s.url}.String() },
			wantErr: helpers.ErrInvalidURLLocator,
		},
		{
			name:     "origin answers 404",
			meta:     "dependencies: []\n",
			source:   func(s *urlRoleScenario) string { return s.role().Source },
			gone:     true,
			wantErr:  helpers.ErrDownloadFailed,
			requests: 1,
		},
		{
			name:     "pinned bytes are not a role",
			meta:     "- not a mapping\n",
			source:   func(s *urlRoleScenario) string { return s.role().Source },
			wantErr:  helpers.ErrRoleMetaInvalid,
			requests: 1,
		},
	}
}

// TestURLRoleFetchToCacheRefusals pins the url role refetch's refusals: a
// torn or unpinned locator, a 404, and pinned bytes that are no role, each
// releasing its download.
func TestURLRoleFetchToCacheRefusals(t *testing.T) {
	t.Parallel()
	for _, tt := range urlRoleRefusalCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newURLRoleScenario(t, tt.meta)
			if tt.gone {
				s.server.Fail(fakegalaxy.EndpointTarball, "", "", fakegalaxy.Fault{Status: http.StatusNotFound, Count: 1})
			}
			r := s.role()
			r.Source = tt.source(s)

			got, err := urlRoleFetchToCache(context.Background(), s.deps, r, true)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("urlRoleFetchToCache() error = %v, want %v", err, tt.wantErr)
			}
			if got.Path != "" || got.Cleanup != nil {
				t.Fatalf("urlRoleFetchToCache() = %+v on a refusal, want the zero result", got)
			}
			if n := s.server.Count(fakegalaxy.EndpointTarball); n != tt.requests {
				t.Fatalf("tarball requests = %d, want %d", n, tt.requests)
			}
			s.assertNoTempFiles(t)
		})
	}
}

// TestURLRoleFetchToCacheArms pins the refetch past its checks: without the
// cache the repacked artifact is handed back, and a failed commit releases
// it.
func TestURLRoleFetchToCacheArms(t *testing.T) {
	t.Parallel()

	t.Run("no cache", func(t *testing.T) {
		t.Parallel()
		s := newURLRoleScenario(t, "dependencies: []\n")
		got, err := urlRoleFetchToCache(context.Background(), s.deps, s.role(), false)
		if err != nil {
			t.Fatalf("urlRoleFetchToCache() error = %v", err)
		}
		if got.SHA == s.sha || got.Cleanup == nil {
			t.Fatalf("urlRoleFetchToCache() = (%q, %q), want a repacked artifact, not the origin bytes", got.Path, got.SHA)
		}
		assertFileSHA256(t, got.Path, got.SHA)
		if _, err := os.Stat(filepath.Join(s.deps.cfg.CacheDir, roleArtifactKey(s.role()))); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("cache slot stat error = %v, want nothing committed", err)
		}
		got.Cleanup()
		s.assertNoTempFiles(t)
	})

	t.Run("commit fails", func(t *testing.T) {
		t.Parallel()
		s := newURLRoleScenario(t, "dependencies: []\n")
		s.deps.artifacts = local.NewArtifacts(filepath.Join(t.TempDir(), "absent"))
		if _, err := urlRoleFetchToCache(context.Background(), s.deps, s.role(), true); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("urlRoleFetchToCache() error = %v, want the store's fs.ErrNotExist", err)
		}
		s.assertNoTempFiles(t)
	})
}

// TestResolveURLRoleAcquiresAndRecordsThePin pins a fresh url role resolve:
// the build warning names the URL, the version is the sha prefix, and pin
// and artifact are recorded.
func TestResolveURLRoleAcquiresAndRecordsThePin(t *testing.T) {
	t.Parallel()
	s := newURLRoleScenario(t, "")
	req := requirements.RoleRequirement{Name: "web", Src: s.url, Type: requirements.TypeURL}

	pin, err := resolveURLRole(context.Background(), s.deps.collectionDeps, req)
	if err != nil {
		t.Fatalf("resolveURLRole() error = %v", err)
	}
	if pin.sha256 != s.sha || pin.version != s.sha[:urlRoleVersionLen] || pin.locator != s.role().Source {
		t.Fatalf("resolveURLRole() = %+v, want sha %s at version %s", pin, s.sha, s.sha[:urlRoleVersionLen])
	}
	if !s.printer.hasWarnContaining(s.url + ": meta file meta/main.yml is empty") {
		t.Fatalf("warnings = %v, want the empty-meta warning against the URL", s.printer.warns)
	}
	recorded, ok := s.deps.st.GetRolePin(urlRolePinKeyPrefix + s.url)
	recorded.FetchedAt = time.Time{}
	want := store.RolePinEntry{URL: s.url, SHA256: s.sha, Version: pin.version, Deps: []store.RolePinDep{}}
	if !ok || !reflect.DeepEqual(recorded, want) {
		t.Fatalf("recorded pin = (%+v, %v), want %+v", recorded, ok, want)
	}
	if !roleArtifactCached(context.Background(), s.deps.collectionDeps, pin.locator, "web", pin.version) {
		t.Fatal("the repacked artifact is not in the git artifact store")
	}
}

// TestResolveURLRoleRefusesTornInput pins that a requirement URL carrying
// credentials, and a recorded pin whose sha, version or URL is malformed, are
// refused before the origin is asked.
func TestResolveURLRoleRefusesTornInput(t *testing.T) {
	t.Parallel()
	s := newURLRoleScenario(t, "dependencies: []\n")
	req := requirements.RoleRequirement{Name: "web", Src: s.url, Type: requirements.TypeURL}

	userinfo := req
	userinfo.Src = strings.Replace(s.url, "http://", "http://u:p@", 1)
	if _, err := resolveURLRole(context.Background(), s.deps.collectionDeps, userinfo); !errors.Is(err, helpers.ErrURLRequirementUserinfo) {
		t.Fatalf("resolveURLRole(userinfo) error = %v, want ErrURLRequirementUserinfo", err)
	}

	pins := []struct {
		wantErr error
		name    string
		pin     store.RolePinEntry
	}{
		{name: "sha is not a digest", pin: store.RolePinEntry{URL: s.url, SHA256: "zz", Version: "1.0.0"}, wantErr: helpers.ErrInvalidURLLocator},
		{
			name:    "version is not a role version",
			pin:     store.RolePinEntry{URL: s.url, SHA256: s.sha, Version: "../x"},
			wantErr: helpers.ErrInvalidRoleVersion,
		},
		{
			name:    "url is another one",
			pin:     store.RolePinEntry{URL: s.url + "x", SHA256: s.sha, Version: "1.0.0"},
			wantErr: helpers.ErrInvalidURLLocator,
		},
	}
	for _, tt := range pins {
		s.deps.st.SetRolePin(urlRolePinKeyPrefix+s.url, tt.pin)
		if _, err := resolveURLRole(context.Background(), s.deps.collectionDeps, req); !errors.Is(err, tt.wantErr) {
			t.Errorf("%s: resolveURLRole() error = %v, want %v", tt.name, err, tt.wantErr)
		}
	}
	if n := s.server.Count(fakegalaxy.EndpointTarball); n != 0 {
		t.Fatalf("tarball requests = %d, want 0", n)
	}
}
