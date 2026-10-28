package gitfetch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh"

	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/testing/fakegit"
)

// TestAuthForRefusesAnUnknownScheme pins that a URL whose scheme is neither
// http(s) nor ssh gets no auth at all.
func TestAuthForRefusesAnUnknownScheme(t *testing.T) {
	t.Parallel()
	auth, err := authFor(gitsource.URL{Scheme: "file", Path: "/srv/repo.git"}, gitsource.Credential{Password: "secret"})
	if auth != nil || !errors.Is(err, helpers.ErrInvalidGitURL) {
		t.Fatalf("authFor(file) = (%v, %v), want ErrInvalidGitURL and no auth", auth, err)
	}
}

// TestTransportForRefusesUnspokenProtocols pins that the file and git
// protocols never get a transport, whatever go-git's registry holds.
func TestTransportForRefusesUnspokenProtocols(t *testing.T) {
	t.Parallel()
	f := newFetcher(t)
	for _, proto := range []string{protocolFile, protocolGit, "ftp"} {
		if tr, err := f.transportFor(&transport.Endpoint{Protocol: proto}); tr != nil || !errors.Is(err, helpers.ErrInvalidGitURL) {
			t.Errorf("transportFor(%q) = (%v, %v), want ErrInvalidGitURL", proto, tr, err)
		}
	}
	if _, err := f.transportFor(&transport.Endpoint{Protocol: protocolHTTPS}); err != nil {
		t.Errorf("transportFor(https) = %v", err)
	}
}

// TestClassifyTransportErrorKeepsContextErrors pins that no error is no
// error and that a cancellation or deadline is returned as itself, so the run
// exits as interrupted rather than as a transport failure.
func TestClassifyTransportErrorKeepsContextErrors(t *testing.T) {
	t.Parallel()
	if err := classifyTransportError(nil, "remote"); err != nil {
		t.Errorf("classifyTransportError(nil) = %v", err)
	}
	for _, cause := range []error{context.Canceled, fmt.Errorf("dial: %w", context.DeadlineExceeded)} {
		got := classifyTransportError(cause, "remote")
		if !errors.Is(got, cause) || errors.Is(got, helpers.ErrGitTransportFailed) || got.Error() != cause.Error() {
			t.Errorf("classifyTransportError(%v) = %v, want it unchanged", cause, got)
		}
	}
}

// errBroken is the ClientConfig failure stubSSHAuth reports when told to.
var errBroken = errors.New("no config")

// stubSSHAuth is a go-git ssh auth method answering ClientConfig with cfg and
// err.
type stubSSHAuth struct {
	err error
	cfg *ssh.ClientConfig
}

func (stubSSHAuth) Name() string                               { return "stub" }
func (stubSSHAuth) String() string                             { return "stub" }
func (s stubSSHAuth) ClientConfig() (*ssh.ClientConfig, error) { return s.cfg, s.err }

// TestTimedAuthBoundsTheDial pins that every ssh client config carries the
// dial timeout, and that a config the inner method cannot build stays an
// error.
func TestTimedAuthBoundsTheDial(t *testing.T) {
	t.Parallel()
	cfg, err := timedAuth{AuthMethod: stubSSHAuth{cfg: &ssh.ClientConfig{}}}.ClientConfig()
	if err != nil || cfg.Timeout != helpers.FetchDialContextTimeout {
		t.Fatalf("ClientConfig = (%+v, %v), want the dial timeout %v", cfg, err, helpers.FetchDialContextTimeout)
	}
	if cfg, err := (timedAuth{AuthMethod: stubSSHAuth{err: errBroken}}).ClientConfig(); cfg != nil || !errors.Is(err, errBroken) {
		t.Fatalf("ClientConfig = (%+v, %v), want the inner error", cfg, err)
	}
}

