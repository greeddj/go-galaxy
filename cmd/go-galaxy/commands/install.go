package commands

import (
	"context"
	"net/http"
	"net/url"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// Install returns the CLI command that installs the collections and roles of
// the requirements file.
func Install() *cli.Command {
	flags := cliflags.CollectionFlags()
	flags = append(flags, cliflags.SignatureFlags()...)
	flags = append(flags, cliflags.S3Flags()...)

	return &cli.Command{
		Name:    "install",
		Aliases: []string{"i"},
		Usage:   "Install collections and roles from requirements file",
		Flags:   flags,
		Action: func(ctx context.Context, c *cli.Command) error {
			return runCollectionCommand(ctx, c, collections.Start)
		},
	}
}

// newHTTPClient builds the Galaxy HTTP client, offline-aware, attaching each
// server's token and relaxed TLS policy to that server's exact origin only.
func newHTTPClient(cfg *config.Config) *http.Client {
	if cfg != nil && cfg.Offline {
		return fetch.NewOffline(cfg.Timeout)
	}
	return fetch.New(cfg.Timeout, serverAuths(cfg.Servers))
}

// serverAuths converts cfg.Servers into fetch.ServerAuth. It is the only Reveal
// call site for a Galaxy token (fetch sits below config and cannot hold a
// Secret), and it reveals only to put the token on the wire.
func serverAuths(servers []config.Server) []fetch.ServerAuth {
	auths := make([]fetch.ServerAuth, 0, len(servers))
	for _, s := range servers {
		parsed, err := url.Parse(s.URL)
		if err != nil {
			// config already normalized and validated s.URL, so this guards
			// that invariant rather than a case reachable today.
			continue
		}
		auths = append(auths, fetch.ServerAuth{
			Origin:      helpers.Origin(parsed),
			Token:       s.Token.Reveal(),
			InsecureTLS: s.InsecureSkipTLSVerify,
		})
	}
	return auths
}

// gitCredentials converts cfg.GitCredentials into gitsource.Credential. It is
// the only Reveal call site for a git credential, and its plaintext result
// must never be printed, logged or persisted.
func gitCredentials(cfg *config.Config) []gitsource.Credential {
	if cfg == nil {
		return nil
	}
	creds := make([]gitsource.Credential, 0, len(cfg.GitCredentials))
	for _, c := range cfg.GitCredentials {
		creds = append(creds, gitsource.Credential{
			URL:           c.URL,
			Username:      c.Username,
			Password:      c.Password.Reveal(),
			SSHKey:        []byte(c.SSHKeyPEM.Reveal()),
			SSHPassphrase: c.SSHPassphrase.Reveal(),
		})
	}
	return creds
}

// urlBindings converts cfg.URLCredentials into fetch.URLBinding, the only Reveal
// call site for a url token. urlsource.Prefix.Origin must render an origin as
// helpers.Origin does, since the transport matches the two byte for byte.
func urlBindings(cfg *config.Config) []fetch.URLBinding {
	if cfg == nil {
		return nil
	}
	bindings := make([]fetch.URLBinding, 0, len(cfg.URLCredentials))
	for _, c := range cfg.URLCredentials {
		bindings = append(bindings, fetch.URLBinding{
			Origin:     c.URL.Origin(),
			PathPrefix: c.URL.Path,
			Token:      c.Token.Reveal(),
		})
	}
	return bindings
}
