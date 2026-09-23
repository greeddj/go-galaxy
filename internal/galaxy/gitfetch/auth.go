package gitfetch

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const sshDefaultPort = "22"

// authFor builds u's go-git auth: over http(s) a bound credential is Basic auth
// (a token is the password), none is anonymous; over ssh a bound key, else the
// agent, else helpers.ErrGitSSHNoCredential, with a bounded dial and known_hosts.
func authFor(u gitsource.URL, cred gitsource.Credential) (transport.AuthMethod, error) {
	switch u.Scheme {
	case protocolHTTP, protocolHTTPS:
		if cred.Password == "" {
			return nil, nil //nolint:nilnil // nil auth is go-git's spelling of an anonymous session
		}
		return &githttp.BasicAuth{Username: cred.Username, Password: cred.Password}, nil
	case protocolSSH:
		return sshAuthFor(u, cred)
	default:
		return nil, fmt.Errorf("%w: scheme %q", helpers.ErrInvalidGitURL, u.Scheme)
	}
}

func sshAuthFor(u gitsource.URL, cred gitsource.Credential) (transport.AuthMethod, error) {
	var (
		method interface {
			gogitssh.AuthMethod
			helper() *gogitssh.HostKeyCallbackHelper
		}
		err error
	)
	if len(cred.SSHKey) > 0 {
		keys, keyErr := gogitssh.NewPublicKeys(u.User, cred.SSHKey, cred.SSHPassphrase)
		if keyErr != nil {
			return nil, fmt.Errorf("%w: the ssh key bound to %s does not parse: %w",
				helpers.ErrGitCredentialInvalid, u.Origin(), keyErr)
		}
		method = publicKeys{keys}
	} else {
		agent, agentErr := gogitssh.NewSSHAgentAuth(u.User)
		if agentErr != nil {
			return nil, fmt.Errorf("%w: no key is bound for %s and no agent answers on SSH_AUTH_SOCK: %w",
				helpers.ErrGitSSHNoCredential, u.Origin(), agentErr)
		}
		method = agentKeys{agent}
	}
	db, err := gogitssh.NewKnownHostsDb()
	if err != nil {
		return nil, fmt.Errorf("%w: loading known_hosts: %w", helpers.ErrGitTransportFailed, err)
	}
	port := u.Port
	if port == "" {
		port = sshDefaultPort
	}
	hostWithPort := net.JoinHostPort(strings.Trim(u.Host, "[]"), port)
	h := method.helper()
	h.HostKeyCallback = db.HostKeyCallback()
	h.HostKeyAlgorithms = db.HostKeyAlgorithms(hostWithPort)
	return timedAuth{AuthMethod: method}, nil
}

// publicKeys and agentKeys expose the embedded HostKeyCallbackHelper of the
// two go-git auth types through one accessor, so sshAuthFor can set the
// host-key policy on either without repeating itself.
type publicKeys struct{ *gogitssh.PublicKeys }

func (p publicKeys) helper() *gogitssh.HostKeyCallbackHelper {
	return &p.HostKeyCallbackHelper
}

type agentKeys struct{ *gogitssh.PublicKeysCallback }

func (a agentKeys) helper() *gogitssh.HostKeyCallbackHelper {
	return &a.HostKeyCallbackHelper
}

// timedAuth bounds the ssh dial. go-git dials with context.Background and
// only the client config's Timeout, so without this a black-holed host would
// be bounded by nothing but the operating system's connect timeout.
type timedAuth struct {
	gogitssh.AuthMethod
}

func (a timedAuth) ClientConfig() (*ssh.ClientConfig, error) {
	cfg, err := a.AuthMethod.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.Timeout = helpers.FetchDialContextTimeout
	return cfg, nil
}

// classifyTransportError maps a refused credential or host key to
// ErrGitAuthFailed, an empty remote to ErrGitRefNotFound, and all else, even a
// 404 (GitHub's answer for a private repo), to ErrGitTransportFailed.
func classifyTransportError(err error, display string) error {
	if err == nil {
		return nil
	}
	if isContextError(err) {
		return err
	}
	var keyErr *knownhosts.KeyError
	switch {
	case errors.Is(err, transport.ErrAuthenticationRequired), errors.Is(err, transport.ErrAuthorizationFailed):
		return fmt.Errorf("%w: %s: %w", helpers.ErrGitAuthFailed, display, err)
	case errors.As(err, &keyErr):
		return fmt.Errorf("%w: %s: host key is not vouched for by known_hosts: %w", helpers.ErrGitAuthFailed, display, err)
	case strings.Contains(err.Error(), "unable to authenticate"):
		return fmt.Errorf("%w: %s: %w", helpers.ErrGitAuthFailed, display, err)
	case errors.Is(err, transport.ErrEmptyRemoteRepository):
		return fmt.Errorf("%w: %s advertises no refs", helpers.ErrGitRefNotFound, display)
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return fmt.Errorf("%w: %s: repository not found (or not readable with this credential)", helpers.ErrGitTransportFailed, display)
	default:
		return fmt.Errorf("%w: %s: %w", helpers.ErrGitTransportFailed, display, err)
	}
}
