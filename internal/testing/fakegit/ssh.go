package fakegit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Strings of the ssh exchange: the exec command go-git issues, the extension
// carrying the key fingerprint to the exec handler, and a not-found line
// worded as GitHub words it, which go-git maps to its not-found error.
const (
	sessionChannelType = "session"
	execRequestType    = "exec"
	exitStatusRequest  = "exit-status"
	execCommandPrefix  = uploadPackService + " '"
	execCommandSuffix  = "'"
	fingerprintExt     = "fakegit-fingerprint"
	repoNotFoundLine   = "fakegit: Repository not found.\n"
	agentSocketName    = "agent.sock"
	knownHostsName     = "known_hosts"
	// agentDirPattern is deliberately short: a unix socket path is capped at
	// 104 bytes on darwin and the directory holding it is created under the
	// system temp root, not under t.TempDir, for that reason.
	agentDirPattern = "fg"
	// knownHostsMode is the permission the known_hosts file is written with;
	// ssh clients only read it, so nothing stricter is needed.
	knownHostsMode os.FileMode = 0o600
)

// Errors the ssh half answers a client with. None is part of the contract
// with a test, which observes the client-side error instead.
var (
	errKeyNotAuthorized = errors.New("fakegit: public key not authorized")
	errAgentReadOnly    = errors.New("fakegit: agent is read-only")
)

// execPayload is the wire shape of an ssh exec request's payload: one
// string, which ssh.Unmarshal reads as uint32 length plus bytes.
type execPayload struct {
	Command string
}

// exitStatusPayload is the wire shape of an exit-status request's payload.
type exitStatusPayload struct {
	Status uint32
}

// SSHServer is the ssh half of a Server: a 127.0.0.1 listener accepting only
// keys AuthorizeKey registered and answering the upload-pack exec with the
// HTTP half's exchange. Server.SSH starts it lazily; it closes with the Server.
type SSHServer struct {
	parent     *Server
	listener   net.Listener
	hostKey    ssh.Signer
	authorized map[string]bool
	conns      map[net.Conn]struct{}
	ctx        context.Context //nolint:containedctx // the lifetime every handler's blocking fault is bound to; canceled by close.
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	mu         sync.Mutex
}

// SSH returns the ssh half of s, starting it on the first call. The
// repositories Add registered, their capabilities and their armed faults are
// shared; only the transport differs.
func (s *Server) SSH() *SSHServer {
	s.tb.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ssh != nil {
		return s.ssh
	}
	s.ssh = newSSHServer(s)
	return s.ssh
}

// newSSHServer generates the host key, binds the listener and starts the
// accept loop. It is called with parent.mu held.
func newSSHServer(parent *Server) *SSHServer {
	parent.tb.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		parent.tb.Fatalf("fakegit: generate host key: %v", err)
	}
	hostKey, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		parent.tb.Fatalf("fakegit: host key signer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		parent.tb.Fatalf("fakegit: listen ssh: %v", err)
	}
	srv := &SSHServer{
		parent:     parent,
		listener:   listener,
		hostKey:    hostKey,
		authorized: make(map[string]bool),
		conns:      make(map[net.Conn]struct{}),
		ctx:        ctx,
		cancel:     cancel,
	}
	srv.wg.Add(1)
	go srv.acceptLoop()
	return srv
}

// Addr returns the listener's "host:port".
func (s *SSHServer) Addr() string {
	return s.listener.Addr().String()
}

// RepoURL returns the ssh clone URL of the repository registered as name:
// "ssh://git@127.0.0.1:PORT/<name>.git". The user is not checked.
func (s *SSHServer) RepoURL(name string) string {
	return "ssh://git@" + s.Addr() + "/" + name + repoSuffix
}

// AuthorizeKey admits pub for publickey authentication. Every other key is
// refused, and there is no password or keyboard-interactive method.
func (s *SSHServer) AuthorizeKey(pub ssh.PublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authorized[string(pub.Marshal())] = true
}

// HostKey returns the public half of the generated host key, for a test
// that writes its own known_hosts or asserts against the fingerprint.
func (s *SSHServer) HostKey() ssh.PublicKey {
	return s.hostKey.PublicKey()
}

// KnownHostsFile writes a known_hosts file admitting this listener's host key
// into a directory tb owns and returns the path, for
// t.Setenv("SSH_KNOWN_HOSTS", path).
func (s *SSHServer) KnownHostsFile(tb testing.TB) string {
	tb.Helper()
	return WriteKnownHosts(tb, s.Addr(), s.HostKey())
}

