package fakegit

import (
	"context"
	"runtime"
	"testing"

	"github.com/go-git/go-git/v5"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	"golang.org/x/crypto/ssh"
)

// sshUser is the user every ssh URL this package builds carries; the server
// does not check it.
const sshUser = "git"

// sshFixture stands the ssh half up with a fresh authorized key. It sets
// SSH_KNOWN_HOSTS through t.Setenv, so its tests run serially, and clears
// ALL_PROXY since go-git dials ssh through golang.org/x/net/proxy.
type sshFixture struct {
	signer ssh.Signer
	ssh    *SSHServer
	pem    []byte
	web    fixture
}

func newSSHFixture(t *testing.T, passphrase string) sshFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the ssh half needs unix sockets and a posix temp root")
	}
	f := newFixture(t)
	srv := f.srv.SSH()
	pemBytes, signer := GenerateKey(t, passphrase)
	srv.AuthorizeKey(signer.PublicKey())
	t.Setenv("SSH_KNOWN_HOSTS", srv.KnownHostsFile(t))
	t.Setenv("ALL_PROXY", "")
	t.Setenv("SSH_AUTH_SOCK", "")
	return sshFixture{web: f, ssh: srv, pem: pemBytes, signer: signer}
}

// sshClone clones the fixture repository over ssh with auth into memory
// storage.
func sshClone(t *testing.T, f sshFixture, auth git.CloneOptions) (*git.Repository, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), abortAfter)
	defer cancel()
	auth.URL = f.ssh.RepoURL(fixtureRepo)
	return git.CloneContext(ctx, memory.NewStorage(), nil, &auth)
}

// sshKeyCase is one clone with a key file: plain or passphrase-protected,
// full or depth-1.
type sshKeyCase struct {
	name       string
	passphrase string
	depth      int
}

func sshKeyCases() []sshKeyCase {
	return []sshKeyCase{
		{name: "plain key full clone"},
		{name: "plain key depth 1", depth: 1},
		{name: "passphrase key full clone", passphrase: "open sesame"},
	}
}

func TestSSHCloneWithKey(t *testing.T) {
	for _, tc := range sshKeyCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newSSHFixture(t, tc.passphrase)
			f.web.srv.SetCapabilities(fixtureRepo, Capabilities{Shallow: true})
			auth, err := gogitssh.NewPublicKeys(sshUser, f.pem, tc.passphrase)
			if err != nil {
				t.Fatalf("NewPublicKeys: %v", err)
			}
			opts := branchOpts(tc.depth)
			opts.Auth = auth
			r, err := sshClone(t, f, opts)
			if err != nil {
				t.Fatalf("clone over ssh: %v", err)
			}
			wantCommits := 2
			if tc.depth == 1 {
				wantCommits = 1
			}
			if got := countCommits(t, r); got != wantCommits {
				t.Fatalf("commits = %d, want %d", got, wantCommits)
			}
			if f.web.srv.Count(EndpointSSHExec) != 1 || f.web.srv.Total() != 1 {
				t.Fatalf("counts = %d/%d, want 1/1", f.web.srv.Count(EndpointSSHExec), f.web.srv.Total())
			}
			got, present := f.web.srv.SeenAuth(EndpointSSHExec)
			if want := ssh.FingerprintSHA256(f.signer.PublicKey()); !present || got != want {
				t.Fatalf("SeenAuth = %q, %v; want %q", got, present, want)
			}
		})
	}
}

func TestSSHCloneThroughAgent(t *testing.T) {
	f := newSSHFixture(t, "")
	t.Setenv("SSH_AUTH_SOCK", StartAgent(t, f.signer))
	auth, err := gogitssh.NewSSHAgentAuth(sshUser)
	if err != nil {
		t.Fatalf("NewSSHAgentAuth: %v", err)
	}
	opts := branchOpts(0)
	opts.Auth = auth
	r, err := sshClone(t, f, opts)
	if err != nil {
		t.Fatalf("clone through the agent: %v", err)
	}
	if got := countCommits(t, r); got != 2 {
		t.Fatalf("commits = %d, want 2", got)
	}
	// With no Auth at all go-git falls back to the agent on its own.
	if _, err := sshClone(t, f, branchOpts(0)); err != nil {
		t.Fatalf("clone with the default agent auth: %v", err)
	}
}

func TestSSHUnauthorizedKeyRefused(t *testing.T) {
	f := newSSHFixture(t, "")
	strangerPEM, _ := GenerateKey(t, "")
	auth, err := gogitssh.NewPublicKeys(sshUser, strangerPEM, "")
	if err != nil {
		t.Fatalf("NewPublicKeys: %v", err)
	}
	opts := branchOpts(0)
	opts.Auth = auth
	if _, err := sshClone(t, f, opts); err == nil {
		t.Fatal("clone with an unauthorized key succeeded")
	}
	if f.web.srv.Count(EndpointSSHExec) != 0 {
		t.Fatalf("exec count = %d, want 0: auth must fail before any exec", f.web.srv.Count(EndpointSSHExec))
	}
}

func TestSSHWrongKnownHostsRefused(t *testing.T) {
	f := newSSHFixture(t, "")
	otherHost := New(t).SSH()
	t.Setenv("SSH_KNOWN_HOSTS", WriteKnownHosts(t, f.ssh.Addr(), otherHost.HostKey()))
	auth, err := gogitssh.NewPublicKeys(sshUser, f.pem, "")
	if err != nil {
		t.Fatalf("NewPublicKeys: %v", err)
	}
	opts := branchOpts(0)
	opts.Auth = auth
	if _, err := sshClone(t, f, opts); err == nil {
		t.Fatal("clone against a mismatching known_hosts succeeded")
	}
	if f.web.srv.Count(EndpointSSHExec) != 0 {
		t.Fatalf("exec count = %d, want 0: host key check must fail before any exec", f.web.srv.Count(EndpointSSHExec))
	}
}

func TestSSHHangFaultIsAbortedByContext(t *testing.T) {
	f := newSSHFixture(t, "")
	f.web.srv.Fail(EndpointSSHExec, fixtureRepo, Fault{Hang: true, Count: 1})
	auth, err := gogitssh.NewPublicKeys(sshUser, f.pem, "")
	if err != nil {
		t.Fatalf("NewPublicKeys: %v", err)
	}
	opts := branchOpts(0)
	opts.Auth = auth
	if _, err := sshClone(t, f, opts); err == nil {
		t.Fatal("clone against a hung exec succeeded")
	}
	if f.web.srv.Count(EndpointSSHExec) != 1 {
		t.Fatalf("exec count = %d, want 1", f.web.srv.Count(EndpointSSHExec))
	}
	// The Count is spent; the server is still serving.
	if _, err := sshClone(t, f, opts); err != nil {
		t.Fatalf("clone after the hang was consumed: %v", err)
	}
}

func TestSSHUnknownRepositoryRefused(t *testing.T) {
	f := newSSHFixture(t, "")
	auth, err := gogitssh.NewPublicKeys(sshUser, f.pem, "")
	if err != nil {
		t.Fatalf("NewPublicKeys: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), abortAfter)
	defer cancel()
	opts := branchOpts(0)
	opts.Auth = auth
	opts.URL = f.ssh.RepoURL("nothing")
	if _, err := git.CloneContext(ctx, memory.NewStorage(), nil, &opts); err == nil {
		t.Fatal("clone of an unregistered repository over ssh succeeded")
	}
}
