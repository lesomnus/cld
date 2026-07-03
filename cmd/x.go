package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lesomnus/cld/internal/agentx"
	"github.com/lesomnus/cld/internal/claude"
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
			new_cmd_x_agent(),
		},
		Handler: xli.RequireSubcommand(),
	}
}

func new_cmd_x_exec() *xli.Command {
	// The daemon owns all session env policy and passes it via repeatable
	// --env; secrets go through --oauth-token-file so the token never appears
	// in the tmux command or `ps`.
	var envs []string
	return &xli.Command{
		Name:  "exec",
		Brief: "run a command in a container with this terminal attached",
		Flags: flg.Flags{
			&flg.String{Name: "user", Brief: "user to run as"},
			&flg.String{Name: "workdir", Brief: "working directory in the container"},
			&flg.String{
				Name:  "env",
				Brief: "environment variable KEY=VALUE (repeatable)",
				// Not mode-gated: xli runs flag handlers before the run mode
				// is set, so a mode.Run gate would never fire and silently
				// drop every --env.
				Handler: flg.Handle(func(_ context.Context, v string) error {
					envs = append(envs, v)
					return nil
				}),
			},
			&flg.String{Name: "oauth-token-file", Brief: "host file holding a Claude Code OAuth token"},
			&flg.String{Name: "notify", Brief: "daemon socket to notify when the command exits"},
			&flg.String{Name: "session-gen", Brief: "session generation token echoed back on notify"},
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

			env := append([]string{}, envs...)
			if f, ok := flg.Find[string](cmd, "oauth-token-file"); ok && f != "" {
				b, err := os.ReadFile(f)
				if err != nil {
					// Don't fail the session, but make the cause visible in
					// the pane instead of an unexplained login prompt.
					fmt.Fprintf(os.Stderr, "cld: cannot read oauth token file %q: %v\n", f, err)
				} else if tok := strings.TrimSpace(string(b)); tok != "" {
					env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+tok)
				}
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
				gen, _ := flg.Get[string](cmd, "session-gen")
				nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				daemon.NotifyExited(nctx, socket, ctr, gen)
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

func new_cmd_x_agent() *xli.Command {
	return &xli.Command{
		Name:  "agent",
		Brief: "serve an ssh-agent socket, relaying connections to the daemon over stdio",
		Args: arg.Args{
			&arg.String{Name: "socket", Brief: "unix socket path to listen on"},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			sock := arg.MustGet[string](cmd, "socket")

			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()

			err := agentx.ListenAndServe(ctx, sock, os.Stdin, os.Stdout)
			if err == context.Canceled {
				return nil
			}
			return err
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

			// Don't watch or emit paths the daemon would discard anyway.
			skip := func(rel string) bool { return claude.Classify(rel) == claude.BackupSkip }
			err := watchx.Run(ctx, dir, cmd, skip)
			if err == context.Canceled {
				return nil
			}
			return err
		}),
	}
}
