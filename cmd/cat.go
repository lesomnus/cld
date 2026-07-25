package cmd

import (
	"context"
	"fmt"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
)

func new_cmd_settings_cat() *xli.Command {
	return &xli.Command{
		Name:  "cat",
		Brief: "print a devcontainer's effective Claude Code config",
		Args: arg.Args{
			&arg.String{Name: "name", Brief: "devcontainer name (`cld ls`); default: this container's own / the only one", Optional: true, Handler: completeNames()},
			&arg.String{Name: "file", Brief: "which config file: `settings` (default) or `claude-md`", Optional: true, Handler: completeEditTargets()},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			which, _ := arg.Get[string](cmd, "file")
			t, err := resolveEditTarget(which)
			if err != nil {
				return err
			}

			name, _ := arg.Get[string](cmd, "name")
			if name == "" {
				// Like `cld it`, a bare `cld cat` targets the sole devcontainer —
				// which, inside a managed container, is the caller's own.
				resolved, err := sole_devcontainer(ctx, c.SocketPath())
				if err != nil {
					return err
				}
				name = resolved
			} else if target := find_target(ctx, c.SocketPath(), name); target != nil {
				// Accept a short alias too.
				name = target.Name
			}

			data, err := daemon.GetClaudeConfig(ctx, c.SocketPath(), name, t.name)
			if err != nil {
				return err
			}

			// Print the file verbatim so it stays pipeable (e.g. `cld cat app | jq`).
			if _, err := cmd.Writer.Write(data); err != nil {
				return err
			}
			// A file with no trailing newline would otherwise run into the shell
			// prompt; add one for terminal readability without altering piped bytes
			// that already end in one.
			if len(data) > 0 && data[len(data)-1] != '\n' {
				fmt.Fprintln(cmd.Writer)
			}
			return nil
		}),
	}
}
