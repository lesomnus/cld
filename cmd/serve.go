package cmd

import (
	"context"
	"errors"
	"os"
	"syscall"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/z"
	"github.com/moby/moby/client"

	"os/signal"
)

func NewCmdServe() *xli.Command {
	return &xli.Command{
		Name:  "serve",
		Brief: "watch docker events and provision devcontainers with claude",
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			cli, err := client.New(client.FromEnv)
			if err != nil {
				return z.Err(err, "docker client")
			}
			defer cli.Close()

			d, err := daemon.New(c, cli, log.From(ctx))
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()

			err = d.Run(ctx)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}),
	}
}
