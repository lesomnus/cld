package cmd

import (
	"context"
	"fmt"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
)

func NewCmdDown() *xli.Command {
	return &xli.Command{
		Name:  "down",
		Brief: "stop and remove a devcontainer (its conversation backup is kept)",
		Args: arg.Args{
			&arg.String{Name: "name", Brief: "devcontainer name as shown by `cld ls`"},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			name := arg.MustGet[string](cmd, "name")

			if err := daemon.Down(ctx, c.SocketPath(), name); err != nil {
				return err
			}

			fmt.Fprintf(cmd.ErrWriter, "cld: removed %s\n", name)
			return nil
		}),
	}
}
