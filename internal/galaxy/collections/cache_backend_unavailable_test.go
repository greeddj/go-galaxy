package collections

// These tests pin that a SaveStore failure carrying ErrCacheBackendUnavailable
// survives installWithState: bare on a clean run, joined behind
// ErrInstallationFailed when a collection also failed.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// errCacheBackendUnavailableSaveFailure reproduces the wrap a dead S3 bucket's
// SaveStore failure carries, since internal/cache/s3's fake is unreachable
// from this package.
var errCacheBackendUnavailableSaveFailure = fmt.Errorf("%w: simulated dead bucket", helpers.ErrCacheBackendUnavailable)

// cacheBackendUnavailableSaveFailBackend wraps a real local backend and
// overrides only SaveStore to fail with errCacheBackendUnavailableSaveFailure.
type cacheBackendUnavailableSaveFailBackend struct {
	cacheManager.Backend
}

// SaveStore always fails with the cache-backend-unavailable shape,
// simulating a dead S3 bucket at the very last step of a run.
func (cacheBackendUnavailableSaveFailBackend) SaveStore(context.Context, *store.Store) error {
	return errCacheBackendUnavailableSaveFailure
}

// newCacheBackendUnavailableSaveFailState is newLocalState with its backend
// wrapped in cacheBackendUnavailableSaveFailBackend, so only SaveStore fails.
func newCacheBackendUnavailableSaveFailState(t *testing.T, cfg *config.Config) *installState {
	t.Helper()
	state := newLocalState(t, cfg)
	state.backend = cacheBackendUnavailableSaveFailBackend{Backend: state.backend}
	return state
}

// TestInstallWithStateCacheBackendUnavailableSaveFailureCarriesSentinelOnCleanRun
// asserts that a clean run returns the SaveStore failure bare, matching
// ErrCacheBackendUnavailable and not ErrInstallationFailed.
func TestInstallWithStateCacheBackendUnavailableSaveFailureCarriesSentinelOnCleanRun(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))
	metricsPath := filepath.Join(root, "metrics.json")

	cfg := &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
		Offline:          true,
	}
	state := newCacheBackendUnavailableSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, http.DefaultClient)

	err := installWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is helpers.ErrCacheBackendUnavailable, got %v", err)
	}
	// Contrast with the joined-run test below: a clean run's tail save
	// failure is never folded behind helpers.ErrInstallationFailed, since
	// annotateSaveFailure only runs when summary.count > 0.
	if errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected a clean run's bare save failure NOT to carry helpers.ErrInstallationFailed, got %v", err)
	}
}

// TestInstallWithStateCacheBackendUnavailableSaveFailureJoinsBehindInstallationFailed
// asserts that with a collection failure the save error joins behind
// ErrInstallationFailed, so exitcode still reports ExitInstall.
func TestInstallWithStateCacheBackendUnavailableSaveFailureJoinsBehindInstallationFailed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)
	srv.Fail(fakegalaxy.EndpointArtifact, "acme", "widgets", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
	}
	state := newCacheBackendUnavailableSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())

	err := installWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is helpers.ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is helpers.ErrCacheBackendUnavailable, got %v", err)
	}
}
