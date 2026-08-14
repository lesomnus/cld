package cmd

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
)

func new_cmd_settings_env() *xli.Command {
	return &xli.Command{
		Name:  "env",
		Brief: "print the environment a devcontainer's claude session runs with",
		Flags: flg.Flags{
			&flg.Switch{Name: "export", Brief: "print as shell assignments, without the source column"},
		},
		Args: arg.Args{
			&arg.String{Name: "name", Brief: "devcontainer name (`cld ls`); default: this container's own / the only one", Optional: true, Handler: completeNames()},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			name, _ := arg.Get[string](cmd, "name")
			if name == "" {
				// Like `cld it`, a bare invocation targets the sole devcontainer —
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

			vars, err := daemon.GetSessionEnv(ctx, c.SocketPath(), name)
			if err != nil {
				return err
			}

			if v, _ := flg.Get[bool](cmd, "export"); v {
				for _, e := range vars {
					if e.Unset {
						fmt.Fprintf(cmd.Writer, "unset %s\n", e.Key)
						continue
					}
					fmt.Fprintf(cmd.Writer, "%s=%s\n", e.Key, shellQuote(e.Value))
				}
				return nil
			}

			// Two columns: the assignment, and where it came from. The source is
			// the whole point — a variable that did not take is explained by the
			// layer that won, not by its value.
			tw := tabwriter.NewWriter(cmd.Writer, 2, 8, 2, ' ', 0)
			for _, e := range vars {
				if e.Unset {
					fmt.Fprintf(tw, "%s\t(removed)\t%s\n", e.Key, e.Origin)
					continue
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Key, e.Value, e.Origin)
			}
			return tw.Flush()
		}),
	}
}

// shellQuote makes a value safe to paste into a shell, so `cld setting env
// --export` can be eval'd.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
