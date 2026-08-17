package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/termx"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
	"github.com/moby/moby/client"
)

// NewCmdDocker drives the engine cld runs for a devcontainer
// (`docker: {mode: dind}`). That engine lives on a private network with the
// devcontainer and is deliberately not reachable from the host, so the command
// runs inside the engine's own container — where the image already ships the
// docker CLI — rather than dialing it.
func NewCmdDocker() *xli.Command {
	return &xli.Command{
		Name:  "docker",
		Brief: "run a docker command against the engine cld runs for a devcontainer",
		Flags: flg.Flags{
			&flg.String{Name: "name", Brief: "devcontainer name (`cld ls`); default: the only one", Handler: completeNames()},
		},
		Args: arg.Args{
			&arg.Remains{Name: "args", Brief: "arguments for `docker` (none: describe the engine)", Optional: true},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			name, _ := flg.Get[string](cmd, "name")
			if name == "" {
				resolved, err := sole_devcontainer(ctx, c.SocketPath())
				if err != nil {
					return err
				}
				name = resolved
			} else if target := find_target(ctx, c.SocketPath(), name); target != nil {
				name = target.Name
			}

			engine, err := daemon.GetDockerEngine(ctx, c.SocketPath(), name)
			if err != nil {
				return err
			}

			args, _ := arg.Get[[]string](cmd, "args")
			if len(args) == 0 {
				// A bare `cld docker` says what and where the engine is, which
				// is the first thing anyone wants to know about it.
				state := "stopped"
				if engine.Running {
					state = "running"
				}
				fmt.Fprintf(cmd.Writer, "%s\t%s\t%s\n", engine.Name, engine.Endpoint, state)
				return nil
			}
			if !engine.Running {
				return fmt.Errorf("engine %s is not running; start the devcontainer to bring it up", engine.Name)
			}

			cli, err := client.New(client.FromEnv)
			if err != nil {
				return z.Err(err, "docker client")
			}
			defer cli.Close()

			// The terminal is handed over, so `cld docker -- run -it alpine sh`
			// and `cld docker -- logs -f` behave like the real thing.
			code, err := termx.Run(ctx, cli, termx.ExecOptions{
				Container: engine.Container,
				Cmd:       append([]string{"docker"}, args...),
			})
			if err != nil {
				return z.Err(err, "run docker in %s (this needs access to the host's own engine)", engine.Name)
			}
			os.Exit(code)
			return nil
		}),
	}
}
