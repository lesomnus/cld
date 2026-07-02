package cmd

import (
	"context"
	"log/slog"

	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/xli/frm"
	"github.com/lesomnus/z"
)

func NewCmdRoot() *xli.Command {
	return &xli.Command{
		Name: "cld",

		Flags: flg.Flags{
			&flg.String{Name: "config", Brief: "path to config file"},
		},

		Commands: []*xli.Command{
			NewCmdVersion(),
			NewCmdConfig(),
			NewCmdServe(),
			NewCmdLs(),
			NewCmdIt(),
			NewCmdX(),
		},

		Handler: xli.Chain(
			xli.RequireSubcommand(),
			xli.OnRunPass(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
				f := frm.From(ctx).Next()

				// `version` needs nothing; `x` runs inside the container or as
				// a tmux pane client with its own docker client, and must not
				// read a cwd-relative cld.yaml that belongs to the project.
				if frm.HasSeq(f, "version") || frm.HasSeq(f, "x") {
					return next(ctx)
				}

				ctx, c, err := UseConfigInit(ctx, cmd)
				if err != nil {
					return err
				}

				// Only the daemon emits telemetry. Client commands own their
				// stdout (`config` prints YAML, `ls` a table, `it` hands over
				// the terminal), so building otel could corrupt it and a bad
				// otel config should not break inspecting the config.
				if !frm.HasSeq(f, "serve") {
					return next(ctx)
				}

				ctx, o, err := c.Otel.Build(ctx)
				if err != nil {
					return z.Err(err, "build otel")
				}
				if err := o.Start(ctx); err != nil {
					return z.Err(err, "start otel")
				}
				defer o.Shutdown(ctx)

				l := log.From(ctx)
				if p := c.Path(); p == "" {
					l.Info("use default config")
				} else {
					l.Info("config loaded", slog.String("path", p))
				}

				return next(ctx)
			}),
		),
	}
}
