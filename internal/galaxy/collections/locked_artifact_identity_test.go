package collections

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
	"github.com/psvmcc/hub/pkg/types"
)

// lockedDownloadArm is one path downloadCollectionToCache may take: the
// streaming one through the extracted store, or the temp file with no store.
type lockedDownloadArm struct {
	name      string
	streaming bool
}

// lockedDownloadArms lists both arms, so each pins the identity check alike.
func lockedDownloadArms() []lockedDownloadArm {
	return []lockedDownloadArm{{name: "temp file"}, {name: "extracted store", streaming: true}}
}

// lockedDownloadFixture serves data at a hub-shaped URL, no file name in its
// path, for acme.widgets@1.0.0 carrying it as its locked download_url with no
// pin, so the identity check alone judges the bytes.
func lockedDownloadFixture(t *testing.T, data []byte, streaming bool) (installDeps, *types.GalaxyCollectionVersionInfo, collection) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)

	cacheDir := t.TempDir()
	cfg := &config.Config{CacheDir: cacheDir, Workers: 1, NoDeps: true}
	deps := installDeps{
		collectionDeps: newCollectionDeps(cfg, infra.New(noopPrinter{}, http.DefaultClient), store.New()),
		artifacts:      local.NewArtifacts(cacheDir),
	}
	if streaming {
		deps.extractStore = extracted.NewStore(cacheDir)
	}

	downloadURL := server.URL + "/galaxy/ansible/get/acme/widgets/" + testVersion100
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100, Source: server.URL, DownloadURL: downloadURL}
	return deps, &types.GalaxyCollectionVersionInfo{Version: testVersion100, DownloadURL: downloadURL}, col
}

// TestLockedDownloadCommitsTheLockedCollection pins that a locked URL whose
// path is not the artifact's file name fills the entry's cache slot when its
// manifest names the entry.
func TestLockedDownloadCommitsTheLockedCollection(t *testing.T) {
	t.Parallel()
	for _, arm := range lockedDownloadArms() {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			data, sha := fakegalaxy.BuildArtifact("acme", "widgets", testVersion100, nil)
			deps, meta, col := lockedDownloadFixture(t, data, arm.streaming)

			ctx := context.Background()
			result, err := downloadCollectionToCache(ctx, deps, col, meta, true)
			if err != nil {
				t.Fatalf("downloadCollectionToCache = %v, want nil", err)
			}
			defer cleanupIfNeeded(result.Cleanup)

			cached, err := deps.artifacts.Has(ctx, artifactKey(col))
			if err != nil {
				t.Fatalf("artifacts.Has: %v", err)
			}
			if !cached {
				t.Errorf("the locked collection did not enter its artifact cache slot")
			}
			if arm.streaming && !deps.extractStore.Ready(sha) {
				t.Errorf("the locked collection's tree was not promoted into the extracted store")
			}
		})
	}
}

// TestLockedDownloadRefusesAnotherCollection pins the guard: bytes whose
// manifest names another collection or version are refused before they reach
// the entry's cache slot or the extracted store.
func TestLockedDownloadRefusesAnotherCollection(t *testing.T) {
	t.Parallel()
	served := map[string][]string{
		"another name":    {"acme", "other", testVersion100},
		"another version": {"acme", "widgets", "2.0.0"},
	}
	for _, arm := range lockedDownloadArms() {
		for name, identity := range served {
			t.Run(arm.name+"/"+name, func(t *testing.T) {
				t.Parallel()
				data, sha := fakegalaxy.BuildArtifact(identity[0], identity[1], identity[2], nil)
				deps, meta, col := lockedDownloadFixture(t, data, arm.streaming)

				ctx := context.Background()
				_, err := downloadCollectionToCache(ctx, deps, col, meta, true)
				// Errorf, not Fatalf, so the cache assertions below are always reached.
				if !errors.Is(err, helpers.ErrLockedArtifactIdentityMismatch) {
					t.Errorf("downloadCollectionToCache = %v, want errors.Is helpers.ErrLockedArtifactIdentityMismatch", err)
				}

				cached, hasErr := deps.artifacts.Has(ctx, artifactKey(col))
				if hasErr != nil {
					t.Fatalf("artifacts.Has: %v", hasErr)
				}
				if cached {
					t.Errorf("another collection's bytes entered the locked entry's cache slot")
				}
				if arm.streaming && deps.extractStore.Ready(sha) {
					t.Errorf("another collection's tree was promoted into the extracted store")
				}
			})
		}
	}
}
