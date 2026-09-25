package collections_test

// This file pins the frozen cold path a locked download_url opens: artifacts
// are fetched with no metadata request, a failing URL ends the run, and a URL
// off its server's own artifact is refused before any request is made.

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
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

// TestFrozenRefusesALockedDownloadURLOffItsServer pins the cache-slot guard:
// a download_url on another origin, or naming another artifact of the same
// server, is refused as an invalid lockfile before any request is made.
func TestFrozenRefusesALockedDownloadURLOffItsServer(t *testing.T) {
	t.Parallel()
	for name, downloadURL := range map[string]func(*e2eFixture) string{
		"another origin":   func(*e2eFixture) string { return "https://cdn.example.invalid/download/acme-app-1.0.0.tar.gz" },
		"another artifact": func(f *e2eFixture) string { return f.libV1.DownloadURL },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newE2EFixture(t)
			lockPath := lockThenGoCold(t, f)
			lf, err := lockfile.Load(lockPath)
			if err != nil {
				t.Fatalf("load the lockfile Lock wrote: %v", err)
			}
			for i := range lf.Collections {
				if lf.Collections[i].Name == "acme.app" {
					lf.Collections[i].DownloadURL = downloadURL(f)
				}
			}
			if err := lockfile.Save(lockPath, lf); err != nil {
				t.Fatalf("save the edited lockfile: %v", err)
			}

			err = collections.Start(context.Background(), f.cfg, f.runtime)
			if !errors.Is(err, helpers.ErrLockfileInvalid) || !errors.Is(err, helpers.ErrDownloadURLNotServerArtifact) {
				t.Fatalf("Start = %v, want ErrLockfileInvalid and ErrDownloadURLNotServerArtifact", err)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitLock {
				t.Errorf("exit code = %d, want %d", got, exitcode.ExitLock)
			}
			if got := f.server.Total(); got != 0 {
				t.Errorf("requests = %d, want 0: the refusal comes before any fetch", got)
			}
		})
	}
}
