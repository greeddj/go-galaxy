package fetch

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// NewGit creates the client git remotes are fetched over: no Galaxy token or
// relaxed TLS for any origin, and a redirect off the first request's origin is
// refused, since net/http would forward go-git's Basic Authorization header.
func NewGit(timeout time.Duration) *http.Client {
	client := newClient(timeout, false, nil)
	client.CheckRedirect = checkGitRedirect
	client.Transport = errorBodyCapTransport{base: client.Transport, max: helpers.GitErrorBodyMaxSize}
	return client
}

// errorBodyCapTransport caps a non-2xx body, which go-git reads whole and which
// neither the watchdog (inactivity) nor the pack cap (stored bytes) bounds. It
// wraps the watchdog, so a stalled error body still times out.
type errorBodyCapTransport struct {
	base http.RoundTripper
	max  int64
}

func (t errorBodyCapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil || (resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices) {
		return resp, err
	}
	resp.Body = cappedBody{Reader: io.LimitReader(resp.Body, t.max), Closer: resp.Body}
	return resp, nil
}

// cappedBody is a response body truncated at the cap; Close still closes the
// underlying body so the connection is released.
type cappedBody struct {
	io.Reader
	io.Closer
}

// checkGitRedirect is checkRedirect plus refusing a hop whose scheme, host or
// port differs from via[0], wrapping ErrGitTransportFailed so the refusal
// classifies as the wire failure it is.
func checkGitRedirect(req *http.Request, via []*http.Request) error {
	if err := checkRedirect(req, via); err != nil {
		return err
	}
	if len(via) == 0 {
		return nil
	}
	first := via[0].URL
	if req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
		return fmt.Errorf("%w: redirect from %s to %s leaves the origin; write the address the remote serves",
			helpers.ErrGitTransportFailed, helpers.URLForMessage(first.String()), helpers.URLForMessage(req.URL.String()))
	}
	return nil
}