// WriteKnownHosts writes a known_hosts file admitting key for addr
// ("host:port") into a directory tb owns and returns its path, so a test can
// pin a mismatching key to a listener's address.
func WriteKnownHosts(tb testing.TB, addr string, key ssh.PublicKey) string {
	tb.Helper()
	line := knownhosts.Line([]string{knownhosts.Normalize(addr)}, key) + "\n"
	p := filepath.Join(tb.TempDir(), knownHostsName)
	if err := os.WriteFile(p, []byte(line), knownHostsMode); err != nil {
		tb.Fatalf("fakegit: write known_hosts: %v", err)
	}
	return p
}

// close stops accepting, cancels every blocking handler, closes every live
// connection and waits for the handlers to return.
func (s *SSHServer) close() {
	s.cancel()
	_ = s.listener.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// acceptLoop hands each incoming connection to handleConn until the
// listener is closed.
func (s *SSHServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// serverConfig builds the per-connection ssh.ServerConfig: publickey only,
// stamping the accepted key's fingerprint into the permissions for SeenAuth.
func (s *SSHServer) serverConfig() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			s.mu.Lock()
			ok := s.authorized[string(key.Marshal())]
			s.mu.Unlock()
			if !ok {
				return nil, errKeyNotAuthorized
			}
			return &ssh.Permissions{Extensions: map[string]string{fingerprintExt: ssh.FingerprintSHA256(key)}}, nil
		},
	}
	cfg.AddHostKey(s.hostKey)
	return cfg
}

// handleConn runs the ssh handshake on conn and serves its session
// channels until the connection ends.
func (s *SSHServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()

	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.serverConfig())
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)

	// connCtx ends with the connection or with the server, whichever is
	// first; a Hang or a stall blocks on it.
	connCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	go func() {
		_ = sconn.Wait()
		cancel()
	}()

	fingerprint := ""
	if sconn.Permissions != nil {
		fingerprint = sconn.Permissions.Extensions[fingerprintExt]
	}
	for newChan := range chans {
		if newChan.ChannelType() != sessionChannelType {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session channels are served")
			continue
		}
		channel, requests, acceptErr := newChan.Accept()
		if acceptErr != nil {
			continue
		}
		s.wg.Add(1)
		go s.handleSession(connCtx, channel, requests, fingerprint)
	}
}

// handleSession serves one session channel: the first exec gets the
// upload-pack exchange, an exit-status and a close; any other request that
// wants a reply is refused.
func (s *SSHServer) handleSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, fingerprint string) {
	defer s.wg.Done()
	defer func() { _ = channel.Close() }()
	for req := range requests {
		if req.Type != execRequestType {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		var payload execPayload
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			return
		}
		name, ok := parseExecCommand(payload.Command)
		if !ok {
			_ = req.Reply(false, nil)
			return
		}
		_ = req.Reply(true, nil)
		status := s.runExec(ctx, channel, name, fingerprint)
		_, _ = channel.SendRequest(exitStatusRequest, false, ssh.Marshal(&exitStatusPayload{Status: status}))
		return
	}
}

// runExec is the ssh counterpart of the two HTTP handlers: count, record the
// fingerprint, apply the fault, then advertise, read and reply over channel.
// It returns the exit status to report.
func (s *SSHServer) runExec(ctx context.Context, channel ssh.Channel, name, fingerprint string) uint32 {
	p := s.parent
	p.incr(EndpointSSHExec)
	p.recordAuth(EndpointSSHExec, fingerprint, fingerprint != "")
	fault, _ := p.consumeFault(EndpointSSHExec, name)
	if fault.Hang {
		<-ctx.Done()
		return 1
	}
	repo, caps, ok := p.lookup(name)
	if !ok {
		_, _ = channel.Stderr().Write([]byte(repoNotFoundLine))
		return 1
	}
	if err := advertise(channel, repo, caps, false); err != nil {
		p.tb.Errorf("fakegit: advertise %s over ssh: %v", name, err)
		return 1
	}
	req, closed, err := readUploadRequest(channel)
	if err != nil {
		_, _ = channel.Stderr().Write([]byte("fakegit: malformed upload request\n"))
		return 1
	}
	if closed {
		return 0
	}
	reply, err := buildReply(repo, caps, req, fault)
	if err != nil {
		p.tb.Errorf("fakegit: build reply for %s over ssh: %v", name, err)
		return 1
	}
	if reply.badRequest != "" {
		_, _ = channel.Stderr().Write([]byte("fakegit: " + reply.badRequest + "\n"))
		return 1
	}
	writeReply(channel, nil, ctx.Done(), reply, fault.StallAfterBytes)
	return 0
}

