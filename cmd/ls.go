package cmd

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/xli"
)

func NewCmdLs() *xli.Command {
	return &xli.Command{
		Name:  "ls",
		Brief: "list devcontainers provisioned with claude",
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			items, err := daemon.FetchItems(ctx, c.SocketPath())
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(cmd, 2, 8, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tALIAS\tCONTAINER\tSTATUS\tVERSION\tLOCAL FOLDER")
			for _, it := range items {
				id := it.ID
				if len(id) > 12 {
					id = id[:12]
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", it.Name, it.Alias, id, it.Status, it.Version, it.LocalFolder)
			}
			return w.Flush()
		}),
	}
}
