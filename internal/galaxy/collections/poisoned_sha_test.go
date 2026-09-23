package collections

// Tests that a traversal string posing as a sha256, from metadata or the
// snapshot, never reaches the filesystem calls markerRel guards. Victims sit
// inside the os.Root at DownloadPath so markerRel, not the root, refuses them.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
	"github.com/psvmcc/hub/pkg/types"
)

// TestInstallRejectsPoisonedMetadataSHAOnCacheHit pins that a traversal
// meta.Artifact.Sha256 on a cache hit fails as ErrMalformedArtifactSHA256 with
// no refetch and the cached artifact kept, never evicted over bad metadata.
func TestInstallRejectsPoisonedMetadataSHAOnCacheHit(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	version := srv.AddVersion("acme", "widgets", "1.0.0", nil)

	sandbox := t.TempDir()
	cacheDir := filepath.Join(sandbox, "cache")
	downloadPath := filepath.Join(sandbox, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	// Seeded so artifacts.Has reports true and prepareInstall takes the
	// cache-hit path; its actual content is irrelevant, since a rejected
	// meta.Artifact.Sha256 must fail before this file is ever read for real.
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	mustWriteFile(t, artifactPath, []byte("cached tarball bytes, irrelevant to this test"))

	// The canary for assertion (b): where the traversal sha would land, outside
	// the install directory but inside the root, as the file comment explains.
	victim := filepath.Join(downloadPath, "home", "ci", ".ssh", "authorized_keys")
	const victimContent = "ssh-ed25519 AAAA... ci@legit\n"
	mustMkdirAll(t, filepath.Dir(victim))
	mustWriteFile(t, victim, []byte(victimContent))

	cfg := &config.Config{
		Server:       srv.URL(),
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      false,
		NoCache:      false,
	}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	st := store.New()
	artifacts := local.NewArtifacts(cacheDir)
	root := newTestCollectionsRoot(t, downloadPath)
	deps := installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
		root:           root,
	}

	// A working DownloadURL, so an unguarded run's evict-and-refetch would
	// really reach the origin and trip the srv.Count assertion.
	metaOverride := &types.GalaxyCollectionVersionInfo{}
	metaOverride.DownloadURL = srv.URL() + "/download/" + fmt.Sprintf("%s-%s-%s.tar.gz", version.Namespace, version.Name, version.Version)
	metaOverride.Artifact.Sha256 = "../../../../../home/ci/.ssh/authorized_keys"

	err := installCollection(context.Background(), col, deps, nil, metaOverride, downloadResult{})
	if err == nil {
		t.Fatal("(a) expected installCollection to fail on a poisoned meta.Artifact.Sha256, got nil")
	}
	assertFileContent(t, victim, victimContent) // (b)
	if _, ok := st.GetInstalled(col.key()); ok {
		t.Fatal("(c) expected no installed entry: a poisoned sha must never reach the persisted snapshot")
	}
	if got := srv.Count(fakegalaxy.EndpointArtifact); got != 0 {
		t.Errorf("(d) expected zero download attempts (rejected before any refetch), got %d", got)
	}
	if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
		t.Errorf("(e) installCollection error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
	}
	// (f): a poisoned metadata entry must never cost a good cached artifact;
	// eviction cannot repair a metadata problem, so it would recur every run.
	if _, statErr := os.Stat(artifactPath); statErr != nil {
		t.Errorf("(f) expected the cached artifact to survive, stat error: %v", statErr)
	}
}

// TestCanSkipInstallRefusesPoisonedSnapshotSHA pins that a traversal
// ArtifactSHA256 in the snapshot makes canSkipInstall report false without
// touching the victim; the sidecar is seeded so only markerRel can refuse.
func TestCanSkipInstallRefusesPoisonedSnapshotSHA(t *testing.T) {
	t.Parallel()
	sandbox := t.TempDir()
	downloadPath := filepath.Join(sandbox, "install")

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	cfg := &config.Config{DownloadPath: downloadPath}
	target := newTestInstallTarget(t, cfg, col)
	mustMkdirAll(t, target.path)

	infoDir := filepath.Join(downloadPath, "ansible_collections", col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, infoDir)
	mustWriteFile(t, filepath.Join(infoDir, "GALAXY.yml"), sidecarFor(col))

	// installPath is three elements under downloadPath, so these ".." segments
	// land the victim back inside downloadPath, within this test's sandbox.
	const traversalSHA = "../../../../../home/ci/.ssh/authorized_keys"
	victim := filepath.Join(downloadPath, "home", "ci", ".ssh", "authorized_keys")
	const victimContent = "ssh-ed25519 AAAA... ci@legit\n"
	mustMkdirAll(t, filepath.Dir(victim))
	mustWriteFile(t, victim, []byte(victimContent))

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: traversalSHA,
		InstalledAt:    time.Now().UTC(),
	})

	if _, ok := canSkipInstall(target, col, st, noopPrinter{}); ok {
		t.Fatal("expected canSkipInstall to refuse a poisoned snapshot sha, not report it as already installed")
	}
	assertFileContent(t, victim, victimContent)
}

// TestInstallRecordMatchesRefusesUnsafeMarkerSHA pins matchingInstalledRecord's
// own markerRel guard: a file sits where an unguarded marker join would stat,
// and installRecordMatches must still report false for the unsafe sha.
func TestInstallRecordMatchesRefusesUnsafeMarkerSHA(t *testing.T) {
	t.Parallel()
	sandbox := t.TempDir()
	downloadPath := filepath.Join(sandbox, "install")

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	cfg := &config.Config{DownloadPath: downloadPath}
	target := newTestInstallTarget(t, cfg, col)
	mustMkdirAll(t, target.path)

	infoDir := filepath.Join(downloadPath, "ansible_collections", col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, infoDir)
	mustWriteFile(t, filepath.Join(infoDir, "GALAXY.yml"), sidecarFor(col))

	// Where an unguarded join of the .info marker directory with
	// helpers.ExtractMarkerPrefix+sha would land, so a bare stat would find it.
	const traversalSHA = "../../../../home/ci/.ssh/authorized_keys"
	coincidental := filepath.Join(downloadPath, "home", "ci", ".ssh", "authorized_keys")
	mustMkdirAll(t, filepath.Dir(coincidental))
	mustWriteFile(t, coincidental, []byte("not actually an extract marker"))

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: traversalSHA,
		InstalledAt:    time.Now().UTC(),
	})

	if installRecordMatches(target, col, st) {
		t.Fatal("expected installRecordMatches to refuse an unsafe marker sha rather than coincidentally match an unrelated file")
	}
}
