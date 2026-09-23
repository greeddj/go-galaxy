package gitfetch

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/testing/fakegit"
)

// newSSHFixture pins a hermetic environment (known_hosts from the listener, no
// agent, no proxy); every test here sets environment variables, so none may
// run in parallel.
func newSSHFixture(t *testing.T, passphrase string) sshFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets and an in-process ssh listener are not exercised on windows")
	}
	app := newAppRepo(t)
	srv := fakegit.New(t)
	srv.Add("app", app.repo)
	sshSrv := srv.SSH()
	pem, signer := fakegit.GenerateKey(t, passphrase)
	sshSrv.AuthorizeKey(signer.PublicKey())
	t.Setenv("SSH_KNOWN_HOSTS", sshSrv.KnownHostsFile(t))
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("ALL_PROXY", "")
	return sshFixture{srv: srv, sshSrv: sshSrv, pem: pem}
}

// sshFixture is what newSSHFixture hands a test: the recording server, its
// ssh listener and the PEM of the one key the listener authorizes.
type sshFixture struct {
	srv    *fakegit.Server
	sshSrv *fakegit.SSHServer
	pem    []byte
}

func TestSSHAcquireWithBoundKey(t *testing.T) {
	fx := newSSHFixture(t, "")
	srv := fx.srv
	sshSrv := fx.sshSrv
	pem := fx.pem
	res, err := acquire(t, newFetcher(t), gitsource.Request{
		URL:  mustURL(t, sshSrv.RepoURL("app")),
		Ref:  mustRef(t, ""),
		Auth: gitsource.Credential{SSHKey: pem},
	})
	if err != nil {
		t.Fatalf("Acquire over ssh: %v", err)
	}
	assertBuiltCollection(t, res.Collections[0], "1.2.3")
	if srv.Count(fakegit.EndpointSSHExec) != 1 {
		t.Fatalf("ssh exec count = %d, want 1", srv.Count(fakegit.EndpointSSHExec))
	}
	if _, ok := srv.SeenAuth(fakegit.EndpointSSHExec); !ok {
		t.Fatalf("the listener recorded no key for the session")
	}
}

func TestSSHAcquireWithPassphraseKey(t *testing.T) {
	fx := newSSHFixture(t, "open sesame")
	sshSrv := fx.sshSrv
	pem := fx.pem
	f := newFetcher(t)
	_, err := acquire(t, f, gitsource.Request{
		URL:  mustURL(t, sshSrv.RepoURL("app")),
		Ref:  mustRef(t, ""),
		Auth: gitsource.Credential{SSHKey: pem, SSHPassphrase: "wrong"},
	})
	if !errors.Is(err, helpers.ErrGitCredentialInvalid) {
		t.Fatalf("wrong passphrase: %v, want ErrGitCredentialInvalid", err)
	}
	res, err := acquire(t, f, gitsource.Request{
		URL:  mustURL(t, sshSrv.RepoURL("app")),
		Ref:  mustRef(t, "dev"),
		Auth: gitsource.Credential{SSHKey: pem, SSHPassphrase: "open sesame"},
	})
	if err != nil {
		t.Fatalf("Acquire with passphrase: %v", err)
	}
	assertBuiltCollection(t, res.Collections[0], "1.3.0")
}

func TestSSHAcquireThroughAgent(t *testing.T) {
	fx := newSSHFixture(t, "")
	sshSrv := fx.sshSrv
	_, signer := fakegit.GenerateKey(t, "")
	sshSrv.AuthorizeKey(signer.PublicKey())
	t.Setenv("SSH_AUTH_SOCK", fakegit.StartAgent(t, signer))
	res, err := acquire(t, newFetcher(t), gitsource.Request{URL: mustURL(t, sshSrv.RepoURL("app")), Ref: mustRef(t, "")})
	if err != nil {
		t.Fatalf("Acquire through the agent: %v", err)
	}
	assertBuiltCollection(t, res.Collections[0], "1.2.3")
}

func TestSSHWithoutKeyOrAgentIsAConfigurationRefusal(t *testing.T) {
	fx := newSSHFixture(t, "")
	srv := fx.srv
	sshSrv := fx.sshSrv
	_, err := acquire(t, newFetcher(t), gitsource.Request{URL: mustURL(t, sshSrv.RepoURL("app")), Ref: mustRef(t, "")})
	if !errors.Is(err, helpers.ErrGitSSHNoCredential) {
		t.Fatalf("no key and no agent: %v, want ErrGitSSHNoCredential", err)
	}
	if srv.Count(fakegit.EndpointSSHExec) != 0 {
		t.Fatalf("a session was opened without a credential")
	}
}

func TestSSHUnauthorizedKeyIsAnAuthFailure(t *testing.T) {
	fx := newSSHFixture(t, "")
	sshSrv := fx.sshSrv
	stranger, _ := fakegit.GenerateKey(t, "")
	_, err := acquire(t, newFetcher(t), gitsource.Request{
		URL:  mustURL(t, sshSrv.RepoURL("app")),
		Ref:  mustRef(t, ""),
		Auth: gitsource.Credential{SSHKey: stranger},
	})
	if !errors.Is(err, helpers.ErrGitAuthFailed) {
		t.Fatalf("unauthorized key: %v, want ErrGitAuthFailed", err)
	}
}

