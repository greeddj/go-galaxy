package fakegit

import (
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
)

// TestAllowTipSHA1ServesTipsAlone pins the tip-only capability: advertised apart
// from allow-reachable-sha1-in-want, it serves a tip by hash and refuses the
// commit behind it at upload-pack, which AllowReachableSHA1 beside it serves.
func TestAllowTipSHA1ServesTipsAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.SetCapabilities(fixtureRepo, Capabilities{AllowTipSHA1: true})
	assertCapabilities(t, fetchAdvertisement(t, f.srv, fixtureRepo),
		[]capability.Capability{capability.AllowTipSHA1InWant},
		[]capability.Capability{capability.AllowReachableSHA1InWant})

	if _, err := fetchSHA(t, f.srv.RepoURL(fixtureRepo), f.second); err != nil {
		t.Fatalf("fetch of the tip by hash: %v", err)
	}
	f.srv.ResetCounts()
	_, err := fetchSHA(t, f.srv.RepoURL(fixtureRepo), f.first)
	if want := "not our ref " + f.first.String(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("fetch of the commit behind the tip: %v, want the remote's %q", err, want)
	}
	if got := f.srv.Count(EndpointUploadPack); got != 1 {
		t.Fatalf("upload-pack count = %d, want 1: the remote refuses the want, not the client", got)
	}

	f.srv.SetCapabilities(fixtureRepo, Capabilities{AllowReachableSHA1: true, AllowTipSHA1: true})
	if _, err := fetchSHA(t, f.srv.RepoURL(fixtureRepo), f.first); err != nil {
		t.Fatalf("fetch of the commit behind the tip with both capabilities: %v", err)
	}
}
