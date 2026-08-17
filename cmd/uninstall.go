package cmd

import (
	"context"
	"fmt"

	"github.com/lesomnus/cld/internal/installer"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/z"
	"github.com/moby/moby/client"
)

func NewCmdUninstall() *xli.Command {
	return &xli.Command{
		Name:  "uninstall",
		Brief: "stop and remove the cld daemon container and the engines it ran",
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			cli, err := client.New(client.FromEnv)
			if err != nil {
				return z.Err(err, "docker client")
			}
			defer cli.Close()

			removed, err := installer.Uninstall(ctx, cli)
			if err != nil {
				return err
			}
			if removed {
				fmt.Fprintln(cmd.ErrWriter, "cld: daemon removed (conversation backups under the data dir are kept)")
			} else {
				fmt.Fprintln(cmd.ErrWriter, "cld: no daemon container found")
			}

			// The engines cld ran for sessions go too: they are privileged
			// containers whose only purpose was to serve those sessions. Their
			// build caches are named volumes and are kept, so a later install
			// starts warm. Reported separately, and a failure here does not
			// undo the daemon removal above.
			engines, err := installer.RemoveEngines(ctx, cli)
			if engines > 0 {
				fmt.Fprintf(cmd.ErrWriter,
					"cld: %d docker engine(s) removed (their image and build caches are kept)\n", engines)
			}
			return err
		}),
	}
}