func TestSSHHostKeyPolicy(t *testing.T) {
	fx := newSSHFixture(t, "")
	srv := fx.srv
	sshSrv := fx.sshSrv
	pem := fx.pem
	other := fakegit.New(t).SSH()
	t.Setenv("SSH_KNOWN_HOSTS", fakegit.WriteKnownHosts(t, sshSrv.Addr(), other.HostKey()))
	_, err := acquire(t, newFetcher(t), gitsource.Request{
		URL: mustURL(t, sshSrv.RepoURL("app")), Ref: mustRef(t, ""), Auth: gitsource.Credential{SSHKey: pem},
	})
	if !errors.Is(err, helpers.ErrGitAuthFailed) {
		t.Fatalf("changed host key: %v, want ErrGitAuthFailed", err)
	}
	if srv.Count(fakegit.EndpointSSHExec) != 0 {
		t.Fatalf("a session ran against a host whose key is not vouched for")
	}

	// An empty known_hosts: the host is unknown, and there is no first-use
	// trust.
	t.Setenv("SSH_KNOWN_HOSTS", fakegit.WriteKnownHosts(t, "203.0.113.1:22", other.HostKey()))
	_, err = acquire(t, newFetcher(t), gitsource.Request{
		URL: mustURL(t, sshSrv.RepoURL("app")), Ref: mustRef(t, ""), Auth: gitsource.Credential{SSHKey: pem},
	})
	if !errors.Is(err, helpers.ErrGitAuthFailed) {
		t.Fatalf("unknown host: %v, want ErrGitAuthFailed", err)
	}
	if !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("error does not name known_hosts: %q", err.Error())
	}
}

// TestSSHIgnoresConfigUnderHOME pins that a hostile ~/.ssh/config under a
// redirected HOME does not redirect the ssh fetch; harden's switch-off itself
// is pinned by TestHardenRemovesFileAndGitTransports.
func TestSSHIgnoresConfigUnderHOME(t *testing.T) {
	fx := newSSHFixture(t, "")
	sshSrv := fx.sshSrv
	pem := fx.pem
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSSHConfig(t, home, "Host 127.0.0.1\n  Hostname 203.0.113.1\n  Port 1\n")
	res, err := acquire(t, newFetcher(t), gitsource.Request{
		URL: mustURL(t, sshSrv.RepoURL("app")), Ref: mustRef(t, ""), Auth: gitsource.Credential{SSHKey: pem},
	})
	if err != nil {
		t.Fatalf("Acquire with a hostile ~/.ssh/config present: %v", err)
	}
	assertBuiltCollection(t, res.Collections[0], "1.2.3")
}

func writeSSHConfig(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSSHCommitBehindTheTipUsesTwoExchanges pins the ssh fallback for a pinned
// commit that is neither a tip nor served by hash: the hint ref, then every
// tip on a fresh session, since go-git's ssh session is a single command.
func TestSSHCommitBehindTheTipUsesTwoExchanges(t *testing.T) {
	fx := newSSHFixture(t, "")
	r := fakegit.NewRepo(t)
	r.AddCollection("", "acme", "app", "1.0.0", nil, nil)
	old := r.Commit("first")
	r.AddCollection("", "acme", "app", "1.1.0", nil, nil)
	tip := r.Commit("second")
	r.Branch("main", tip)
	r.SetHEAD("main")
	// side is an orphan commit: a hint that exists but whose history does not
	// reach old, which is what forces the second exchange.
	side := r.RawTreeCommit([]object.TreeEntry{{Name: "README.md", Mode: filemode.Regular, Hash: r.Blob([]byte("side\n"))}})
	r.Branch("side", side)
	fx.srv.Add("old", r)

	f := newFetcher(t)
	res, err := acquire(t, f, gitsource.Request{
		URL: mustURL(t, fx.sshSrv.RepoURL("old")), Ref: mustRef(t, "main"), Commit: old.String(),
		Auth: gitsource.Credential{SSHKey: fx.pem},
	})
	if err != nil {
		t.Fatalf("Acquire via hint over ssh: %v", err)
	}
	assertBuiltCollection(t, res.Collections[0], "1.0.0")
	res, err = acquire(t, f, gitsource.Request{
		URL: mustURL(t, fx.sshSrv.RepoURL("old")), Ref: mustRef(t, "side"), Commit: old.String(),
		Auth: gitsource.Credential{SSHKey: fx.pem},
	})
	if err != nil {
		t.Fatalf("Acquire via every tip over ssh: %v", err)
	}
	assertBuiltCollection(t, res.Collections[0], "1.0.0")
	if got := fx.srv.Count(fakegit.EndpointSSHExec); got != 3 {
		t.Fatalf("ssh exec count = %d, want 3 (the hint session, then the orphan hint and the fresh all-tips session)", got)
	}
}
