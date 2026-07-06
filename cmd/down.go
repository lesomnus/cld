package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
)

func NewCmdDown() *xli.Command {
	return &xli.Command{
		Name:  "down",
		Brief: "stop and remove a devcontainer (its conversation backup is kept)",
		Flags: flg.Flags{
			&flg.Switch{Name: "all", Brief: "remove every devcontainer cld manages"},
			&flg.Switch{Name: "yes", Alias: 'y', Brief: "skip the confirmation prompt (with --all)"},
		},
		Args: arg.Args{
			&arg.String{Name: "name", Brief: "devcontainer name as shown by `cld ls`", Optional: true, Handler: completeNames()},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			name, _ := arg.Get[string](cmd, "name")
			all, _ := flg.Get[bool](cmd, "all")

			if all {
				if name != "" {
					return fmt.Errorf("pass a name or --all, not both")
				}
				yes, _ := flg.Get[bool](cmd, "yes")
				return down_all(ctx, cmd, c.SocketPath(), yes)
			}
			if name == "" {
				return fmt.Errorf("name required (or pass --all to remove every devcontainer)")
			}

			// Accept a short alias in place of the display name.
			if t := find_target(ctx, c.SocketPath(), name); t != nil {
				name = t.Name
			}

			if err := daemon.Down(ctx, c.SocketPath(), name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrWriter, "cld: removed %s\n", name)
			return nil
		}),
	}
}

// down_all removes every devcontainer cld manages. Scope is enforced by the
// daemon, which re-validates each container against the live ignore/devcontainer
// gate before removing it — so a container labelled cld.ignore, matched by an
// ignore glob, or that is not a devcontainer at all is never touched. Because it
// is bulk and irreversible for the containers (the conversation backups are
// still kept), it lists what will go and asks first unless yes.
func down_all(ctx context.Context, cmd *xli.Command, socket string, yes bool) error {
	items, err := daemon.FetchItems(ctx, socket)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Fprintln(cmd.ErrWriter, "cld: no devcontainers to remove")
		return nil
	}

	if !yes {
		fmt.Fprintf(cmd.ErrWriter, "The following %d devcontainer(s) will be stopped and removed\n", len(items))
		fmt.Fprintln(cmd.ErrWriter, "(named volumes and conversation backups are kept):")
		for _, it := range items {
			fmt.Fprintf(cmd.ErrWriter, "  - %s\n", it.Name)
		}
		fmt.Fprint(cmd.ErrWriter, "Proceed? [y/N] ")
		if !confirmed(cmd.ReadCloser) {
			fmt.Fprintln(cmd.ErrWriter, "cld: aborted")
			return nil
		}
	}

	results, err := daemon.DownAll(ctx, socket)
	if err != nil {
		return err
	}

	failed := 0
	for _, r := range results {
		label := r.Name
		if label == "" {
			label = r.ID
		}
		if r.OK {
			fmt.Fprintf(cmd.ErrWriter, "cld: removed %s\n", label)
			continue
		}
		failed++
		fmt.Fprintf(cmd.ErrWriter, "cld: failed to remove %s: %s\n", label, r.Error)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d devcontainer(s) failed to remove", failed, len(results))
	}
	return nil
}

// confirmed reports whether r's next line is an affirmative (y/yes,
// case-insensitive). A blank line, EOF, or non-interactive stdin scans nothing
// and errors, which reads as "no" — nothing is removed without explicit consent.
func confirmed(r io.Reader) bool {
	var answer string
	if _, err := fmt.Fscanln(r, &answer); err != nil {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(answer))
	return a == "y" || a == "yes"
}