// parseExecCommand extracts the repository name from the exact command
// go-git sends, "git-upload-pack '/<name>.git'", and refuses anything else, so
// the fake also pins the command shape the production client emits.
func parseExecCommand(cmd string) (string, bool) {
	if !strings.HasPrefix(cmd, execCommandPrefix) || !strings.HasSuffix(cmd, execCommandSuffix) {
		return "", false
	}
	p := strings.TrimSuffix(strings.TrimPrefix(cmd, execCommandPrefix), execCommandSuffix)
	return repoName(p)
}

// StartAgent serves a read-only ssh agent holding signers on a unix socket and
// returns its path, for t.Setenv("SSH_AUTH_SOCK", path). Its directory avoids
// tb.TempDir because darwin caps a unix socket path at 104 bytes.
func StartAgent(tb testing.TB, signers ...ssh.Signer) string {
	tb.Helper()
	dir, err := os.MkdirTemp("", agentDirPattern) //nolint:usetesting // see above: the socket path must stay under darwin's 104-byte cap.
	if err != nil {
		tb.Fatalf("fakegit: agent dir: %v", err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, agentSocketName)
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", sock)
	if err != nil {
		tb.Fatalf("fakegit: listen agent socket: %v", err)
	}
	ag := &signerAgent{signers: signers}
	// go-git's agent client keeps its connection open for the life of the
	// process, so cleanup has to close the accepted connections itself, not
	// only the listener, for the serving goroutines to return.
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns = make(map[net.Conn]struct{})
	)
	wg.Go(func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			mu.Lock()
			conns[conn] = struct{}{}
			mu.Unlock()
			wg.Go(func() {
				_ = agent.ServeAgent(ag, conn)
				_ = conn.Close()
			})
		}
	})
	tb.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		for c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return sock
}

// signerAgent is the read-only agent StartAgent serves: it lists and signs
// with a fixed set of signers and refuses every mutation.
type signerAgent struct {
	signers []ssh.Signer
}

// List returns the public keys of every signer.
func (a *signerAgent) List() ([]*agent.Key, error) {
	keys := make([]*agent.Key, 0, len(a.signers))
	for _, s := range a.signers {
		pub := s.PublicKey()
		keys = append(keys, &agent.Key{Format: pub.Type(), Blob: pub.Marshal()})
	}
	return keys, nil
}

// Sign signs data with the signer whose public key is key.
func (a *signerAgent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	want := string(key.Marshal())
	for _, s := range a.signers {
		if string(s.PublicKey().Marshal()) == want {
			return s.Sign(rand.Reader, data)
		}
	}
	return nil, errKeyNotAuthorized
}

// Add refuses: the agent is read-only.
func (a *signerAgent) Add(agent.AddedKey) error { return errAgentReadOnly }

// Remove refuses: the agent is read-only.
func (a *signerAgent) Remove(ssh.PublicKey) error { return errAgentReadOnly }

// RemoveAll refuses: the agent is read-only.
func (a *signerAgent) RemoveAll() error { return errAgentReadOnly }

// Lock refuses: the agent is read-only.
func (a *signerAgent) Lock([]byte) error { return errAgentReadOnly }

// Unlock refuses: the agent is read-only.
func (a *signerAgent) Unlock([]byte) error { return errAgentReadOnly }

// Signers returns the signers themselves.
func (a *signerAgent) Signers() ([]ssh.Signer, error) {
	return append([]ssh.Signer(nil), a.signers...), nil
}

// GenerateKey creates an ed25519 key pair and returns its private half as
// OpenSSH PEM, encrypted with passphrase when one is given, together with a
// signer over it for AuthorizeKey and StartAgent.
func GenerateKey(tb testing.TB, passphrase string) ([]byte, ssh.Signer) {
	tb.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatalf("fakegit: generate key: %v", err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		tb.Fatalf("fakegit: marshal key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		tb.Fatalf("fakegit: key signer: %v", err)
	}
	return pem.EncodeToMemory(block), signer
}
