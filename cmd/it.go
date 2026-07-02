package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/tmuxx"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
)

func NewCmdIt() *xli.Command {
	return &xli.Command{
		Name:  "it",
		Brief: "attach to the claude session of a devcontainer",
		Args: arg.Args{
			&arg.String{Name: "name", Brief: "devcontainer name as shown by `cld ls`"},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			name := arg.MustGet[string](cmd, "name")

			tmux := &tmuxx.Server{Socket: c.TmuxSocketPath()}
			session := devc.SessionName(name)

			has, err := tmux.HasSession(ctx, session)
			if err != nil {
				return err
			}
			if !has {
				return fmt.Errorf("no session for %q: %s", name, hint(ctx, c.SocketPath(), name))
			}

			bin, err := exec.LookPath("tmux")
			if err != nil {
				return err
			}

			// Drop TMUX so attaching from inside another tmux works.
			env := []string{}
			for _, kv := range os.Environ() {
				if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
					continue
				}
				env = append(env, kv)
			}
			return syscall.Exec(bin, tmux.AttachArgv(session), env)
		}),
	}
}

// hint explains why a session is missing, best-effort via the daemon.
func hint(ctx context.Context, socket string, name string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	items, err := daemon.FetchItems(ctx, socket)
	if err != nil {
		return "is `cld serve` running?"
	}
	for _, it := range items {
		if it.Name != name {
			continue
		}
		switch it.Status {
		case daemon.StatusSessionEnded:
			return "the session was closed; restart the container to get a new one"
		case daemon.StatusProvisioning:
			return "still provisioning; try again in a moment"
		case daemon.StatusFailed:
			return "provisioning failed: " + it.Error
		default:
			return fmt.Sprintf("status is %q", it.Status)
		}
	}
	return "no such devcontainer; see `cld ls`"
}
