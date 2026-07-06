package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
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
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", it.Name, it.Alias, id, it.Status, it.Version, abbreviate_home(it.LocalFolder))
			}
			return w.Flush()
		}),
	}
}

// abbreviate_home shortens a path under this client's home directory to a
// leading "~". The local folder is a host path, so this only fires when the
// client shares that home (running on the host); run inside a container with a
// different home it leaves the full path, never mis-abbreviating it.
func abbreviate_home(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return abbreviate_home_in(p, home)
}

func abbreviate_home_in(p, home string) string {
	if p == "" || home == "" || home == "/" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+"/"); ok {
		return "~/" + rest
	}
	return p
}
