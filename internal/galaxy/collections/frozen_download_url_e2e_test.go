package collections_test

// This file pins the frozen cold path a locked download_url opens: artifacts
// are fetched with no metadata request, a failing URL ends the run, and a URL
// off its server's origin or serving another collection is refused.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// lockThenGoCold locks f's requirements, then points f at an empty cache and
// install tree under --frozen with the server's counts reset, and returns the
// lockfile path.
func lockThenGoCold(t *testing.T, f *e2eFixture) string {
	t.Helper()
	if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	root := t.TempDir()
	f.cfg.CacheDir = filepath.Join(root, "cache")
	f.cfg.DownloadPath = filepath.Join(root, "install")
	f.downloadPath = f.cfg.DownloadPath
	f.cfg.Frozen = true
	f.server.ResetCounts()
	return lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
}

// frozenRun is one command a locked download_url serves on a cold cache.
type frozenRun struct {
	run  func(context.Context, *config.Config, *infra.Infra) error
	name string
}

// frozenRuns is install and warm, the two commands that fetch artifacts.
func frozenRuns() []frozenRun {
	return []frozenRun{{name: "install", run: collections.Start}, {name: "warm", run: collections.Warm}}
}

// TestFrozenColdRunFetchesOnlyTheLockedArtifacts pins the point of the
// locked download_url: a frozen install or warm on an empty cache asks the
// server for the two artifacts and nothing else.
func TestFrozenColdRunFetchesOnlyTheLockedArtifacts(t *testing.T) {
	t.Parallel()
	for _, tc := range frozenRuns() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newE2EFixture(t)
			lockThenGoCold(t, f)

			if err := tc.run(context.Background(), f.cfg, f.runtime); err != nil {
				t.Fatalf("frozen %s on a cold cache: %v", tc.name, err)
			}
			for _, ep := range []fakegalaxy.Endpoint{
				fakegalaxy.EndpointRootMetadata, fakegalaxy.EndpointVersionsList, fakegalaxy.EndpointVersionDetail,
			} {
				if got := f.server.Count(ep); got != 0 {
					t.Errorf("endpoint %d requested %d times, want 0: the lockfile named every download URL", ep, got)
				}
			}
			if got := f.server.Count(fakegalaxy.EndpointArtifact); got != 2 {
				t.Errorf("artifact requests = %d, want 2 (acme.app and acme.lib)", got)
			}
			assertArtifactFilePresent(t, f.cfg.CacheDir, f.cfg.Server, "acme-app-1.0.0.tar.gz")
			assertArtifactFilePresent(t, f.cfg.CacheDir, f.cfg.Server, "acme-lib-1.0.0.tar.gz")
		})
	}
}

// TestFrozenLockedDownloadURLFailureFailsTheRun pins that a locked URL the
// server no longer serves fails the install as a download failure, with no
// fallback to the version metadata the lockfile made unnecessary.
func TestFrozenLockedDownloadURLFailureFailsTheRun(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	lockThenGoCold(t, f)
	f.server.Fail(fakegalaxy.EndpointArtifact, "acme", "lib", fakegalaxy.Fault{Status: http.StatusNotFound, Count: -1})

	err := collections.Start(context.Background(), f.cfg, f.runtime)
	if !errors.Is(err, helpers.ErrDownloadFailed) {
		t.Fatalf("Start = %v, want errors.Is helpers.ErrDownloadFailed", err)
	}
	// A per-collection download failure stays behind the install headline.
	if got := exitcode.FromError(err); got != exitcode.ExitInstall {
		t.Errorf("exit code = %d, want %d", got, exitcode.ExitInstall)
	}
	if got := f.server.Count(fakegalaxy.EndpointVersionDetail) + f.server.Count(fakegalaxy.EndpointRootMetadata); got != 0 {
		t.Errorf("metadata requests = %d, want 0: a failed locked URL is not retried through the API", got)
	}
	assertPathAbsent(t, installPathFor(f.downloadPath, "lib"))
}

// editLockEntry rewrites the entry named name in the lockfile at lockPath,
// the way a change under review could, and saves the file.
func editLockEntry(t *testing.T, lockPath, name string, edit func(*lockfile.Entry)) {
	t.Helper()
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatalf("load the lockfile Lock wrote: %v", err)
	}
	for i := range lf.Collections {
		if lf.Collections[i].Name == name {
			edit(&lf.Collections[i])
		}
	}
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save the edited lockfile: %v", err)
	}
}

