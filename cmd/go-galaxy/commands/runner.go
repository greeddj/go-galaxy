package commands

import (
	"context"
	"io"
	"log"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitfetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/urfave/cli/v3"
)

// collectionAction is the common run signature for
// install/cleanup/lock/warm/outdated.
type collectionAction func(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error

// runCollectionCommand is the shared body of install/cleanup/lock/warm/outdated
// CLI actions: build cfg, configure progress + logging, hand off to action.
func runCollectionCommand(ctx context.Context, c *cli.Command, action collectionAction) error {
	cfg, err := config.BuildCollectionConfig(c)
	if err != nil {
		return err
	}
	p := progress.New(cfg.Verbose, cfg.Quiet)
	if cfg.Verbose {
		log.SetOutput(p)
	} else {
		log.SetOutput(io.Discard)
	}
	defer p.Close()
	runtime := infra.New(p, newHTTPClient(cfg))
	// The git client is wired for every command: it holds no connection until
	// a git requirement is met, and its own HTTP client (fetch.NewGit) keeps
	// any Galaxy token and relaxed TLS policy away from a repository.
	runtime.Git = gitfetch.New(fetch.NewGit(cfg.Timeout), runtime.TempDir)
	runtime.GitCredentials = gitCredentials(cfg)
	// The url client is wired the same way, and fetch.NewURLDownload keeps
	// any Galaxy token and relaxed TLS policy away from an artifact host.
	runtime.URLHTTP = fetch.NewURLDownload(cfg.Timeout, cfg.Offline, urlBindings(cfg))
	runtime.DebugAnsibleConfig(cfg)
	runtime.WarnConfig(cfg)
	return action(ctx, cfg, runtime)
}
