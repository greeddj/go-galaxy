package collections_test

// A root pinned with source: at an exact version, settled by the solver
// through Dependencies alone, must be fetched from that server by every phase,
// not only by the resolve.

import (
	"context"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// mirrorPath is the base path of the caching-proxy fixture's mirror
// endpoint - a second Galaxy endpoint under the configured server's own
// origin, the shape a proxy fronting several upstreams serves.
const mirrorPath = "/galaxy/mirror"

// pinnedReqSpec is one requirements.yml entry for buildPinnedRequirements: a
// collection at an exact version, optionally pinned to a source.
type pinnedReqSpec struct {
	name    string
	version string
	source  string
}

// buildPinnedRequirements renders entries into a requirements.yml body, each
// at its own exact version, with a source: line only where one is set.
func buildPinnedRequirements(entries []pinnedReqSpec) string {
	var b strings.Builder
	b.WriteString("collections:\n")
	for _, e := range entries {
		b.WriteString("  - name: " + e.name + "\n    version: \"" + e.version + "\"\n")
		if e.source != "" {
			b.WriteString("    source: " + e.source + "\n")
		}
	}
	return b.String()
}

// TestSourcePinnedExactRootInstallsFromItsOwnServer pins that prewarmed roots
// pinned to a mirror path under the configured server's origin install, lock
// and replay from the mirror, never from the configured path serving nothing.
func TestSourcePinnedExactRootInstallsFromItsOwnServer(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.NewAtBasePath(t, mirrorPath)
	one := srv.AddVersion("ns", "one", "1.0.0", nil)
	two := srv.AddVersion("ns", "two", "2.0.0", nil)

	mirror := srv.URL() + mirrorPath
	// Same origin as the mirror, so the pin matches it and no unmatched-source
	// warning fires, but this path serves nothing.
	servers := []config.Server{{ID: "proxy", URL: srv.URL() + "/galaxy/upstream"}}
	cfg := newMultiServerConfig(t, servers, buildPinnedRequirements([]pinnedReqSpec{
		{name: "ns.one", version: "1.0.0", source: mirror},
		{name: "ns.two", version: "2.0.0", source: mirror},
	}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "one")
	msAssertInstalled(t, cfg.DownloadPath, "two")

	// The artifact cache key folds in the resolved Source, so the mirror's
	// bytes under the mirror's key prove the mirror was recorded.
	if got := msArtifactSHA256(t, cfg.CacheDir, mirror, "one", "1.0.0"); got != one.SHA256 {
		t.Fatalf("cached ns.one sha = %s, want the mirror's own %s", got, one.SHA256)
	}
	if got := msArtifactSHA256(t, cfg.CacheDir, mirror, "two", "2.0.0"); got != two.SHA256 {
		t.Fatalf("cached ns.two sha = %s, want the mirror's own %s", got, two.SHA256)
	}

	lf := msLockFile(t, cfg, runtime)
	for _, fqdn := range []string{"ns.one", "ns.two"} {
		if e := findLockEntry(t, lf, fqdn); e.Source != mirror {
			t.Fatalf("%s lockfile source = %q, want the pinned mirror %q", fqdn, e.Source, mirror)
		}
	}

	srv.ResetCounts()
	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if got := srv.Total(); got != 0 {
		t.Fatalf("srv.Total() on reinstall = %d, want 0 - the recorded source must replay, not re-resolve", got)
	}
}

// TestSourcePinnedExactRootUnderNoDepsInstallsFromItsOwnServer pins that under
// --no-deps, where nothing binds the root, a source: naming a server_list id
// installs from that server and is recorded as its URL, not the id.
func TestSourcePinnedExactRootUnderNoDepsInstallsFromItsOwnServer(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	published := srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildPinnedRequirements([]pinnedReqSpec{
		{name: "ns.x", version: "1.0.0", source: "b"},
	}))
	cfg.NoDeps = true
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "x")
	if got := srvA.Total(); got != 0 {
		t.Fatalf("srvA.Total() = %d, want 0 - a pinned root never consults the first configured server", got)
	}
	if got := msArtifactSHA256(t, cfg.CacheDir, srvB.URL(), "x", "1.0.0"); got != published.SHA256 {
		t.Fatalf("cached ns.x sha = %s, want B's own %s", got, published.SHA256)
	}

	lf := msLockFile(t, cfg, runtime)
	if e := findLockEntry(t, lf, "ns.x"); e.Source != srvB.URL() {
		t.Fatalf("ns.x lockfile source = %q, want B's URL %q - an id must resolve to the server it names", e.Source, srvB.URL())
	}
}
