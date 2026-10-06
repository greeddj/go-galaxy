package gitfetch

import (
	"fmt"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/testing/fakegit"
)

// byHashCase is one remote's fetch-by-hash capabilities and the exchanges it
// takes for the commit behind main's tip and for a commit it refuses: one each
// where the refusal is final, else the hint and then every tip searched too.
type byHashCase struct {
	name      string
	caps      fakegit.Capabilities
	behindTip int
	refused   int
}

func byHashCases(shallow bool) []byHashCase {
	return []byHashCase{
		{name: "tips only", caps: fakegit.Capabilities{Shallow: shallow, AllowTipSHA1: true}, behindTip: 2, refused: 3},
		{
			name: "reachable and tips", caps: fakegit.Capabilities{Shallow: shallow, AllowReachableSHA1: true, AllowTipSHA1: true},
			behindTip: 1, refused: 1,
		},
	}
}

// acquireBehindTip acquires pruned.old, the commit behind main's tip, with
// auth, and checks the build: acme.app 1.0.0, or the role, from that commit.
func acquireBehindTip(t *testing.T, f *Fetcher, u gitsource.URL, auth gitsource.Credential, role bool, pruned prunedRepo) {
	t.Helper()
	ref, old := mustRef(t, "main"), pruned.old.String()
	if role {
		res, err := acquireRole(t, f, gitsource.RoleRequest{URL: u, Ref: ref, Commit: old, Auth: auth})
		if err != nil {
			t.Fatalf("role commit behind the tip: %v", err)
		}
		if res.Commit != old {
			t.Fatalf("role built from %s, want %s", res.Commit, old)
		}
		assertRoleArtifact(t, res)
		return
	}
	res, err := acquire(t, f, gitsource.Request{URL: u, Ref: ref, Commit: old, Auth: auth})
	if err != nil {
		t.Fatalf("collection commit behind the tip: %v", err)
	}
	assertBuiltCollection(t, res.Collections[0], "1.0.0")
}

// TestTipOnlyRemoteIsSearchedAfterARefusal pins that a remote serving tips alone by
// hash has a refused commit searched for: one behind the tip is found, one it does not
// hold is ErrGitCommitNotFound. Serving reachable commits too, its refusal stays final.
func TestTipOnlyRemoteIsSearchedAfterARefusal(t *testing.T) {
	t.Parallel()
	for _, role := range []bool{false, true} {
		for _, shallow := range []bool{false, true} {
			for _, tc := range byHashCases(shallow) {
				t.Run(fmt.Sprintf("%s role=%t shallow=%t", tc.name, role, shallow), func(t *testing.T) {
					t.Parallel()
					pruned := newPrunedRepo(t, role)
					srv := fakegit.New(t)
					srv.Add("app", pruned.repo)
					srv.SetCapabilities("app", tc.caps)
					f := newFetcher(t)
					u := mustURL(t, srv.RepoURL("app"))
					acquireBehindTip(t, f, u, gitsource.Credential{}, role, pruned)
					if got := srv.Count(fakegit.EndpointUploadPack); got != tc.behindTip {
						t.Fatalf("commit behind the tip took %d pack requests, want %d", got, tc.behindTip)
					}
					for _, commit := range []string{neverCommitted, pruned.gone.String()} {
						srv.ResetCounts()
						assertRefusedIsNotFound(t, acquireOne(t, f, u, role, commit), u, commit)
						if got := srv.Count(fakegit.EndpointUploadPack); got != tc.refused {
							t.Fatalf("refused commit %s took %d pack requests, want %d", commit, got, tc.refused)
						}
					}
				})
			}
		}
	}
}

// TestSSHTipOnlyRemoteIsSearchedAfterARefusal is the ssh leg: over ssh git itself
// refuses a commit behind a tip when it serves tips alone. It sets environment
// variables through newSSHFixture, so it runs alone.
func TestSSHTipOnlyRemoteIsSearchedAfterARefusal(t *testing.T) {
	fx := newSSHFixture(t, "")
	f := newFetcher(t)
	auth := gitsource.Credential{SSHKey: fx.pem}
	for _, role := range []bool{false, true} {
		for i, tc := range byHashCases(true) {
			t.Run(fmt.Sprintf("%s role=%t", tc.name, role), func(t *testing.T) {
				name := fmt.Sprintf("app-%d-%t", i, role)
				pruned := newPrunedRepo(t, role)
				fx.srv.Add(name, pruned.repo)
				fx.srv.SetCapabilities(name, tc.caps)
				u := mustURL(t, fx.sshSrv.RepoURL(name))
				fx.srv.ResetCounts()
				acquireBehindTip(t, f, u, auth, role, pruned)
				if got := fx.srv.Count(fakegit.EndpointSSHExec); got != tc.behindTip {
					t.Fatalf("commit behind the tip took %d ssh sessions, want %d", got, tc.behindTip)
				}
				for _, commit := range []string{neverCommitted, pruned.gone.String()} {
					fx.srv.ResetCounts()
					assertRefusedIsNotFound(t, sshAcquireOne(t, f, u, fx.pem, role, commit), u, commit)
					if got := fx.srv.Count(fakegit.EndpointSSHExec); got != tc.refused {
						t.Fatalf("refused commit %s took %d ssh sessions, want %d", commit, got, tc.refused)
					}
				}
			})
		}
	}
}
