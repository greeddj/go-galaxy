package gitfetch

import (
	"fmt"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	protocolFile  = "file"
	protocolGit   = "git"
	protocolHTTP  = "http"
	protocolHTTPS = "https"
	protocolSSH   = "ssh"
)

// hardenOnce guards the one-time, idempotent edit of go-git's process-global
// transport registry and ssh config reader; every Fetcher constructor runs it.
//
//nolint:gochecknoglobals // go-git's transport registry is process-global; this Once is its single editor
var hardenOnce sync.Once

// harden deregisters go-git's file transport (it execs git-upload-pack) and git
// transport (plaintext TCP) behind gitsource.ParseURL's refusal, and stops
// ~/.ssh/config from redirecting which host a run dials and verifies.
func harden() {
	hardenOnce.Do(func() {
		client.InstallProtocol(protocolFile, nil)
		client.InstallProtocol(protocolGit, nil)
		gogitssh.DefaultSSHConfig = nil
	})
}

// transportFor picks an endpoint's transport: http(s) on this Fetcher's own
// client, never go-git's global registry, and ssh on go-git's default client
// with per-session auth from authFor; any other protocol is refused.
func (f *Fetcher) transportFor(ep *transport.Endpoint) (transport.Transport, error) {
	switch ep.Protocol {
	case protocolHTTP, protocolHTTPS:
		return githttp.NewClient(f.httpClient), nil
	case protocolSSH:
		return gogitssh.DefaultClient, nil
	default:
		return nil, fmt.Errorf("%w: protocol %q is not one this tool speaks", helpers.ErrInvalidGitURL, ep.Protocol)
	}
}