// assertArtifactFileAbsent fails the test if a cache entry for filename,
// scoped to source, exists directly under cacheDir.
func assertArtifactFileAbsent(t *testing.T, cacheDir, source, filename string) {
	t.Helper()
	path := filepath.Join(cacheDir, helpers.ArtifactKey(source, filename))
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no cached artifact at %s, stat error: %v", path, err)
	}
}

// TestFrozenRefusesALockedDownloadURLOffItsServer pins the origin guard: a
// download_url on another origin is refused as an invalid lockfile before
// any request is made.
func TestFrozenRefusesALockedDownloadURLOffItsServer(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	lockPath := lockThenGoCold(t, f)
	editLockEntry(t, lockPath, "acme.app", func(e *lockfile.Entry) {
		e.DownloadURL = "https://cdn.example.invalid/download/acme-app-1.0.0.tar.gz"
	})

	err := collections.Start(context.Background(), f.cfg, f.runtime)
	if !errors.Is(err, helpers.ErrLockfileInvalid) || !errors.Is(err, helpers.ErrDownloadURLOffServerOrigin) {
		t.Fatalf("Start = %v, want ErrLockfileInvalid and ErrDownloadURLOffServerOrigin", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitLock {
		t.Errorf("exit code = %d, want %d", got, exitcode.ExitLock)
	}
	if got := f.server.Total(); got != 0 {
		t.Errorf("requests = %d, want 0: the refusal comes before any fetch", got)
	}
}

// TestFrozenFetchesALockedURLWithoutTheFileName pins the proxy shape: a
// download_url on its server's origin whose path does not end in the file
// name serves a cold run, since the manifest, not the path, names the bytes.
func TestFrozenFetchesALockedURLWithoutTheFileName(t *testing.T) {
	t.Parallel()
	for _, tc := range frozenRuns() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newE2EFixture(t)
			lockPath := lockThenGoCold(t, f)
			data, _ := fakegalaxy.BuildArtifact("acme", "app", testVersion100, map[string]string{"acme.lib": ">=1.0.0"})
			proxyURL := f.server.AddTarball("galaxy/ansible/get/acme/app/"+testVersion100, data)
			editLockEntry(t, lockPath, "acme.app", func(e *lockfile.Entry) { e.DownloadURL = proxyURL })

			if err := tc.run(context.Background(), f.cfg, f.runtime); err != nil {
				t.Fatalf("frozen %s from %s: %v", tc.name, proxyURL, err)
			}
			if got := f.server.Count(fakegalaxy.EndpointTarball); got != 1 {
				t.Errorf("requests to the locked URL = %d, want 1", got)
			}
			assertArtifactFilePresent(t, f.cfg.CacheDir, f.cfg.Server, "acme-app-1.0.0.tar.gz")
		})
	}
}

// TestFrozenRefusesALockedURLServingAnotherCollection pins the cache-slot
// guard: a download_url on its server's origin serving another collection,
// pinned to those bytes or not, fails the run and leaves the slot empty.
func TestFrozenRefusesALockedURLServingAnotherCollection(t *testing.T) {
	t.Parallel()
	for _, tc := range frozenRuns() {
		for _, pinned := range []bool{true, false} {
			t.Run(tc.name+"/pinned="+strconv.FormatBool(pinned), func(t *testing.T) {
				t.Parallel()
				f := newE2EFixture(t)
				lockPath := lockThenGoCold(t, f)
				editLockEntry(t, lockPath, "acme.app", func(e *lockfile.Entry) {
					e.DownloadURL = f.libV1.DownloadURL
					e.SHA256 = ""
					if pinned {
						e.SHA256 = f.libV1.SHA256
					}
				})

				err := tc.run(context.Background(), f.cfg, f.runtime)
				if !errors.Is(err, helpers.ErrLockedArtifactIdentityMismatch) {
					t.Fatalf("frozen %s = %v, want errors.Is helpers.ErrLockedArtifactIdentityMismatch", tc.name, err)
				}
				if got := exitcode.FromError(err); got != exitcode.ExitIntegrity {
					t.Errorf("exit code = %d, want %d", got, exitcode.ExitIntegrity)
				}
				assertArtifactFileAbsent(t, f.cfg.CacheDir, f.cfg.Server, "acme-app-1.0.0.tar.gz")
				assertPathAbsent(t, installPathFor(f.downloadPath, "app"))
			})
		}
	}
}
