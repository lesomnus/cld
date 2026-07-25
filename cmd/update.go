package cmd

import (
	"context"
	"fmt"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
)

func NewCmdUpdate() *xli.Command {
	return &xli.Command{
		Name:  "update",
		Brief: "reinstall the latest Claude Code into a devcontainer and restart its session",
		Flags: flg.Flags{
			&flg.Switch{Name: "all", Brief: "update every devcontainer cld manages"},
			&flg.String{Name: "channel", Brief: "release channel to install (`stable` or `latest`); default: the daemon's configured channel"},
		},
		Args: arg.Args{
			&arg.String{Name: "name", Brief: "devcontainer name (`cld ls`); default: this container's own / the only one", Optional: true, Handler: completeNames()},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			name, _ := arg.Get[string](cmd, "name")
			all, _ := flg.Get[bool](cmd, "all")
			channel, _ := flg.Get[string](cmd, "channel")
			if err := validate_channel(channel); err != nil {
				return err
			}

			if all {
				if name != "" {
					return fmt.Errorf("pass a name or --all, not both")
				}
				return update_all(ctx, cmd, c.SocketPath(), channel)
			}

			// Like `cld it`, a bare `cld update` targets the sole devcontainer —
			// which, inside a managed container, is the caller's own.
			if name == "" {
				resolved, err := sole_devcontainer(ctx, c.SocketPath())
				if err != nil {
					return err
				}
				name = resolved
			} else if t := find_target(ctx, c.SocketPath(), name); t != nil {
				// Accept a short alias too.
				name = t.Name
			}

			res, err := daemon.UpdateClaude(ctx, c.SocketPath(), name, channel)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrWriter, "cld: updated %s to Claude Code %s\n", res.Name, res.Version)
			return nil
		}),
	}
}

// validate_channel rejects a --channel value that is not one the release server
// serves, so a typo fails fast with a clear message instead of a cryptic 404
// from the daemon. An empty value means "use the daemon's configured channel".
func validate_channel(channel string) error {
	switch channel {
	case "", "stable", "latest":
		return nil
	default:
		return fmt.Errorf("unknown channel %q: use `stable` or `latest`", channel)
	}
}

// update_all re-injects the current Claude Code binary into every devcontainer
// cld manages and restarts each session. Scope is enforced by the daemon, which
// only ever tracks cld-managed devcontainers; a container without a live session
// is skipped and omitted from the report.
func update_all(ctx context.Context, cmd *xli.Command, socket string, channel string) error {
	results, err := daemon.UpdateClaudeAll(ctx, socket, channel)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Fprintln(cmd.ErrWriter, "cld: no running devcontainers to update")
		return nil
	}

	failed := 0
	for _, r := range results {
		label := r.Name
		if label == "" {
			label = r.ID
		}
		if r.OK {
			fmt.Fprintf(cmd.ErrWriter, "cld: updated %s to Claude Code %s\n", label, r.Version)
			continue
		}
		failed++
		fmt.Fprintf(cmd.ErrWriter, "cld: failed to update %s: %s\n", label, r.Error)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d devcontainer(s) failed to update", failed, len(results))
	}
	return nil
}