// TestSSHAuthKnownHosts pins that a missing known_hosts is a transport
// failure and a portless URL is looked up at port 22. It sets the
// environment, so it is not parallel.
func TestSSHAuthKnownHosts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("known_hosts handling is exercised on unix only")
	}
	pem, signer := fakegit.GenerateKey(t, "")
	u := gitsource.URL{Scheme: protocolSSH, User: "git", Host: "git.example.invalid", Path: "/acme/app.git"}
	cred := gitsource.Credential{SSHKey: pem}
	t.Setenv("SSH_AUTH_SOCK", "")

	t.Setenv("SSH_KNOWN_HOSTS", filepath.Join(t.TempDir(), "absent"))
	if _, err := sshAuthFor(u, cred); !errors.Is(err, helpers.ErrGitTransportFailed) || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("sshAuthFor without a known_hosts file = %v, want ErrGitTransportFailed naming known_hosts", err)
	}

	for _, tc := range []struct {
		addr      string
		wantAlgos bool
	}{
		{addr: "git.example.invalid:22", wantAlgos: true},
		{addr: "git.example.invalid:2222", wantAlgos: false},
	} {
		t.Setenv("SSH_KNOWN_HOSTS", fakegit.WriteKnownHosts(t, tc.addr, signer.PublicKey()))
		auth, err := sshAuthFor(u, cred)
		if err != nil {
			t.Fatalf("sshAuthFor with known_hosts for %s: %v", tc.addr, err)
		}
		timed, ok := auth.(timedAuth)
		if !ok {
			t.Fatalf("sshAuthFor returned %T, want a dial-bounded method", auth)
		}
		keys, ok := timed.AuthMethod.(publicKeys)
		if !ok {
			t.Fatalf("sshAuthFor wraps %T, want the bound key", timed.AuthMethod)
		}
		if got := len(keys.HostKeyAlgorithms) > 0; got != tc.wantAlgos {
			t.Fatalf("known_hosts entry for %s: host key algorithms %v, want present=%t", tc.addr, keys.HostKeyAlgorithms, tc.wantAlgos)
		}
		if keys.HostKeyCallback == nil {
			t.Fatal("no host key callback: the host would be trusted on first use")
		}
	}
}

// TestAcquireReportsAnUnusableTempDir pins that a temp dir the object store
// cannot be created in fails the acquisition as a transport failure.
func TestAcquireReportsAnUnusableTempDir(t *testing.T) {
	t.Parallel()
	app := newAppRepo(t)
	srv := fakegit.New(t)
	srv.Add("app", app.repo)
	missing := filepath.Join(t.TempDir(), "absent")
	f := New(fetch.NewGit(testTimeout), func() string { return missing })

	_, err := acquire(t, f, gitsource.Request{URL: mustURL(t, srv.RepoURL("app")), Ref: mustRef(t, "")})
	if !errors.Is(err, helpers.ErrGitTransportFailed) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Acquire = %v, want ErrGitTransportFailed wrapping fs.ErrNotExist", err)
	}
}

// TestFailedBuildRemovesTheArtifactsAlreadyBuilt pins buildAll's cleanup: when
// one collection of a repository fails to build, the artifacts built before
// it are removed rather than left in the run's temp dir.
func TestFailedBuildRemovesTheArtifactsAlreadyBuilt(t *testing.T) {
	t.Parallel()
	r := fakegit.NewRepo(t)
	r.AddCollection("collections/one", "acme", "one", "0.1.0", nil, nil)
	r.AddCollection("collections/two", "acme", "two", "0.2.0", nil, nil)
	r.Symlink("nowhere", "collections/two/plugins/dangling")
	head := r.Commit("mono")
	r.Branch("main", head)
	r.SetHEAD("main")
	srv := fakegit.New(t)
	srv.Add("mono", r)
	artifacts := t.TempDir()

	_, err := acquire(t, newFetcher(t), gitsource.Request{
		URL: mustURL(t, srv.RepoURL("mono")), Ref: mustRef(t, ""), Subdir: "collections", TempFile: tempFileIn(artifacts),
	})
	if !errors.Is(err, helpers.ErrGitSymlinkUnresolvable) {
		t.Fatalf("Acquire = %v, want ErrGitSymlinkUnresolvable from the second build", err)
	}
	left, readErr := os.ReadDir(artifacts)
	if readErr != nil {
		t.Fatalf("read artifact dir: %v", readErr)
	}
	if len(left) != 0 {
		t.Fatalf("a failed acquisition left artifacts behind: %v", left)
	}
}
