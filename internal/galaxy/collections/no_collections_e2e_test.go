package collections_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
)

// noCollectionsRoles is a roles list every case below shares: one git role
// with no dependencies, so an idle rerun replays its pin and fetches nothing.
const noCollectionsRoles = "roles:\n  - src: git+" + roleBaseURL + "\n    name: base\n"

// TestIdleRunWithoutCollectionsSkipsItsSave pins that a requirements file
// with no collections, roles only or collections: [], leaves the snapshot
// unsaved on an idle install and lock, until a collection is added.
func TestIdleRunWithoutCollectionsSkipsItsSave(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"git role only":    noCollectionsRoles,
		"Galaxy role only": "roles:\n  - geerlingguy.docker\n",
		"collections: []":  "collections: []\n" + noCollectionsRoles,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGalaxyRoleFixture(t)
			f.writeRequirements(t, body)
			f.mustInstall(t)
			stamp := reloadLastSnapshot(t, f.cacheDir)

			f.mustInstall(t)
			if got := reloadLastSnapshot(t, f.cacheDir); !got.Equal(stamp) {
				t.Fatalf("LastSnapshot after an idle install = %v, want unchanged %v", got, stamp)
			}
			f.lockfile(t)
			if got := reloadLastSnapshot(t, f.cacheDir); !got.Equal(stamp) {
				t.Fatalf("LastSnapshot after an idle lock = %v, want unchanged %v", got, stamp)
			}

			// Positive control: a collection added is resolved, recorded and saved.
			f.writeRequirements(t, "collections:\n  - acme.lib\n"+noCollectionsRoles)
			f.mustInstall(t)
			if got := reloadLastSnapshot(t, f.cacheDir); !got.After(stamp) {
				t.Fatalf("LastSnapshot after adding a collection = %v, want after %v", got, stamp)
			}
			if got := loadStoreSnapshot(t, f.cfg, f.runtime).ResolvedSnapshot()["acme.lib"].Version; got != e2eVersion200 {
				t.Fatalf("recorded acme.lib = %q, want %s", got, e2eVersion200)
			}
		})
	}
}

// TestRunWithoutCollectionsKeepsAnotherProjectsResolution pins that a
// roles-only project on a shared cache leaves the one recorded resolution
// to the project that recorded it, which then replays it.
func TestRunWithoutCollectionsKeepsAnotherProjectsResolution(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	f.writeRequirements(t, "collections:\n  - acme.lib\n")
	f.mustInstall(t)
	withCollections := f.cfg.RequirementsFile

	rolesOnly := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(rolesOnly, []byte(noCollectionsRoles), 0o600); err != nil {
		t.Fatalf("write the roles-only requirements: %v", err)
	}
	f.cfg.RequirementsFile = rolesOnly
	f.mustInstall(t)
	if got := loadStoreSnapshot(t, f.cfg, f.runtime).ResolvedSnapshot()["acme.lib"].Version; got != e2eVersion200 {
		t.Fatalf("recorded acme.lib after a roles-only run = %q, want %s still", got, e2eVersion200)
	}

	f.cfg.RequirementsFile = withCollections
	stamp := reloadLastSnapshot(t, f.cacheDir)
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("rerun of the project with collections: %v", err)
	}
	if got := reloadLastSnapshot(t, f.cacheDir); !got.Equal(stamp) {
		t.Fatalf("LastSnapshot after replaying the kept resolution = %v, want unchanged %v", got, stamp)
	}
}
