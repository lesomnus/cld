package cmd

import (
	"context"
	"fmt"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
)

func NewCmdPurge() *xli.Command {
	return &xli.Command{
		Name:  "purge",
		Brief: "stop and remove a devcontainer, deleting its volumes and conversation backup",
		Flags: flg.Flags{
			&flg.Switch{Name: "all", Brief: "purge every devcontainer cld manages"},
			&flg.Switch{Name: "yes", Alias: 'y', Brief: "skip the confirmation prompt"},
		},
		Args: arg.Args{
			&arg.String{Name: "name", Brief: "devcontainer name as shown by `cld ls`", Optional: true, Handler: completeNames()},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			name, _ := arg.Get[string](cmd, "name")
			all, _ := flg.Get[bool](cmd, "all")
			yes, _ := flg.Get[bool](cmd, "yes")

			if all {
				if name != "" {
					return fmt.Errorf("pass a name or --all, not both")
				}
				return purge_all(ctx, cmd, c.SocketPath(), yes)
			}
			if name == "" {
				return fmt.Errorf("name required (or pass --all to purge every devcontainer)")
			}

			// Accept a short alias in place of the display name.
			if t := find_target(ctx, c.SocketPath(), name); t != nil {
				name = t.Name
			}

			// A single purge is irreversible — its volumes and conversation backup
			// are deleted — so it asks first unless -y, unlike `cld down`.
			if !yes {
				fmt.Fprintf(cmd.ErrWriter, "Purge %s? Its named volumes and conversation backup will be DELETED — this cannot be undone.\n", name)
				fmt.Fprint(cmd.ErrWriter, "Proceed? [y/N] ")
				if !confirmed(cmd.ReadCloser) {
					fmt.Fprintln(cmd.ErrWriter, "cld: aborted")
					return nil
				}
			}

			if err := daemon.Purge(ctx, c.SocketPath(), name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrWriter, "cld: purged %s\n", name)
			return nil
		}),
	}
}

// purge_all removes every devcontainer cld manages and deletes each one's named
// volumes and conversation backup. Scope is enforced by the daemon exactly as in
// down --all — a container labelled cld.ignore, matched by an ignore glob, or
// that is not a devcontainer at all is never touched. Because it is bulk and
// wholly irreversible, it lists what will go and asks first unless yes.
func purge_all(ctx context.Context, cmd *xli.Command, socket string, yes bool) error {
	items, err := daemon.FetchItems(ctx, socket)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Fprintln(cmd.ErrWriter, "cld: no devcontainers to purge")
		return nil
	}

	if !yes {
		fmt.Fprintf(cmd.ErrWriter, "The following %d devcontainer(s) will be stopped and removed\n", len(items))
		fmt.Fprintln(cmd.ErrWriter, "(their named volumes and conversation backups will be DELETED — this cannot be undone):")
		for _, it := range items {
			fmt.Fprintf(cmd.ErrWriter, "  - %s\n", it.Name)
		}
		fmt.Fprint(cmd.ErrWriter, "Proceed? [y/N] ")
		if !confirmed(cmd.ReadCloser) {
			fmt.Fprintln(cmd.ErrWriter, "cld: aborted")
			return nil
		}
	}

	results, err := daemon.PurgeAll(ctx, socket)
	if err != nil {
		return err
	}
	return report_teardown(cmd, results, "purged", "purge")
}
