package commands

import (
	"io"
	"log"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/cleanup"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/urfave/cli/v2"
)

// Cleanup returns the CLI command that removes unused cached collections.
func Cleanup() *cli.Command {
	flags := helpers.CommonFlags()
	flags = append(flags, helpers.S3Flags()...)

	return &cli.Command{
		Name:    "cleanup",
		Aliases: []string{"c"},
		Usage:   "Cleanup unused cached collections across all projects",
		Flags:   flags,
		Action: func(c *cli.Context) error {
			cfg, err := config.BuildCollectionConfig(c)
			if err != nil {
				progress.Errorf("%s", err.Error())
				return err
			}
			p := progress.New(cfg.Verbose, cfg.Quiet)
			if cfg.Verbose {
				log.SetOutput(p)
			} else {
				log.SetOutput(io.Discard)
			}
			defer p.Close()
			runtime := infra.New(p, fetch.New(cfg.Timeout))
			runtime.DebugAnsibleConfig(cfg)
			return cleanup.Start(c.Context, cfg, runtime)
		},
	}
}
