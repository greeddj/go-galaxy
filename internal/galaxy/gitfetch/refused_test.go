package gitfetch

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/testing/fakegit"
)

// neverCommitted is a well-formed commit hash no fixture repository holds.
const neverCommitted = "0123456789abcdef0123456789abcdef01234567"

// prunedRepo holds one collection, or one role, at three commits on main: old
// behind the tip, and gone, the tip until main moved back, as a force-push
// leaves it: still stored, reachable from no ref.
type prunedRepo struct {
	repo *fakegit.Repo
	old  plumbing.Hash
	gone plumbing.Hash
}

func newPrunedRepo(t *testing.T, role bool) prunedRepo {
	t.Helper()
	r := fakegit.NewRepo(t)
	r.SetHEAD("main")
	if role {
		r.AddRole(nil, "app")
	}
	commit := func(version string) plumbing.Hash {
		if !role {
			r.AddCollection("", "acme", "app", version, nil, nil)
		}
		return r.Commit("app " + version)
	}
	old := commit("1.0.0")
	tip := commit("1.1.0")
	gone := commit("1.2.0")
	r.Branch("main", tip)
	return prunedRepo{repo: r, old: old, gone: gone}
}

// acquireOne runs Acquire, or AcquireRole when role is set, for commit with
// main as the ref it was pinned from.
func acquireOne(t *testing.T, f *Fetcher, u gitsource.URL, role bool, commit string) error {
	t.Helper()
	ref := mustRef(t, "main")
	if role {
		_, err := acquireRole(t, f, gitsource.RoleRequest{URL: u, Ref: ref, Commit: commit})
		return err
	}
	_, err := acquire(t, f, gitsource.Request{URL: u, Ref: ref, Commit: commit})
	return err
}

// assertRefusedIsNotFound checks err reads as a search's miss does and carries
// no transport sentinel, which outranks it: the run would exit 4, not 3.
func assertRefusedIsNotFound(t *testing.T, err error, u gitsource.URL, commit string) {
	t.Helper()
	if !errors.Is(err, helpers.ErrGitCommitNotFound) || errors.Is(err, helpers.ErrGitTransportFailed) {
		t.Fatalf("refused commit %s: %v, want ErrGitCommitNotFound without ErrGitTransportFailed", commit, err)
	}
	if want := u.String() + " does not hold commit " + commit; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not read %q", err.Error(), want)
	}
}

// TestRefusedCommitIsNotFound pins that a commit a remote serving by hash
// refuses, never committed or no longer reachable, is ErrGitCommitNotFound
// after the one direct want; a reachable commit behind the tip is the control.
func TestRefusedCommitIsNotFound(t *testing.T) {
	t.Parallel()
	for _, role := range []bool{false, true} {
		for _, shallow := range []bool{false, true} {
			t.Run(fmt.Sprintf("role=%t shallow=%t", role, shallow), func(t *testing.T) {
				t.Parallel()
				pruned := newPrunedRepo(t, role)
				srv := fakegit.New(t)
				srv.Add("app", pruned.repo)
				srv.SetCapabilities("app", fakegit.Capabilities{Shallow: shallow, AllowReachableSHA1: true})
				f := newFetcher(t)
				u := mustURL(t, srv.RepoURL("app"))
				if err := acquireOne(t, f, u, role, pruned.old.String()); err != nil {
					t.Fatalf("reachable commit behind the tip: %v", err)
				}
				for _, commit := range []string{neverCommitted, pruned.gone.String()} {
					srv.ResetCounts()
					assertRefusedIsNotFound(t, acquireOne(t, f, u, role, commit), u, commit)
					if got := srv.Count(fakegit.EndpointUploadPack); got != 1 {
						t.Fatalf("refused commit %s took %d pack requests, want 1: the refusal is final", commit, got)
					}
				}
			})
		}
	}
}

