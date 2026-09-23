package collections

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/cleanup"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestLockOnAColdCacheLeavesNoEvidenceForCleanupToActOn pins that a lock on a
// cold cache saves a snapshot cleanup must not read as evidence: an extracted
// tree that survived a schema bump outlives the following cleanup.
func TestLockOnAColdCacheLeavesNoEvidenceForCleanupToActOn(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))
	lockPath := filepath.Join(root, "galaxy.lock")

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)

	// A tree that survived the snapshot drop, named by a sha nothing in the
	// cold snapshot can possibly reference.
	const survivor = "sha-survived-the-schema-bump"
	survivorDir := filepath.Join(cacheDir, extracted.RootDirName, survivor)
	if err := os.MkdirAll(survivorDir, helpers.DirMod); err != nil {
		t.Fatalf("seed extracted tree: %v", err)
	}
	marker := filepath.Join(survivorDir, extracted.ReadyMarker)
	if err := os.WriteFile(marker, []byte(extracted.ReadyMarkerPayload), helpers.FileMod); err != nil {
		t.Fatalf("seed ready marker: %v", err)
	}

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		LockFile:         lockPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())

	if err := lockWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("lockWithState: %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected lock to have written its lockfile: %v", err)
	}

	// Without a recorded project cleanup returns before the sweep; this one
	// has no install tree, so the sweep runs with an empty keep set.
	if err := state.backend.RecordProject(context.Background(), reqPath, cfg.DownloadPath, ""); err != nil {
		t.Fatalf("RecordProject: %v", err)
	}

	// Bolt admits one holder at a time, so the lock run's backend is closed
	// before cleanup opens its own, as a real process exit would.
	if err := state.backend.Close(context.Background()); err != nil {
		t.Fatalf("closing the lock run's backend: %v", err)
	}

	if err := cleanup.Start(context.Background(), cfg, infra.New(noopPrinter{}, srv.Client())); err != nil {
		t.Fatalf("cleanup.Start: %v", err)
	}
	if _, err := os.Stat(survivorDir); err != nil {
		t.Fatalf("expected the extracted tree to survive a cleanup following a cold-cache lock, stat error: %v", err)
	}
}
