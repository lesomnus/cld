package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/termx"
	"github.com/lesomnus/cld/internal/watchx"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
	"github.com/moby/moby/client"
)

// NewCmdX groups internal commands: the exec-attach client that tmux panes
// run, and the in-container file watcher.
func NewCmdX() *xli.Command {
	return &xli.Command{
		Name:  "x",
		Brief: "internal commands used by the daemon",
		Commands: []*xli.Command{
			new_cmd_x_exec(),
			new_cmd_x_watch(),
		},
		Handler: xli.RequireSubcommand(),
	}
}

func new_cmd_x_exec() *xli.Command {
	return &xli.Command{
		Name:  "exec",
		Brief: "run a command in a container with this terminal attached",
		Flags: flg.Flags{
			&flg.String{Name: "user", Brief: "user to run as"},
			&flg.String{Name: "workdir", Brief: "working directory in the container"},
			&flg.String{Name: "config-dir", Brief: "value for CLAUDE_CONFIG_DIR"},
			&flg.String{Name: "notify", Brief: "daemon socket to notify when the command exits"},
		},
		Args: arg.Args{
			&arg.String{Name: "container", Brief: "container ID"},
			&arg.Remains{Name: "cmd", Brief: "command to run"},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			ctr := arg.MustGet[string](cmd, "container")
			argv := arg.MustGet[[]string](cmd, "cmd")

			cli, err := client.New(client.FromEnv)
			if err != nil {
				return z.Err(err, "docker client")
			}
			defer cli.Close()

			env := []string{
				"DISABLE_AUTOUPDATER=1",
				"TERM=xterm-256color",
			}
			if v, ok := flg.Find[string](cmd, "config-dir"); ok {
				env = append(env, "CLAUDE_CONFIG_DIR="+v)
			}

			o := termx.ExecOptions{
				Container: ctr,
				Env:       env,
				Cmd:       argv,
			}
			flg.VisitP(cmd, "user", &o.User)
			flg.VisitP(cmd, "workdir", &o.WorkingDir)

			code, err := termx.Run(ctx, cli, o)

			if socket, ok := flg.Find[string](cmd, "notify"); ok {
				nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				daemon.NotifyExited(nctx, socket, ctr)
				cancel()
			}

			if err != nil {
				return err
			}
			os.Exit(code)
			return nil
		}),
	}
}

func new_cmd_x_watch() *xli.Command {
	return &xli.Command{
		Name:  "watch",
		Brief: "print changed paths under a directory, one per line",
		Args: arg.Args{
			&arg.String{Name: "dir", Brief: "directory to watch recursively"},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			dir := arg.MustGet[string](cmd, "dir")

			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()

			err := watchx.Run(ctx, dir, cmd)
			if err == context.Canceled {
				return nil
			}
			return err
		}),
	}
}