// TestRefusalOtherwiseStaysATransportFailure bounds that mapping: another
// failure of the same direct want, and a refusal of a commit the remote
// advertised (its ref moved before the pack request), stay transport failures.
func TestRefusalOtherwiseStaysATransportFailure(t *testing.T) {
	t.Parallel()
	t.Run("server error on a direct want", func(t *testing.T) {
		t.Parallel()
		pruned := newPrunedRepo(t, false)
		srv := fakegit.New(t)
		srv.Add("app", pruned.repo)
		srv.SetCapabilities("app", fakegit.Capabilities{AllowReachableSHA1: true})
		srv.Fail(fakegit.EndpointUploadPack, "app", fakegit.Fault{Status: http.StatusInternalServerError, Count: 1})
		err := acquireOne(t, newFetcher(t), mustURL(t, srv.RepoURL("app")), false, neverCommitted)
		if !errors.Is(err, helpers.ErrGitTransportFailed) || errors.Is(err, helpers.ErrGitCommitNotFound) {
			t.Fatalf("server error: %v, want ErrGitTransportFailed alone", err)
		}
	})
	t.Run("advertised commit refused", func(t *testing.T) {
		t.Parallel()
		pruned := newPrunedRepo(t, false)
		srv := fakegit.New(t)
		srv.Add("app", pruned.repo)
		client := fetch.NewGit(testTimeout)
		base := client.Transport
		client.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				pruned.repo.Branch("main", pruned.old)
			}
			return base.RoundTrip(r)
		})
		_, err := acquire(t, New(client, t.TempDir), gitsource.Request{URL: mustURL(t, srv.RepoURL("app")), Ref: mustRef(t, "main")})
		if !errors.Is(err, helpers.ErrGitTransportFailed) || errors.Is(err, helpers.ErrGitCommitNotFound) {
			t.Fatalf("advertised commit refused: %v, want ErrGitTransportFailed alone", err)
		}
		if !strings.Contains(err.Error(), "not our ref") {
			t.Fatalf("the remote did not refuse the advertised tip: %v", err)
		}
	})
}

// TestSSHRefusedCommitIsNotFound is TestRefusedCommitIsNotFound's ssh leg. It
// sets environment variables through newSSHFixture, so it runs alone.
func TestSSHRefusedCommitIsNotFound(t *testing.T) {
	fx := newSSHFixture(t, "")
	f := newFetcher(t)
	for _, tc := range []struct {
		name string
		role bool
	}{{name: "collection", role: false}, {name: "role", role: true}} {
		pruned := newPrunedRepo(t, tc.role)
		fx.srv.Add(tc.name, pruned.repo)
		fx.srv.SetCapabilities(tc.name, fakegit.Capabilities{Shallow: true, AllowReachableSHA1: true})
		u := mustURL(t, fx.sshSrv.RepoURL(tc.name))
		if err := sshAcquireOne(t, f, u, fx.pem, tc.role, pruned.old.String()); err != nil {
			t.Fatalf("%s: reachable commit behind the tip over ssh: %v", tc.name, err)
		}
		for _, commit := range []string{neverCommitted, pruned.gone.String()} {
			fx.srv.ResetCounts()
			assertRefusedIsNotFound(t, sshAcquireOne(t, f, u, fx.pem, tc.role, commit), u, commit)
			if got := fx.srv.Count(fakegit.EndpointSSHExec); got != 1 {
				t.Fatalf("%s: refused commit %s took %d ssh sessions, want 1: the refusal is final", tc.name, commit, got)
			}
		}
	}
}

// sshAcquireOne is acquireOne with the fixture's bound key as the credential.
func sshAcquireOne(t *testing.T, f *Fetcher, u gitsource.URL, pem []byte, role bool, commit string) error {
	t.Helper()
	auth := gitsource.Credential{SSHKey: pem}
	if role {
		_, err := acquireRole(t, f, gitsource.RoleRequest{URL: u, Ref: mustRef(t, "main"), Commit: commit, Auth: auth})
		return err
	}
	_, err := acquire(t, f, gitsource.Request{URL: u, Ref: mustRef(t, "main"), Commit: commit, Auth: auth})
	return err
}
