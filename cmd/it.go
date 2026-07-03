package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/termx"
	"github.com/lesomnus/cld/internal/tmuxx"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
	"github.com/moby/moby/client"
)

func NewCmdIt() *xli.Command {
	return &xli.Command{
		Name:  "it",
		Brief: "attach to the claude session of a devcontainer",
		Flags: flg.Flags{
			&flg.Switch{Name: "new", Brief: "recreate the session if the user had ended it"},
		},
		Args: arg.Args{
			&arg.String{Name: "name", Brief: "devcontainer name as shown by `cld ls`"},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			name := arg.MustGet[string](cmd, "name")
			session := devc.SessionName(name)

			if v, _ := flg.Get[bool](cmd, "new"); v {
				if err := daemon.RecreateSession(ctx, c.SocketPath(), name); err != nil {
					return err
				}
			}

			// Publish this login session's ssh-agent for forwarding into the
			// container before we hand the terminal over.
			prepareHostShare(c)

			// Ask the daemon where the tmux server lives. When the daemon runs
			// in a container, attach through a docker exec into it — the host
			// then needs no tmux at all. Without a reachable daemon, fall back
			// to a local attach so `cld it` keeps working while only the
			// daemon is down.
			ictx, cancel := context.WithTimeout(ctx, 2*time.Second)
			info, ierr := daemon.FetchInfo(ictx, c.SocketPath())
			cancel()
			if ierr == nil && info.ContainerID != "" {
				return attach_via_exec(ctx, info, session, name, c.SocketPath())
			}
			return attach_local(ctx, c.TmuxSocketPath(), session, name, c.SocketPath())
		}),
	}
}

// attach_local runs the local tmux client against the shared server socket:
// the daemon runs on this host.
func attach_local(ctx context.Context, tmux_socket string, session string, name string, api_socket string) error {
	tmux := &tmuxx.Server{Socket: tmux_socket}
	has, err := tmux.HasSession(ctx, session)
	if err != nil {
		return err
	}
	if !has {
		return fmt.Errorf("no session for %q: %s", name, hint(ctx, api_socket, name))
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
}

// attach_via_exec attaches to the tmux server inside the daemon's container
// with cld's own exec-attach client, as the daemon's uid (tmux rejects other
// users' clients).
func attach_via_exec(ctx context.Context, info *daemon.Info, session string, name string, api_socket string) error {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return z.Err(err, "docker client")
	}
	defer cli.Close()

	term := os.Getenv("TERM")
	if term == "" {
		term = "xterm-256color"
	}

	code, err := termx.Run(ctx, cli, termx.ExecOptions{
		Container: info.ContainerID,
		User:      strconv.Itoa(info.UID),
		// Without a UTF-8 locale tmux renders every non-ASCII glyph to this
		// client as "_".
		Env: []string{"TERM=" + term, "LC_ALL=C.UTF-8"},
		Cmd: []string{"tmux", "-S", info.TmuxSocket, "attach-session", "-t", "=" + session},
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("no session for %q: %s", name, hint(ctx, api_socket, name))
	}
	os.Exit(0)
	return nil
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
			return "the session was closed; reattach with `cld it --new " + name + "`"
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
