package commands

import (
	"io"
	"log"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/urfave/cli/v2"
)

// Install returns the CLI command that installs collections from requirements.
func Install() *cli.Command {
	flags := helpers.CommonFlags()
	flags = append(flags, helpers.CollectionFlags()...)
	flags = append(flags, helpers.S3Flags()...)

	return &cli.Command{
		Name:    "install",
		Aliases: []string{"i"},
		Usage:   "Install collections from requirements file",
		Flags:   flags,
		Action: func(c *cli.Context) error {
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
			runtime := infra.New(p, fetch.New(cfg.Timeout))
			runtime.DebugAnsibleConfig(cfg)
			return collections.Start(c.Context, cfg, runtime)
		},
	}
}
